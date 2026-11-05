package operations

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests of this file read the process's counters exactly, so they do not call t.Parallel: a
// top-level test that is not parallel runs while every parallel one is paused, and nothing else can
// move the counters under them.

func init() {
	// proposals.SweepTimeout is real time, which a loaded machine can run out of in any test of this
	// package that sweeps through the service: here a sweep runs out of time only when a test says so.
	proposals.ProcessSweeper = unboundedSweeper()
}

// unboundedSweeper is a sweeper without the bound of SweepTimeout.
func unboundedSweeper(opts ...proposals.SweeperOption) *proposals.Sweeper {
	return proposals.NewSweeper(append([]proposals.SweeperOption{
		proposals.WithDeadlineContext(func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithCancel(ctx)
		}),
	}, opts...)...)
}

// sweepCounters reads what the counters of the sweeps moved by since it was called.
func sweepCounters() func() (errors, timeouts float64) {
	errorsBefore, timeoutsBefore := testutil.ToFloat64(landProposalPruneErrors), testutil.ToFloat64(landProposalSweepTimeouts)
	return func() (float64, float64) {
		return testutil.ToFloat64(landProposalPruneErrors) - errorsBefore, testutil.ToFloat64(landProposalSweepTimeouts) - timeoutsBefore
	}
}

// proposalsOf lists the proposal references of the default branch.
func proposalsOf(t *testing.T, fx resolveFixture) []string {
	t.Helper()
	return strings2Fields(text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "for-each-ref", "--format=%(refname)", proposals.BranchDir(git.DefaultBranch))))
}

func strings2Fields(s string) []string {
	var fields []string
	start := -1
	for i, c := range s {
		switch {
		case c == '\n' && start >= 0:
			fields = append(fields, s[start:i])
			start = -1
		case c != '\n' && start < 0:
			start = i
		}
	}
	if start >= 0 {
		fields = append(fields, s[start:])
	}
	return fields
}

// writeDatedProposal makes a proposal reference named by a fake commit ID, n, dated when.
func writeDatedProposal(t *testing.T, fx resolveFixture, n int, when time.Time) git.ReferenceName {
	t.Helper()
	name := git.ReferenceName(fmt.Sprintf("%s%040x", proposals.BranchDir(git.DefaultBranch), n+1))
	target := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage(fmt.Sprintf("proposal %d", n)), gittest.WithCommitterDate(when))
	require.NoError(t, proposals.Update(fx.ctx, fx.repo, []proposals.Op{{Name: name, NewOID: target, OldOID: gittest.DefaultObjectHash.ZeroOID}}))
	return name
}

// expiredDate is past the default proposal_ttl whatever the clock says: the sweeps read the clock.
func expiredDate() time.Time { return time.Now().Add(-8 * 24 * time.Hour) }

// TestLandPruneProposals_aFailedSweepIsCounted: a sweep of the land that loses to a rewrite of a
// reference fails nothing of the land: it is counted once, as a failure, the rewritten proposal
// survives, and the next sweep cleans up the surplus.
func TestLandPruneProposals_aFailedSweepIsCounted(t *testing.T) {
	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, modelAnswer())
	rcfg := enabledResolver(argv)
	fx := newResolveFixture(t, rcfg)

	branchDir := proposals.BranchDir(git.DefaultBranch)
	raced := git.ReferenceName(branchDir + fx.commit.String())
	target := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage("raced"), gittest.WithCommitterDate(expiredDate()))
	rewritten := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage("rewritten"), gittest.WithCommitterDate(time.Now()))
	require.NoError(t, proposals.Update(fx.ctx, fx.repo, []proposals.Op{{Name: raced, NewOID: target, OldOID: gittest.DefaultObjectHash.ZeroOID}}))
	surplus := writeDatedProposal(t, fx, 1, expiredDate())

	var once sync.Once
	srv := fx.server(t, rcfg)
	srv.proposalSweeper = unboundedSweeper(proposals.WithBeforeDelete(func() {
		once.Do(func() {
			require.NoError(t, proposals.Update(fx.ctx, fx.repo, []proposals.Op{{Name: raced, NewOID: rewritten, OldOID: target}}))
		})
	}))

	moved := sweepCounters()
	srv.landPruneProposals(fx.ctx, fx.repo, git.DefaultBranch)
	failures, timeouts := moved()
	require.EqualValues(t, 1, failures, "the failed sweep is counted, once")
	require.Zero(t, timeouts)
	require.Equal(t, rewritten.String(), proposalRefTarget(t, fx), "the rewritten proposal survives")
	require.Contains(t, proposalsOf(t, fx), surplus.String(), "the failed transaction deleted nothing")

	srv.landPruneProposals(fx.ctx, fx.repo, git.DefaultBranch)
	failures, _ = moved()
	require.EqualValues(t, 1, failures, "the later sweep did not fail")
	require.Equal(t, rewritten.String(), proposalRefTarget(t, fx))
	require.NotContains(t, proposalsOf(t, fx), surplus.String(), "the later sweep cleaned up")
}

