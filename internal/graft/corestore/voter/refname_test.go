package voter_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestPreparedFileDirectoryConflictWithTheDatabase covers a reference that git
// accepts, because the name it conflicts with is not in the local copy yet: the
// database refuses it, and the voter reports git's own error for such a conflict.
func TestPreparedFileDirectoryConflictWithTheDatabase(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, tc := range []struct{ desc, existing, created string }{
			{"below a reference of the database", "refs/heads/a", "refs/heads/a/b"},
			{"above a reference of the database", "refs/heads/a/b", "refs/heads/a"},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				t.Run("authoritative", func(t *testing.T) {
					f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
					f.seed(t, tc.existing, corestore.Target(f.b.String()))
					head := f.headSeq(t)

					err := f.prepared(line(f.zero, f.a.String(), tc.created))
					var conflict updateref.FileDirectoryConflictError
					require.True(t, errors.As(err, &conflict), "error: %v", err)
					require.Equal(t, tc.created, conflict.ConflictingReferenceName)
					require.Equal(t, tc.existing, conflict.ExistingReferenceName)

					require.Empty(t, f.dbRef(t, tc.created), "a conflict writes nothing")
					require.Equal(t, head, f.headSeq(t))
					require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "authoritative", "result": "conflict"}))
				})

				// Git's value is what shadow mode records, but it must not make the
				// database something no node can apply once the mode is switched: the write
				// proceeds, unrecorded, and verification reports the difference.
				t.Run("shadow", func(t *testing.T) {
					f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect})
					f.seed(t, tc.existing, corestore.Target(f.b.String()))

					require.NoError(t, f.prepared(line(f.zero, f.a.String(), tc.created)))

					require.Empty(t, f.dbRef(t, tc.created))
					require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
					require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "conflict"}))
				})
			})
		}
	})
}
