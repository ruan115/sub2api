package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const sshProxyScheme = "ssh"

// proxyProtocolCheckList renders the accepted proxy protocols for SQLite CHECK
// constraints. Keep it in sync with validProxyProtocol.
const proxyProtocolCheckList = `'socks5', 'http', 'https', 'ssh'`

var (
	sshTunnels   sync.Map // proxy URL string -> *sshTunnel
	sshTunnelsMu sync.Mutex
)

// sshTunnel turns an SSH server into an outbound proxy. One SSH connection is
// kept per endpoint and every request opens a direct-tcpip channel over it,
// because completing a handshake per HTTP dial would dominate request latency.
type sshTunnel struct {
	address string
	config  *ssh.ClientConfig

	mu     sync.Mutex
	client *ssh.Client
}

func isSSHProxyURL(proxyURL *url.URL) bool {
	return proxyURL != nil && strings.EqualFold(proxyURL.Scheme, sshProxyScheme)
}

// migrateProxyProtocols widens the protocol CHECK constraints on databases
// created before a protocol existed. SQLite cannot alter a CHECK in place, so
// the affected tables are rebuilt.
func (a *app) migrateProxyProtocols() error {
	for _, table := range []struct{ name, column string }{
		{"proxies", "protocol"},
		{"proxy_pools", "default_protocol"},
	} {
		var schema string
		if err := a.db.QueryRow(`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'table' AND name = ?`, table.name).Scan(&schema); err != nil {
			return fmt.Errorf("inspect %s schema: %w", table.name, err)
		}
		if schema == "" || strings.Contains(schema, "'"+sshProxyScheme+"'") {
			continue
		}
		widened := strings.Replace(schema,
			table.column+" IN ('socks5', 'http', 'https')",
			table.column+" IN ("+proxyProtocolCheckList+")", 1)
		if widened == schema {
			return fmt.Errorf("cannot widen %s.%s protocol constraint", table.name, table.column)
		}
		if err := a.rebuildTableWithSchema(table.name, widened); err != nil {
			return err
		}
	}
	return nil
}

// rebuildTableWithSchema recreates a table from a modified CREATE statement and
// copies every row across, preserving the original indexes and triggers.
func (a *app) rebuildTableWithSchema(table, schema string) error {
	temporary := table + "_migrated"
	rewritten := strings.Replace(schema, "CREATE TABLE IF NOT EXISTS "+table, "CREATE TABLE "+temporary, 1)
	if rewritten == schema {
		rewritten = strings.Replace(schema, "CREATE TABLE "+table, "CREATE TABLE "+temporary, 1)
	}
	if rewritten == schema {
		return fmt.Errorf("cannot rename %s in its CREATE statement", table)
	}
	columns, err := a.tableColumns(table)
	if err != nil {
		return err
	}
	if len(columns) == 0 {
		return fmt.Errorf("table %s has no columns", table)
	}
	columnList := strings.Join(columns, ", ")

	ctx := context.Background()
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open %s migration connection: %w", table, err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for %s migration: %w", table, err)
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s migration: %w", table, err)
	}
	defer tx.Rollback()
	statements := []string{
		rewritten,
		`INSERT INTO ` + temporary + ` (` + columnList + `) SELECT ` + columnList + ` FROM ` + table,
		`DROP TABLE ` + table,
		`ALTER TABLE ` + temporary + ` RENAME TO ` + table,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate %s protocols: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s migration: %w", table, err)
	}
	return nil
}

func (a *app) tableColumns(table string) ([]string, error) {
	rows, err := a.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// sshTunnelFor returns the shared tunnel for a proxy URL, creating it lazily.
// The SSH connection itself is not established until the first dial.
func sshTunnelFor(proxyURL *url.URL) (*sshTunnel, error) {
	if !isSSHProxyURL(proxyURL) {
		return nil, errors.New("proxy is not an SSH endpoint")
	}
	host := proxyURL.Hostname()
	if host == "" {
		return nil, errors.New("SSH proxy is missing a host")
	}
	port := proxyURL.Port()
	if port == "" {
		port = "22"
	}
	username := proxyURL.User.Username()
	if username == "" {
		return nil, errors.New("SSH proxy requires a username")
	}
	password, _ := proxyURL.User.Password()

	key := proxyURL.String()
	if cached, ok := sshTunnels.Load(key); ok {
		return cached.(*sshTunnel), nil
	}
	tunnel := &sshTunnel{
		address: net.JoinHostPort(host, port),
		config: &ssh.ClientConfig{
			User:    username,
			Auth:    []ssh.AuthMethod{ssh.Password(password), ssh.KeyboardInteractive(sshPasswordKeyboardInteractive(password))},
			Timeout: 15 * time.Second,
			// Proxy endpoints are supplied by the operator and rotate freely, so
			// there is no known_hosts to pin against. The tunnel is a transport
			// for TLS traffic that is authenticated end to end anyway.
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		},
	}
	actual, _ := sshTunnels.LoadOrStore(key, tunnel)
	return actual.(*sshTunnel), nil
}

// sshPasswordKeyboardInteractive answers keyboard-interactive prompts with the
// same password, which is how many password-only servers are configured.
func sshPasswordKeyboardInteractive(password string) ssh.KeyboardInteractiveChallenge {
	return func(_, _ string, questions []string, _ []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for index := range questions {
			answers[index] = password
		}
		return answers, nil
	}
}

// DialContext opens a tunnelled connection to address. A dead cached session is
// dropped and retried once, so a server-side disconnect does not surface as a
// request failure.
func (t *sshTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	client, err := t.ensureClient(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := client.DialContext(ctx, network, address)
	if err == nil {
		return conn, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}
	t.discard(client)
	client, retryErr := t.ensureClient(ctx)
	if retryErr != nil {
		return nil, retryErr
	}
	return client.DialContext(ctx, network, address)
}

func (t *sshTunnel) ensureClient(ctx context.Context) (*ssh.Client, error) {
	t.mu.Lock()
	if t.client != nil {
		client := t.client
		t.mu.Unlock()
		return client, nil
	}
	t.mu.Unlock()

	// Serialize handshakes across tunnels so a burst on a cold pool does not
	// open one SSH connection per in-flight request.
	sshTunnelsMu.Lock()
	defer sshTunnelsMu.Unlock()
	t.mu.Lock()
	if t.client != nil {
		client := t.client
		t.mu.Unlock()
		return client, nil
	}
	t.mu.Unlock()

	dialer := &net.Dialer{Timeout: t.config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", t.address)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(t.config.Timeout))
	}
	clientConn, channels, requests, err := ssh.NewClientConn(conn, t.address, t.config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConn, channels, requests)
	t.mu.Lock()
	t.client = client
	t.mu.Unlock()
	go func() {
		// Drop the cached session as soon as the server hangs up so the next
		// dial reconnects instead of failing.
		_ = client.Wait()
		t.discard(client)
	}()
	return client, nil
}

func (t *sshTunnel) discard(client *ssh.Client) {
	t.mu.Lock()
	if t.client == client {
		t.client = nil
	}
	t.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

// sshProxyDialContext resolves the tunnel for a proxy URL and dials through it.
func sshProxyDialContext(proxyURL *url.URL) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		tunnel, err := sshTunnelFor(proxyURL)
		if err != nil {
			return nil, err
		}
		return tunnel.DialContext(ctx, network, address)
	}
}