// TestLandPruneProposals_aSweepOutOfTimeIsCountedApart: a sweep that runs out of its bound is counted
// as that and not as a failure, and the next sweep removes what it left.
func TestLandPruneProposals_aSweepOutOfTimeIsCountedApart(t *testing.T) {
	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, modelAnswer())
	rcfg := enabledResolver(argv)
	fx := newResolveFixture(t, rcfg)
	surplus := writeDatedProposal(t, fx, 1, expiredDate())

	var (
		mu    sync.Mutex
		fires []func()
		once  sync.Once
	)
	srv := fx.server(t, rcfg)
	srv.proposalSweeper = proposals.NewSweeper(
		proposals.WithDeadlineContext(func(ctx context.Context) (context.Context, context.CancelFunc) {
			bounded, cancel := context.WithCancelCause(ctx)
			mu.Lock()
			defer mu.Unlock()
			fires = append(fires, func() { cancel(proposals.ErrSweepTimedOut) })
			return bounded, func() { cancel(nil) }
		}),
		// The deadline passes with the sweep about to delete.
		proposals.WithBeforeDelete(func() {
			once.Do(func() {
				mu.Lock()
				defer mu.Unlock()
				fires[0]()
			})
		}),
	)

	moved := sweepCounters()
	srv.landPruneProposals(fx.ctx, fx.repo, git.DefaultBranch)
	failures, timeouts := moved()
	require.EqualValues(t, 1, timeouts, "counted as out of time")
	require.Zero(t, failures, "not as a failure")
	require.Contains(t, proposalsOf(t, fx), surplus.String())

	srv.landPruneProposals(fx.ctx, fx.repo, git.DefaultBranch)
	failures, timeouts = moved()
	require.EqualValues(t, 1, timeouts)
	require.Zero(t, failures)
	require.Empty(t, proposalsOf(t, fx), "the next sweep removed it")
}

// TestLandPruneAfterLand_aFailedDropIsCounted: a drop that loses to a rewrite is counted once, and
// the rewritten proposal survives both the drop and the sweep after it.
func TestLandPruneAfterLand_aFailedDropIsCounted(t *testing.T) {
	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, modelAnswer())
	rcfg := enabledResolver(argv)
	fx := newResolveFixture(t, rcfg)

	ref := proposals.Ref(git.DefaultBranch, fx.commit)
	target := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage("read"), gittest.WithCommitterDate(time.Now()))
	rewritten := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage("rewritten"), gittest.WithCommitterDate(time.Now()))
	require.NoError(t, proposals.Update(fx.ctx, fx.repo, []proposals.Op{{Name: ref, NewOID: target, OldOID: gittest.DefaultObjectHash.ZeroOID}}))

	var once sync.Once
	srv := fx.server(t, rcfg)
	srv.proposalSweeper = unboundedSweeper(proposals.WithBeforeDelete(func() {
		once.Do(func() {
			require.NoError(t, proposals.Update(fx.ctx, fx.repo, []proposals.Op{{Name: ref, NewOID: rewritten, OldOID: target}}))
		})
	}))

	moved := sweepCounters()
	srv.landPruneAfterLand(fx.ctx, fx.request("Land"))
	failures, timeouts := moved()
	require.EqualValues(t, 1, failures, "the failed drop is counted, once")
	require.Zero(t, timeouts)
	require.Equal(t, rewritten.String(), proposalRefTarget(t, fx))
}

