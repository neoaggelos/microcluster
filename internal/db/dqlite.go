package db

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	dqlite "github.com/canonical/go-dqlite/app"
	dqliteClient "github.com/canonical/go-dqlite/client"
	"github.com/lxc/lxd/lxd/db/schema"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/cancel"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/tcp"

	"github.com/canonical/microcluster/internal/rest/client"
	"github.com/canonical/microcluster/internal/rest/types"
	"github.com/canonical/microcluster/internal/sys"
)

// DB holds all information internal to the dqlite database.
type DB struct {
	clusterCert *shared.CertInfo // Cluster certificate for dqlite authentication.
	serverCert  *shared.CertInfo // Server certificate for dqlite authentication.
	listenAddr  api.URL          // Listen address for this dqlite node.

	dbName string // This is db.bin.
	os     *sys.OS

	db        *sql.DB
	dqlite    *dqlite.App
	acceptCh  chan net.Conn
	upgradeCh chan struct{}

	openCanceller *cancel.Canceller

	ctx context.Context

	heartbeatLock sync.Mutex
}

// Accept sends the outbound connection through the acceptCh channel to be received by dqlite.
func (db *DB) Accept(conn net.Conn) {
	db.acceptCh <- conn
}

// NewDB creates an empty db struct with no dqlite connection.
func NewDB(serverCert *shared.CertInfo, os *sys.OS, listenAddr api.URL) *DB {
	return &DB{
		serverCert:    serverCert,
		listenAddr:    listenAddr,
		dbName:        filepath.Base(os.DatabasePath()),
		os:            os,
		acceptCh:      make(chan net.Conn),
		upgradeCh:     make(chan struct{}),
		ctx:           context.Background(),
		openCanceller: cancel.New(context.Background()),
	}
}

// Bootstrap dqlite.
func (db *DB) Bootstrap(clusterCert *shared.CertInfo) error {
	var err error
	db.clusterCert = clusterCert
	db.dqlite, err = dqlite.New(db.os.DatabaseDir,
		dqlite.WithAddress(db.listenAddr.URL.Host),
		dqlite.WithExternalConn(db.dialFunc(), db.acceptCh),
		dqlite.WithUnixSocket(os.Getenv(sys.DqliteSocket)))
	if err != nil {
		return fmt.Errorf("Failed to bootstrap dqlite: %w", err)
	}

	return db.Open(true)
}

// Join a dqlite cluster with the address of a member.
func (db *DB) Join(clusterCert *shared.CertInfo, joinAddresses ...string) error {
	for {
		var err error
		db.clusterCert = clusterCert
		db.dqlite, err = dqlite.New(db.os.DatabaseDir,
			dqlite.WithCluster(joinAddresses),
			dqlite.WithAddress(db.listenAddr.URL.Host),
			dqlite.WithExternalConn(db.dialFunc(), db.acceptCh),
			dqlite.WithUnixSocket(os.Getenv(sys.DqliteSocket)))
		if err != nil {
			return fmt.Errorf("Failed to join dqlite cluster %w", err)
		}

		err = db.Open(false)
		if err == nil {
			break
		}

		// If this is a graceful abort, then we should loop back and try to start the database again.
		if errors.Is(err, schema.ErrGracefulAbort) {
			logger.Debug("Closing database after upgrade notification", logger.Ctx{"address": db.listenAddr.String()})
			err = db.db.Close()
			if err != nil {
				logger.Error("Failed to close database", logger.Ctx{"address": db.listenAddr.String(), "error": err})
			}

			continue
		}

		return err
	}

	return nil
}

// StartWithCluster starts up dqlite and joins the cluster.
func (db *DB) StartWithCluster(clusterMembers map[string]types.AddrPort, clusterCert *shared.CertInfo) error {
	allClusterAddrs := []string{}
	for _, clusterMemberAddrs := range clusterMembers {
		allClusterAddrs = append(allClusterAddrs, clusterMemberAddrs.String())
	}

	return db.Join(clusterCert, allClusterAddrs...)
}

// Leader returns a client connected to the leader of the dqlite cluster.
func (db *DB) Leader() (*dqliteClient.Client, error) {
	return db.dqlite.Leader(db.ctx)
}

// Cluster returns information about dqlite cluster members.
func (db *DB) Cluster() ([]dqliteClient.NodeInfo, error) {
	leader, err := db.dqlite.Leader(db.ctx)
	if err != nil {
		return nil, fmt.Errorf("Failed to get dqlite leader: %w", err)
	}

	members, err := leader.Cluster(db.ctx)
	if err != nil {
		return nil, fmt.Errorf("Failed to get dqlite cluster information: %w", err)
	}

	return members, nil
}

// IsOpen returns true only if the DB has been opened and the schema loaded.
func (db *DB) IsOpen() bool {
	if db == nil {
		return false
	}

	return db.openCanceller.Err() != nil
}

