package syncer_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestSyncerNodesCannotCommitConflictingNames has two nodes, each up to date
// when it commits, create the two sides of a file/directory conflict. Git would
// accept each on its own copy. The database accepts one, so the repository stays
// something every node can apply: the nodes catch up, and a new node hydrates.
func TestSyncerNodesCannotCommitConflictingNames(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, tc := range []struct{ desc, first, second string }{
			{"the parent first", "refs/heads/a", "refs/heads/a/b"},
			{"the child first", "refs/heads/a/b", "refs/heads/a"},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				a := newEnv(t, envOptions{dialect: dialect})
				b := a.secondNode(t, dialect)
				src := a.newSource(t)
				c1 := a.writeCommit(t, src)
				repo := a.register(t, a.key(localRelPath), src, c1)
				require.NoError(t, a.sy.EnsureFresh(a.ctx, a.pbRepo(localRelPath)))
				require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))

				a.changeset(t, repo.ID, src, []corestore.RefUpdate{createRef(tc.first, corestore.Target(c1))})

				_, err := b.core.Meta.CommitChangeset(b.ctx, corestore.ChangesetInput{
					RepoID:    repo.ID,
					Updates:   []corestore.RefUpdate{createRef(tc.second, corestore.Target(c1))},
					Operation: "test",
					Actor:     "tester",
					NodeID:    "node-b",
				})
				var conflict *corestore.ErrRefNameConflict
				require.True(t, errors.As(err, &conflict), "error: %v", err)

				require.NoError(t, a.sy.EnsureFresh(a.ctx, a.pbRepo(localRelPath)))
				require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))
				a.verifySynced(t, repo)
				b.verifySynced(t, repo)

				fresh := a.secondNode(t, dialect)
				require.NoError(t, fresh.sy.EnsureFresh(fresh.ctx, fresh.pbRepo(localRelPath)))
				fresh.verifySynced(t, repo)
			})
		}
	})
}

// TestSyncerAppliesDeleteThenCreateOfConflictingNames covers the changeset that
// must stay legal, in both directions: it replaces a reference by one in its
// directory, or the other way round. A node with the old reference catches up, a
// new node hydrates.
func TestSyncerAppliesDeleteThenCreateOfConflictingNames(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, tc := range []struct{ desc, old, replacement string }{
			{"a reference by one below it", "refs/heads/a", "refs/heads/a/b"},
			{"a reference by one above it", "refs/heads/a/b", "refs/heads/a"},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				a := newEnv(t, envOptions{dialect: dialect})
				b := a.secondNode(t, dialect)
				src := a.newSource(t)
				c1 := a.writeCommit(t, src)
				repo := a.register(t, a.key(localRelPath), src, c1)

				a.changeset(t, repo.ID, src, []corestore.RefUpdate{createRef(tc.old, corestore.Target(c1))})
				require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))

				a.changeset(t, repo.ID, src, []corestore.RefUpdate{
					{Name: tc.old, Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: ""},
					createRef(tc.replacement, corestore.Target(c1)),
				})

				require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))
				b.verifySynced(t, repo)

				fresh := a.secondNode(t, dialect)
				require.NoError(t, fresh.sy.EnsureFresh(fresh.ctx, fresh.pbRepo(localRelPath)))
				fresh.verifySynced(t, repo)
			})
		}
	})
}

// TestSyncerRepositoryWithConflictingNames covers a database that already holds
// a pair (written before the database refused it). Every catch-up and hydration
// fails, on every attempt, which is why such a repository needs a repair. Once
// one side is deleted by a changeset, every node recovers: the nodes that hold
// either side, and a new one.
func TestSyncerRepositoryWithConflictingNames(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		a := newEnv(t, envOptions{dialect: dialect})
		b := a.secondNode(t, dialect)
		src := a.newSource(t)
		c1 := a.writeCommit(t, src)
		repo := a.register(t, a.key(localRelPath), src, c1)
		require.NoError(t, a.sy.EnsureFresh(a.ctx, a.pbRepo(localRelPath)))
		require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))

		a.changeset(t, repo.ID, src, []corestore.RefUpdate{createRef("refs/heads/a", corestore.Target(c1))})
		coretest.CommitRefWithoutNameCheck(t, a.ctx, a.core.Meta, repo.ID, "refs/heads/a/b", corestore.Target(c1))

		fresh := a.secondNode(t, dialect)
		for attempt := 0; attempt < 2; attempt++ {
			for _, node := range []*env{a, b, fresh} {
				err := node.sy.EnsureFresh(node.ctx, node.pbRepo(localRelPath))
				require.Equal(t, codes.Internal, status.Code(err), "attempt %d: %v", attempt, err)
			}
		}

		_, err := a.core.Meta.CommitChangeset(a.ctx, corestore.ChangesetInput{
			RepoID:    repo.ID,
			Updates:   []corestore.RefUpdate{{Name: "refs/heads/a", Expect: corestore.ExpectValue, Old: corestore.Target(c1), New: ""}},
			Operation: "test",
			Actor:     "tester",
			NodeID:    "node-a",
		})
		require.NoError(t, err)

		for _, node := range []*env{a, b, fresh} {
			require.NoError(t, node.sy.EnsureFresh(node.ctx, node.pbRepo(localRelPath)))
			node.verifySynced(t, repo)
		}
	})
}
