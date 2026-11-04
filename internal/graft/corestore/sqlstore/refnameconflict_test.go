package sqlstore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// requireNameConflict checks that err is the refusal of name because of existing.
func requireNameConflict(t *testing.T, err error, name, existing string) {
	t.Helper()

	var conflict *corestore.ErrRefNameConflict
	require.True(t, errors.As(err, &conflict), "error: %v", err)
	require.Equal(t, name, conflict.Name)
	require.Equal(t, existing, conflict.Existing)
}

// TestCommitChangesetRefusesRefNameConflicts covers the names that the database
// refuses: a new reference below an existing one, a new reference above one, at
// any depth, against the references left by the same changeset, and forced.
func TestCommitChangesetRefusesRefNameConflicts(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		existing []string
		updates  func() []corestore.RefUpdate
		name     string
		conflict string
	}{
		{
			desc:     "new reference below an existing one",
			existing: []string{"refs/heads/a"},
			updates:  func() []corestore.RefUpdate { return []corestore.RefUpdate{create("refs/heads/a/b", oid(2))} },
			name:     "refs/heads/a/b",
			conflict: "refs/heads/a",
		},
		{
			desc:     "new reference several levels below an existing one",
			existing: []string{"refs/heads/a"},
			updates:  func() []corestore.RefUpdate { return []corestore.RefUpdate{create("refs/heads/a/b/c/d", oid(2))} },
			name:     "refs/heads/a/b/c/d",
			conflict: "refs/heads/a",
		},
		{
			desc:     "new reference above an existing one",
			existing: []string{"refs/heads/a/b"},
			updates:  func() []corestore.RefUpdate { return []corestore.RefUpdate{create("refs/heads/a", oid(2))} },
			name:     "refs/heads/a",
			conflict: "refs/heads/a/b",
		},
		{
			desc:     "new reference several levels above an existing one",
			existing: []string{"refs/heads/a/b/c/d"},
			updates:  func() []corestore.RefUpdate { return []corestore.RefUpdate{create("refs/heads/a", oid(2))} },
			name:     "refs/heads/a",
			conflict: "refs/heads/a/b/c/d",
		},
		{
			desc:     "a forced creation",
			existing: []string{"refs/heads/a"},
			updates:  func() []corestore.RefUpdate { return []corestore.RefUpdate{force("refs/heads/a/b", oid(2))} },
			name:     "refs/heads/a/b",
			conflict: "refs/heads/a",
		},
		{
			desc: "both names created by one changeset",
			updates: func() []corestore.RefUpdate {
				return []corestore.RefUpdate{create("refs/heads/a/b", oid(2)), create("refs/heads/a", oid(2))}
			},
			name:     "refs/heads/a/b",
			conflict: "refs/heads/a",
		},
		{
			desc:     "deleting a sibling leaves the conflict",
			existing: []string{"refs/heads/a/b", "refs/heads/a/c"},
			updates: func() []corestore.RefUpdate {
				return []corestore.RefUpdate{update("refs/heads/a/b", oid(1), ""), create("refs/heads/a", oid(2))}
			},
			name:     "refs/heads/a",
			conflict: "refs/heads/a/c",
		},
		{
			desc:     "a name that is not UTF-8",
			existing: []string{"refs/heads/\xff"},
			updates:  func() []corestore.RefUpdate { return []corestore.RefUpdate{create("refs/heads/\xff/b", oid(2))} },
			name:     "refs/heads/\xff/b",
			conflict: "refs/heads/\xff",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
				repo := register(t, ctx, s, testKey("r"))
				want := map[string]corestore.Target{}
				for _, name := range tc.existing {
					_, err := commit(ctx, s, repo.ID, create(name, oid(1)))
					require.NoError(t, err)
					want[name] = oid(1)
				}
				before, err := s.GetRepository(ctx, testKey("r"))
				require.NoError(t, err)

				meta := changelog.Meta{Operation: "test", Actor: "tester"}
				_, err = s.CommitChangeset(ctx, corestore.ChangesetInput{
					RepoID:    repo.ID,
					Updates:   tc.updates(),
					Force:     tc.desc == "a forced creation",
					Operation: "test",
					Actor:     "tester",
					NodeID:    "node-a",
					Changelog: &meta,
				})
				requireNameConflict(t, err, tc.name, tc.conflict)

				// Nothing of the changeset stays: not its other updates, its row, its
				// changelog entries or the sequences.
				requireRefs(t, ctx, s, repo.ID, want)
				after, err := s.GetRepository(ctx, testKey("r"))
				require.NoError(t, err)
				require.Equal(t, before.HeadSeq, after.HeadSeq)
				changesets, err := s.ListChangesets(ctx, repo.ID, before.HeadSeq, before.HeadSeq+10)
				require.NoError(t, err)
				require.Empty(t, changesets)
				entries, err := s.ScanChangelog(ctx, repo.ID, 0, 0)
				require.NoError(t, err)
				require.Empty(t, entries)
			})
		})
	}
}

