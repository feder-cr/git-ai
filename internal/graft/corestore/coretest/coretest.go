// Package coretest opens storage cores for tests: a SQLite database in a
// temporary directory, or a fresh Postgres database per test when
// GRAFT_TEST_POSTGRES_DSN is set, and a file bucket in a temporary
// directory (or GRAFT_TEST_S3_URL).
//
// Tests that also need testserver must live in an external test package
// (package foo_test): testserver imports the voter, syncer and gate packages,
// so an internal test of those packages importing testserver is an import
// cycle.
package coretest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/blobstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/stretchr/testify/require"
)

// PostgresDSNEnv names the environment variable with the URL of a Postgres
// server on which tests may create and drop databases, for example
// postgres://postgres@127.0.0.1:54329/postgres?sslmode=disable.
const PostgresDSNEnv = "GRAFT_TEST_POSTGRES_DSN"

// S3URLEnv names the environment variable with a gocloud s3:// URL of a test
// bucket (for example MinIO), used by BucketURL(t, true).
const S3URLEnv = "GRAFT_TEST_S3_URL"

// Dialects returns the dialects to run conformance tests with: SQLite always,
// Postgres when PostgresDSNEnv is set.
func Dialects() []sqlstore.Dialect {
	dialects := []sqlstore.Dialect{sqlstore.SQLite}
	if os.Getenv(PostgresDSNEnv) != "" {
		dialects = append(dialects, sqlstore.Postgres)
	}
	return dialects
}

// DatabaseURL returns the URL of a new, empty database of the dialect, which
// is dropped when the test ends. For Postgres the test is skipped when
// PostgresDSNEnv is not set.
func DatabaseURL(tb testing.TB, dialect sqlstore.Dialect) string {
	tb.Helper()

	switch dialect {
	case sqlstore.SQLite:
		return "sqlite://" + filepath.Join(tb.TempDir(), "meta.db")
	case sqlstore.Postgres:
		admin := os.Getenv(PostgresDSNEnv)
		if admin == "" {
			tb.Skipf("%s is not set", PostgresDSNEnv)
		}
		return newPostgresDatabase(tb, admin)
	default:
		tb.Fatalf("unknown dialect %q", dialect)
		return ""
	}
}

