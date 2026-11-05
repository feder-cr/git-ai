package voter_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	releasedUnreportedMetric = "graft_storage_ref_admission_released_unreported_total"
	waitingMetric            = "graft_storage_ref_admission_waiting"
	bypassedMetric           = "graft_storage_ref_admission_bypassed_total"
	expiredMetric            = "graft_storage_ref_admission_expired_total"
)

// process is a git process of a reference transaction: its invocation, live until end is called,
// and the hooks payload it runs the hook with.
type process struct {
	payload gitcmd.HooksPayload
	end     func()
}

// startProcess declares a new git process of the fixture's repository, as the owner of the process
// does.
func (f *fixture) startProcess(t *testing.T) process {
	t.Helper()

	invocation, err := gitcmd.NewChangelogInvocation()
	require.NoError(t, err)

	end := gitcmd.BeginInvocation(invocation)
	t.Cleanup(end)

	payload := f.payload
	payload.Changelog.Invocation = invocation
	return process{payload: payload, end: end}
}

// releasedUnreported returns the admissions released by the end of a process.
func (f *fixture) releasedUnreported(t *testing.T) float64 {
	t.Helper()
	return metricValue(t, f.core.RefAdmission, releasedUnreportedMetric, nil)
}

// requireWaiting waits for the given number of writers to be queued. The test waits for a state:
// the bound only stops a test that went wrong.
func requireWaiting(t *testing.T, admission *localrefs.Admission, writers float64) {
	t.Helper()
	require.Eventually(t, func() bool {
		return metricValue(t, admission, waitingMetric, nil) == writers
	}, time.Minute, time.Millisecond, "%v writers are queued", writers)
}

// TestAdmissionEndsWithTheProcess verifies that the end of a transaction's git process releases its
// admission, whatever git reported, and that only the case without a report counts as one.
func TestAdmissionEndsWithTheProcess(t *testing.T) {
	t.Parallel()

	t.Run("a process that ends without a report releases it, once", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
		input := line(f.a.String(), f.zero, "refs/heads/main")

		p := f.startProcess(t)
		f.voter.Preparing(f.ctx, p.payload, input)
		require.True(t, f.admissionHeld(t))
		require.Zero(t, f.releasedUnreported(t))

		// A prepare that fails in git: update-ref dies without an Aborted phase.
		p.end()
		require.False(t, f.admissionHeld(t))
		require.Equal(t, 1.0, f.releasedUnreported(t))

		var logged int
		for _, entry := range f.logs.AllEntries() {
			if entry.Data["invocation"] == p.payload.Changelog.Invocation {
				require.Equal(t, f.repoPath, entry.Data["repository"])
				require.Contains(t, entry.Message, "without reporting Committed or Aborted")
				logged++
			}
		}
		require.Equal(t, 1, logged)

		p.end()
		require.Equal(t, 1.0, f.releasedUnreported(t))
	})

	t.Run("after Committed the exit releases nothing", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
		input := line(f.zero, f.a.String(), "refs/heads/main")

		p := f.startProcess(t)
		f.voter.Preparing(f.ctx, p.payload, input)
		require.NoError(t, f.prepared(input))
		gittest.WriteRef(t, f.cfg, f.repoPath, "refs/heads/main", f.a)
		f.voter.Committed(f.ctx, p.payload, input)
		require.False(t, f.admissionHeld(t))

		p.end()
		require.Zero(t, f.releasedUnreported(t))
	})

	t.Run("after Aborted the exit releases nothing", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
		input := line(f.a.String(), f.zero, "refs/heads/main")

		p := f.startProcess(t)
		f.voter.Preparing(f.ctx, p.payload, input)
		f.voter.Aborted(f.ctx, p.payload, input)
		require.False(t, f.admissionHeld(t))

		p.end()
		require.Zero(t, f.releasedUnreported(t))
	})

	t.Run("Committed naming another reference than Preparing releases it", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})

		// git reports a symbolic reference by its target in the last phase, so the phases of one
		// transaction do not always have one hook input.
		p := f.startProcess(t)
		f.voter.Preparing(f.ctx, p.payload, line(f.zero, f.a.String(), "refs/heads/main"))
		f.voter.Committed(f.ctx, p.payload, line(f.zero, f.a.String(), "HEAD"))
		require.False(t, f.admissionHeld(t))
	})

	t.Run("the exit releases the transaction in progress of a long-lived process", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
		first := line(f.zero, f.a.String(), "refs/heads/first")
		second := line(f.zero, f.a.String(), "refs/heads/second")

		p := f.startProcess(t)
		f.voter.Preparing(f.ctx, p.payload, first)
		f.voter.Committed(f.ctx, p.payload, first)
		require.False(t, f.admissionHeld(t))

		f.voter.Preparing(f.ctx, p.payload, second)
		require.True(t, f.admissionHeld(t))

		p.end()
		require.False(t, f.admissionHeld(t))
		require.Equal(t, 1.0, f.releasedUnreported(t))
	})
}

