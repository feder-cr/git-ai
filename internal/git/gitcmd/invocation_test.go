package gitcmd_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

func newInvocation(t *testing.T) string {
	t.Helper()
	invocation, err := gitcmd.NewChangelogInvocation()
	require.NoError(t, err)
	return invocation
}

func TestInvocationLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("the end calls the function that was set last, once", func(t *testing.T) {
		t.Parallel()

		invocation := newInvocation(t)
		end := gitcmd.BeginInvocation(invocation)

		var first, last int
		require.True(t, gitcmd.OnInvocationExit(invocation, func() { first++ }))
		require.True(t, gitcmd.OnInvocationExit(invocation, func() { last++ }))

		end()
		end()
		require.Zero(t, first)
		require.Equal(t, 1, last)
	})

	t.Run("the end without a function does nothing", func(t *testing.T) {
		t.Parallel()

		invocation := newInvocation(t)
		end := gitcmd.BeginInvocation(invocation)
		end()
		require.False(t, gitcmd.OnInvocationExit(invocation, func() {}))
	})

	t.Run("an ended invocation refuses a function", func(t *testing.T) {
		t.Parallel()

		invocation := newInvocation(t)
		gitcmd.BeginInvocation(invocation)()

		require.False(t, gitcmd.OnInvocationExit(invocation, func() {
			t.Error("the function of an ended invocation must not run")
		}))
	})

	t.Run("an unknown invocation refuses a function", func(t *testing.T) {
		t.Parallel()
		require.False(t, gitcmd.OnInvocationExit(newInvocation(t), func() {}))
	})

	t.Run("an invocation is live until its end", func(t *testing.T) {
		t.Parallel()

		invocation := newInvocation(t)
		require.False(t, gitcmd.InvocationLive(invocation))

		end := gitcmd.BeginInvocation(invocation)
		require.True(t, gitcmd.InvocationLive(invocation))

		// The function of the end sees it as ended already.
		var liveInExit bool
		require.True(t, gitcmd.OnInvocationExit(invocation, func() { liveInExit = gitcmd.InvocationLive(invocation) }))
		end()
		require.False(t, liveInExit)
		require.False(t, gitcmd.InvocationLive(invocation))
	})

	t.Run("the empty invocation is none", func(t *testing.T) {
		t.Parallel()

		end := gitcmd.BeginInvocation("")
		require.False(t, gitcmd.OnInvocationExit("", func() {}))
		end()
	})

	t.Run("either the function is refused or it runs", func(t *testing.T) {
		t.Parallel()

		for round := 0; round < 1000; round++ {
			invocation := newInvocation(t)
			end := gitcmd.BeginInvocation(invocation)

			var called atomic.Int32
			var accepted atomic.Bool
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				end()
			}()
			go func() {
				defer wg.Done()
				accepted.Store(gitcmd.OnInvocationExit(invocation, func() { called.Add(1) }))
			}()
			wg.Wait()

			want := int32(0)
			if accepted.Load() {
				want = 1
			}
			require.Equal(t, want, called.Load(), "round %d", round)
		}
	})
}

// TestCommandEndsItsInvocation verifies that the invocation of a git command is live until the
// process has exited, in every way a process ends.
func TestCommandEndsItsInvocation(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T) (func(context.Context, gitcmd.Command, ...gitcmd.CmdOpt) (string, func() error, error), context.Context) {
		t.Helper()

		ctx := testhelper.Context(t)
		cfg := testcfg.Build(t)
		repoProto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
			SkipCreationViaService: true,
		})
		repo := localrepo.NewTestRepo(t, cfg, repoProto)
		objectHash, err := repo.ObjectHash(ctx)
		require.NoError(t, err)
		factory := gittest.NewCommandFactory(t, cfg, gitcmd.WithSkipHooks())

		return func(ctx context.Context, sc gitcmd.Command, opts ...gitcmd.CmdOpt) (string, func() error, error) {
			var invocation string
			opts = append([]gitcmd.CmdOpt{gitcmd.WithRefTxHook(objectHash, repo), gitcmd.CaptureInvocation(&invocation)}, opts...)
			cmd, err := factory.New(ctx, repoProto, sc, opts...)
			if err != nil {
				return invocation, nil, err
			}
			return invocation, cmd.Wait, nil
		}, ctx
	}

	updateRef := gitcmd.Command{Name: "update-ref", Flags: []gitcmd.Option{gitcmd.Flag{Name: "-z"}, gitcmd.Flag{Name: "--stdin"}}}

	// requireEndsAfter sets a function on the invocation and verifies the invocation is live until
	// the action, and has ended after it.
	requireEndsAfter := func(t *testing.T, invocation string, action func()) {
		t.Helper()

		var ended atomic.Bool
		require.True(t, gitcmd.OnInvocationExit(invocation, func() { ended.Store(true) }), "the invocation is live")
		require.False(t, ended.Load())

		action()

		require.True(t, ended.Load())
		require.False(t, gitcmd.OnInvocationExit(invocation, func() {}))
	}

	t.Run("a process that exits normally", func(t *testing.T) {
		t.Parallel()

		start, ctx := setup(t)
		invocation, wait, err := start(ctx, updateRef, gitcmd.WithStdin(strings.NewReader("")))
		require.NoError(t, err)
		require.NotEmpty(t, invocation)

		requireEndsAfter(t, invocation, func() { require.NoError(t, wait()) })
	})

	t.Run("a process that fails", func(t *testing.T) {
		t.Parallel()

		start, ctx := setup(t)
		invocation, wait, err := start(ctx, gitcmd.Command{Name: "update-ref", Args: []string{"refs/heads/main", "not-a-revision"}})
		require.NoError(t, err)

		requireEndsAfter(t, invocation, func() { require.Error(t, wait()) })
	})

	t.Run("a process killed by the context, without a caller that waits", func(t *testing.T) {
		t.Parallel()

		start, ctx := setup(t)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		invocation, _, err := start(ctx, updateRef, gitcmd.WithSetupStdin())
		require.NoError(t, err)

		var ended atomic.Bool
		require.True(t, gitcmd.OnInvocationExit(invocation, func() { ended.Store(true) }))
		require.False(t, ended.Load(), "git waits on its standard input")

		cancel()
		require.Eventually(t, ended.Load, time.Minute, time.Millisecond)
		require.False(t, gitcmd.OnInvocationExit(invocation, func() {}))
	})

	t.Run("a process that does not start", func(t *testing.T) {
		t.Parallel()

		start, ctx := setup(t)
		invocation, _, err := start(ctx, gitcmd.Command{Name: "update-ref", Args: []string{"nul\x00byte"}})
		require.Error(t, err)
		require.NotEmpty(t, invocation, "the command got as far as its options")
		require.False(t, gitcmd.OnInvocationExit(invocation, func() {}))
	})
}

func TestInvocationRequired(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		changelogDisabled bool
		mode              string
		want              bool
	}{
		{want: true},
		{mode: "shadow", want: true},
		{mode: "authoritative", want: true},
		{changelogDisabled: true, want: false},
		{changelogDisabled: true, mode: "shadow", want: false},
		{changelogDisabled: true, mode: "authoritative", want: true},
	} {
		t.Run(fmt.Sprintf("disabled=%v mode=%q", tc.changelogDisabled, tc.mode), func(t *testing.T) {
			t.Parallel()

			cfg := testcfg.Build(t)
			cfg.Graft.Changelog.Disabled = tc.changelogDisabled
			cfg.Graft.Storage.Mode = tc.mode
			require.Equal(t, tc.want, gitcmd.InvocationRequired(cfg))
		})
	}
}