// TestLandAfterLand_releasesTheTicketsBeforeTheSweep: the lands that wait for their turn on the files
// of a land that landed do not wait for its sweep.
func TestLandAfterLand_releasesTheTicketsBeforeTheSweep(t *testing.T) {
	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, modelAnswer())
	rcfg := enabledResolver(argv)
	fx := newResolveFixture(t, rcfg)
	writeDatedProposal(t, fx, 1, expiredDate())

	keys := landTicketKeys(landTicketScope(fx.repoProto.GetStorageName(), fx.repoProto.GetRelativePath(), []byte(git.DefaultBranch)), []string{"a"}, false)
	ticket, err := landTickets.acquire(fx.ctx, keys)
	require.NoError(t, err)
	defer ticket.release()
	pre := newLandPreResolution()
	pre.ticket = ticket
	require.True(t, landTickets.blocked(keys, nil))

	swept := false
	srv := fx.server(t, rcfg)
	srv.proposalSweeper = unboundedSweeper(proposals.WithBeforeDelete(func() {
		swept = true
		require.False(t, landTickets.blocked(keys, nil), "the ticket is free while the sweep runs")
	}))
	srv.landAfterLand(fx.ctx, fx.request("Land"), pre)
	require.True(t, swept, "the sweep ran")
	require.Empty(t, proposalsOf(t, fx))

	// A land without a pre-resolution has no ticket to give up.
	srv.landAfterLand(fx.ctx, fx.request("Land"), nil)
}

// landingFixture is a repository whose lands do not conflict: every change adds a file of its own.
func landingFixture(t *testing.T, graft config.Graft) resolveFixture {
	t.Helper()
	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, modelAnswer())
	graft.Land.Resolver = enabledResolver(argv)
	return newResolveFixtureIn(t, graft,
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "base\n"}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "ours\n"}},
		[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "base\n"}, {Mode: "100644", Path: "d", Content: "d\n"}},
	)
}

// TestUserLand_sweepsTheBranchAfterTheLand: a land that asked for resolution sweeps the surplus of
// its branch, and drops its own proposal, with nothing counted as failed.
func TestUserLand_sweepsTheBranchAfterTheLand(t *testing.T) {
	fx := landingFixture(t, config.Graft{})
	writeDatedProposal(t, fx, 1, expiredDate())
	require.NoError(t, proposals.Update(fx.ctx, fx.repo, []proposals.Op{{
		Name:   proposals.Ref(git.DefaultBranch, fx.commit),
		NewOID: gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage("own proposal"), gittest.WithCommitterDate(time.Now())),
		OldOID: gittest.DefaultObjectHash.ZeroOID,
	}}))

	moved := sweepCounters()
	_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	require.NoError(t, err)
	failures, timeouts := moved()
	require.Zero(t, failures)
	require.Zero(t, timeouts)
	require.Empty(t, proposalsOf(t, fx), "the surplus is swept and the proposal of the change that landed is dropped")
}

// TestUserLand_concurrentLandsWithSurplus: concurrent lands of a branch with surplus, through the real
// service, alone or through its lanes: they all land, no sweep fails, and the surplus goes. Before the
// sweeps went one at a time they issued the same deletions, and all but one failed.
func TestUserLand_concurrentLandsWithSurplus(t *testing.T) {
	for desc, graft := range map[string]config.Graft{
		"alone":          {},
		"through a lane": {Standalone: true},
	} {
		t.Run(desc, func(t *testing.T) {
			fx := landingFixture(t, graft)
			for i := 0; i < proposals.MaxPerBranch+3; i++ {
				writeDatedProposal(t, fx, 100+i, time.Now().Add(-time.Duration(i+1)*time.Hour))
			}
			for i := 0; i < 3; i++ {
				writeDatedProposal(t, fx, 200+i, expiredDate())
			}

			const landers = 8
			requests := make([]*gitalypb.UserLandRequest, landers)
			for i := range requests {
				name := fmt.Sprintf("file-%02d", i)
				requests[i] = fx.request("Land " + name)
				requests[i].CommitId = gittest.WriteCommit(t, fx.cfg, fx.repoPath,
					gittest.WithParents(fx.base),
					gittest.WithTreeEntries(
						gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base\n"},
						gittest.TreeEntry{Mode: "100644", Path: name, Content: name},
					),
				).String()
			}

			moved := sweepCounters()
			start := make(chan struct{})
			errs := make([]error, landers)
			var wg sync.WaitGroup
			for i := range requests {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, errs[i] = fx.client.UserLand(fx.ctx, requests[i])
				}()
			}
			close(start)
			wg.Wait()

			for i, err := range errs {
				require.NoError(t, err, "land %d", i)
			}
			failures, timeouts := moved()
			require.Zero(t, failures, "no sweep failed")
			require.Zero(t, timeouts)
			require.Len(t, proposalsOf(t, fx), proposals.MaxPerBranch)
		})
	}
}
