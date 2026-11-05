package updateref_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// localValue returns the immediate value of the reference in the repository at repoPath, as the
// storage core's voter reads it under the lock: an object ID, "ref:<target>" for a symbolic
// reference, or "" when it does not exist.
func localValue(t *testing.T, cfg config.Cfg, repoPath, reference string) string {
	t.Helper()

	out := gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref",
		"--format=%(refname) %(if)%(symref)%(then)ref:%(symref)%(else)%(objectname)%(end)", reference)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, value, ok := strings.Cut(line, " "); ok && name == reference {
			return value
		}
	}
	return ""
}

// TestUpdaterWithHooks_GitChecksTheOldValueBeforeThePreparedHook decides whether the storage core
// must read the local references in the Prepared phase of the server's own writes
// (doc/graft/design/dbfirst-writes.md, stage 1a). UpdaterWithHooks always passes git an old value
// (the zero object ID meaning "must not exist") and drives the reference-transaction hook itself,
// calling the Prepared phase only after git's prepare succeeded. For every kind of update, on both
// reference backends, it checks whether git's prepare refuses an old value that the local reference
// does not have before the Prepared phase runs, and what the local value is when it does run.
func TestUpdaterWithHooks_GitChecksTheOldValueBeforeThePreparedHook(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	zero := gittest.DefaultObjectHash.ZeroOID

	const existing, absent = "refs/heads/existing", "refs/heads/absent"

	for _, backend := range []git.ReferenceBackend{git.ReferenceBackendReftables, git.ReferenceBackendFiles} {
		t.Run(backend.Name, func(t *testing.T) {
			t.Parallel()

			type setup struct {
				repo     *gitalypb.Repository
				repoPath string
				// current is the commit of existing, next a child of it, other an unrelated commit.
				current, next, other git.ObjectID
			}
			newSetup := func(t *testing.T) setup {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
					SkipCreationViaService: true,
					ReferenceBackend:       backend,
				})
				current := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("current"))
				next := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("next"), gittest.WithParents(current))
				other := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("other"))
				gittest.WriteRef(t, cfg, repoPath, existing, current)
				return setup{repo: repo, repoPath: repoPath, current: current, next: next, other: other}
			}

			for _, tc := range []struct {
				desc      string
				reference git.ReferenceName
				// newValue and oldValue pick the values of the update from the setup.
				newValue, oldValue func(s setup) git.ObjectID
				// refused tells that git refuses the update before the Prepared phase.
				refused bool
			}{
				{
					desc: "create of an absent reference", reference: absent,
					newValue: func(s setup) git.ObjectID { return s.next },
					oldValue: func(setup) git.ObjectID { return zero },
				},
				{
					desc: "create of an existing reference", reference: existing, refused: true,
					newValue: func(s setup) git.ObjectID { return s.next },
					oldValue: func(setup) git.ObjectID { return zero },
				},
				{
					desc: "update with the current old value", reference: existing,
					newValue: func(s setup) git.ObjectID { return s.next },
					oldValue: func(s setup) git.ObjectID { return s.current },
				},
				{
					desc: "update with another old value", reference: existing, refused: true,
					newValue: func(s setup) git.ObjectID { return s.next },
					oldValue: func(s setup) git.ObjectID { return s.other },
				},
				{
					desc: "update of an absent reference with an old value", reference: absent, refused: true,
					newValue: func(s setup) git.ObjectID { return s.next },
					oldValue: func(s setup) git.ObjectID { return s.current },
				},
				{
					desc: "deletion with the current old value", reference: existing,
					newValue: func(setup) git.ObjectID { return zero },
					oldValue: func(s setup) git.ObjectID { return s.current },
				},
				{
					desc: "deletion with another old value", reference: existing, refused: true,
					newValue: func(setup) git.ObjectID { return zero },
					oldValue: func(s setup) git.ObjectID { return s.other },
				},
				{
					desc: "deletion of an absent reference", reference: absent, refused: true,
					newValue: func(setup) git.ObjectID { return zero },
					oldValue: func(s setup) git.ObjectID { return s.current },
				},
			} {
				t.Run(tc.desc, func(t *testing.T) {
					t.Parallel()

					s := newSetup(t)
					before := localValue(t, cfg, s.repoPath, tc.reference.String())
					oldValue, newValue := tc.oldValue(s), tc.newValue(s)

					var (
						mu       sync.Mutex
						states   []hook.ReferenceTransactionState
						atLocked string
					)
					hookManager := hook.NewMockManager(t, hook.NopPreReceive, hook.NopPostReceive, hook.NopUpdate,
						func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
							mu.Lock()
							defer mu.Unlock()
							states = append(states, state)
							if state == hook.ReferenceTransactionPrepared {
								// git holds the reference lock now, as when the voter reads.
								atLocked = localValue(t, cfg, s.repoPath, tc.reference.String())
							}
							return nil
						})

					updater := updateref.NewUpdaterWithHooks(cfg, testhelper.NewLogger(t), config.NewLocator(cfg), hookManager, gittest.NewCommandFactory(t, cfg), nil)
					err := updater.UpdateReference(ctx, s.repo, gittest.TestUser, nil, tc.reference, newValue, oldValue)

					mu.Lock()
					defer mu.Unlock()
					if tc.refused {
						var updateErr updateref.Error
						require.ErrorAs(t, err, &updateErr, "git's prepare refuses the update")
						require.NotContains(t, states, hook.ReferenceTransactionPrepared, "the Prepared phase never runs")
						require.Equal(t, before, localValue(t, cfg, s.repoPath, tc.reference.String()), "nothing is written")
						return
					}

					require.NoError(t, err)
					require.Contains(t, states, hook.ReferenceTransactionPrepared)
					want := oldValue.String()
					if oldValue == zero {
						want = ""
					}
					require.Equal(t, want, atLocked, "under the lock the local value is the old value")
				})
			}

			t.Run("an empty old value is refused before any hook", func(t *testing.T) {
				t.Parallel()

				s := newSetup(t)
				var called bool
				hookManager := hook.NewMockManager(t,
					func(*testing.T, context.Context, *gitalypb.Repository, []string, []string, io.Reader, io.Writer, io.Writer) error {
						called = true
						return nil
					},
					hook.NopPostReceive, hook.NopUpdate,
					func(*testing.T, context.Context, hook.ReferenceTransactionState, []string, io.Reader) error {
						called = true
						return nil
					})
				updater := updateref.NewUpdaterWithHooks(cfg, testhelper.NewLogger(t), config.NewLocator(cfg), hookManager, gittest.NewCommandFactory(t, cfg), nil)

				// UpdaterWithHooks has no forced update: an old value is required.
				require.Error(t, updater.UpdateReference(ctx, s.repo, gittest.TestUser, nil, existing, s.next, ""))
				require.False(t, called)
				require.Equal(t, s.current.String(), localValue(t, cfg, s.repoPath, existing))
			})
		})
	}
}
