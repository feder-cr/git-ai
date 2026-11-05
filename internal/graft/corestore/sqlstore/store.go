// Package sqlstore implements corestore.MetaStore on SQLite and Postgres with
// database/sql. Owned by WP51 (doc/graft/PLAN6.md); the schema in
// migrations.go is fixed by the foundation.
package sqlstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"    // registers the "pgx" driver
	sqlite3 "github.com/mattn/go-sqlite3" // registers the "sqlite3" driver
	migrate "github.com/rubenv/sql-migrate"
	"google.golang.org/protobuf/encoding/protojson"
)

// Dialect is the SQL dialect of a Store.
type Dialect string

const (
	// SQLite is mattn/go-sqlite3.
	SQLite Dialect = "sqlite"
	// Postgres is jackc/pgx through database/sql.
	Postgres Dialect = "postgres"
)

const (
	// maxTxRetries is the number of times a transaction that failed with a
	// busy, locked, serialization or deadlock error is retried.
	maxTxRetries = 5
	// maxMigrationAttempts bounds the retries of the migrations on SQLite, where
	// two processes opening the same new database may race.
	maxMigrationAttempts = 5
	// migrationLockKey is the Postgres advisory lock that serializes the
	// migrations of the processes sharing one database.
	migrationLockKey int64 = 7364892715031
	// maxNamesPerQuery bounds the number of reference names of one query.
	maxNamesPerQuery = 500
	// sqliteBusyTimeout is the longest a SQLite connection waits for a lock
	// (the _busy_timeout of the DSN), and the bound of the retries of Open.
	sqliteBusyTimeout = 10 * time.Second
	// sqliteBusySlice is the longest a SQLite connection waits for a lock in
	// one call. SQLite's busy handler ignores the context (sqlite3_interrupt
	// does not end its sleep), so a transaction acquires its lock in slices of
	// at most this duration and checks the context in between.
	sqliteBusySlice = 100 * time.Millisecond
)

// Store is the database of the storage core.
type Store struct {
	db *sql.DB
	// readDB runs the read transactions. On SQLite it is a separate pool of
	// query-only connections with deferred transactions, so that readers take
	// a WAL snapshot instead of the write lock that the immediate transactions
	// of db take. On Postgres it is db.
	readDB  *sql.DB
	dialect Dialect
	// writers queues the write transactions on SQLite (nil on Postgres, whose
	// row locks queue their waiters themselves); see writeQueue.
	writers *writeQueue
	// snapshotWritten, when set by a test, is called by CreateCheckpoint once
	// the reference snapshot is written, before created_at is stamped.
	snapshotWritten func()
}

var _ corestore.MetaStore = (*Store)(nil)

// ParseDSN splits a configured database URL into its dialect and the
// data source name of the driver:
//
//   - "sqlite:///abs/path/meta.db" → SQLite, "/abs/path/meta.db"
//   - "sqlite://:memory:" is not supported (every connection would see its own
//     database); tests use a file in a temporary directory.
//   - "postgres://..." or "postgresql://..." → Postgres, the URL unchanged.
func ParseDSN(dsn string) (Dialect, string, error) {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return Postgres, dsn, nil
	case strings.HasPrefix(dsn, "sqlite://"):
		u, err := url.Parse(dsn)
		if err != nil {
			return "", "", fmt.Errorf("parse sqlite database URL: %w", err)
		}
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
			return "", "", fmt.Errorf("sqlite database URL must be sqlite:///absolute/path, got %q", dsn)
		}
		return SQLite, u.Path, nil
	default:
		return "", "", fmt.Errorf("unsupported database URL %q (want sqlite:///path or postgres://...)", dsn)
	}
}

// Open opens the database, creating it and applying the migrations when
// needed.
//
// SQLite is opened with _journal_mode=WAL, _synchronous=FULL,
// _busy_timeout=10000, _foreign_keys=on and _txlock=immediate, so that every
// write transaction takes the database write lock when it begins (the
// per-repository serialization of CommitChangeset). Read transactions (Refs,
// ListChangesets) run on a second pool of query-only connections with
// deferred transactions, which read a WAL snapshot alongside the writers.
// Postgres uses row locks (SELECT ... FOR UPDATE on graft_repositories) in
// READ COMMITTED.
//
// Every transaction honours the context while it waits for a lock: on SQLite
// the lock is acquired in slices of sqliteBusySlice, never longer than the
// context's deadline.
func Open(ctx context.Context, dsn string) (*Store, error) {
	return OpenWithPool(ctx, dsn, DefaultPool)
}

