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
	"google.golang.org/grpc/metadata"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/transaction/txinfo"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const sweepTTL = 7 * 24 * time.Hour

// Every date of these tests is relative to this fixed instant, the "now" of the sweeps: nothing here
// depends on the wall clock.
var sweepNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func refName(branch string, i int) git.ReferenceName {
	return git.ReferenceName(fmt.Sprintf("%s%040x", BranchDir(branch), i+1))
}

// testGiveUp bounds what a test waits for when a regression would make it wait forever. It decides
// no result of a correct sweeper, which needs nothing but the scheduler to get there.
const testGiveUp = 30 * time.Second

// waitForUsers yields until n callers are inside the guard for the branch of repo, or stop says
// that waiting is pointless. A sweeper that never gets there, as with a key that is wrong, fails the
// test after testGiveUp.
func waitForUsers(t *testing.T, s *Sweeper, repo *localrepo.Repo, branch string, n int, stop func() bool) {
	t.Helper()
	key := sweepKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), dir: BranchDir(branch)}
	giveUp := time.Now().Add(testGiveUp)
	for !stop() {
		if time.Now().After(giveUp) {
			require.FailNow(t, "the callers did not get into the guard", "wanted %d", n)
		}
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

// testSweeper is a sweeper without the bound of SweepTimeout, which is real time and which a loaded
// machine can run out of: in these tests a deadline passes only when a test says so.
func testSweeper(opts ...SweeperOption) *Sweeper {
	return NewSweeper(append([]SweeperOption{
		WithDeadlineContext(func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithCancel(ctx)
		}),
	}, opts...)...)
}

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
	s := testSweeper()
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
	s := testSweeper()

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

// TestSweeper_keysAreIndependent: a running sweep holds back no other branch, relative path or
// storage, each differing from it in that part of the key alone. A key that lost a part makes the
// second sweep wait, and it then ends with the context that bounds it.
func TestSweeper_keysAreIndependent(t *testing.T) {
	t.Parallel()

	repo, _, _, _ := setup(t)
	for desc, tc := range map[string]struct {
		repo   *localrepo.Repo
		branch string
	}{
		"another branch": {repo: repo, branch: "other"},
		"another relative path": {repo: localrepo.NewFrom(repo, &gitalypb.Repository{
			StorageName: repo.GetStorageName(), RelativePath: repo.GetRelativePath() + "-other",
		}), branch: "main"},
		"another storage": {repo: localrepo.NewFrom(repo, &gitalypb.Repository{
			StorageName: repo.GetStorageName() + "-other", RelativePath: repo.GetRelativePath(),
		}), branch: "main"},
	} {
		t.Run(desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			s := testSweeper()
			var calls atomic.Int64
			entered := make(chan struct{})
			release := make(chan struct{})
			s.run = func(context.Context, *localrepo.Repo, string, time.Duration, time.Time) (int, error) {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
				}
				return 0, nil
			}
			done := make(chan error, 1)
			go func() { done <- s.Sweep(ctx, repo, "main", sweepTTL, sweepNow) }()
			<-entered

			bounded, cancel := context.WithTimeout(ctx, testGiveUp)
			defer cancel()
			err := s.Sweep(bounded, tc.repo, tc.branch, sweepTTL, sweepNow)
			close(release)
			require.NoError(t, err, "the other key's sweep did not wait")
			require.NoError(t, <-done)
			require.EqualValues(t, 2, calls.Load())
			requireReleased(t, s)
		})
	}
}

// manualDeadlines replaces the bound of SweepTimeout: a test decides when each caller's passes, in
// the order the callers came.
type manualDeadlines struct {
	mu    sync.Mutex
	fires []func()
}

func (m *manualDeadlines) context(ctx context.Context) (context.Context, context.CancelFunc) {
	bounded, cancel := context.WithCancelCause(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fires = append(m.fires, func() { cancel(ErrSweepTimedOut) })
	return bounded, func() { cancel(nil) }
}

func (m *manualDeadlines) pass(t *testing.T, caller int) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Greater(t, len(m.fires), caller, "the caller has no deadline")
	m.fires[caller]()
}

