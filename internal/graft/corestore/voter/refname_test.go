package voter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

// failingConflictRead is a database whose read of conflicting names fails.
type failingConflictRead struct{ corestore.MetaStore }

func (failingConflictRead) ConflictingRef(context.Context, int64, string) (string, bool, error) {
	return "", false, errors.New("the database is gone")
}

// TestCreatedRefConflict covers the check of the update hook: the name the database holds, in
// both directions; nothing for a repository the database does not track or whose import is not
// complete; and a failed read, which refuses in authoritative mode only. Shadow mode refuses
// nothing, and counts the conflicts.
func TestCreatedRefConflict(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, mode := range []corestore.Mode{corestore.ModeAuthoritative, corestore.ModeShadow} {
			t.Run(string(mode), func(t *testing.T) {
				f := newFixture(t, fixtureOptions{mode: mode, dialect: dialect})
				f.seed(t, "refs/heads/a", corestore.Target(f.b.String()))
				f.seed(t, "refs/heads/x/y", corestore.Target(f.b.String()))

				shadow := mode == corestore.ModeShadow
				for ref, want := range map[string]string{
					"refs/heads/a/b":   "refs/heads/a",
					"refs/heads/a/b/c": "refs/heads/a",
					"refs/heads/x":     "refs/heads/x/y",
					"refs/heads/ab":    "",
					"refs/heads/x/z":   "",
					"refs/heads/a":     "",
				} {
					got, err := f.voter.CreatedRefConflict(f.ctx, f.repo, ref)
					require.NoError(t, err)
					if shadow {
						// Shadow mode never refuses a git write: it counts the conflict only.
						want = ""
					}
					require.Equal(t, want, got, ref)
				}
				if shadow {
					require.Equal(t, float64(3), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
				}

				f.core.Meta = failingConflictRead{f.core.Meta}
				got, err := f.voter.CreatedRefConflict(f.ctx, f.repo, "refs/heads/a/b")
				if !shadow {
					require.Equal(t, codes.Unavailable, status.Code(err), "error: %v", err)
					return
				}
				require.NoError(t, err, "shadow mode never refuses a git write")
				require.Empty(t, got)
				require.Equal(t, float64(4), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
			})
		}

		for desc, opts := range map[string]fixtureOptions{
			"untracked":  {mode: corestore.ModeShadow, dialect: dialect, untracked: true},
			"incomplete": {mode: corestore.ModeAuthoritative, dialect: dialect, incomplete: true},
		} {
			t.Run(desc, func(t *testing.T) {
				f := newFixture(t, opts)
				got, err := f.voter.CreatedRefConflict(f.ctx, f.repo, "refs/heads/a/b")
				require.NoError(t, err)
				require.Empty(t, got)
			})
		}
	})
}