// TestTransactionStartsEndsTheEarlierOneOfItsProcess verifies what the start of a transaction ends of
// an earlier transaction of its process, of which git reported nothing more: its admission, before
// the work of the new transaction, and its pack, which does not hold the objects of the new one.
func TestTransactionStartsEndsTheEarlierOneOfItsProcess(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	blobs := f.countPuts()
	const earlierRef, nextRef = "refs/heads/earlier", "refs/heads/next"

	p := f.startProcess(t)
	f.voter.TransactionStarts(f.ctx, p.payload)
	require.Zero(t, f.releasedUnreported(t), "nothing is open yet")

	f.voter.Preparing(f.ctx, p.payload, line(f.zero, f.a.String(), earlierRef))
	require.True(t, f.admissionHeld(t))
	require.Equal(t, 1, blobs.count())
	require.Equal(t, 1, f.voter.PreparedPacks())

	f.voter.TransactionStarts(f.ctx, p.payload)
	require.False(t, f.admissionHeld(t), "the writers behind it do not wait for the new transaction's work")
	require.Equal(t, 1.0, f.releasedUnreported(t))
	require.Zero(t, f.voter.PreparedPacks())

	// The Preparing phase of the new transaction may be skipped: Prepared builds the pack.
	next := line(f.zero, f.c.String(), nextRef)
	require.NoError(t, f.voter.Prepared(f.ctx, p.payload, next, changelog.Meta{Operation: "test", Actor: "tester"}))
	require.Equal(t, 2, blobs.count())
	require.Equal(t, blobs.puts[1], f.lastChangeset(t).PackKey)
}

// TestPackOfATransactionWhoseInputChanges verifies that the pack of the Preparing phase serves the
// Prepared phase of the same process also when the hook input of that phase differs from the input
// of Preparing.
func TestPackOfATransactionWhoseInputChanges(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative})
	blobs := f.countPuts()

	p := f.startProcess(t)
	both := append(line(f.zero, f.c.String(), "refs/heads/kept"), line(f.zero, f.b.String(), "refs/heads/rejected")...)
	f.voter.Preparing(f.ctx, p.payload, both)
	require.Equal(t, 1, blobs.count())

	require.NoError(t, f.voter.Prepared(f.ctx, p.payload, line(f.zero, f.c.String(), "refs/heads/kept"), changelog.Meta{Operation: "test", Actor: "tester"}))
	require.Equal(t, 1, blobs.count(), "Prepared uses the pack of Preparing")
	require.Equal(t, blobs.puts[0], f.lastChangeset(t).PackKey)
	require.Zero(t, f.voter.PreparedPacks())
}

// failedPruneHooks makes the prune of a git fetch --prune fail in git's prepare, after the preparing
// phase of its transaction: the reftable lock of the repository exists from the end of that phase to
// the start of the next preparing phase. It records the phases of the fetch.
type failedPruneHooks struct {
	hook.Manager
	lock  string
	armed atomic.Bool

	mu     sync.Mutex
	phases []hook.ReferenceTransactionState
}