// TestSweeper_timeoutBoundsEachCaller: a stuck sweep holds nobody past their own bound, whoever else
// comes. The waiter leaves at its deadline with the sweep still stuck; the sweep ends at its own
// deadline and frees the key, and the surplus goes with the next sweep.
func TestSweeper_timeoutBoundsEachCaller(t *testing.T) {
	t.Parallel()

	repo, _, commit, list := setup(t)
	ctx := testhelper.Context(t)
	require.NoError(t, Update(ctx, repo, []Op{{Name: refName("main", 0), NewOID: commit(sweepNow.Add(-8 * 24 * time.Hour)), OldOID: gittest.DefaultObjectHash.ZeroOID}}))

	deadlines := &manualDeadlines{}
	s := testSweeper(WithDeadlineContext(deadlines.context))
	var calls atomic.Int64
	entered := make(chan struct{})
	s.run = func(ctx context.Context, r *localrepo.Repo, dir string, ttl time.Duration, now time.Time) (int, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			return 0, ctx.Err()
		}
		return Prune(ctx, r, dir, ttl, now)
	}

	runner := make(chan error, 1)
	go func() { runner <- s.Sweep(ctx, repo, "main", sweepTTL, sweepNow) }()
	<-entered
	waiter := make(chan error, 1)
	go func() { waiter <- s.Sweep(ctx, repo, "main", sweepTTL, sweepNow) }()
	waitForUsers(t, s, repo, "main", 2, func() bool { return false })

	deadlines.pass(t, 1)
	require.ErrorIs(t, <-waiter, ErrSweepTimedOut, "the waiter left at its own deadline")
	require.EqualValues(t, 1, calls.Load(), "the sweep is still stuck")
	require.Len(t, list(BranchDir("main")), 1)

	deadlines.pass(t, 0)
	require.ErrorIs(t, <-runner, ErrSweepTimedOut)
	requireReleased(t, s)
	require.Len(t, list(BranchDir("main")), 1, "a sweep out of time left the surplus")

	require.NoError(t, s.Sweep(ctx, repo, "main", sweepTTL, sweepNow))
	require.Empty(t, list(BranchDir("main")), "the next sweep removed it")
	requireReleased(t, s)
}

// TestSweeper_aCallerThatGivesUpIsNotATimeout: only the server's own bound counts as one.
func TestSweeper_aCallerThatGivesUpIsNotATimeout(t *testing.T) {
	t.Parallel()

	repo, _, _, _ := setup(t)
	ctx, cancel := context.WithCancel(testhelper.Context(t))
	cancel()
	s := testSweeper()
	s.run = func(ctx context.Context, _ *localrepo.Repo, _ string, _ time.Duration, _ time.Time) (int, error) {
		return 0, ctx.Err()
	}
	err := s.Sweep(ctx, repo, "main", sweepTTL, sweepNow)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrSweepTimedOut)
}

// TestSweeper_theDefaultBoundIsSweepTimeout: without the clock deciding anything, a sweeper's default
// bound is at most SweepTimeout away, and what passes it has ErrSweepTimedOut as its cause.
func TestSweeper_theDefaultBoundIsSweepTimeout(t *testing.T) {
	t.Parallel()

	before := time.Now()
	bounded, cancel := NewSweeper().deadline(testhelper.Context(t))
	defer cancel()
	deadline, ok := bounded.Deadline()
	require.True(t, ok)
	require.False(t, deadline.Before(before.Add(SweepTimeout)), "not shorter than SweepTimeout")
	require.False(t, deadline.After(time.Now().Add(SweepTimeout)), "not longer than SweepTimeout")

	passed, cancelPassed := sweepDeadline(testhelper.Context(t), 0)
	defer cancelPassed()
	<-passed.Done()
	require.ErrorIs(t, context.Cause(passed), ErrSweepTimedOut)
}

// fakeCache is an MVCC cache that only marks a context as an RPC on a pinned snapshot.
type fakeCache struct{ mvcc.Cache }

