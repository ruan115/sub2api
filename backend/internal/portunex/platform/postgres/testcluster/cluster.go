//go:build portunex_integration && (darwin || linux)

package testcluster

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lib/pq"
)

// Cluster always owns a newly initialized, private, socket-only synthetic DB.
// No existing DSN/data directory is accepted and no production schema is loaded.
type Cluster struct {
	DB                                   *sql.DB
	root, data, socket, database, marker string
	cmd                                  *exec.Cmd
	done                                 chan struct{}
	waitErr                              error // Read only after done closes.
	closeOnce                            sync.Once
	closeErr                             error
}

// Start fails (never skips) when the explicit runtime is unavailable or unsafe.
func Start(t testing.TB) *Cluster {
	t.Helper()
	bin := os.Getenv("PORTUNEX_TEST_PG_BIN")
	if checkEnvironment(os.Environ()) != nil || binaryDirectory(bin) != nil || os.Geteuid() == 0 {
		t.Fatal("isolated PostgreSQL requires explicit PORTUNEX_TEST_PG_BIN, a non-root user and no PG*/DATABASE_URL/DSN variables")
	}
	root, err := os.MkdirTemp("", "ptx-pg-")
	if err != nil {
		t.Fatal("cannot create isolated PostgreSQL directory")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		_ = os.RemoveAll(root) // This is the empty directory just created above.
		t.Fatal("cannot resolve isolated PostgreSQL directory")
	}
	root = canonical
	c := &Cluster{root: root, data: filepath.Join(root, "data"), socket: filepath.Join(root, "s"), done: make(chan struct{})}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	if socketPath(c.socket) != nil {
		t.Fatal("isolated PostgreSQL socket path is outside the supported safe subset")
	}
	if err := os.Mkdir(c.socket, 0700); err != nil {
		t.Fatal("cannot create private PostgreSQL socket directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	init := ownedCommand(ctx, filepath.Join(bin, "initdb"), "-D", c.data, "--no-locale", "--encoding=UTF8", "--auth-local=trust", "--auth-host=reject", "-U", "portunex_test_admin", "--no-sync")
	if err := init.Run(); err != nil {
		// A failed bootstrap parent is not proof that all of its children have
		// exited. Keep its new directory for inspection instead of deleting it.
		c.closeErr = errors.New("isolated PostgreSQL bootstrap failed; synthetic data retained")
		t.Fatal("isolated PostgreSQL initdb failed; no existing cluster was used")
	}
	// No network port is opened. Port is only part of the private socket name.
	c.cmd = exec.Command(filepath.Join(bin, "postgres"), "-D", c.data, "-p", "5432",
		"-c", "listen_addresses=", "-c", "unix_socket_directories="+c.socket, "-c", "unix_socket_permissions=0700",
		"-c", "shared_buffers=8MB", "-c", "max_connections=16", "-c", "max_worker_processes=0",
		"-c", "max_wal_senders=0", "-c", "autovacuum=off", "-c", "logging_collector=off",
		"-c", "log_statement=none", "-c", "log_min_error_statement=panic")
	c.cmd.Env = processEnvironment()
	c.cmd.Stdout = io.Discard
	c.cmd.Stderr = io.Discard
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.cmd.Start(); err != nil {
		c.cmd = nil
		t.Fatal("isolated PostgreSQL start failed")
	}
	go func() { c.waitErr = c.cmd.Wait(); close(c.done) }()
	admin, err := c.open("postgres")
	if err != nil {
		t.Fatal("isolated PostgreSQL connector rejected")
	}
	c.DB = admin // Close also handles any failure while bootstrapping.
	ready, readyCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer readyCancel()
	for {
		pingCtx, pingCancel := context.WithTimeout(ready, time.Second)
		err = admin.PingContext(pingCtx)
		pingCancel()
		if err == nil {
			break
		}
		select {
		case <-c.done:
			t.Fatal("isolated PostgreSQL exited before readiness")
		case <-ready.Done():
			t.Fatal("isolated PostgreSQL readiness deadline exceeded")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if c.checkConnection(ready, admin, "postgres") != nil {
		t.Fatal("isolated PostgreSQL identity check failed")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal("cannot generate isolated database identity")
	}
	c.marker = hex.EncodeToString(random)
	c.database = "portunex_test_" + c.marker
	if _, err := admin.ExecContext(ready, "CREATE DATABASE "+pq.QuoteIdentifier(c.database)+" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		t.Fatal("cannot create synthetic database")
	}
	db, err := c.open(c.database)
	if err != nil {
		t.Fatal("cannot open synthetic database")
	}
	if err := admin.Close(); err != nil {
		db.Close()
		t.Fatal("cannot close synthetic bootstrap connection")
	}
	c.DB = db
	if c.checkConnection(ready, db, c.database) != nil {
		t.Fatal("synthetic database identity check failed")
	}
	if _, err := db.ExecContext(ready, "CREATE SCHEMA portunex_recovery_meta; CREATE TABLE portunex_recovery_meta.instance (marker text NOT NULL)"); err != nil {
		t.Fatal("cannot create synthetic instance marker")
	}
	if _, err := db.ExecContext(ready, "INSERT INTO portunex_recovery_meta.instance (marker) VALUES ($1)", c.marker); err != nil {
		t.Fatal("cannot initialize synthetic instance marker")
	}
	var marker string
	if err := db.QueryRowContext(ready, "SELECT marker FROM portunex_recovery_meta.instance").Scan(&marker); err != nil || marker != c.marker {
		t.Fatal("synthetic instance marker check failed")
	}
	return c
}

// ownedCommand kills only its own process group if a bootstrap deadline expires.
func ownedCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = processEnvironment()
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

type socketDialer struct{ address string }

func (d socketDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}
func (d socketDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.DialContext(ctx, network, address)
}
func (d socketDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "unix" || address != d.address {
		return nil, errIsolation
	}
	return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, address)
}

func (c *Cluster) open(database string) (*sql.DB, error) {
	if err := checkEnvironment(os.Environ()); err != nil {
		return nil, err
	}
	connector, err := pq.NewConnector(connectionString(c.socket, database))
	if err != nil {
		return nil, errIsolation
	}
	connector.Dialer(socketDialer{filepath.Join(c.socket, ".s.PGSQL.5432")})
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(6)
	db.SetMaxIdleConns(2)
	return db, nil
}

func (c *Cluster) checkConnection(ctx context.Context, db *sql.DB, database string) error {
	var actualDatabase, user, data, listen, socket string
	var version int
	var address sql.NullString
	err := db.QueryRowContext(ctx, `SELECT current_database(), current_user, current_setting('data_directory'), current_setting('listen_addresses'), current_setting('server_version_num')::int, inet_server_addr()::text, current_setting('unix_socket_directories')`).Scan(&actualDatabase, &user, &data, &listen, &version, &address, &socket)
	if err != nil || actualDatabase != database || user != "portunex_test_admin" || data != c.data || listen != "" || version != 180006 || address.Valid || socket != c.socket {
		return errIsolation
	}
	return nil
}

// Close stops only the process this helper started. Synthetic data is removed
// only after confirmed process exit; an unconfirmed shutdown retains the folder.
func (c *Cluster) Close() error {
	c.closeOnce.Do(func() {
		if c.DB != nil {
			if err := c.DB.Close(); err != nil {
				c.closeErr = errors.New("synthetic database connection close failed")
			}
		}
		if c.cmd != nil && c.cmd.Process != nil {
			select {
			case <-c.done:
				c.closeErr = errors.New("isolated PostgreSQL exited unexpectedly")
			default:
				if err := c.cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
					c.closeErr = errors.New("isolated PostgreSQL shutdown signal failed")
				}
				select {
				case <-c.done:
					if c.waitErr != nil {
						c.closeErr = errors.New("isolated PostgreSQL shutdown failed")
					}
				case <-time.After(10 * time.Second):
					c.closeErr = errors.New("isolated PostgreSQL required forced shutdown; synthetic data retained")
					// PostgreSQL children create separate sessions. Ask the owned
					// postmaster to coordinate immediate shutdown; killing its
					// process group alone does NOT prove that all children exit.
					_ = c.cmd.Process.Signal(syscall.SIGQUIT)
					select {
					case <-c.done:
					case <-time.After(2 * time.Second):
						_ = c.cmd.Process.Kill()
						select {
						case <-c.done:
						case <-time.After(2 * time.Second):
							return
						}
					}
				}
			}
		}
		// Only successful normal shutdown (or no started process) permits
		// removal. On every abnormal path, even a reaped postmaster may have
		// surviving children. Report failure and retain the synthetic data.
		if c.closeErr != nil {
			return
		}
		if c.root != "" {
			if err := os.RemoveAll(c.root); err != nil {
				c.closeErr = errors.New("synthetic PostgreSQL directory cleanup failed")
			}
		}
	})
	return c.closeErr
}