func newPostgresDatabase(tb testing.TB, admin string) string {
	tb.Helper()

	suffix := make([]byte, 8)
	_, err := rand.Read(suffix)
	require.NoError(tb, err)
	name := "graft_test_" + hex.EncodeToString(suffix)

	db, err := sql.Open("pgx", admin)
	require.NoError(tb, err)
	defer db.Close()

	_, err = db.ExecContext(context.Background(), "CREATE DATABASE "+name)
	require.NoError(tb, err)

	tb.Cleanup(func() {
		db, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	u, err := url.Parse(admin)
	require.NoError(tb, err)
	u.Path = "/" + name
	return u.String()
}

// BucketURL returns the URL of a new, empty bucket directory, or of the S3
// test bucket (with a unique key prefix) when s3 is true and S3URLEnv is
// set; with s3 true and no S3URLEnv the test is skipped.
func BucketURL(tb testing.TB, s3 bool) string {
	tb.Helper()

	if !s3 {
		return "file://" + tb.TempDir()
	}

	base := os.Getenv(S3URLEnv)
	if base == "" {
		tb.Skipf("%s is not set", S3URLEnv)
	}

	suffix := make([]byte, 8)
	_, err := rand.Read(suffix)
	require.NoError(tb, err)

	u, err := url.Parse(base)
	require.NoError(tb, err)
	q := u.Query()
	q.Set("prefix", "graft-test-"+hex.EncodeToString(suffix)+"/")
	u.RawQuery = q.Encode()
	return u.String()
}

// Options configure Open.
type Options struct {
	// Mode is the mode of the core; empty means authoritative.
	Mode corestore.Mode
	// Dialect is the database dialect; empty means SQLite.
	Dialect sqlstore.Dialect
	// DatabaseURL and BucketURL, when set, are used instead of new ones, so
	// that several cores (nodes) share one database and bucket.
	DatabaseURL string
	BucketURL   string
	// NodeID is the node name; empty means "node-a".
	NodeID string
	// GCGrace and GCMargin are the garbage collection times; zero means the
	// production defaults (1h and 5m). A test of the writers' deadline sets them
	// short.
	GCGrace  time.Duration
	GCMargin time.Duration
	// ShadowTimeout is the shadow budget of a transaction; zero means the
	// production default (5s).
	ShadowTimeout time.Duration
}

// Open opens a storage core for the test and closes it when the test ends.
func Open(tb testing.TB, opts Options) *corestore.Core {
	tb.Helper()

	if opts.Mode == "" {
		opts.Mode = corestore.ModeAuthoritative
	}
	if opts.Dialect == "" {
		opts.Dialect = sqlstore.SQLite
	}
	if opts.DatabaseURL == "" {
		opts.DatabaseURL = DatabaseURL(tb, opts.Dialect)
	}
	if opts.BucketURL == "" {
		opts.BucketURL = BucketURL(tb, false)
	}
	if opts.NodeID == "" {
		opts.NodeID = "node-a"
	}
	if opts.GCGrace == 0 {
		opts.GCGrace = time.Hour
	}
	if opts.GCMargin == 0 {
		opts.GCMargin = 5 * time.Minute
	}
	if opts.ShadowTimeout == 0 {
		opts.ShadowTimeout = 5 * time.Second
	}

	ctx := context.Background()

	meta, err := sqlstore.Open(ctx, opts.DatabaseURL)
	require.NoError(tb, err)

	blobs, err := blobstore.Open(ctx, opts.BucketURL)
	require.NoError(tb, err)

	core := &corestore.Core{
		Mode:          opts.Mode,
		NodeID:        opts.NodeID,
		Meta:          meta,
		Blobs:         blobs,
		SyncTimeout:   10 * time.Second,
		ShadowTimeout: opts.ShadowTimeout,
		GCGrace:       opts.GCGrace,
		GCMargin:      opts.GCMargin,
	}
	tb.Cleanup(func() { _ = core.Close() })

	return core
}

// EnableLocalRefs gives the core the admission and the deferred durability of
// the local reference writes, owning the storages of cfg, as gitaly serve does
// in authoritative mode (doc/graft/PLAN7.md, LOCAL REFERENCE WRITES RULE). The
// boot identity is a fixed test value and the recovery grace 0 unless opts set
// others. A deferred
// durability the core had is closed first, as when its process exits.
func EnableLocalRefs(tb testing.TB, core *corestore.Core, cfg config.Cfg, opts ...localrefs.DurabilityOption) {
	tb.Helper()

	if core.RefDurability != nil {
		require.NoError(tb, core.RefDurability.Close())
	}
	core.RefAdmission = localrefs.NewAdmission(10*time.Second, 10*time.Second)
	durability, err := localrefs.NewDurability(config.NewLocator(cfg), 0, append([]localrefs.DurabilityOption{
		localrefs.WithBootID(TestBootID),
		localrefs.WithRecoveryGrace(0),
		localrefs.WithWatermark(gitops.WatermarkPath),
	}, opts...)...)
	require.NoError(tb, err)
	// A git process forked at the moment a previous durability closed its lock
	// holds a copy of it until it execs (close-on-exec): the lock can be busy for
	// that short while.
	require.Eventually(tb, func() bool {
		owned, err := durability.TryOwn(context.Background(), StorageNames(cfg))
		require.NoError(tb, err)
		return owned
	}, 10*time.Second, time.Millisecond, "another durability of the test owns the storages")
	durability.Activate()
	tb.Cleanup(func() { _ = durability.Close() })
	core.RefDurability = durability
}

// StorageNames returns the names of the storages of cfg.
func StorageNames(cfg config.Cfg) []string {
	names := make([]string, 0, len(cfg.Storages))
	for _, s := range cfg.Storages {
		names = append(names, s.Name)
	}
	return names
}

// TestBootID is the boot identity EnableLocalRefs gives the deferred
// durability by default.
const TestBootID = "test-boot"

// CommitRefWithoutNameCheck records a changeset that creates the reference, with
// direct writes that skip CommitChangeset's refusal of names that conflict: the
// state a repository can be in from a version that had no such refusal, for the
// tests of its repair.
func CommitRefWithoutNameCheck(tb testing.TB, ctx context.Context, meta corestore.MetaStore, repoID int64, name string, target corestore.Target) {
	tb.Helper()

	s, ok := meta.(*sqlstore.Store)
	require.True(tb, ok, "the metadata store is a %T", meta)

	bind := func(query string) string {
		if s.Dialect() != sqlstore.Postgres {
			return query
		}
		for i := 1; strings.Contains(query, "?"); i++ {
			query = strings.Replace(query, "?", "$"+strconv.Itoa(i), 1)
		}
		return query
	}
	var createdAt any = time.Now().UTC()
	if s.Dialect() == sqlstore.SQLite {
		createdAt = time.Now().UTC().Format(time.RFC3339Nano)
	}

	tx, err := s.DB().BeginTx(ctx, nil)
	require.NoError(tb, err)
	defer func() { _ = tx.Rollback() }()

	var seq int64
	require.NoError(tb, tx.QueryRowContext(ctx, bind("SELECT head_seq + 1 FROM graft_repositories WHERE repo_id = ?"), repoID).Scan(&seq))

	for _, statement := range []struct {
		query string
		args  []any
	}{
		{
			"INSERT INTO graft_changesets (repo_id, seq, operation, actor, node_id, pack_key, pack_bytes, forced, created_at) VALUES (?, ?, 'test', 'tester', 'an-older-node', NULL, 0, ?, ?)",
			[]any{repoID, seq, false, createdAt},
		},
		{
			"INSERT INTO graft_changeset_refs (repo_id, seq, ordinal, name, old_target, new_target) VALUES (?, ?, 0, ?, NULL, ?)",
			[]any{repoID, seq, []byte(name), string(target)},
		},
		{
			"INSERT INTO graft_refs (repo_id, name, target, seq) VALUES (?, ?, ?, ?)",
			[]any{repoID, []byte(name), string(target), seq},
		},
		{
			"UPDATE graft_repositories SET head_seq = ? WHERE repo_id = ?",
			[]any{seq, repoID},
		},
	} {
		_, err := tx.ExecContext(ctx, bind(statement.query), statement.args...)
		require.NoError(tb, err)
	}
	require.NoError(tb, tx.Commit())
}
