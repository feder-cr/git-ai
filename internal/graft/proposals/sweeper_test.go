package proposals

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

const sweepTTL = 7 * 24 * time.Hour

// Every date of these tests is relative to this fixed instant, the "now" of the sweeps: nothing here
// depends on the wall clock.
var sweepNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func refName(branch string, i int) git.ReferenceName {
	return git.ReferenceName(fmt.Sprintf("%s%040x", BranchDir(branch), i+1))
}

// waitForUsers yields until n callers are inside Sweep for the branch of repo, or stop says that
// waiting is pointless. There is no timeout: the callers need nothing but the scheduler to get there.
func waitForUsers(t *testing.T, s *Sweeper, repo *localrepo.Repo, branch string, n int, stop func() bool) {
	t.Helper()
	key := sweepKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), dir: BranchDir(branch)}
	for !stop() {
		s.mu.Lock()
		users := 0
		if state := s.sweeps[key]; state != nil {
			users = state.users
		}
		s.mu.Unlock()
		if users >= n {
			return
		}
		runtime.Gosched()
	}
}

func never() bool { return false }

func requireReleased(t *testing.T, s *Sweeper) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Empty(t, s.sweeps, "the guard holds no key once its callers left")
}

// TestSweeper_concurrentSweepsIssueOneTransaction: with real git, N concurrent sweeps of a branch
// with surplus issue one transaction, none fails, and the surplus is gone.
func TestSweeper_concurrentSweepsIssueOneTransaction(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	counting := gittest.NewCountingCommandFactory(t, cfg)
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)
	repo := localrepo.New(testhelper.SharedLogger(t), config.NewLocator(cfg), counting, catfileCache, repoProto)

	zero := gittest.DefaultObjectHash.ZeroOID
	var ops []Op
	for i := 0; i < MaxPerBranch+3; i++ {
		c := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage(fmt.Sprintf("recent %d", i)), gittest.WithCommitterDate(sweepNow.Add(-time.Duration(i+1)*time.Minute)))
		ops = append(ops, Op{Name: refName("main", i), NewOID: c, OldOID: zero})
	}
	for i := 100; i < 103; i++ {
		c := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage(fmt.Sprintf("old %d", i)), gittest.WithCommitterDate(sweepNow.Add(-8*24*time.Hour)))
		ops = append(ops, Op{Name: refName("main", i), NewOID: c, OldOID: zero})
	}
	require.NoError(t, Update(ctx, repo, ops))
	counting.ResetCount()

	const callers = 16
	s := NewSweeper()
	start := make(chan struct{})
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = s.Sweep(ctx, repo, "main", sweepTTL, sweepNow)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "caller %d", i)
	}
	counting.RequireCommandCount(t, "update-ref", 1)
	refs := gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref", "--format=%(refname)", BranchDir("main"))
	require.Len(t, splitLines(refs), MaxPerBranch)
	requireReleased(t, s)
}

func splitLines(b []byte) []string {
	var lines []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			lines = append(lines, string(b[start:i]))
			start = i + 1
		}
	}
	return lines
}

// TestSweeper_coalesces: while a sweep runs no other runs for its key, and the callers that arrived
// meanwhile share one sweep that lists after them, since the running one may not have seen what they
// just wrote.
func TestSweeper_coalesces(t *testing.T) {
	t.Parallel()

	repo, _, _, _ := setup(t)
	ctx := testhelper.Context(t)
	s := NewSweeper()

	var current, peak, runs atomic.Int64
	entered := make(chan struct{}, 64)
	release := make(chan struct{})
	s.run = func(context.Context, *localrepo.Repo, string, time.Duration, time.Time) (int, error) {
		now := current.Add(1)
		for {
			p := peak.Load()
			if now <= p || peak.CompareAndSwap(p, now) {
				break
			}
		}
		runs.Add(1)
		entered <- struct{}{}
		<-release
		current.Add(-1)
		return 0, nil
	}

	const late = 8
	var wg sync.WaitGroup
	errs := make([]error, late+1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[0] = s.Sweep(ctx, repo, "main", sweepTTL, sweepNow)
	}()
	<-entered
	for i := 1; i <= late; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Sweep(ctx, repo, "main", sweepTTL, sweepNow)
		}()
	}
	waitForUsers(t, s, repo, "main", late+1, func() bool { return peak.Load() > 1 })
	require.EqualValues(t, 1, peak.Load(), "a second sweep ran while the first was running")
	close(release)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "caller %d", i)
	}
	require.EqualValues(t, 1, peak.Load())
	require.EqualValues(t, 2, runs.Load(), "the running sweep, and one for all who came during it")
	requireReleased(t, s)
}

