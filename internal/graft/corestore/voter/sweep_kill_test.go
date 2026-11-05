package voter_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// killPoint is a point of a reference transaction where the hook of a real git update-ref stops, for
// the context of the sweep to end there.
type killPoint struct {
	// state is the phase of the hook, and afterVoter tells whether the hook stops after the voter
	// ran that phase or before.
	state      hook.ReferenceTransactionState
	afterVoter bool
}

// killHooks stops the next transaction at a point of its hook, until the hook's caller is gone.
type killHooks struct {
	hook.Manager
	armed atomic.Pointer[armedKill]
	// abort ends the wait of a test that went wrong.
	abort chan struct{}
}

type armedKill struct {
	point   killPoint
	reached chan struct{}
}

func (m *killHooks) arm(point killPoint) <-chan struct{} {
	armed := &armedKill{point: point, reached: make(chan struct{})}
	m.armed.Store(armed)
	return armed.reached
}

func (m *killHooks) ReferenceTransactionHook(ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
	armed := m.armed.Load()
	if armed == nil || armed.point.state != state || !m.armed.CompareAndSwap(armed, nil) {
		return m.Manager.ReferenceTransactionHook(ctx, state, env, stdin)
	}

	stop := func() {
		close(armed.reached)
		select {
		case <-ctx.Done():
		case <-m.abort:
		}
	}

	if !armed.point.afterVoter {
		// The killed hook never reached the voter.
		stop()
		return ctx.Err()
	}
	err := m.Manager.ReferenceTransactionHook(ctx, state, env, stdin)
	stop()
	return err
}

// sweepKillServer is a server in authoritative mode with real hooks, and a branch whose proposals
// have expired.
type sweepKillServer struct {
	*admissionServer
	hooks *killHooks
	voter *voter.Voter
	blobs *countingBlobs
	round int
}

const killedBranch = "main"

func newSweepKillServer(t *testing.T, dialect sqlstore.Dialect) *sweepKillServer {
	t.Helper()

	hooks := &killHooks{abort: make(chan struct{})}
	s := &sweepKillServer{hooks: hooks}
	s.admissionServer = newAdmissionServer(t, dialect,
		testserver.WithHookManagerWrapper(func(manager hook.Manager) hook.Manager {
			hooks.Manager = manager
			return hooks
		}),
		testserver.WithStorageVoterObserver(func(v *voter.Voter) { s.voter = v }),
	)
	t.Cleanup(func() { close(hooks.abort) })
	s.blobs = &countingBlobs{BlobStore: s.core.Blobs}
	s.core.Blobs = s.blobs
	return s
}

// writeExpiredProposals writes two proposals of the branch, through the hooks, that any sweep deletes.
func (s *sweepKillServer) writeExpiredProposals(t *testing.T) []string {
	t.Helper()

	var names []string
	var ops []proposals.Op
	for i := 0; i < 2; i++ {
		s.round++
		commit := gittest.WriteCommit(t, s.cfg, s.repoPath,
			gittest.WithMessage(fmt.Sprintf("proposal %d", s.round)),
			gittest.WithCommitterDate(time.Now().Add(-8*24*time.Hour)))
		name := proposals.Ref(killedBranch, commit)
		names = append(names, name.String())
		ops = append(ops, proposals.Op{Name: name, NewOID: commit, OldOID: gittest.DefaultObjectHash.ZeroOID})
	}
	require.NoError(t, proposals.Update(s.ctx, s.repo, ops))
	return names
}

// sweep runs a sweep without a bound of time, as the next caller does.
func (s *sweepKillServer) sweep(ctx context.Context) error {
	return proposals.NewSweeper(proposals.WithDeadlineContext(func(ctx context.Context) (context.Context, context.CancelFunc) {
		return context.WithCancel(ctx)
	})).Sweep(ctx, s.repo, killedBranch, sweepKillTTL, time.Now())
}

const sweepKillTTL = 7 * 24 * time.Hour

// startKillableSweep starts a sweep whose deadline passes when the returned kill function is called.
func (s *sweepKillServer) startKillableSweep() (kill func(), done <-chan error) {
	expire := make(chan struct{})
	sweeper := proposals.NewSweeper(proposals.WithDeadlineContext(func(ctx context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancelCause(ctx)
		go func() {
			select {
			case <-expire:
				cancel(proposals.ErrSweepTimedOut)
			case <-ctx.Done():
			}
		}()
		return ctx, func() { cancel(nil) }
	}))

	result := make(chan error, 1)
	go func() {
		result <- sweeper.Sweep(s.ctx, s.repo, killedBranch, sweepKillTTL, time.Now())
	}()
	return func() { close(expire) }, result
}

