package sqlstore_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// forPostgres runs fn with the URL of a fresh Postgres database, and skips the test without one.
func forPostgres(t *testing.T, fn func(t *testing.T, dsn string)) {
	for _, dialect := range coretest.Dialects() {
		if dialect == sqlstore.Postgres {
			fn(t, coretest.DatabaseURL(t, dialect))
			return
		}
	}
	t.Skip("no Postgres database (GRAFT_TEST_POSTGRES_DSN)")
}

func openPool(t *testing.T, ctx context.Context, dsn string, pool sqlstore.Pool) *sqlstore.Store {
	t.Helper()
	s, err := sqlstore.OpenWithPool(ctx, dsn, pool)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// TestPoolBoundsPostgresConnections checks that the pool's bounds apply to a Postgres store, and that
// a query that finds every connection in use waits for one only until its context ends.
func TestPoolBoundsPostgresConnections(t *testing.T) {
	forPostgres(t, func(t *testing.T, dsn string) {
		ctx := testhelper.Context(t)
		s := openPool(t, ctx, dsn, sqlstore.Pool{MaxOpenConns: 1, MaxIdleConns: 1, ConnMaxIdleTime: time.Minute, ConnMaxLifetime: time.Hour})
		require.Equal(t, 1, s.DB().Stats().MaxOpenConnections)

		held, err := s.DB().Conn(ctx)
		require.NoError(t, err)

		waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err = s.GetRepository(waitCtx, testKey("absent"))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, time.Since(start), 5*time.Second, "the wait for a connection ends with the context")

		require.NoError(t, held.Close())
		_, err = s.GetRepository(ctx, testKey("absent"))
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound, "the released connection serves the next query")
	})
}

// TestPoolOfOneConnectionServesConcurrentOperations runs the operations of the write path and the
// catch-up concurrently on a Postgres store with a single connection. Every operation holds one
// connection at a time, so they queue for it and all complete: none holds a connection, or a row
// lock, while it waits for another.
func TestPoolOfOneConnectionServesConcurrentOperations(t *testing.T) {
	forPostgres(t, func(t *testing.T, dsn string) {
		ctx, cancel := context.WithTimeout(testhelper.Context(t), time.Minute)
		defer cancel()
		s := openPool(t, ctx, dsn, sqlstore.Pool{MaxOpenConns: 1, MaxIdleConns: 1})

		const repos, writersPerRepo, commits = 2, 4, 10
		repoIDs := make([]int64, repos)
		for r := range repoIDs {
			repo, _, err := s.RegisterRepository(ctx, testKey(fmt.Sprint("pool-", r)), "sha1", corestore.InitialState{})
			require.NoError(t, err)
			repoIDs[r] = repo.ID
		}

		var wg sync.WaitGroup
		errs := make(chan error, repos*writersPerRepo)
		for r := 0; r < repos; r++ {
			for w := 0; w < writersPerRepo; w++ {
				wg.Add(1)
				go func(repoID int64, w int) {
					defer wg.Done()
					branch := fmt.Sprintf("refs/heads/writer-%d", w)
					old := corestore.Target("")
					for i := 0; i < commits; i++ {
						tip := oid(1000*(w+1) + i)
						if _, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
							RepoID:    repoID,
							Updates:   []corestore.RefUpdate{{Name: branch, Expect: corestore.ExpectValue, Old: old, New: tip}},
							Operation: "land",
							Actor:     "tester",
							NodeID:    "node-a",
							Changelog: &changelog.Meta{Operation: "land", Actor: "tester"},
						}); err != nil {
							errs <- err
							return
						}
						old = tip
						if _, _, err := s.Refs(ctx, repoID); err != nil {
							errs <- err
							return
						}
						if _, err := s.ScanChangelog(ctx, repoID, 0, 0); err != nil {
							errs <- err
							return
						}
						if _, _, err := s.ConflictingRef(ctx, repoID, branch+"/below"); err != nil {
							errs <- err
							return
						}
					}
				}(repoIDs[r], w)
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}

		for _, repoID := range repoIDs {
			refs, _, err := s.Refs(ctx, repoID)
			require.NoError(t, err)
			require.Len(t, refs, writersPerRepo)
		}
	})
}

// TestPoolDoesNotApplyToSQLite checks that a SQLite store keeps the pool of database/sql as before:
// its writers are queued by the store, and its connections are local files.
func TestPoolDoesNotApplyToSQLite(t *testing.T) {
	ctx := testhelper.Context(t)
	s := openPool(t, ctx, coretest.DatabaseURL(t, sqlstore.SQLite), sqlstore.Pool{MaxOpenConns: 1, MaxIdleConns: 1})
	require.Zero(t, s.DB().Stats().MaxOpenConnections, "unbounded, as without a pool")
}

func TestPoolValidate(t *testing.T) {
	require.NoError(t, sqlstore.DefaultPool.Validate())
	require.NoError(t, sqlstore.Pool{MaxOpenConns: 1}.Validate())
	for _, pool := range []sqlstore.Pool{
		{},
		{MaxOpenConns: 2, MaxIdleConns: 3},
		{MaxOpenConns: 2, MaxIdleConns: -1},
		{MaxOpenConns: 2, ConnMaxIdleTime: -time.Second},
		{MaxOpenConns: 2, ConnMaxLifetime: -time.Second},
	} {
		require.Error(t, pool.Validate(), "%+v", pool)
	}
}