func (m *failedPruneHooks) ReferenceTransactionHook(ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
	if !m.armed.Load() {
		return m.Manager.ReferenceTransactionHook(ctx, state, env, stdin)
	}

	m.mu.Lock()
	m.phases = append(m.phases, state)
	preparings := 0
	for _, phase := range m.phases {
		if phase == hook.ReferenceTransactionPreparing {
			preparings++
		}
	}
	m.mu.Unlock()

	if state == hook.ReferenceTransactionPreparing && preparings == 2 {
		if err := os.Remove(m.lock); err != nil {
			return err
		}
	}
	err := m.Manager.ReferenceTransactionHook(ctx, state, env, stdin)
	if state == hook.ReferenceTransactionPreparing && preparings == 1 {
		if lockErr := os.WriteFile(m.lock, nil, 0o600); lockErr != nil {
			return lockErr
		}
	}
	return err
}

// TestAdmissionOfAFetchWhosePruneFailed runs a real git fetch --prune whose prune fails in git's
// prepare, after its preparing phase and without an aborted phase, and which then goes on to the
// transaction of the fetch in the same process. That transaction must not queue behind the first one:
// the bounds of the admission are an hour, so it would hang until the context of the test ends.
func TestAdmissionOfAFetchWhosePruneFailed(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		hooks := &failedPruneHooks{}
		s := newAdmissionServer(t, dialect, testserver.WithHookManagerWrapper(func(manager hook.Manager) hook.Manager {
			hooks.Manager = manager
			return hooks
		}))
		hooks.lock = filepath.Join(s.repoPath, "reftable", "tables.list.lock")

		gone := gittest.WriteCommit(t, s.cfg, s.repoPath, gittest.WithMessage("gone"))
		s.createBranch(t, "gone", gone)

		_, sourcePath := gittest.CreateRepository(t, s.ctx, s.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		fetched := gittest.WriteCommit(t, s.cfg, sourcePath, gittest.WithBranch("fetched"), gittest.WithMessage("fetched"))

		objectHash, err := s.repo.ObjectHash(s.ctx)
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
		defer cancel()

		hooks.armed.Store(true)
		var stderr bytes.Buffer
		// The exit status tells of the failed prune; the phases below are the point.
		_ = s.repo.ExecAndWait(ctx, gitcmd.Command{
			Name:  "fetch",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--prune"}},
			Args:  []string{sourcePath, "+refs/heads/*:refs/heads/*"},
		},
			gitcmd.WithRefTxHook(objectHash, s.repo),
			gitcmd.WithConfig(gitcmd.ConfigPair{Key: "reftable.lockTimeout", Value: "0"}),
			gitcmd.WithStderr(&stderr),
		)
		hooks.armed.Store(false)

		require.Equal(t, []hook.ReferenceTransactionState{
			hook.ReferenceTransactionPreparing,
			hook.ReferenceTransactionPreparing,
			hook.ReferenceTransactionPrepared,
			hook.ReferenceTransactionCommitted,
		}, hooks.phases, "stderr: %s", stderr.String())

		refs, _, err := s.core.Meta.Refs(s.ctx, s.repoID)
		require.NoError(t, err)
		require.Equal(t, corestore.Target(fetched.String()), refs["refs/heads/fetched"])
		require.Equal(t, corestore.Target(gone.String()), refs["refs/heads/gone"], "the prune failed")

		s.requireAdmissionFree(t)
		require.Equal(t, 1.0, metricValue(t, s.core.RefAdmission, releasedUnreportedMetric, nil), "git reported no end of the prune")
		require.Equal(t, 0.0, metricValue(t, s.core.RefAdmission, bypassedMetric, nil))
		require.Equal(t, 0.0, metricValue(t, s.core.RefAdmission, expiredMetric, nil))
	})
}

// TestAdmissionOfAHookThatOutlivedItsProcess verifies that a hook call that arrives after git was
// killed holds nothing: nothing would release it.
func TestAdmissionOfAHookThatOutlivedItsProcess(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	input := line(f.a.String(), f.zero, "refs/heads/main")

	p := f.startProcess(t)
	p.end()

	f.voter.Preparing(f.ctx, p.payload, input)
	require.False(t, f.admissionHeld(t))
	require.Zero(t, f.releasedUnreported(t))
}

