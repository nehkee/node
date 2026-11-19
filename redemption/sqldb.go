package redemption

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/livepeer/node/internal/sqldialect"
)

// sqlDB lets the store write each query once. SQLite receives query text
// unchanged and keeps its single-connection serialization. PostgreSQL receives
// rebound placeholders, and every write transaction first takes a global
// transaction-scoped advisory lock and proves process ownership, so writers
// observe the same serial order SQLite gives them. See docs/redeemer-postgres.md.
type sqlDB struct {
	*sql.DB
	dialect sqldialect.Dialect
	owner   *ownership
}

type sqlTx struct {
	*sql.Tx
	dialect sqldialect.Dialect
}

func newSQLDB(db *sql.DB) *sqlDB {
	d := &sqlDB{DB: db, dialect: sqldialect.Of(db)}
	if d.dialect == sqldialect.PostgreSQL {
		d.owner = &ownership{}
	}
	return d
}

func (d *sqlDB) postgres() bool { return d.dialect == sqldialect.PostgreSQL }

func (d *sqlDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sqlTx, error) {
	if !d.postgres() {
		tx, err := d.DB.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &sqlTx{tx, d.dialect}, nil
	}
	if opts != nil && opts.ReadOnly {
		// Multi-statement reports read one consistent snapshot, as SQLite does.
		o := *opts
		if o.Isolation == sql.LevelDefault {
			o.Isolation = sql.LevelRepeatableRead
		}
		tx, err := d.DB.BeginTx(ctx, &o)
		if err != nil {
			return nil, err
		}
		return &sqlTx{tx, d.dialect}, nil
	}
	tx, err := d.owner.begin(ctx, d.DB, opts)
	if err != nil {
		return nil, err
	}
	return &sqlTx{tx, d.dialect}, nil
}

func (d *sqlDB) Begin() (*sqlTx, error) { return d.BeginTx(context.Background(), nil) }

// ExecContext runs a single write. PostgreSQL writes outside a transaction
// still take the writer lock so they serialize with transactional writers.
func (d *sqlDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if !d.postgres() {
		return d.DB.ExecContext(ctx, query, args...)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

func (d *sqlDB) Exec(query string, args ...any) (sql.Result, error) {
	return d.ExecContext(context.Background(), query, args...)
}

func (d *sqlDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.DB.QueryContext(ctx, d.dialect.Rebind(query), args...)
}

func (d *sqlDB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.QueryContext(context.Background(), query, args...)
}

func (d *sqlDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.DB.QueryRowContext(ctx, d.dialect.Rebind(query), args...)
}

func (d *sqlDB) QueryRow(query string, args ...any) *sql.Row {
	return d.QueryRowContext(context.Background(), query, args...)
}

func (d *sqlDB) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return d.DB.PrepareContext(ctx, d.dialect.Rebind(query))
}

func (d *sqlDB) Close() error {
	if d.owner != nil {
		d.owner.release()
	}
	return d.DB.Close()
}

func (t *sqlTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *sqlTx) Exec(query string, args ...any) (sql.Result, error) {
	return t.ExecContext(context.Background(), query, args...)
}

func (t *sqlTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *sqlTx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.QueryContext(context.Background(), query, args...)
}

func (t *sqlTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *sqlTx) QueryRow(query string, args ...any) *sql.Row {
	return t.QueryRowContext(context.Background(), query, args...)
}

// ErrDatabaseOwned reports that another process holds the redemption database.
var ErrDatabaseOwned = errors.New("redemption database is owned by another process")

// Advisory lock keys, in the two-int4 namespace: class "LPRD", then the
// session-held ownership lock and the transaction-held writer lock.
const (
	lockClass = 1280332356
	ownerLock = 1
	writeLock = 2
)

// ownedQuery proves that the recorded owner session still holds the ownership
// lock. Matching backend_start as well as pid rejects a reused process id.
const ownedQuery = `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_locks l JOIN pg_catalog.pg_stat_activity a ON a.pid=l.pid
 WHERE l.locktype='advisory' AND l.database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database())
 AND l.classid::bigint=$1 AND l.objid::bigint=$2 AND l.objsubid=2 AND l.granted AND l.pid=$3 AND a.backend_start=$4)`

// ownership is a session-level advisory lock held on a dedicated connection
// for the lifetime of the store. It is acquired by the first write, so
// read-only inspection never contends with a running redeemer.
type ownership struct {
	mu    sync.Mutex
	conn  *sql.Conn
	pid   int32
	start time.Time
}

func (o *ownership) session() (int32, time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pid, o.start
}

// begin opens a serialized write transaction. Losing the owner session (for
// example after a server restart) is repaired once, unless another process
// acquired ownership in the meantime.
func (o *ownership) begin(ctx context.Context, db *sql.DB, opts *sql.TxOptions) (*sql.Tx, error) {
	for attempt := 0; ; attempt++ {
		pid, start := o.session()
		tx, err := db.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		owned := false
		_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1,$2)`, lockClass, writeLock)
		if err == nil && pid != 0 {
			err = tx.QueryRowContext(ctx, ownedQuery, lockClass, ownerLock, pid, start).Scan(&owned)
		}
		if err == nil && owned {
			return tx, nil
		}
		_ = tx.Rollback()
		if err != nil {
			return nil, err
		}
		if attempt > 0 {
			return nil, ErrDatabaseOwned
		}
		if err := o.acquire(ctx, db, pid); err != nil {
			return nil, err
		}
	}
}

func (o *ownership) acquire(ctx context.Context, db *sql.DB, stale int32) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pid != stale {
		return nil // Another writer already replaced the session.
	}
	o.releaseLocked()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1,$2)`, lockClass, ownerLock).Scan(&acquired); err != nil {
		_ = conn.Close()
		return err
	}
	if !acquired {
		_ = conn.Close()
		return ErrDatabaseOwned
	}
	if err := conn.QueryRowContext(ctx, `SELECT pid,backend_start FROM pg_catalog.pg_stat_activity WHERE pid=pg_backend_pid()`).Scan(&o.pid, &o.start); err != nil {
		o.conn = conn
		o.releaseLocked()
		return err
	}
	o.conn = conn
	return nil
}

func (o *ownership) release() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.releaseLocked()
}

// releaseLocked unlocks before returning the connection to the pool, so a
// pooled connection never carries ownership. A broken connection is discarded.
func (o *ownership) releaseLocked() {
	if o.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = o.conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1,$2)`, lockClass, ownerLock)
	_ = o.conn.Close()
	o.conn, o.pid, o.start = nil, 0, time.Time{}
}