// TestSweeper_keysAreIndependent: a running sweep holds back neither another branch nor another
// repository.
func TestSweeper_keysAreIndependent(t *testing.T) {
	t.Parallel()

	repo, _, _, _ := setup(t)
	otherRepo, _, _, _ := setup(t)
	ctx := testhelper.Context(t)
	s := NewSweeper()

	entered := make(chan struct{})
	release := make(chan struct{})
	s.run = func(_ context.Context, r *localrepo.Repo, dir string, _ time.Duration, _ time.Time) (int, error) {
		if r == repo && dir == BranchDir("main") {
			close(entered)
			<-release
		}
		return 0, nil
	}
	done := make(chan error, 1)
	go func() { done <- s.Sweep(ctx, repo, "main", sweepTTL, sweepNow) }()
	<-entered

	require.NoError(t, s.Sweep(ctx, repo, "other", sweepTTL, sweepNow), "another branch")
	require.NoError(t, s.Sweep(ctx, otherRepo, "main", sweepTTL, sweepNow), "another repository")
	close(release)
	require.NoError(t, <-done)
	requireReleased(t, s)
}

// TestSweeper_cancelledRunnerDoesNotStrandTheWaiter: a sweep cancelled with its caller's context
// answers that caller only; a waiting caller runs its own sweep under its own context and the
// surplus goes.
func TestSweeper_cancelledRunnerDoesNotStrandTheWaiter(t *testing.T) {
	t.Parallel()

	repo, _, commit, list := setup(t)
	base := testhelper.Context(t)
	require.NoError(t, Update(base, repo, []Op{{Name: refName("main", 0), NewOID: commit(sweepNow.Add(-8 * 24 * time.Hour)), OldOID: gittest.DefaultObjectHash.ZeroOID}}))

	type marker struct{}
	s := NewSweeper()
	var calls atomic.Int64
	entered := make(chan struct{})
	s.run = func(ctx context.Context, r *localrepo.Repo, dir string, ttl time.Duration, now time.Time) (int, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			return 0, ctx.Err()
		}
		require.Equal(t, "waiter", ctx.Value(marker{}), "the second sweep runs under the waiter's context")
		return Prune(ctx, r, dir, ttl, now)
	}

	runnerCtx, cancelRunner := context.WithCancel(base)
	defer cancelRunner()
	runner := make(chan error, 1)
	go func() { runner <- s.Sweep(runnerCtx, repo, "main", sweepTTL, sweepNow) }()
	<-entered

	waiter := make(chan error, 1)
	go func() {
		waiter <- s.Sweep(context.WithValue(base, marker{}, "waiter"), repo, "main", sweepTTL, sweepNow)
	}()
	waitForUsers(t, s, repo, "main", 2, never)
	cancelRunner()

	require.ErrorIs(t, <-runner, context.Canceled)
	require.NoError(t, <-waiter)
	require.EqualValues(t, 2, calls.Load())
	require.Empty(t, list(BranchDir("main")), "the waiter's sweep removed the surplus")
	requireReleased(t, s)
}

// TestSweeper_cancelledRunnerAloneReleases: with nobody waiting, a cancelled sweep leaves the guard
// free and the surplus in place, and the next sweep of the branch removes it.
func TestSweeper_cancelledRunnerAloneReleases(t *testing.T) {
	t.Parallel()

	repo, _, commit, list := setup(t)
	base := testhelper.Context(t)
	require.NoError(t, Update(base, repo, []Op{{Name: refName("main", 0), NewOID: commit(sweepNow.Add(-8 * 24 * time.Hour)), OldOID: gittest.DefaultObjectHash.ZeroOID}}))

	s := NewSweeper()
	ctx, cancel := context.WithCancel(base)
	cancel()
	require.Error(t, s.Sweep(ctx, repo, "main", sweepTTL, sweepNow))
	requireReleased(t, s)
	require.Len(t, list(BranchDir("main")), 1)

	require.NoError(t, s.Sweep(base, repo, "main", sweepTTL, sweepNow))
	require.Empty(t, list(BranchDir("main")))
	requireReleased(t, s)
}