// Pool bounds the connections of a Postgres store, which database/sql pools.
// Without bounds the pool keeps two idle connections and opens a new one for
// every concurrent query beyond them: under load each transaction then pays for
// a connection, and a burst of hundreds of writers is a burst of connection
// attempts that the server or the network in front of it resets. A SQLite store
// does not use it: its writers are queued (writeQueue) and its connections are
// local files.
type Pool struct {
	// MaxOpenConns is the most connections open at once. A query that finds
	// them all in use waits for one, until its context ends. Every operation of
	// the store holds one connection at a time, so a full pool queues
	// operations and never blocks one that holds a connection.
	MaxOpenConns int
	// MaxIdleConns is the most connections kept open while unused; at most
	// MaxOpenConns.
	MaxIdleConns int
	// ConnMaxIdleTime closes a connection unused for this long (0: never).
	ConnMaxIdleTime time.Duration
	// ConnMaxLifetime closes a connection this long after it was opened, once it
	// is not in use (0: never).
	ConnMaxLifetime time.Duration
}

// DefaultPool is the pool of Open. With it, BenchmarkCommitChangesetConcurrent
// commits 2,019-2,127 changesets per second for 16 writers in separate
// repositories on the development machine, and 2,586-2,630 for 64 writers
// queued on the 32 connections: more than twenty times what a node committed end
// to end in the land benchmarks (90 to 120 per second). As many connections stay
// idle, so that steady load reuses them; the idle ones close after five minutes,
// and every connection is replaced after thirty, so that the pool follows a
// failover or a changed DNS name. Every node, and during a graceful upgrade both
// the old and the new process of a node, has a pool of its own: their sum must
// stay below the server's max_connections, since a refused connection (53300)
// fails the operation and is not retried.
var DefaultPool = Pool{
	MaxOpenConns:    32,
	MaxIdleConns:    32,
	ConnMaxIdleTime: 5 * time.Minute,
	ConnMaxLifetime: 30 * time.Minute,
}

// Validate checks that the pool can serve queries.
func (p Pool) Validate() error {
	switch {
	case p.MaxOpenConns < 1:
		return fmt.Errorf("database pool: max open connections %d must be at least 1", p.MaxOpenConns)
	case p.MaxIdleConns < 0 || p.MaxIdleConns > p.MaxOpenConns:
		return fmt.Errorf("database pool: max idle connections %d must be between 0 and the max open connections %d", p.MaxIdleConns, p.MaxOpenConns)
	case p.ConnMaxIdleTime < 0 || p.ConnMaxLifetime < 0:
		return fmt.Errorf("database pool: connection max idle time %s and lifetime %s must not be negative", p.ConnMaxIdleTime, p.ConnMaxLifetime)
	}
	return nil
}

// OpenWithPool is Open with the given pool for a Postgres database. The pool
// does not apply to SQLite.
func OpenWithPool(ctx context.Context, dsn string, pool Pool) (*Store, error) {
	return open(ctx, dsn, 0, pool)
}

// open is OpenWithPool applying only the first migrations of the schema when
// migrations is greater than 0 (tests build a database as an earlier version
// left it), every migration otherwise.
func open(ctx context.Context, dsn string, migrations int, pool Pool) (*Store, error) {
	if err := pool.Validate(); err != nil {
		return nil, err
	}

	dialect, target, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}

	var driverName, driverDSN string
	switch dialect {
	case SQLite:
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, fmt.Errorf("create sqlite database directory: %w", err)
		}
		driverName = "sqlite3"
		driverDSN = target + "?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=10000&_foreign_keys=on&_txlock=immediate"
	default:
		driverName = "pgx"
		driverDSN = dsn
	}

	db, err := sql.Open(driverName, driverDSN)
	if err != nil {
		return nil, fmt.Errorf("open %s database: %w", dialect, err)
	}

	if err := ping(ctx, db, dialect); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping %s database: %w", dialect, err)
	}

	if err := applyMigrations(ctx, db, dialect, migrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	// The bounds apply once the migrations are done: they hold the advisory lock
	// on a connection of their own while the migration library uses others, the
	// one place where the store holds two connections at once.
	if dialect == Postgres {
		db.SetMaxOpenConns(pool.MaxOpenConns)
		db.SetMaxIdleConns(pool.MaxIdleConns)
		db.SetConnMaxIdleTime(pool.ConnMaxIdleTime)
		db.SetConnMaxLifetime(pool.ConnMaxLifetime)
	}

	readDB := db
	if dialect == SQLite {
		// The journal mode is persistent: the database is in WAL mode since the
		// first connection of db, so the readers do not set it.
		readDB, err = sql.Open(driverName, target+"?_busy_timeout=10000&_foreign_keys=on&_txlock=deferred&_query_only=1")
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open %s read database: %w", dialect, err)
		}
		if err := ping(ctx, readDB, dialect); err != nil {
			_ = readDB.Close()
			_ = db.Close()
			return nil, fmt.Errorf("ping %s read database: %w", dialect, err)
		}
	}

	s := &Store{db: db, readDB: readDB, dialect: dialect}
	if dialect == SQLite {
		s.writers = &writeQueue{}
	}
	return s, nil
}