// requireAdmissionHeld verifies that a writer would have to wait.
func (s *sweepKillServer) requireAdmissionHeld(t *testing.T) {
	t.Helper()

	cancelled, cancel := context.WithCancel(s.ctx)
	cancel()
	probe := localrefs.Key{0xaa, 0xcc}
	if s.core.RefAdmission.Acquire(cancelled, s.repoPath, probe, localrefs.WriterSyncer) {
		s.core.RefAdmission.Release(s.repoPath, probe)
		require.FailNow(t, "the admission is free")
	}
}

// reftableLeftovers returns what a git process left in the reftable directory: its lock of the table
// list, or the lock and temporary file of a table.
func (s *sweepKillServer) reftableLeftovers(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(s.repoPath, "reftable"))
	require.NoError(t, err)
	var leftovers []string
	for _, entry := range entries {
		if name := entry.Name(); name != "tables.list" && !strings.HasSuffix(name, ".ref") {
			leftovers = append(leftovers, name)
		}
	}
	return leftovers
}

func (s *sweepKillServer) dbRefs(t *testing.T) map[string]corestore.Target {
	t.Helper()

	refs, _, err := s.core.Meta.Refs(s.ctx, s.repoID)
	require.NoError(t, err)
	return refs
}

func (s *sweepKillServer) localRefExists(t *testing.T, name string) bool {
	t.Helper()

	ok, err := s.repo.HasRevision(s.ctx, git.Revision(name+"^{commit}"))
	require.NoError(t, err)
	return ok
}

func (s *sweepKillServer) headSeq(t *testing.T) uint64 {
	t.Helper()

	record, err := s.core.Meta.GetRepository(s.ctx, corestore.KeyOf(s.repoProto))
	require.NoError(t, err)
	return record.HeadSeq
}

// requireConsistent runs the check of graft storage verify, which catches the node up first: the
// local references are those of the database, and every object they reach is there and in the bucket.
func (s *sweepKillServer) requireConsistent(t *testing.T) {
	t.Helper()

	response, err := gitalypb.NewStorageCoreServiceClient(s.conn).VerifyRepository(s.ctx, &gitalypb.VerifyRepositoryRequest{
		Repository:        s.repoProto,
		CheckConnectivity: true,
	})
	require.NoError(t, err)
	require.Empty(t, response.GetDivergences())
	require.True(t, response.GetConnectivityOk(), response.GetConnectivityError())
}

// requireNothingLeft verifies what the killed process may not leave behind, in the voter and in the
// repository.
func (s *sweepKillServer) requireNothingLeft(t *testing.T) {
	t.Helper()

	require.Empty(t, s.reftableLeftovers(t), "git left its lock behind")
	require.Zero(t, s.voter.PreparedPacks(), "a pack of the Preparing phase stayed behind")
	require.Zero(t, s.voter.PendingEntries(), "a pending entry stayed behind")
}