// TestAdmissionQueuedWriterLeavesWithItsProcess verifies that a writer whose process ended while it
// was queued leaves the queue, instead of taking a turn that nobody would use or end.
func TestAdmissionQueuedWriterLeavesWithItsProcess(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	f.core.RefAdmission = localrefs.NewAdmission(time.Hour, time.Hour)
	input := line(f.a.String(), f.zero, "refs/heads/main")

	holder := f.startProcess(t)
	f.voter.Preparing(f.ctx, holder.payload, line(f.b.String(), f.zero, "refs/heads/other"))

	queued := f.startProcess(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.voter.Preparing(f.ctx, queued.payload, input)
	}()
	requireWaiting(t, f.core.RefAdmission, 1)

	queued.end()
	<-done
	require.Equal(t, 0.0, metricValue(t, f.core.RefAdmission, waitingMetric, nil))
	require.True(t, f.admissionHeld(t), "the holder keeps its admission")

	holder.end()
	require.False(t, f.admissionHeld(t), "the writer that left has no turn")
	require.Equal(t, 1.0, f.releasedUnreported(t), "only the holder held an admission")
}

// TestAdmissionLateReleaseDoesNotFreeTheNextWriter runs two processes with the same hook input: A
// fails and B is admitted. Whatever then comes late from A, the second end of its process or a late
// phase of its hook, B keeps its admission.
func TestAdmissionLateReleaseDoesNotFreeTheNextWriter(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	f.core.RefAdmission = localrefs.NewAdmission(time.Hour, time.Hour)
	input := line(f.a.String(), f.zero, "refs/heads/main")

	a, b := f.startProcess(t), f.startProcess(t)
	f.voter.Preparing(f.ctx, a.payload, input)

	admitted := make(chan struct{})
	go func() {
		defer close(admitted)
		f.voter.Preparing(f.ctx, b.payload, input)
	}()
	requireWaiting(t, f.core.RefAdmission, 1)

	a.end()
	<-admitted
	require.True(t, f.admissionHeld(t), "B holds the admission")

	a.end()
	f.voter.Aborted(f.ctx, a.payload, input)
	f.voter.Committed(f.ctx, a.payload, input)
	require.True(t, f.admissionHeld(t), "nothing of A frees the admission of B")
	require.Equal(t, 1.0, f.releasedUnreported(t))

	f.voter.Committed(f.ctx, b.payload, input)
	require.False(t, f.admissionHeld(t))

	b.end()
	require.Equal(t, 1.0, f.releasedUnreported(t), "B reported its end")
}

// TestAdmissionOfAProcessThatEndsBeforeItsAcquire ends the process between the moment the voter set
// the release and its Acquire. The release finds nothing and Acquire admits at once, also with its
// context cancelled, so the voter itself must give the admission back.
func TestAdmissionOfAProcessThatEndsBeforeItsAcquire(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	input := line(f.a.String(), f.zero, "refs/heads/main")

	p := f.startProcess(t)
	voter.SetBeforeAcquire(f.voter, p.end)
	f.voter.Preparing(f.ctx, p.payload, input)

	require.False(t, f.admissionHeld(t))
	require.Equal(t, 1.0, f.releasedUnreported(t))
}

// TestAdmissionExitRacingTheHandOver races the end of a queued writer's process with its hand-over,
// the interleaving in which the release of the exit comes before the writer holds the admission.
// The admission must never stay held by a process that ended.
func TestAdmissionExitRacingTheHandOver(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	f.core.RefAdmission = localrefs.NewAdmission(time.Hour, time.Hour)
	input := line(f.a.String(), f.zero, "refs/heads/main")

	for round := 0; round < 500; round++ {
		holder := f.startProcess(t)
		f.voter.Preparing(f.ctx, holder.payload, input)

		queued := f.startProcess(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.voter.Preparing(f.ctx, queued.payload, input)
		}()
		requireWaiting(t, f.core.RefAdmission, 1)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			holder.end()
		}()
		go func() {
			defer wg.Done()
			queued.end()
		}()
		wg.Wait()
		<-done

		require.False(t, f.admissionHeld(t), "round %d", round)
	}
}