// ping checks the connection to the database. On SQLite, the first
// connection to a new database switches it to WAL mode, which fails with a
// busy error instead of waiting when another process does the same at once:
// such errors are retried for up to sqliteBusyTimeout.
func ping(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect != SQLite {
		return db.PingContext(ctx)
	}

	limit := time.Now().Add(sqliteBusyTimeout)
	for attempt := 0; ; attempt++ {
		err := db.PingContext(ctx)
		if err == nil || !isBusy(err) || ctx.Err() != nil || time.Now().After(limit) {
			return err
		}
		if serr := sleep(ctx, min(attempt, 9)); serr != nil {
			return err
		}
	}
}

// applyMigrations brings the schema of the dialect up to date, or up to its
// first limit migrations when limit is greater than 0. Postgres processes are
// serialized with an advisory lock held on a dedicated connection; SQLite
// processes that race on a new database retry.
func applyMigrations(ctx context.Context, db *sql.DB, dialect Dialect, limit int) error {
	migrations := sqliteMigrations
	name := "sqlite3"
	attempts := maxMigrationAttempts
	if dialect == Postgres {
		migrations = postgresMigrations
		name = "postgres"
		attempts = 1
	}
	if limit > 0 {
		migrations = migrations[:min(limit, len(migrations))]
	}

	if dialect == Postgres {
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("get migration connection: %w", err)
		}
		defer conn.Close()

		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
			return fmt.Errorf("lock migrations: %w", err)
		}
		defer func() {
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey)
		}()
	}

	set := migrate.MigrationSet{TableName: migrationTable}
	source := &migrate.MemoryMigrationSource{Migrations: migrations}

	if err := refuseUnknownMigrations(set, db, name, dialect); err != nil {
		return err
	}

	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if _, err = set.Exec(db, name, source, migrate.Up); err == nil {
			return nil
		}
		if attempt+1 < attempts {
			if serr := sleep(ctx, attempt); serr != nil {
				return serr
			}
		}
	}

	return err
}

// ErrUnknownMigrations is returned by Open when the database holds migrations
// that this binary does not know: a newer version migrated it. This version
// cannot run on it: its writes and its reads of the newer data are not
// compatible (doc/graft/STORAGE.md, Upgrades).
var ErrUnknownMigrations = errors.New("storage core: the database was migrated by a newer version of the storage core")

// refuseUnknownMigrations returns ErrUnknownMigrations, naming the migrations,
// when the database records a migration that is not among the migrations of
// this binary for the dialect (all of them, whatever limit applies).
func refuseUnknownMigrations(set migrate.MigrationSet, db *sql.DB, name string, dialect Dialect) error {
	records, err := set.GetMigrationRecords(db, name)
	if err != nil {
		return fmt.Errorf("read the applied migrations: %w", err)
	}
	all := sqliteMigrations
	if dialect == Postgres {
		all = postgresMigrations
	}
	known := make(map[string]struct{}, len(all))
	for _, m := range all {
		known[m.Id] = struct{}{}
	}
	var unknown []string
	for _, r := range records {
		if _, ok := known[r.Id]; !ok {
			unknown = append(unknown, r.Id)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("%w (unknown migrations %s): this binary cannot run on it; every node must run the same version, so upgrade this node (doc/graft/STORAGE.md, Upgrades)",
			ErrUnknownMigrations, strings.Join(unknown, ", "))
	}
	return nil
}

// Dialect returns the dialect of the store.
func (s *Store) Dialect() Dialect { return s.dialect }

// DB returns the underlying database, for tests.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	var readErr error
	if s.readDB != nil && s.readDB != s.db {
		readErr = s.readDB.Close()
	}
	return errors.Join(s.db.Close(), readErr)
}