// TestSweeper_cancelledWaiterLeavesAtOnce: a waiter's context ends its wait, not the running sweep.
func TestSweeper_cancelledWaiterLeavesAtOnce(t *testing.T) {
	t.Parallel()

	repo, _, _, _ := setup(t)
	base := testhelper.Context(t)
	s := NewSweeper()
	entered := make(chan struct{})
	release := make(chan struct{})
	s.run = func(ctx context.Context, _ *localrepo.Repo, _ string, _ time.Duration, _ time.Time) (int, error) {
		close(entered)
		<-release
		return 0, ctx.Err()
	}
	runner := make(chan error, 1)
	go func() { runner <- s.Sweep(base, repo, "main", sweepTTL, sweepNow) }()
	<-entered

	ctx, cancel := context.WithCancel(base)
	waiter := make(chan error, 1)
	go func() { waiter <- s.Sweep(ctx, repo, "main", sweepTTL, sweepNow) }()
	waitForUsers(t, s, repo, "main", 2, never)
	cancel()
	require.ErrorIs(t, <-waiter, context.Canceled, "it left while the sweep was still held")
	close(release)
	require.NoError(t, <-runner, "the running sweep did not see the waiter's cancellation")
	requireReleased(t, s)
}

// TestSweeper_failureIsNotSuccessForTheWaiter: a failed sweep answers its caller with the failure,
// and a waiter is not told that its surplus was handled: it sweeps again.
func TestSweeper_failureIsNotSuccessForTheWaiter(t *testing.T) {
	t.Parallel()

	repo, _, _, _ := setup(t)
	ctx := testhelper.Context(t)
	s := NewSweeper()
	failure := errors.New("the sweep failed")
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	s.run = func(context.Context, *localrepo.Repo, string, time.Duration, time.Time) (int, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return 0, failure
		}
		return 0, nil
	}
	runner := make(chan error, 1)
	go func() { runner <- s.Sweep(ctx, repo, "main", sweepTTL, sweepNow) }()
	<-entered
	waiter := make(chan error, 1)
	go func() { waiter <- s.Sweep(ctx, repo, "main", sweepTTL, sweepNow) }()
	waitForUsers(t, s, repo, "main", 2, never)
	close(release)

	require.ErrorIs(t, <-runner, failure)
	require.NoError(t, <-waiter)
	require.EqualValues(t, 2, calls.Load())
	requireReleased(t, s)
}

// TestSweeper_sharedSweepThatEndsBadlyCoversNobody: two callers queue behind a running sweep, and
// the sweep that lists after both, which would cover both, is run by one of them. When it fails, or
// is cancelled with the context of the one that runs it, that one gets the error and the other is
// not told that its surplus was handled: it sweeps under its own context, and the surplus goes.
func TestSweeper_sharedSweepThatEndsBadlyCoversNobody(t *testing.T) {
	t.Parallel()

	type cancelKey struct{}
	failure := errors.New("the sweep failed")
	for desc, endBadly := range map[string]func(ctx context.Context) error{
		"it fails": func(context.Context) error { return failure },
		"its caller's context is cancelled": func(ctx context.Context) error {
			ctx.Value(cancelKey{}).(context.CancelFunc)()
			<-ctx.Done()
			return ctx.Err()
		},
	} {
		t.Run(desc, func(t *testing.T) {
			t.Parallel()

			repo, _, commit, list := setup(t)
			base := testhelper.Context(t)
			require.NoError(t, Update(base, repo, []Op{{Name: refName("main", 0), NewOID: commit(sweepNow.Add(-8 * 24 * time.Hour)), OldOID: gittest.DefaultObjectHash.ZeroOID}}))

			s := NewSweeper()
			var calls atomic.Int64
			entered := make(chan struct{})
			release := make(chan struct{})
			s.run = func(ctx context.Context, r *localrepo.Repo, dir string, ttl time.Duration, now time.Time) (int, error) {
				switch calls.Add(1) {
				case 1:
					close(entered)
					<-release
					return 0, nil
				case 2:
					return 0, endBadly(ctx)
				default:
					return Prune(ctx, r, dir, ttl, now)
				}
			}

			first := make(chan error, 1)
			go func() { first <- s.Sweep(base, repo, "main", sweepTTL, sweepNow) }()
			<-entered

			queued := make(chan error, 2)
			for i := 0; i < 2; i++ {
				ctx, cancel := context.WithCancel(base)
				defer cancel()
				go func() {
					queued <- s.Sweep(context.WithValue(ctx, cancelKey{}, cancel), repo, "main", sweepTTL, sweepNow)
				}()
			}
			waitForUsers(t, s, repo, "main", 3, never)
			close(release)
			require.NoError(t, <-first)

			one, other := <-queued, <-queued
			if one != nil {
				one, other = other, one
			}
			require.NoError(t, one, "the caller that did not run the bad sweep ran its own")
			require.Error(t, other, "the caller that ran the bad sweep gets its error")
			require.EqualValues(t, 3, calls.Load())
			require.Empty(t, list(BranchDir("main")), "the surplus is gone")
			requireReleased(t, s)
		})
	}
}