// TestSweepKilledByItsContext runs real proposal sweeps in authoritative mode, with real git update-ref
// processes and hooks, and ends the context of each at every point of its transaction, as
// proposals.SweepTimeout does: the sweep's process dies mid-transaction. The bounds of the admission
// are an hour, so nothing here can depend on them: a turn that the end of the process does not release
// stays held. The state probes tell: requireAdmissionFree admits only a writer that does not have to
// wait.
//
// Where the database committed and git did not, the database is ahead of the local repository, as
// after a client that cancels: the catch-up of the next request repairs it.
func TestSweepKilledByItsContext(t *testing.T) {
	type expectation struct {
		// point is where the hook stops; nil for the sweep queued for its admission.
		point *killPoint
		// committedInDatabase and committedInGit tell how far the transaction got.
		committedInDatabase, committedInGit bool
		// released is what the end of the process released: the queued sweep never held anything.
		released float64
	}

	for _, tc := range []struct {
		name   string
		expect expectation
	}{
		{
			name:   "queued for the admission",
			expect: expectation{},
		},
		{
			name: "admitted, before git takes its lock",
			expect: expectation{
				point:    &killPoint{state: hook.ReferenceTransactionPreparing, afterVoter: true},
				released: 1,
			},
		},
		{
			name: "admitted, under git's lock, before the database commit",
			expect: expectation{
				point:    &killPoint{state: hook.ReferenceTransactionPrepared},
				released: 1,
			},
		},
		{
			name: "after the database commit, before git's commit",
			expect: expectation{
				point:               &killPoint{state: hook.ReferenceTransactionPrepared, afterVoter: true},
				committedInDatabase: true,
				released:            1,
			},
		},
		{
			name: "after git's commit, before the voter hears of it",
			expect: expectation{
				point:               &killPoint{state: hook.ReferenceTransactionCommitted},
				committedInDatabase: true,
				committedInGit:      true,
				released:            1,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				s := newSweepKillServer(t, dialect)

				// A kill is a race between what the process is doing and the signal: the rounds give
				// each outcome its chance.
				for round := 0; round < sweepKillRounds; round++ {
					t.Logf("round %d", round)

					names := s.writeExpiredProposals(t)
					s.requireAdmissionFree(t)
					before := s.dbRefs(t)
					headBefore := s.headSeq(t)
					releasedBefore := metricValue(t, s.core.RefAdmission, releasedUnreportedMetric, nil)
					putsBefore := s.blobs.count()

					var holder func()
					var reached <-chan struct{}
					if tc.expect.point != nil {
						reached = s.hooks.arm(*tc.expect.point)
					} else {
						holder = s.core.RefAdmission.AcquireUnique(s.ctx, s.repoPath, localrefs.WriterSyncer)
						require.NotNil(t, holder)
					}

					kill, done := s.startKillableSweep()
					if reached != nil {
						select {
						case <-reached:
						case err := <-done:
							require.FailNow(t, "the sweep ended before the point", "%v", err)
						case <-time.After(time.Minute):
							require.FailNow(t, "the sweep did not get to the point")
						}
						s.requireAdmissionHeld(t)
					} else {
						requireWaiting(t, s.core.RefAdmission, 1)
					}

					kill()
					err := <-done
					require.ErrorIs(t, err, proposals.ErrSweepTimedOut)

					if holder != nil {
						// The killed writer left the queue: it has no turn after the holder's.
						requireWaiting(t, s.core.RefAdmission, 0)
						s.requireAdmissionHeld(t)
						holder()
					}

					// The state right after the sweep returned, with nothing having passed that a
					// bound could account for.
					s.requireAdmissionFree(t)
					require.Equal(t, tc.expect.released, metricValue(t, s.core.RefAdmission, releasedUnreportedMetric, nil)-releasedBefore)
					require.Equal(t, 0.0, metricValue(t, s.core.RefAdmission, expiredMetric, nil))
					require.Equal(t, 0.0, metricValue(t, s.core.RefAdmission, bypassedMetric, nil))
					require.Equal(t, putsBefore, s.blobs.count(), "a sweep only deletes: it has no pack")
					s.requireNothingLeft(t)

					// How far the transaction got.
					after := s.dbRefs(t)
					for _, name := range names {
						if tc.expect.committedInDatabase {
							require.NotContains(t, after, name)
						} else {
							require.Equal(t, before[name], after[name])
						}
						require.Equal(t, !tc.expect.committedInGit, s.localRefExists(t, name))
					}
					if tc.expect.committedInDatabase {
						require.Equal(t, headBefore+1, s.headSeq(t))
					} else {
						require.Equal(t, headBefore, s.headSeq(t))
					}

					// The catch-up of the next request repairs a database that is ahead, and the
					// references agree.
					s.requireConsistent(t)
					for _, name := range names {
						require.Equal(t, !tc.expect.committedInDatabase, s.localRefExists(t, name), "the local copy follows the database")
					}

					// The next sweep and the next writer go through.
					require.NoError(t, s.sweep(s.ctx))
					s.requireAdmissionFree(t)
					next := git.ReferenceName(fmt.Sprintf("refs/heads/next-%d", s.round))
					commit := gittest.WriteCommit(t, s.cfg, s.repoPath, gittest.WithMessage(next.String()))
					require.NoError(t, s.repo.UpdateRef(s.ctx, next, commit, gittest.DefaultObjectHash.ZeroOID))
					s.requireAdmissionFree(t)
					for _, name := range names {
						require.False(t, s.localRefExists(t, name))
						require.NotContains(t, s.dbRefs(t), name)
					}
					s.requireConsistent(t)
				}
			})
		})
	}
}

