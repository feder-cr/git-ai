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

// TestPreparedStaleOldValueOfAReceivePack covers an update of a receive-pack whose old
// value is not the local value: git checked it under the lock it still holds, so it
// rejected the update, and a non-atomic receive-pack feeds it to the hook all the same.
// The database can hold that old value while the local copy is behind it (another node
// wrote the reference), so its compare-and-swap would pass: the update must be refused
// before it (doc/graft/design/dbfirst-writes.md, stage 1b).
func TestPreparedStaleOldValueOfAReceivePack(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		// The database holds c, the local copy is behind at b.
		setup := func(t *testing.T, mode corestore.Mode, hooks gitcmd.Hook) *fixture {
			f := newFixture(t, fixtureOptions{mode: mode, dialect: dialect})
			f.seed(t, ref, corestore.Target(f.c.String()))
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.b)
			f.payload.RequestedHooks = hooks
			return f
		}

		for _, tc := range []struct {
			desc     string
			newValue func(f *fixture) string
		}{
			{desc: "update", newValue: func(f *fixture) string { return f.a.String() }},
			{desc: "deletion", newValue: func(f *fixture) string { return f.zero }},
		} {
			t.Run("authoritative receive-pack "+tc.desc+" is refused", func(t *testing.T) {
				f := setup(t, corestore.ModeAuthoritative, gitcmd.ReceivePackHooks)
				head := f.headSeq(t)

				err := f.prepared(line(f.c.String(), tc.newValue(f), ref))
				var mismatch updateref.MismatchingStateError
				require.True(t, errors.As(err, &mismatch), "error: %v", err)
				require.Equal(t, updateref.MismatchingStateError{
					ReferenceName:    ref,
					ExpectedObjectID: f.c.String(),
					ActualObjectID:   f.b.String(),
				}, mismatch)

				require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref), "the rejected update is not recorded")
				require.Equal(t, head, f.headSeq(t))
				require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "authoritative", "result": "conflict"}))
			})
		}

		t.Run("an absent local reference is reported with the zero object ID", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative, gitcmd.ReceivePackHooks)
			const missing = "refs/heads/missing"
			f.seed(t, missing, corestore.Target(f.c.String()))

			err := f.prepared(line(f.c.String(), f.a.String(), missing))
			var mismatch updateref.MismatchingStateError
			require.True(t, errors.As(err, &mismatch), "error: %v", err)
			require.Equal(t, f.zero, mismatch.ActualObjectID)
			require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, missing))
		})

		t.Run("shadow receive-pack is not recorded and not refused", func(t *testing.T) {
			f := setup(t, corestore.ModeShadow, gitcmd.ReceivePackHooks)
			head := f.headSeq(t)

			require.NoError(t, f.prepared(line(f.c.String(), f.a.String(), ref)))

			require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref), "the rejected update is not recorded")
			require.Equal(t, head, f.headSeq(t))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
		})

		t.Run("an update whose old value is the local value passes", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative, gitcmd.ReceivePackHooks)

			// The database's compare-and-swap decides: here the local copy is behind.
			err := f.prepared(line(f.b.String(), f.a.String(), ref))
			var mismatch updateref.MismatchingStateError
			require.True(t, errors.As(err, &mismatch), "error: %v", err)
			require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref))

			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.c)
			require.NoError(t, f.prepared(line(f.c.String(), f.a.String(), ref)))
			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref))
		})

		t.Run("the reference-transaction hook alone is not checked against the local value", func(t *testing.T) {
			f := setup(t, corestore.ModeAuthoritative, gitcmd.ReferenceTransactionHook)

			require.NoError(t, f.prepared(line(f.c.String(), f.a.String(), ref)))
			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref))
		})
	})
}