// NotifyUpgraded sends a notification that we can stop waiting for a cluster member to be upgraded.
func (db *DB) NotifyUpgraded() {
	select {
	case db.upgradeCh <- struct{}{}:
	default:
	}
}

// dialFunc to be passed to dqlite.
func (db *DB) dialFunc() dqliteClient.DialFunc {
	return func(ctx context.Context, address string) (net.Conn, error) {
		conn, err := dqliteNetworkDial(ctx, address, db)
		if err != nil {
			return nil, fmt.Errorf("Failed to dial https socket: %w", err)
		}

		go db.heartbeat(db.ctx)

		return conn, nil
	}
}

func (db *DB) heartbeat(ctx context.Context) {
	if !db.IsOpen() {
		logger.Debug("Database is not yet open, aborting heartbeat", logger.Ctx{"address": db.listenAddr.String()})
		return
	}

	// Use the heartbeat lock to prevent another heartbeat attempt if we are currently initiating one.
	db.heartbeatLock.Lock()
	defer db.heartbeatLock.Unlock()

	leaderClient, err := db.dqlite.Leader(ctx)
	if err != nil {
		logger.Error("Failed to get dqlite leader", logger.Ctx{"address": db.listenAddr.String(), "error": err})
		return
	}

	leaderInfo, err := leaderClient.Leader(ctx)
	if err != nil {
		logger.Error("Failed to get dqlite leader info", logger.Ctx{"address": db.listenAddr.String(), "error": err})
		return
	}

	// Only send heartbeats from the leader.
	if leaderInfo.Address != db.dqlite.Address() {
		return
	}

	client, err := client.New(db.os.ControlSocket(), nil, nil, false)
	if err != nil {
		logger.Error("Failed to get local client", logger.Ctx{"address": db.listenAddr.String(), "error": err})
		return
	}

	// Initiate a heartbeat from this node.
	err = client.Heartbeat(ctx, types.HeartbeatInfo{BeginRound: true})
	if err != nil {
		logger.Error("Failed to initiate heartbeat round", logger.Ctx{"address": db.dqlite.Address(), "error": err})
		return
	}

	return
}

// dqliteNetworkDial creates a connection to the internal database endpoint.
func dqliteNetworkDial(ctx context.Context, addr string, db *DB) (net.Conn, error) {
	peerCert, err := db.clusterCert.PublicKeyX509()
	if err != nil {
		return nil, err
	}

	config, err := client.TLSClientConfig(db.serverCert, peerCert)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse TLS config: %w", err)
	}

	// Establish the connection
	request := &http.Request{
		Method:     "POST",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Host:       addr,
	}

	path := fmt.Sprintf("https://%s%s", addr, "/cluster/internal/database")
	request.URL, err = url.Parse(path)
	if err != nil {
		return nil, err
	}

	request.Header.Set("Upgrade", "dqlite")
	request.Header.Set("X-Dqlite-Version", fmt.Sprintf("%d", 1))
	request = request.WithContext(ctx)

	revert := revert.New()
	defer revert.Fail()

	deadline, _ := ctx.Deadline()
	dialer := &net.Dialer{Timeout: time.Until(deadline)}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("Failed connecting to HTTP endpoint %q: %w", addr, err)
	}

	revert.Add(func() { conn.Close() })
	logCtx := logger.AddContext(nil, logger.Ctx{"local": conn.LocalAddr().String(), "remote": conn.RemoteAddr().String()})
	logCtx.Debug("Dqlite connected outbound")

	// Set outbound timeouts.
	remoteTCP, err := tcp.ExtractConn(conn)
	if err != nil {
		logCtx.Error("Failed extracting TCP connection from remote connection", logger.Ctx{"error": err})
	} else {
		err := tcp.SetTimeouts(remoteTCP)
		if err != nil {
			logCtx.Error("Failed setting TCP timeouts on remote connection", logger.Ctx{"error": err})
		}
	}

	err = request.Write(conn)
	if err != nil {
		return nil, fmt.Errorf("Failed sending HTTP requrest to %q: %w", request.URL, err)
	}

	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		return nil, fmt.Errorf("Failed to read response: %w", err)
	}

	defer response.Body.Close()

	// If the remote server has detected that we are out of date, let's
	// trigger an upgrade.
	if response.StatusCode == http.StatusUpgradeRequired {
		// TODO: trigger update.
		return nil, fmt.Errorf("Upgrade needed")
	}

	if response.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("Dialing failed: expected status code 101 got %d", response.StatusCode)
	}

	if response.Header.Get("Upgrade") != "dqlite" {
		return nil, fmt.Errorf("Missing or unexpected Upgrade header in response")
	}

	revert.Success()
	return conn, nil
}