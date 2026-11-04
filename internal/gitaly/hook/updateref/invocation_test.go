package updateref_test

import (
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// invocationWatchingHookManager watches the end of the invocation from the preparing phase of the
// reference-transaction hook, as the storage core does, and panics in the prepared phase on demand.
type invocationWatchingHookManager struct {
	hook.Manager
	panicInPrepared bool
	live            atomic.Bool
	ended           atomic.Bool
}

func (m *invocationWatchingHookManager) ReferenceTransactionHook(ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
	switch state {
	case hook.ReferenceTransactionPreparing:
		meta, err := gitcmd.ChangelogMetaFromEnv(env)
		if err != nil {
			return err
		}
		m.live.Store(gitcmd.OnInvocationExit(meta.Invocation, func() { m.ended.Store(true) }))
	case hook.ReferenceTransactionPrepared:
		if m.panicInPrepared {
			panic("the driver of the hook panics")
		}
	}

	return m.Manager.ReferenceTransactionHook(ctx, state, env, stdin)
}

// TestUpdaterWithHooks_invocationEnds checks that the invocation of a call of UpdateReference is live
// while it drives the hook and has ended when it returns, however it returns.
func TestUpdaterWithHooks_invocationEnds(t *testing.T) {
	t.Parallel()

	const reference = git.ReferenceName("refs/heads/main")

	for _, tc := range []struct {
		desc              string
		changelogDisabled bool
		panicInPrepared   bool
		// update runs the update of the branch, which points at base.
		update func(t *testing.T, run func(newValue, oldValue git.ObjectID) error, base, a git.ObjectID)
	}{
		{
			desc: "a successful update",
			update: func(t *testing.T, run func(newValue, oldValue git.ObjectID) error, base, a git.ObjectID) {
				require.NoError(t, run(a, base))
			},
		},
		{
			desc: "an update whose prepare fails on a stale old value",
			update: func(t *testing.T, run func(newValue, oldValue git.ObjectID) error, base, a git.ObjectID) {
				require.Error(t, run(base, a))
			},
		},
		{
			desc:            "a panic while driving the hook",
			panicInPrepared: true,
			update: func(t *testing.T, run func(newValue, oldValue git.ObjectID) error, base, a git.ObjectID) {
				require.Panics(t, func() { _ = run(a, base) })
			},
		},
		{
			desc:              "authoritative mode without the changeset log",
			changelogDisabled: true,
			update: func(t *testing.T, run func(newValue, oldValue git.ObjectID) error, base, a git.ObjectID) {
				require.NoError(t, run(a, base))
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			if tc.changelogDisabled {
				cfg.Graft.Changelog.Disabled = true
				cfg.Graft.Storage.Mode = "authoritative"
			}
			logger := testhelper.NewLogger(t)
			locator := config.NewLocator(cfg)
			gitCmdFactory := gittest.NewCommandFactory(t, cfg)

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})
			base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithMessage("base"))
			a := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithMessage("a"))

			manager := &invocationWatchingHookManager{
				Manager: hook.NewManager(cfg, locator, logger, gitCmdFactory, nil, nil, gitlab.NewMockClient(
					t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive,
				), hook.NewPostReceiveRegistry()),
				panicInPrepared: tc.panicInPrepared,
			}
			updater := updateref.NewUpdaterWithHooks(cfg, logger, locator, manager, gitCmdFactory, nil)

			tc.update(t, func(newValue, oldValue git.ObjectID) error {
				return updater.UpdateReference(ctx, repo, gittest.TestUser, nil, reference, newValue, oldValue)
			}, base, a)

			require.True(t, manager.live.Load(), "the invocation is live while the hook runs")
			require.True(t, manager.ended.Load(), "the invocation ended when the call returned")
		})
	}
}