// TestCommitChangesetAcceptsNamesWithoutConflict covers what must stay legal: a
// conflict is about directories, not about string prefixes, and it is judged on
// the state the changeset leaves.
func TestCommitChangesetAcceptsNamesWithoutConflict(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		existing []string
		updates  []corestore.RefUpdate
		want     map[string]corestore.Target
	}{
		{
			desc:     "delete then create below it, in one changeset",
			existing: []string{"refs/heads/a"},
			updates:  []corestore.RefUpdate{update("refs/heads/a", oid(1), ""), create("refs/heads/a/b", oid(2))},
			want:     map[string]corestore.Target{"refs/heads/a/b": oid(2)},
		},
		{
			desc:     "create below it, then delete, in one changeset",
			existing: []string{"refs/heads/a"},
			updates:  []corestore.RefUpdate{create("refs/heads/a/b", oid(2)), update("refs/heads/a", oid(1), "")},
			want:     map[string]corestore.Target{"refs/heads/a/b": oid(2)},
		},
		{
			desc:     "delete every child then create the parent, in one changeset",
			existing: []string{"refs/heads/a/b", "refs/heads/a/c/d"},
			updates: []corestore.RefUpdate{
				update("refs/heads/a/b", oid(1), ""),
				update("refs/heads/a/c/d", oid(1), ""),
				create("refs/heads/a", oid(2)),
			},
			want: map[string]corestore.Target{"refs/heads/a": oid(2)},
		},
		{
			// '-' and '.' sort before '/', '0' and 'b' after it: the bounds of the
			// range below refs/heads/a must exclude all of them.
			desc:     "names with a common prefix, sorted around the directory separator",
			existing: []string{"refs/heads/a-b", "refs/heads/a.b", "refs/heads/a0", "refs/heads/ab"},
			updates:  []corestore.RefUpdate{create("refs/heads/a", oid(2))},
			want: map[string]corestore.Target{
				"refs/heads/a-b": oid(1), "refs/heads/a.b": oid(1), "refs/heads/a0": oid(1), "refs/heads/ab": oid(1),
				"refs/heads/a": oid(2),
			},
		},
		{
			desc:     "a shorter name that is a string prefix of a directory",
			existing: []string{"refs/heads/a"},
			updates:  []corestore.RefUpdate{create("refs/heads/ab/c", oid(2))},
			want:     map[string]corestore.Target{"refs/heads/a": oid(1), "refs/heads/ab/c": oid(2)},
		},
		{
			desc:     "siblings in a directory",
			existing: []string{"refs/heads/a/b"},
			updates:  []corestore.RefUpdate{create("refs/heads/a/c", oid(2))},
			want:     map[string]corestore.Target{"refs/heads/a/b": oid(1), "refs/heads/a/c": oid(2)},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
				repo := register(t, ctx, s, testKey("r"))
				for _, name := range tc.existing {
					_, err := commit(ctx, s, repo.ID, create(name, oid(1)))
					require.NoError(t, err)
				}

				_, err := commit(ctx, s, repo.ID, tc.updates...)
				require.NoError(t, err)
				requireRefs(t, ctx, s, repo.ID, tc.want)
			})
		})
	}
}