// TestAdmissionExitRacingTheHandOverOfThreeWriters queues three writers behind a holder and ends the
// holder and the first of them. The second must then hold the admission and the third stay queued,
// whichever way the end of the first writer's process meets its hand-over: its writer can be admitted
// while it gives up, after the end of its process handed the admission on.
func TestAdmissionExitRacingTheHandOverOfThreeWriters(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	f.core.RefAdmission = localrefs.NewAdmission(time.Hour, time.Hour)
	input := line(f.a.String(), f.zero, "refs/heads/main")

	round := func(t *testing.T, ends func(holder, w process)) {
		t.Helper()

		holder := f.startProcess(t)
		f.voter.Preparing(f.ctx, holder.payload, input)

		start := func(p process, queuedBefore float64) chan struct{} {
			done := make(chan struct{})
			go func() {
				defer close(done)
				f.voter.Preparing(f.ctx, p.payload, input)
			}()
			requireWaiting(t, f.core.RefAdmission, queuedBefore+1)
			return done
		}
		w, x, y := f.startProcess(t), f.startProcess(t), f.startProcess(t)
		wDone := start(w, 0)
		xDone := start(x, 1)
		yDone := start(y, 2)

		ends(holder, w)
		<-wDone
		<-xDone

		// The second hand-over of W happens before W returns.
		require.Equal(t, 1.0, metricValue(t, f.core.RefAdmission, waitingMetric, nil), "Y waits behind X")

		x.end()
		<-yDone
		y.end()
		require.False(t, f.admissionHeld(t))
	}

	for i := 0; i < 200; i++ {
		round(t, func(holder, w process) {
			holder.end()
			w.end()
		})
	}
	for i := 0; i < 200; i++ {
		round(t, func(holder, w process) {
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				holder.end()
			}()
			w.end()
			wg.Wait()
		})
	}
}

// admissionServer is a Gitaly server in authoritative mode with real hooks, whose admission has
// bounds of an hour: nothing in a test can depend on them, and a turn that is not released stays held.
type admissionServer struct {
	ctx       context.Context
	cfg       config.Cfg
	core      *corestore.Core
	conn      *grpc.ClientConn
	repoProto *gitalypb.Repository
	repoPath  string
	repoID    int64
	// repo runs the hooks, unlike NewTestRepo, like a repository of the server.
	repo *localrepo.Repo
}

func newAdmissionServer(t *testing.T, dialect sqlstore.Dialect, opts ...testserver.GitalyServerOpt) *admissionServer {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	cfg.Graft.Storage.Mode = string(corestore.ModeAuthoritative)
	testcfg.BuildGitalyHooks(t, cfg)

	core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative, Dialect: dialect})
	core.RefAdmission = localrefs.NewAdmission(time.Hour, time.Hour)
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll, append([]testserver.GitalyServerOpt{
		testserver.WithGraftStorage(core),
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
	}, opts...)...)
	cfg.SocketPath = addr

	conn, err := client.New(ctx, addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	cache := catfile.NewCache(cfg)
	t.Cleanup(cache.Stop)
	repo := localrepo.New(testhelper.SharedLogger(t), config.NewLocator(cfg), gittest.NewCommandFactory(t, cfg), cache, repoProto)

	return &admissionServer{
		ctx:       ctx,
		cfg:       cfg,
		core:      core,
		conn:      conn,
		repoProto: repoProto,
		repoPath:  repoPath,
		repoID:    trackRepository(t, core, repo, repoPath),
		repo:      repo,
	}
}

// createBranch creates a branch through the server, in the database and in the local repository.
func (s *admissionServer) createBranch(t *testing.T, branch string, commit git.ObjectID) {
	t.Helper()

	_, err := gitalypb.NewOperationServiceClient(s.conn).UserCreateBranch(s.ctx, &gitalypb.UserCreateBranchRequest{
		Repository: s.repoProto,
		BranchName: []byte(branch),
		StartPoint: []byte(commit.String()),
		User:       gittest.TestUser,
	})
	require.NoError(t, err)
}