// querier is implemented by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// bind rewrites the "?" placeholders of a query to the "$1..$n" placeholders of
// Postgres. Queries are written once, with "?", and never contain values.
func (s *Store) bind(query string) string {
	if s.dialect != Postgres {
		return query
	}

	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}

	return b.String()
}

// timeArg returns t in the form the dialect stores: RFC 3339 text with
// nanoseconds in UTC on SQLite, a TIMESTAMPTZ on Postgres.
func (s *Store) timeArg(t time.Time) any {
	if s.dialect == SQLite {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return t.UTC()
}

// scanTime converts a time read from either dialect.
func scanTime(raw any) (time.Time, error) {
	switch v := raw.(type) {
	case time.Time:
		return v.UTC(), nil
	case string:
		return parseSQLiteTime(v)
	case []byte:
		return parseSQLiteTime(string(v))
	default:
		return time.Time{}, fmt.Errorf("unexpected time value of type %T", raw)
	}
}

func parseSQLiteTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t.UTC(), nil
}

// readTxOptions returns the options of a read-only transaction with a
// consistent snapshot. SQLite transactions are always consistent.
func (s *Store) readTxOptions() *sql.TxOptions {
	if s.dialect == Postgres {
		return &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	return nil
}

// withTx runs fn in a write transaction. fn is run again, in a new
// transaction, when the transaction failed with a retryable error (see
// isRetryable), up to maxTxRetries times. fn must therefore reset everything
// it produces.
func (s *Store) withTx(ctx context.Context, opts *sql.TxOptions, fn func(*sql.Tx) error) error {
	return s.retryTx(ctx, s.db, opts, fn)
}

// withReadTx runs fn in a read-only transaction with a consistent snapshot
// (Postgres REPEATABLE READ; SQLite a deferred transaction on the query-only
// pool, which does not take the write lock). It retries like withTx.
func (s *Store) withReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return s.retryTx(ctx, s.readDB, s.readTxOptions(), fn)
}

// retryHook, when set by a test, is called with the error of every transaction
// that is retried, so that a test can assert which kind of conflict occurred
// (doc/graft/PLAN7.md, LOCK ORDER: no deadlock). It is nil in production.
var retryHook func(err error)

func (s *Store) retryTx(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(*sql.Tx) error) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = s.runTx(ctx, db, opts, fn)
		if err == nil || attempt >= maxTxRetries || !isRetryable(err) || ctx.Err() != nil {
			return err
		}
		if retryHook != nil {
			retryHook(err)
		}
		if serr := sleep(ctx, attempt); serr != nil {
			return err
		}
	}
}

func (s *Store) runTx(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(*sql.Tx) error) error {
	if s.dialect != SQLite {
		tx, err := db.BeginTx(ctx, opts)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		return finishTx(tx, fn)
	}

	// SQLite: a dedicated connection, whose busy timeout bounds the wait for
	// the lock by the context (see beginSQLite).
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	defer restoreBusyTimeout(conn)

	// A write transaction waits for its turn among the writers of this Store
	// before it asks SQLite for the write lock (writeQueue). The admission covers
	// one attempt: it is taken right before BEGIN IMMEDIATE and released when the
	// attempt has committed or rolled back, so a writer never holds it while
	// retryTx sleeps before the next attempt, and the writers behind it are never
	// kept waiting for a lock nobody uses. No transaction function starts another
	// write transaction, and the admission is always taken before the SQLite
	// lock, so the two can never wait for each other. Read transactions run on
	// readDB and need no admission.
	if db == s.db && s.writers != nil {
		if err := s.writers.acquire(ctx); err != nil {
			return fmt.Errorf("wait for the write admission: %w", err)
		}
		defer s.writers.release()
	}

	tx, err := beginSQLite(ctx, conn, opts)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	return finishTx(tx, fn)
}