// TestSweeper_callersThatCannotShareSkipTheGuard: on MVCC and in a Praefect transaction every caller
// runs its own sweep, at once, whoever else runs one, with no bound of its own and leaving no trace
// in the guard.
func TestSweeper_callersThatCannotShareSkipTheGuard(t *testing.T) {
	t.Parallel()

	inTransaction := func(t *testing.T, ctx context.Context) context.Context {
		ctx, err := txinfo.InjectTransaction(ctx, 1, "node", true)
		require.NoError(t, err)
		return ctx
	}
	malformedTransaction := func(t *testing.T, ctx context.Context) context.Context {
		return metadata.NewIncomingContext(ctx, metadata.Pairs(txinfo.TransactionMetadataKey, "not a transaction"))
	}
	for desc, withMode := range map[string]func(t *testing.T, ctx context.Context) context.Context{
		"MVCC": func(_ *testing.T, ctx context.Context) context.Context {
			return mvcc.ContextWithCache(ctx, fakeCache{})
		},
		"a Praefect transaction":            inTransaction,
		"a transaction that cannot be read": malformedTransaction,
	} {
		t.Run(desc, func(t *testing.T) {
			t.Parallel()

			repo, _, _, _ := setup(t)
			ctx, cancel := context.WithTimeout(withMode(t, testhelper.Context(t)), testGiveUp)
			defer cancel()
			s := testSweeper(WithDeadlineContext(func(context.Context) (context.Context, context.CancelFunc) {
				panic("a caller that cannot share a sweep waits for nobody and has no bound")
			}))
			var calls atomic.Int64
			entered := make(chan struct{})
			release := make(chan struct{})
			s.run = func(context.Context, *localrepo.Repo, string, time.Duration, time.Time) (int, error) {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
				}
				return 0, nil
			}
			first := make(chan error, 1)
			go func() { first <- s.Sweep(ctx, repo, "main", sweepTTL, sweepNow) }()
			<-entered

			require.NoError(t, s.Sweep(ctx, repo, "main", sweepTTL, sweepNow), "the second sweep ran with the first one stuck")
			require.EqualValues(t, 2, calls.Load())
			requireReleased(t, s)
			close(release)
			require.NoError(t, <-first)
		})
	}
}