// requireAdmissionFree verifies that a writer is admitted at once: with a cancelled context, Acquire
// admits only a writer that does not have to wait.
func (s *admissionServer) requireAdmissionFree(t *testing.T) {
	t.Helper()

	cancelled, cancel := context.WithCancel(s.ctx)
	cancel()
	probe := localrefs.Key{0xaa, 0xbb}
	require.True(t, s.core.RefAdmission.Acquire(cancelled, s.repoPath, probe, localrefs.WriterSyncer), "a writer is admitted at once")
	s.core.RefAdmission.Release(s.repoPath, probe)
}

// pushTransports push a request body through the server, and return what receive-pack wrote.
var pushTransports = map[string]func(t *testing.T, ctx context.Context, s *admissionServer, body []byte, closeSend bool) (string, error){
	"smart HTTP": func(t *testing.T, ctx context.Context, s *admissionServer, body []byte, closeSend bool) (string, error) {
		stream, err := gitalypb.NewSmartHTTPServiceClient(s.conn).PostReceivePack(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.PostReceivePackRequest{
			Repository: s.repoProto, GlId: gittest.GlID, GlRepository: gittest.GlRepository,
		}))
		require.NoError(t, stream.Send(&gitalypb.PostReceivePackRequest{Data: body}))
		if closeSend {
			require.NoError(t, stream.CloseSend())
		}

		var output []byte
		for {
			response, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				return string(output), err
			}
			output = append(output, response.GetData()...)
		}
	},
	"SSH": func(t *testing.T, ctx context.Context, s *admissionServer, body []byte, closeSend bool) (string, error) {
		stream, err := gitalypb.NewSSHServiceClient(s.conn).SSHReceivePack(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.SSHReceivePackRequest{
			Repository: s.repoProto, GlId: gittest.GlID, GlRepository: gittest.GlRepository,
		}))
		require.NoError(t, stream.Send(&gitalypb.SSHReceivePackRequest{Stdin: body}))
		if closeSend {
			require.NoError(t, stream.CloseSend())
		}

		var output []byte
		for {
			response, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				return string(output), err
			}
			output = append(output, response.GetStdout()...)
		}
	},
}

// TestPushOfAClientThatKeepsItsStreamOpen pushes a stale atomic push and never closes the request
// stream. receive-pack has all it needs and exits, so the RPC ends and the admission is free at once:
// otherwise a client holds the repository up to the hold bound with every push.
func TestPushOfAClientThatKeepsItsStreamOpen(t *testing.T) {
	for name, push := range pushTransports {
		t.Run(name, func(t *testing.T) {
			s := newAdmissionServer(t, sqlstore.SQLite)

			first := gittest.WriteCommit(t, s.cfg, s.repoPath, gittest.WithMessage("first"))
			second := gittest.WriteCommit(t, s.cfg, s.repoPath, gittest.WithMessage("second"), gittest.WithParents(first))
			stale := gittest.WriteCommit(t, s.cfg, s.repoPath, gittest.WithMessage("stale"), gittest.WithParents(first))
			s.createBranch(t, "pushed", first)

			emptyPack := gittest.ExecOpts(t, s.cfg, gittest.ExecConfig{Stdin: strings.NewReader("")},
				"-C", s.repoPath, "pack-objects", "--stdout", "-q")
			pkt := fmt.Sprintf("%s %s refs/heads/pushed\x00 report-status atomic\n", stale, second)
			var body bytes.Buffer
			fmt.Fprintf(&body, "%04x%s0000", len(pkt)+4, pkt)
			body.Write(emptyPack)

			// The bound only stops a test that went wrong.
			ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
			defer cancel()

			output, err := push(t, ctx, s, body.Bytes(), false)
			require.NoError(t, err, "the RPC ends with the process")
			require.Contains(t, output, "ng refs/heads/pushed")

			s.requireAdmissionFree(t)
			require.Equal(t, 1.0, metricValue(t, s.core.RefAdmission, releasedUnreportedMetric, nil))
		})
	}
}