// sweepKillRounds is how many sweeps each point of TestSweepKilledByItsContext kills.
const sweepKillRounds = 5

// killAfterDatabaseCommit kills a sweep after its database commit and before git's, and returns the
// references the sweep deleted in the database only.
func (s *sweepKillServer) killAfterDatabaseCommit(t *testing.T) []string {
	t.Helper()

	names := s.writeExpiredProposals(t)
	reached := s.hooks.arm(killPoint{state: hook.ReferenceTransactionPrepared, afterVoter: true})
	kill, done := s.startKillableSweep()
	select {
	case <-reached:
	case err := <-done:
		require.FailNow(t, "the sweep ended before the point", "%v", err)
	}
	kill()
	require.ErrorIs(t, <-done, proposals.ErrSweepTimedOut)

	refs := s.dbRefs(t)
	for _, name := range names {
		require.NotContains(t, refs, name)
		require.True(t, s.localRefExists(t, name), "the database is ahead")
	}
	return names
}

// TestSweepKilledAfterTheDatabaseCommitThenASweepWithoutCatchUp is the case of a kill that leaves the
// database ahead, followed by a writer that does not go through the catch-up of the gate first: the
// next sweep lists the references the database no longer has, and the database refuses its deletions
// (they expect a value it does not hold). That sweep fails and changes nothing, the admission is free
// after it, and the catch-up repairs the local repository.
func TestSweepKilledAfterTheDatabaseCommitThenASweepWithoutCatchUp(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		s := newSweepKillServer(t, dialect)

		names := s.killAfterDatabaseCommit(t)
		s.requireAdmissionFree(t)
		head := s.headSeq(t)

		require.Error(t, s.sweep(s.ctx), "the database refuses a deletion based on a stale local value")
		s.requireAdmissionFree(t)
		require.Equal(t, head, s.headSeq(t))
		for _, name := range names {
			require.True(t, s.localRefExists(t, name), "git aborted the transaction")
		}
		s.requireNothingLeft(t)

		s.requireConsistent(t)
		for _, name := range names {
			require.False(t, s.localRefExists(t, name), "the catch-up repaired the local repository")
		}
		require.NoError(t, s.sweep(s.ctx))
		s.requireAdmissionFree(t)
	})
}

// TestWriteKilledWithItsPack kills a write that brings objects, between the two phases of the voter:
// unlike a sweep's, its transaction has a pack from the Preparing phase. The pack in the bucket is an
// orphan like that of any failed write, for the reconciliation of the garbage collection, and no
// changeset refers to it; the voter does not keep it.
func TestWriteKilledWithItsPack(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		s := newSweepKillServer(t, dialect)

		commit := gittest.WriteCommit(t, s.cfg, s.repoPath, gittest.WithMessage("killed write"))
		name := proposals.Ref(killedBranch, commit)
		head := s.headSeq(t)
		puts := s.blobs.count()

		reached := s.hooks.arm(killPoint{state: hook.ReferenceTransactionPrepared})
		ctx, cancel := context.WithCancel(s.ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- proposals.Update(ctx, s.repo, []proposals.Op{{Name: name, NewOID: commit, OldOID: gittest.DefaultObjectHash.ZeroOID}})
		}()
		select {
		case <-reached:
		case err := <-done:
			require.FailNow(t, "the write ended before the point", "%v", err)
		}
		require.Equal(t, puts+1, s.blobs.count(), "Preparing uploaded the pack")
		require.Equal(t, 1, s.voter.PreparedPacks())

		cancel()
		require.Error(t, <-done)

		s.requireAdmissionFree(t)
		s.requireNothingLeft(t)
		require.Equal(t, head, s.headSeq(t))
		require.NotContains(t, s.dbRefs(t), name.String())
		s.requireConsistent(t)

		// The retry goes through.
		require.NoError(t, proposals.Update(s.ctx, s.repo, []proposals.Op{{Name: name, NewOID: commit, OldOID: gittest.DefaultObjectHash.ZeroOID}}))
		s.requireConsistent(t)
	})
}