// TestSweeper_panicReleasesTheGuard: a sweep that panics does not leave its key held: a caller
// waiting on it runs its own sweep, and so does a caller that comes later.
func TestSweeper_panicReleasesTheGuard(t *testing.T) {
	t.Parallel()

	repo, _, _, _ := setup(t)
	ctx := testhelper.Context(t)
	s := NewSweeper()
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	s.run = func(context.Context, *localrepo.Repo, string, time.Duration, time.Time) (int, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			panic("the sweep panicked")
		}
		return 0, nil
	}
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_ = s.Sweep(ctx, repo, "main", sweepTTL, sweepNow)
	}()
	<-entered
	waiter := make(chan error, 1)
	go func() { waiter <- s.Sweep(ctx, repo, "main", sweepTTL, sweepNow) }()
	waitForUsers(t, s, repo, "main", 2, never)
	close(release)

	require.Equal(t, "the sweep panicked", <-panicked)
	require.NoError(t, <-waiter)
	require.EqualValues(t, 2, calls.Load())
	requireReleased(t, s)

	require.NoError(t, s.Sweep(ctx, repo, "main", sweepTTL, sweepNow))
	require.EqualValues(t, 3, calls.Load())
	requireReleased(t, s)
}

// TestSweep_racingARewriteLosesNothing: a land of the same commit replaces the target of a reference
// that a sweep decided to delete. The sweep fails and deletes nothing; the rewritten proposal
// survives; a later sweep, deciding from what it lists then, removes the rest of the surplus and
// keeps it. The hook runs in the real Prune between its decision and its deletion.
func TestSweep_racingARewriteLosesNothing(t *testing.T) {
	t.Parallel()

	for desc, tc := range map[string]struct {
		// raced is the date of the target that the sweep decides on.
		raced time.Time
		// surplus are the dates of the references of the branch that remain surplus after the rewrite.
		fresh, surplus []time.Time
	}{
		"expired": {
			raced:   sweepNow.Add(-8 * 24 * time.Hour),
			surplus: []time.Time{sweepNow.Add(-9 * 24 * time.Hour)},
		},
		"beyond the newest": {
			raced: sweepNow.Add(-3 * time.Hour),
			fresh: func() (dates []time.Time) {
				for i := 0; i < MaxPerBranch-1; i++ {
					dates = append(dates, sweepNow.Add(-time.Duration(i+1)*time.Minute))
				}
				return dates
			}(),
			surplus: []time.Time{sweepNow.Add(-2 * time.Hour)},
		},
	} {
		t.Run(desc, func(t *testing.T) {
			t.Parallel()

			repo, _, commit, list := setup(t)
			ctx := testhelper.Context(t)
			zero := gittest.DefaultObjectHash.ZeroOID

			raced := refName("main", 0)
			target := commit(tc.raced)
			ops := []Op{{Name: raced, NewOID: target, OldOID: zero}}
			var surplus []git.ReferenceName
			for i, when := range tc.fresh {
				ops = append(ops, Op{Name: refName("main", 100+i), NewOID: commit(when), OldOID: zero})
			}
			for i, when := range tc.surplus {
				name := refName("main", 200+i)
				surplus = append(surplus, name)
				ops = append(ops, Op{Name: name, NewOID: commit(when), OldOID: zero})
			}
			require.NoError(t, Update(ctx, repo, ops))

			rewritten := commit(sweepNow)
			var once sync.Once
			hooked := 0
			s := NewSweeperWithHook(func() {
				once.Do(func() {
					hooked++
					require.NoError(t, Update(ctx, repo, []Op{{Name: raced, NewOID: rewritten, OldOID: target}}))
				})
			})

			err := s.Sweep(ctx, repo, "main", sweepTTL, sweepNow)
			require.Equal(t, 1, hooked, "the sweep decided to delete something")
			require.Error(t, err, "the deletion expected the target the sweep listed")
			requireTarget(t, ctx, repo, raced, rewritten)
			refs := list(BranchDir("main"))
			for _, name := range surplus {
				require.Contains(t, refs, name.String(), "the failed transaction deleted nothing")
			}
			requireReleased(t, s)

			require.NoError(t, s.Sweep(ctx, repo, "main", sweepTTL, sweepNow))
			requireTarget(t, ctx, repo, raced, rewritten)
			refs = list(BranchDir("main"))
			for _, name := range surplus {
				require.NotContains(t, refs, name.String(), "the later sweep cleaned up")
			}
			require.Len(t, refs, 1+len(tc.fresh))
		})
	}
}