// TestSweeper_mvccRepository: on a real MVCC repository, two RPCs pinned their snapshots. The first
// is in the middle of its sweep when the second sweeps and publishes; the first then fails, so its
// deletions are thrown away. Had the second waited for the first and taken its result, the surplus
// would have stayed: it is gone because the second ran its own sweep on its own snapshot.
func TestSweeper_mvccRepository(t *testing.T) {
	testhelper.SkipWithReftable(t, "MVCC backend selection is overridden by GIT_DEFAULT_REF_FORMAT")
	gittest.SkipWithoutMVCCGit(t)
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC), cfg, gittest.CreateRepositoryConfig{
		ReferenceBackend:       git.ReferenceBackendMVCC,
		SkipCreationViaService: true,
	})
	expired := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithCommitterDate(sweepNow.Add(-8*24*time.Hour)))
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	storagePath := cfg.Storages[0].Path
	pinned := func() (context.Context, *mvcc.LocalCache) {
		cache := mvcc.NewLocalCache(mvcc.NewLocalStorage(storagePath), cfg.RuntimeDir, storagePath, repoProto.GetRelativePath(), false)
		cache.Prepare(ctx)
		require.NoError(t, cache.WaitUntilReady(ctx))
		rpcCtx := gitcmd.ContextWithReferenceBackend(mvcc.ContextWithCache(ctx, cache), git.ReferenceBackendMVCC)
		return rpcCtx, cache
	}
	listProposals := func() []string {
		rpcCtx, cache := pinned()
		defer cache.Cleanup()
		refs, err := repo.GetReferences(rpcCtx, RefPrefix)
		require.NoError(t, err)
		names := make([]string, 0, len(refs))
		for _, ref := range refs {
			names = append(names, ref.Name.String())
		}
		return names
	}

	surplus := refName("main", 0)
	seedCtx, seed := pinned()
	require.NoError(t, Update(seedCtx, repo, []Op{{Name: surplus, NewOID: expired, OldOID: gittest.DefaultObjectHash.ZeroOID}}))
	require.NoError(t, seed.Commit(seedCtx))
	seed.Cleanup()
	require.Equal(t, []string{surplus.String()}, listProposals())

	firstCtx, first := pinned()
	defer first.Cleanup()
	secondCtx, second := pinned()
	defer second.Cleanup()

	var hooked atomic.Int64
	var s *Sweeper
	s = testSweeper(WithBeforeDelete(func() {
		if hooked.Add(1) != 1 {
			return
		}
		bounded, cancel := context.WithTimeout(secondCtx, testGiveUp)
		defer cancel()
		require.NoError(t, s.Sweep(bounded, repo, "main", sweepTTL, sweepNow), "the second RPC ran its own sweep with the first one in the middle of its own")
		require.NoError(t, second.Commit(secondCtx))
	}))
	require.NoError(t, s.Sweep(firstCtx, repo, "main", sweepTTL, sweepNow))
	require.EqualValues(t, 2, hooked.Load(), "both swept")
	requireReleased(t, s)

	// The first RPC fails: it never commits, and what it deleted in its snapshot goes with it.
	require.Empty(t, listProposals(), "the published deletion is the second RPC's own")
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
	s := testSweeper()
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

	s := testSweeper()
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
	s := testSweeper()
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
	s := testSweeper()
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

			s := testSweeper()
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
	s := testSweeper()
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
			s := testSweeper(WithBeforeDelete(func() {
				once.Do(func() {
					hooked++
					require.NoError(t, Update(ctx, repo, []Op{{Name: raced, NewOID: rewritten, OldOID: target}}))
				})
			}))

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
		require.NoError(t, testSweeper().Drop(ctx, repo, refName("main", 0)))
		require.Empty(t, list(RefPrefix))
	})

	t.Run("a reference already gone is not an error", func(t *testing.T) {
		t.Parallel()
		repo, _, _, _ := setup(t)
		require.NoError(t, testSweeper().Drop(testhelper.Context(t), repo, refName("main", 0)))
	})

	t.Run("refuses a name that is no proposal", func(t *testing.T) {
		t.Parallel()
		repo, _, _, _ := setup(t)
		require.ErrorContains(t, testSweeper().Drop(testhelper.Context(t), repo, "refs/heads/main"), "not a proposal reference")
	})

	t.Run("a sweep that deletes the reference after the read is not an error", func(t *testing.T) {
		t.Parallel()
		repo, _, commit, list := setup(t)
		ctx := testhelper.Context(t)
		name := refName("main", 0)
		require.NoError(t, Update(ctx, repo, []Op{{Name: name, NewOID: commit(sweepNow.Add(-8 * 24 * time.Hour)), OldOID: zero}}))
		s := testSweeper()
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
		require.NoError(t, testSweeper().Sweep(ctx, repo, "main", sweepTTL, sweepNow))
		require.Empty(t, list(RefPrefix))
		require.NoError(t, testSweeper().Drop(ctx, repo, name))
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
		s := testSweeper(WithBeforeDelete(func() {
			once.Do(func() { require.NoError(t, testSweeper().Drop(ctx, repo, name)) })
		}))
		require.Error(t, s.Sweep(ctx, repo, "main", sweepTTL, sweepNow))
		require.Equal(t, []string{other.String()}, list(RefPrefix))
		require.NoError(t, s.Sweep(ctx, repo, "main", sweepTTL, sweepNow))
		require.Empty(t, list(RefPrefix))
	})

	t.Run("a symbolic reference is left", func(t *testing.T) {
		t.Parallel()
		repo, repoPath, commit, list := setup(t)
		ctx := testhelper.Context(t)
		cfg := testcfg.Build(t)
		name := refName("main", 0)
		// To a branch that exists: a symbolic reference that dangles is not even listed.
		gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/main", commit(sweepNow).String())
		gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", name.String(), "refs/heads/main")
		require.NoError(t, testSweeper().Drop(ctx, repo, name))
		require.Equal(t, []string{name.String()}, list(RefPrefix), "not written by Update, so not Drop's to delete")
		require.Equal(t, "refs/heads/main", text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", name.String())))
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
