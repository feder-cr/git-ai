package voter_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestPreparedZeroOldValueOfAReceivePack covers what the zero old value of an
// existing local reference means, by who runs the transaction. receive-pack always
// sends an old value, so its zero means the reference must not exist: git rejected
// the update, and a non-atomic receive-pack feeds it to the hook all the same. A
// transaction that only asked for the reference-transaction hook (update-ref
// without an old value, a forced fetch) means "whatever it is now".
func TestPreparedZeroOldValueOfAReceivePack(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		setup := func(t *testing.T, mode corestore.Mode, hooks gitcmd.Hook) *fixture {
			f := newFixture(t, fixtureOptions{mode: mode, dialect: dialect})
			f.seed(t, ref, corestore.Target(f.b.String()))
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.b)
			f.payload.RequestedHooks = hooks
			return f
		}

		t.Run("authoritative receive-pack is refused", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative, gitcmd.ReceivePackHooks)
			head := f.headSeq(t)

			err := f.prepared(line(f.zero, f.a.String(), ref))
			var exists updateref.ReferenceAlreadyExistsError
			require.True(t, errors.As(err, &exists), "error: %v", err)
			require.Equal(t, ref, exists.ReferenceName)

			require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, ref), "the rejected update is not recorded")
			require.Equal(t, head, f.headSeq(t))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "authoritative", "result": "conflict"}))
		})

		t.Run("shadow receive-pack is not recorded", func(t *testing.T) {
			f := setup(t, corestore.ModeShadow, gitcmd.ReceivePackHooks)
			head := f.headSeq(t)

			require.NoError(t, f.prepared(line(f.zero, f.a.String(), ref)))

			require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, ref), "the rejected update is not recorded")
			require.Equal(t, head, f.headSeq(t))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "conflict"}))
		})

		t.Run("a creation of a receive-pack passes", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative, gitcmd.ReceivePackHooks)

			require.NoError(t, f.prepared(line(f.zero, f.a.String(), "refs/heads/new")))
			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, "refs/heads/new"))
		})

		t.Run("an update of a receive-pack that names the old value passes", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative, gitcmd.ReceivePackHooks)

			require.NoError(t, f.prepared(line(f.b.String(), f.c.String(), ref)))
			require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref))
		})

		t.Run("the reference-transaction hook alone stays forced", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative, gitcmd.ReferenceTransactionHook)

			require.NoError(t, f.prepared(line(f.zero, f.a.String(), ref)))
			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref))
		})
	})
}