// beginSQLite begins a transaction on conn. BEGIN IMMEDIATE waits for the
// write lock in SQLite's busy handler, which ignores the context, so the lock
// is requested with a busy timeout of at most sqliteBusySlice, never beyond
// the context's deadline, and the context is checked between the attempts. The
// whole wait is bounded by sqliteBusyTimeout, as the DSN's _busy_timeout.
func beginSQLite(ctx context.Context, conn *sql.Conn, opts *sql.TxOptions) (*sql.Tx, error) {
	limit := time.Now().Add(sqliteBusyTimeout)
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline && deadline.Before(limit) {
		limit = deadline
	}

	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, contextError(err, lastErr)
		}

		wait := time.Until(limit)
		if wait <= 0 {
			if hasDeadline && !deadline.After(time.Now()) {
				return nil, contextError(context.DeadlineExceeded, lastErr)
			}
			return nil, lastErr
		}

		if err := setBusyTimeout(ctx, conn, min(wait, sqliteBusySlice)); err != nil {
			return nil, err
		}

		tx, err := conn.BeginTx(ctx, opts)
		if err == nil {
			return tx, nil
		}
		if !isBusy(err) {
			if cerr := ctx.Err(); cerr != nil {
				return nil, contextError(cerr, err)
			}
			return nil, err
		}
		lastErr = err
	}
}

// contextError returns the context's error, with the last lock error if any.
func contextError(ctxErr, lockErr error) error {
	if lockErr == nil {
		return ctxErr
	}
	return fmt.Errorf("%w: %w", ctxErr, lockErr)
}

// setBusyTimeout sets the busy timeout of a SQLite connection, at least 1 ms
// (0 would disable the busy handler).
func setBusyTimeout(ctx context.Context, conn *sql.Conn, d time.Duration) error {
	ms := max(d.Milliseconds(), 1)
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = "+strconv.FormatInt(ms, 10)); err != nil {
		return fmt.Errorf("set busy timeout: %w", err)
	}
	return nil
}

// restoreBusyTimeout gives conn back the busy timeout of the DSN before it
// returns to the pool. A connection whose timeout cannot be restored is
// discarded.
func restoreBusyTimeout(conn *sql.Conn) {
	if err := setBusyTimeout(context.Background(), conn, sqliteBusyTimeout); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// finishTx runs fn in tx and commits it, or rolls it back when fn fails.
func finishTx(tx *sql.Tx, fn func(*sql.Tx) error) error {
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	committed = true

	return nil
}

// isRetryable reports whether err is a SQLite busy or locked error, a Postgres
// serialization failure or a deadlock.
func isRetryable(err error) bool {
	var se sqlite3.Error
	if errors.As(err, &se) {
		return se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked
	}

	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "40001" || pe.Code == "40P01"
	}

	return false
}

// isBusy reports whether err is a SQLite busy or locked error.
func isBusy(err error) bool {
	var se sqlite3.Error
	return errors.As(err, &se) && (se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked)
}

// isUniqueViolation reports whether err is a unique constraint violation.
func isUniqueViolation(err error) bool {
	var se sqlite3.Error
	if errors.As(err, &se) {
		return se.ExtendedCode == sqlite3.ErrConstraintUnique
	}

	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "23505"
	}

	return false
}

// sleep waits for a jittered, growing backoff after the given attempt, or
// returns the context's error.
func sleep(ctx context.Context, attempt int) error {
	base := time.Duration(attempt+1) * 5 * time.Millisecond
	delay := base + time.Duration(rand.Int63n(int64(base)+1))

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// bytesArg returns b as a non-NULL binary value: a nil slice binds NULL, which
// the NOT NULL columns of the names and the targets refuse.
func bytesArg(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// targetArg returns the value of a reference target, NULL when absent.
func targetArg(t corestore.Target) any {
	if t == "" {
		return nil
	}
	return string(t)
}

// validUTF8 replaces the bytes that are not valid UTF-8 and the NUL bytes,
// which Postgres refuses in text columns, by U+FFFD, so that both dialects
// store the same text.
func validUTF8(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, string(utf8.RuneError)), "\x00", string(utf8.RuneError))
}

// storableText reports whether s can be stored unchanged in a text column of
// both dialects: valid UTF-8 without NUL bytes.
func storableText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// zeroOID returns the zero object ID of the object format.
func zeroOID(objectFormat string) string {
	if objectFormat == "sha256" {
		return strings.Repeat("0", 64)
	}
	return strings.Repeat("0", 40)
}

// marshalReview encodes a review as the body of graft_reviews. Requirements and
// approved are computed by the callers and are never stored.
func marshalReview(r *gitalypb.Review) (string, error) {
	data, err := protojson.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encode review %s: %w", r.GetId(), err)
	}
	return string(data), nil
}

// parseReviewID parses a review ID: a decimal number without a leading zero.
func parseReviewID(id string) (int64, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
		return 0, false
	}
	return n, true
}