// TestCommitChangesetOnExistingConflict covers a repository that holds a pair
// already (written before the check existed): the updates that do not create a
// name still pass, so that the pair can be repaired, and the deletion of one
// side does repair it.
func TestCommitChangesetOnExistingConflict(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("r"))
		_, err := commit(ctx, s, repo.ID, create("refs/heads/a", oid(1)))
		require.NoError(t, err)
		coretest.CommitRefWithoutNameCheck(t, ctx, s, repo.ID, "refs/heads/a/b", oid(1))

		// A new name below either side is still refused.
		_, err = commit(ctx, s, repo.ID, create("refs/heads/a/b/c", oid(2)))
		var conflict *corestore.ErrRefNameConflict
		require.True(t, errors.As(err, &conflict), "error: %v", err)

		_, err = commit(ctx, s, repo.ID, update("refs/heads/a", oid(1), oid(2)))
		require.NoError(t, err, "updating an existing reference creates no name")

		_, err = commit(ctx, s, repo.ID, update("refs/heads/a", oid(2), ""))
		require.NoError(t, err)

		refs, _, err := s.Refs(ctx, repo.ID)
		require.NoError(t, err)
		require.Equal(t, map[string]corestore.Target{"refs/heads/a/b": oid(1)}, refs)
		require.Empty(t, corestore.RefNameConflicts(refs))
	})
}

// TestRegisterRepositoryRefusesRefNameConflicts covers the other caller of the
// commit: the initial changeset of a registration, and its rollback.
func TestRegisterRepositoryRefusesRefNameConflicts(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		_, _, err := s.RegisterRepository(ctx, testKey("r"), "sha1", corestore.InitialState{
			Changeset: &corestore.ChangesetInput{
				Updates:   []corestore.RefUpdate{create("refs/heads/a", oid(1)), create("refs/heads/a/b", oid(1))},
				Operation: "test",
				Actor:     "tester",
				NodeID:    "node-a",
			},
		})
		var conflict *corestore.ErrRefNameConflict
		require.True(t, errors.As(err, &conflict), "error: %v", err)

		_, err = s.GetRepository(ctx, testKey("r"))
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound, "the registration is rolled back")
	})
}

// TestCommitChangesetRefNameConflictRace commits both sides of a pair at once
// through two stores, as two nodes do: exactly one wins, in every round.
func TestCommitChangesetRefNameConflictRace(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		other := openStore(t, ctx, coreDSN(t, s))
		repo := register(t, ctx, s, testKey("r"))

		want := map[string]corestore.Target{}
		for round := 0; round < 25; round++ {
			parent := fmt.Sprintf("refs/heads/r%d", round)
			child := parent + "/x"
			// Each side wins some rounds: the order of the commits is not fixed.
			first, second := s, other
			if round%2 == 1 {
				first, second = other, s
			}

			var (
				wg    sync.WaitGroup
				start = make(chan struct{})
				errs  [2]error
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, errs[0] = commit(ctx, first, repo.ID, create(parent, oid(1)))
			}()
			go func() {
				defer wg.Done()
				<-start
				_, errs[1] = commit(ctx, second, repo.ID, create(child, oid(1)))
			}()
			close(start)
			wg.Wait()

			switch {
			case errs[0] == nil:
				requireNameConflict(t, errs[1], child, parent)
				want[parent] = oid(1)
			case errs[1] == nil:
				requireNameConflict(t, errs[0], parent, child)
				want[child] = oid(1)
			default:
				require.Fail(t, "both sides were refused", "%v; %v", errs[0], errs[1])
			}
		}

		requireRefs(t, ctx, s, repo.ID, want)
	})
}