func requireTarget(t *testing.T, ctx context.Context, repo *localrepo.Repo, name git.ReferenceName, want git.ObjectID) {
	t.Helper()
	ref, err := repo.GetReference(ctx, name)
	require.NoError(t, err)
	require.Equal(t, want.String(), ref.Target)
}

// TestDrop: Drop against what can happen to the reference, always with real git.
func TestDrop(t *testing.T) {
	t.Parallel()

	zero := gittest.DefaultObjectHash.ZeroOID

	t.Run("deletes the reference", func(t *testing.T) {
		t.Parallel()
		repo, _, commit, list := setup(t)
		ctx := testhelper.Context(t)
		require.NoError(t, Update(ctx, repo, []Op{{Name: refName("main", 0), NewOID: commit(sweepNow), OldOID: zero}}))
		require.NoError(t, Drop(ctx, repo, refName("main", 0)))
		require.Empty(t, list(RefPrefix))
	})

	t.Run("a reference already gone is not an error", func(t *testing.T) {
		t.Parallel()
		repo, _, _, _ := setup(t)
		require.NoError(t, Drop(testhelper.Context(t), repo, refName("main", 0)))
	})

	t.Run("refuses a name that is no proposal", func(t *testing.T) {
		t.Parallel()
		repo, _, _, _ := setup(t)
		require.ErrorContains(t, Drop(testhelper.Context(t), repo, "refs/heads/main"), "not a proposal reference")
	})

	t.Run("a sweep that deletes the reference after the read is not an error", func(t *testing.T) {
		t.Parallel()
		repo, _, commit, list := setup(t)
		ctx := testhelper.Context(t)
		name := refName("main", 0)
		require.NoError(t, Update(ctx, repo, []Op{{Name: name, NewOID: commit(sweepNow.Add(-8 * 24 * time.Hour)), OldOID: zero}}))
		s := NewSweeper()
		swept := false
		require.NoError(t, drop(ctx, repo, name, func() {
			swept = true
			require.NoError(t, s.Sweep(ctx, repo, "main", sweepTTL, sweepNow))
			require.Empty(t, list(RefPrefix), "the sweep deleted it first")
		}))
		require.True(t, swept)
		require.Empty(t, list(RefPrefix))
	})

	t.Run("a sweep that deleted the reference before the read is not an error", func(t *testing.T) {
		t.Parallel()
		repo, _, commit, list := setup(t)
		ctx := testhelper.Context(t)
		name := refName("main", 0)
		require.NoError(t, Update(ctx, repo, []Op{{Name: name, NewOID: commit(sweepNow.Add(-8 * 24 * time.Hour)), OldOID: zero}}))
		require.NoError(t, NewSweeper().Sweep(ctx, repo, "main", sweepTTL, sweepNow))
		require.Empty(t, list(RefPrefix))
		require.NoError(t, Drop(ctx, repo, name))
	})

	t.Run("a drop that deletes a reference a sweep decided on fails that sweep, and the next one cleans up", func(t *testing.T) {
		t.Parallel()
		repo, _, commit, list := setup(t)
		ctx := testhelper.Context(t)
		name, other := refName("main", 0), refName("main", 1)
		old := sweepNow.Add(-8 * 24 * time.Hour)
		require.NoError(t, Update(ctx, repo, []Op{
			{Name: name, NewOID: commit(old), OldOID: zero},
			{Name: other, NewOID: commit(old), OldOID: zero},
		}))
		var once sync.Once
		s := NewSweeperWithHook(func() {
			once.Do(func() { require.NoError(t, Drop(ctx, repo, name)) })
		})
		require.Error(t, s.Sweep(ctx, repo, "main", sweepTTL, sweepNow))
		require.Equal(t, []string{other.String()}, list(RefPrefix))
		require.NoError(t, s.Sweep(ctx, repo, "main", sweepTTL, sweepNow))
		require.Empty(t, list(RefPrefix))
	})

	t.Run("a proposal rewritten after the read survives, and the drop fails", func(t *testing.T) {
		t.Parallel()
		repo, _, commit, _ := setup(t)
		ctx := testhelper.Context(t)
		name := refName("main", 0)
		target, rewritten := commit(sweepNow.Add(-time.Hour)), commit(sweepNow)
		require.NoError(t, Update(ctx, repo, []Op{{Name: name, NewOID: target, OldOID: zero}}))
		err := drop(ctx, repo, name, func() {
			require.NoError(t, Update(ctx, repo, []Op{{Name: name, NewOID: rewritten, OldOID: target}}))
		})
		require.Error(t, err)
		requireTarget(t, ctx, repo, name, rewritten)
	})
}