// TestStalePushReleasesTheAdmission pushes over smart HTTP and over SSH through the server, with
// real git receive-pack processes and hooks. An atomic push with a stale old value fails in git's
// prepare, after the preparing phase of the hook and without an aborted phase, and receive-pack
// exits normally: only its exit ends the transaction, and the next writer must be admitted at once.
// A non-atomic push tolerates the rejection in git's prepare, reaches the prepared phase, and the
// database refuses it: git reports that abort, and the admission ends with it.
func TestStalePushReleasesTheAdmission(t *testing.T) {
	for name, push := range pushTransports {
		t.Run(name, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				s := newAdmissionServer(t, dialect)

				first := gittest.WriteCommit(t, s.cfg, s.repoPath, gittest.WithMessage("first"))
				second := gittest.WriteCommit(t, s.cfg, s.repoPath, gittest.WithMessage("second"), gittest.WithParents(first))
				stale := gittest.WriteCommit(t, s.cfg, s.repoPath, gittest.WithMessage("stale"), gittest.WithParents(first))
				s.createBranch(t, "pushed", first)

				emptyPack := gittest.ExecOpts(t, s.cfg, gittest.ExecConfig{Stdin: strings.NewReader("")},
					"-C", s.repoPath, "pack-objects", "--stdout", "-q")
				body := func(capabilities string) []byte {
					line := fmt.Sprintf("%s %s refs/heads/pushed\x00 %s\n", stale, second, capabilities)
					var buf bytes.Buffer
					fmt.Fprintf(&buf, "%04x%s0000", len(line)+4, line)
					buf.Write(emptyPack)
					return buf.Bytes()
				}

				ctx := s.ctx

				// A turn that stays held would make the next push wait for an hour: stop at the failure.
				require.True(t, t.Run("a stale atomic push, then a second writer", func(t *testing.T) {
					output, err := push(t, ctx, s, body("report-status atomic"), true)
					require.NoError(t, err, "receive-pack reports the failure and exits normally")
					require.Contains(t, output, "ng refs/heads/pushed")

					// The state right after the process ended, with nothing having passed that a
					// bound could account for.
					s.requireAdmissionFree(t)
					require.Equal(t, 1.0, metricValue(t, s.core.RefAdmission, releasedUnreportedMetric, nil), "the stale push reported no end")

					require.NoError(t, s.repo.UpdateRef(ctx, "refs/heads/next", second, gittest.DefaultObjectHash.ZeroOID))
					s.requireAdmissionFree(t)
				}))

				t.Run("a stale non-atomic push, then a second writer", func(t *testing.T) {
					_, err := push(t, ctx, s, body("report-status"), true)
					require.Error(t, err, "the database refuses the rejected update in the prepared phase, and git dies")

					s.requireAdmissionFree(t)
					require.Equal(t, 1.0, metricValue(t, s.core.RefAdmission, releasedUnreportedMetric, nil), "git reported this abort")

					require.NoError(t, s.repo.UpdateRef(ctx, "refs/heads/next-too", second, gittest.DefaultObjectHash.ZeroOID))
					s.requireAdmissionFree(t)
				})

				require.Equal(t, 0.0, metricValue(t, s.core.RefAdmission, expiredMetric, nil))
				require.Equal(t, 0.0, metricValue(t, s.core.RefAdmission, bypassedMetric, nil))
			})
		})
	}
}

