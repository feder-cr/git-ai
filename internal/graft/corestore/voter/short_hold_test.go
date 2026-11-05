package voter_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestPreparedOfAWriteGitChecked covers stage 1a of doc/graft/design/dbfirst-writes.md: the
// Prepared phase of a transaction whose old values git checked before it
// (corestore.WithOldValuesCheckedByGit, UpdaterWithHooks) takes them from the hook input and does
// not read the local references, and the Prepared phase does not read the repository record again
// when it uses the record and the pack of the Preparing phase.
func TestPreparedOfAWriteGitChecked(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		setup := func(t *testing.T, mode corestore.Mode) *fixture {
			f := newFixture(t, fixtureOptions{mode: mode, dialect: dialect})
			f.payload.RequestedHooks = gitcmd.ReceivePackHooks
			return f
		}
		checked := func(f *fixture, input []byte) error {
			return f.voter.Prepared(corestore.WithOldValuesCheckedByGit(f.ctx), f.payload, input, changelog.Meta{Operation: "test", Actor: "tester"})
		}
		steps := func(t *testing.T, f *fixture, step string) float64 {
			return metricValue(t, f.voter, "graft_storage_prepared_step_seconds", map[string]string{"step": step})
		}

		for _, mode := range []corestore.Mode{corestore.ModeAuthoritative, corestore.ModeShadow} {
			t.Run(string(mode)+": the expected values come from the input", func(t *testing.T) {
				f := setup(t, mode)
				f.seed(t, ref, corestore.Target(f.b.String()))
				// A local value that git's check would never leave in place: if the voter read it,
				// the receive-pack rule (rejectedByGit) would refuse the update.
				gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)

				require.NoError(t, checked(f, line(f.b.String(), f.c.String(), ref)))
				require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref))
				require.Zero(t, steps(t, f, "local_read"))
			})
		}

		t.Run("a create expects the reference absent", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative)

			require.NoError(t, checked(f, line(f.zero, f.a.String(), ref)))
			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref))

			// The database's compare-and-swap still decides: the reference exists now.
			err := checked(f, line(f.zero, f.b.String(), ref))
			var mismatch updateref.MismatchingStateError
			require.True(t, errors.As(err, &mismatch), "error: %v", err)
			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref))
		})

		t.Run("a stale old value loses the database's compare-and-swap", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative)
			f.seed(t, ref, corestore.Target(f.c.String()))

			err := checked(f, line(f.b.String(), f.a.String(), ref))
			var mismatch updateref.MismatchingStateError
			require.True(t, errors.As(err, &mismatch), "error: %v", err)
			require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref))
		})

		t.Run("without the mark a receive-pack still reads the local references", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative)
			f.seed(t, ref, corestore.Target(f.b.String()))
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)

			err := f.prepared(line(f.b.String(), f.c.String(), ref))
			var mismatch updateref.MismatchingStateError
			require.True(t, errors.As(err, &mismatch), "error: %v", err)
			require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, ref))
		})

		t.Run("the record of the Preparing phase is used", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative)
			input := line(f.zero, f.c.String(), ref)

			f.preparing(input)
			require.NoError(t, checked(f, input))
			require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref))
			require.Zero(t, steps(t, f, "record"), "the record is not read under the lock")
			require.Zero(t, steps(t, f, "local_read"))
			require.Equal(t, float64(1), steps(t, f, "commit"))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"result": "committed"}))
		})

		t.Run("without a Preparing phase the record is read", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative)

			require.NoError(t, checked(f, line(f.zero, f.c.String(), ref)))
			require.Equal(t, float64(1), steps(t, f, "record"))
		})

		t.Run("a repository removed since Preparing is counted once", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative)
			input := line(f.zero, f.c.String(), ref)
			f.preparing(input)

			deleted, err := f.core.Meta.DeleteRepository(f.ctx, corestore.KeyOf(f.repo))
			require.NoError(t, err)
			require.True(t, deleted)

			err = checked(f, input)
			require.Error(t, err)
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", nil),
				"the run again from a fresh read counts the transaction, the first run does not")
		})
	})
}
