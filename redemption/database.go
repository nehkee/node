package redemption

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Database backends accepted by Database.Backend.
const (
	BackendSQLite   = "sqlite"
	BackendPostgres = "postgres"
)

// Database selects the redemption store backend. An empty Backend is SQLite,
// so every existing SQLite configuration keeps its meaning.
type Database struct {
	Backend string
	// Path is the SQLite database file.
	Path string
	// PostgresURL is a postgres:// connection URL. It is a credential: never log it.
	PostgresURL *url.URL
	// PostgresInsecure permits unencrypted or unverified TLS connections to
	// non-loopback hosts. Loopback and Unix-socket connections are always allowed.
	PostgresInsecure bool
	// PostgresMaxConns bounds the pool, including the ownership connection.
	PostgresMaxConns int
}

func (d Database) postgres() bool { return d.Backend == BackendPostgres }

// Validate checks backend selection and connection security without connecting.
func (d Database) Validate() error {
	switch d.Backend {
	case "", BackendSQLite:
		if d.Path == "" {
			return errors.New("redeemer SQLite path is required")
		}
		return nil
	case BackendPostgres:
		_, err := d.postgresConfig()
		return err
	default:
		return fmt.Errorf("unsupported redemption database backend %q", d.Backend)
	}
}

// Open opens the configured database without running migrations. For SQLite,
// create permits creating the file; PostgreSQL databases are provisioned by
// the operator and create only governs schema creation by the caller.
func (d Database) Open(ctx context.Context, create bool) (*sql.DB, error) {
	if !d.postgres() {
		if d.Backend != "" && d.Backend != BackendSQLite {
			return nil, d.Validate()
		}
		return OpenDatabase(ctx, d.Path, create)
	}
	config, err := d.postgresConfig()
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*config)
	conns := d.PostgresMaxConns
	if conns <= 0 {
		conns = 8
	}
	// Writers hold one connection each while queued on the writer lock, and
	// the ownership session holds one more for the life of the process.
	db.SetMaxOpenConns(max(conns, 2))
	db.SetMaxIdleConns(max(conns, 2))
	db.SetConnMaxIdleTime(5 * time.Minute)
	ping, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to redemption PostgreSQL: %w", redact(err, d.PostgresURL))
	}
	return db, nil
}

// OpenStore opens, migrates and loads the configured redemption store.
func (d Database) OpenStore(ctx context.Context) (*RedeemerDB, error) {
	db, err := d.Open(ctx, true)
	if err != nil {
		return nil, err
	}
	return NewStore(db)
}

// Describe names the database for logs and printed configuration without
// revealing credentials.
func (d Database) Describe() string {
	if !d.postgres() {
		return d.Path
	}
	if d.PostgresURL == nil {
		return "postgres"
	}
	u := url.URL{Scheme: d.PostgresURL.Scheme, Host: d.PostgresURL.Host, Path: d.PostgresURL.Path}
	if d.PostgresURL.User != nil {
		u.User = url.User(d.PostgresURL.User.Username())
	}
	return u.String()
}

// postgresConfig enforces verified TLS for remote servers. A URL without
// sslmode is upgraded to verify-full for remote hosts; weaker modes require
// PostgresInsecure. Client keys referenced by the URL must be owner-only.
func (d Database) postgresConfig() (*pgx.ConnConfig, error) {
	u := d.PostgresURL
	if u == nil {
		return nil, errors.New("redemption PostgreSQL requires a connection URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, errors.New("redemption PostgreSQL URL must use the postgres:// scheme")
	}
	if u.Fragment != "" {
		return nil, errors.New("redemption PostgreSQL URL must not contain a fragment")
	}
	q := u.Query()
	local := true
	for _, host := range postgresHosts(u) {
		local = local && localPostgresHost(host)
	}
	mode := q.Get("sslmode")
	if mode == "" && !local {
		mode = "verify-full"
		q.Set("sslmode", mode)
	}
	switch mode {
	case "", "disable", "allow", "prefer", "require":
		if !local && !d.PostgresInsecure {
			return nil, errors.New("redemption PostgreSQL requires sslmode=verify-full or verify-ca for non-loopback hosts")
		}
	case "verify-ca", "verify-full":
	default:
		return nil, errors.New("unsupported redemption PostgreSQL sslmode")
	}
	if key := q.Get("sslkey"); key != "" {
		info, err := os.Stat(key)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("redemption PostgreSQL client key must be an owner-only regular file")
		}
	}
	if q.Get("application_name") == "" {
		q.Set("application_name", "livepeer-redeemer")
	}
	if q.Get("connect_timeout") == "" {
		q.Set("connect_timeout", "10")
	}
	normalized := *u
	normalized.RawQuery = q.Encode()
	config, err := pgx.ParseConfig(normalized.String())
	if err != nil {
		return nil, fmt.Errorf("invalid redemption PostgreSQL URL: %w", redact(err, u))
	}
	return config, nil
}

func postgresHosts(u *url.URL) []string {
	var hosts []string
	if h := u.Query().Get("host"); h != "" {
		hosts = append(hosts, strings.Split(h, ",")...)
	} else if u.Host == "" {
		hosts = append(hosts, "")
	}
	for _, h := range strings.Split(u.Host, ",") {
		if h == "" {
			continue
		}
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		hosts = append(hosts, strings.Trim(h, "[]"))
	}
	return hosts
}

func localPostgresHost(host string) bool {
	if host == "" || strings.HasPrefix(host, "/") || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// redact removes the URL password from driver errors, which may echo input.
func redact(err error, u *url.URL) error {
	if err == nil || u == nil || u.User == nil {
		return err
	}
	if password, ok := u.User.Password(); ok && password != "" && strings.Contains(err.Error(), password) {
		return errors.New(strings.ReplaceAll(err.Error(), password, "xxxxx"))
	}
	return err
}