// TestConcurrentDeletionsOfOneReference runs the defect end to end, with real git processes and
// hooks: two deletions of one reference, with the same old value and so the same hook input, wait in
// the queue. The first deletes the reference, the second is admitted and its prepare fails in git,
// after the preparing phase of the hook, without an aborted phase. The next writer must be admitted
// at once. The bounds of the admission are an hour, so nothing here can depend on them: a turn that
// is not released stays held.
func TestConcurrentDeletionsOfOneReference(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		s := newAdmissionServer(t, dialect)
		ctx, cfg, core, repo, repoPath, repoID := s.ctx, s.cfg, s.core, s.repo, s.repoPath, s.repoID
		requireAdmissionFree := s.requireAdmissionFree

		commit := gittest.WriteCommit(t, cfg, repoPath)
		const reference = git.ReferenceName("refs/heads/deleted")
		s.createBranch(t, "deleted", commit)
		requireAdmissionFree(t)

		// A writer holds the admission until both deletions are queued behind it.
		release := core.RefAdmission.AcquireUnique(ctx, repoPath, localrefs.WriterSyncer)
		require.NotNil(t, release)

		zero := gittest.DefaultObjectHash.ZeroOID
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = repo.UpdateRef(ctx, reference, zero, commit)
			}()
		}
		requireWaiting(t, core.RefAdmission, 2)

		release()
		wg.Wait()

		var failed int
		for _, err := range errs {
			if err != nil {
				failed++
			}
		}
		require.Equal(t, 1, failed, "one of the deletions fails: %v", errs)

		refs, _, err := core.Meta.Refs(ctx, repoID)
		require.NoError(t, err)
		require.NotContains(t, refs, reference.String())

		// The state right after the second process ended, with nothing having passed that a
		// bound could account for.
		requireAdmissionFree(t)
		require.Equal(t, 1.0, metricValue(t, core.RefAdmission, releasedUnreportedMetric, nil), "the failed deletion reported no end")
		require.Equal(t, 0.0, metricValue(t, core.RefAdmission, expiredMetric, nil))
		require.Equal(t, 0.0, metricValue(t, core.RefAdmission, bypassedMetric, nil))

		// The next real writer goes through the admission and reports its end.
		require.NoError(t, repo.UpdateRef(ctx, "refs/heads/next", commit, zero))
		requireAdmissionFree(t)
		require.Equal(t, 1.0, metricValue(t, core.RefAdmission, releasedUnreportedMetric, nil))
		require.Equal(t, 0.0, metricValue(t, core.RefAdmission, bypassedMetric, nil))
	})
}

// TestProcessEndForgetsTheTransaction verifies what the voter keeps of a transaction whose process
// ended: nothing, also when the Prepared phase of its hook finishes after the end; and that the entry
// of another process with the same hook input stays.
func TestProcessEndForgetsTheTransaction(t *testing.T) {
	t.Parallel()

	meta := changelog.Meta{Operation: "test", Actor: "tester"}

	t.Run("the end finds the pack and the entry", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
		input := line(f.zero, f.a.String(), "refs/heads/main")

		p := f.startProcess(t)
		f.voter.Preparing(f.ctx, p.payload, input)
		require.Equal(t, 1, f.voter.PreparedPacks())
		p.end()
		require.Zero(t, f.voter.PreparedPacks())

		q := f.startProcess(t)
		f.voter.Preparing(f.ctx, q.payload, input)
		require.NoError(t, f.voter.Prepared(f.ctx, q.payload, input, meta))
		require.Equal(t, 1, f.voter.PendingEntries())
		q.end()
		require.Zero(t, f.voter.PendingEntries())
	})

	t.Run("Prepared finishes after the end", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
		input := line(f.zero, f.a.String(), "refs/heads/main")

		p := f.startProcess(t)
		f.voter.Preparing(f.ctx, p.payload, input)
		p.end()
		require.NoError(t, f.voter.Prepared(f.ctx, p.payload, input, meta))
		require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, "refs/heads/main"))
		require.Zero(t, f.voter.PendingEntries())
	})

	t.Run("the entry of another process with the same input stays", func(t *testing.T) {
		t.Parallel()

		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
		input := line(f.zero, f.a.String(), "refs/heads/main")

		a, b := f.startProcess(t), f.startProcess(t)
		f.voter.Preparing(f.ctx, a.payload, input)
		a.end()
		f.voter.Preparing(f.ctx, b.payload, input)
		require.NoError(t, f.voter.Prepared(f.ctx, b.payload, input, meta))

		a.end()
		require.Equal(t, 1, f.voter.PendingEntries())
		gittest.WriteRef(t, f.cfg, f.repoPath, "refs/heads/main", f.a)
		f.voter.Committed(f.ctx, b.payload, input)
		require.Zero(t, f.voter.PendingEntries())
	})
}