// TestCommitChangesetSeesTheCommitItWaitedFor fixes the order of the race: the
// second commit starts while the first transaction holds the lock of the
// repository, with its row written and not committed. It must wait, then see
// the row. On Postgres the test also checks that it really waited on the row
// lock; READ COMMITTED is what makes the statements after the lock see the row.
func TestCommitChangesetSeesTheCommitItWaitedFor(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		other := openStore(t, ctx, coreDSN(t, s))
		repo := register(t, ctx, s, testKey("r"))

		tx, err := s.DB().BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		if s.Dialect() == sqlstore.Postgres {
			_, err = tx.ExecContext(ctx, "SELECT 1 FROM graft_repositories WHERE repo_id = $1 FOR UPDATE", repo.ID)
			require.NoError(t, err)
		}
		_, err = tx.ExecContext(ctx, s.Bind("INSERT INTO graft_refs (repo_id, name, target, seq) VALUES (?, ?, ?, 1)"),
			repo.ID, []byte("refs/heads/a"), string(oid(1)))
		require.NoError(t, err)

		refused := make(chan error, 1)
		go func() {
			_, err := commit(ctx, other, repo.ID, create("refs/heads/a/b", oid(1)))
			refused <- err
		}()

		if s.Dialect() == sqlstore.Postgres {
			require.Eventually(t, func() bool {
				var waiting int
				err := s.DB().QueryRowContext(ctx,
					"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'",
				).Scan(&waiting)
				require.NoError(t, err)
				return waiting > 0
			}, time.Minute, time.Millisecond, "the second commit waits for the row lock")
		}

		require.NoError(t, tx.Commit())
		requireNameConflict(t, <-refused, "refs/heads/a/b", "refs/heads/a")
	})
}

// TestRefNameConflictProbeUsesThePrimaryKey checks the plan of the query that a
// created reference runs: a primary key range, not a scan of the references.
func TestRefNameConflictProbeUsesThePrimaryKey(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("r"))

		const rows = 20000
		var fill, explain string
		switch s.Dialect() {
		case sqlstore.SQLite:
			fill = `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < ?)
				INSERT INTO graft_refs (repo_id, name, target, seq)
				SELECT ?, CAST('refs/heads/b' || x AS BLOB), 'x', 1 FROM c`
			explain = "EXPLAIN QUERY PLAN "
		default:
			fill = `INSERT INTO graft_refs (repo_id, name, target, seq)
				SELECT $2, convert_to('refs/heads/b' || x, 'UTF8'), 'x', 1 FROM generate_series(1, $1::bigint) AS x`
			explain = "EXPLAIN "
		}
		_, err := s.DB().ExecContext(ctx, fill, rows, repo.ID)
		require.NoError(t, err)
		_, err = s.DB().ExecContext(ctx, "ANALYZE graft_refs")
		require.NoError(t, err)

		lower, upper := sqlstore.RefChildrenRange("refs/heads/a")
		planRows, err := s.DB().QueryContext(ctx, explain+s.Bind(sqlstore.FirstRefInRangeQuery), repo.ID, lower, upper)
		require.NoError(t, err)
		defer planRows.Close()

		columns, err := planRows.Columns()
		require.NoError(t, err)

		var plan strings.Builder
		for planRows.Next() {
			values := make([]any, len(columns))
			for i := range values {
				values[i] = new(any)
			}
			require.NoError(t, planRows.Scan(values...))
			fmt.Fprintln(&plan, *(values[len(values)-1].(*any)))
		}
		require.NoError(t, planRows.Err())

		if s.Dialect() == sqlstore.SQLite {
			require.Contains(t, plan.String(), "SEARCH graft_refs USING", "plan: %s", plan.String())
			require.Contains(t, plan.String(), "name>? AND name<?", "plan: %s", plan.String())
			return
		}
		require.Contains(t, plan.String(), "Index", "plan: %s", plan.String())
		require.NotContains(t, plan.String(), "Seq Scan", "plan: %s", plan.String())
	})
}

func TestRefNameConflicts(t *testing.T) {
	t.Parallel()

	require.Empty(t, corestore.RefNameConflicts(map[string]corestore.Target{
		"HEAD": "ref:refs/heads/a", "refs/heads/a": oid(1), "refs/heads/a-b": oid(1), "refs/heads/ab/c": oid(1),
	}))
	require.Equal(t,
		[]corestore.RefNameConflict{
			{Parent: "refs/heads/a", Child: "refs/heads/a/b"},
			{Parent: "refs/heads/a", Child: "refs/heads/a/b/c"},
			{Parent: "refs/heads/a/b", Child: "refs/heads/a/b/c"},
		},
		corestore.RefNameConflicts(map[string]corestore.Target{
			"refs/heads/a": oid(1), "refs/heads/a/b": oid(1), "refs/heads/a/b/c": oid(1), "refs/heads/z": oid(1),
		}),
	)
}
