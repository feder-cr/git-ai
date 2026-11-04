package operations

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Tests of PLAN10 15.9 after the ruling that every verification runs out of the lane: whatever made the
// resolution (a rule, an agent, a model), verify.command runs on the exact tree of the land, with a
// slot of the process's pool, before the lane; the lane compares the tree it would write with the one that
// passed, and runs nothing.

// heldPool starts a service whose verifications go through a pool the test can hold: it returns the
// service's own Server, to take the slot of the fixture's repository with.
func heldPool(t *testing.T, rcfg config.GraftResolver) (resolveFixture, *Server) {
	t.Helper()

	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	fx := newResolveFixture(t, rcfg)
	require.NotNil(t, s)
	require.NotNil(t, s.verifyPool, "a pool whenever verify.enabled is")
	return fx, s
}

// busyDeadline is the deadline of a land whose resolver timeout and verify timeout are 1m each (the sum the
// land reports).
const busyDeadline = "2m0s"

// armTheLandDeadline gives the lands of s a deadline that no clock cuts: it passes when the test says so. The
// returned function arms the next land: its deadline then passes as soon as it waits for the verify slot of repo,
// that is when the pool counts a second user of the repository besides the test's own holder (a state, not a
// duration). The tests that hold the slot of the repository used a real 2 s: under load the resolver alone could
// take more, and the land then ended in the resolution, not at the verification. A land that is not armed has no
// deadline at all.
func armTheLandDeadline(t *testing.T, s *Server, repo string) (arm func()) {
	var mu sync.Mutex
	var current *lateDeadlineContext
	s.landDeadlineContext = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		c := newLateDeadlineContext(ctx)
		mu.Lock()
		current = c
		mu.Unlock()
		return c, func() {}
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	return func() {
		mu.Lock()
		current = nil
		mu.Unlock()
		go func() {
			for {
				mu.Lock()
				c := current
				mu.Unlock()
				if c != nil && s.verifyPool.Users(repo) >= 2 {
					c.expire()
					return
				}
				select {
				case <-stop:
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}()
	}
}

// TestUserLand_aRulesLandIsVerifiedThroughThePool: the rules' resolution of a land needs the verification
// too, out of the lane and through the pool of the process: with the repository's slot taken, the
// verification cannot start before the deadline, the command never runs, nothing lands, and the rules'
// files are NOT_ATTEMPTED with the reason (the agent gets the package).
func TestUserLand_aRulesLandIsVerifiedThroughThePool(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, validAnswer())
	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(enabledResolver(rargv), vargv, "1m")
	rcfg.Timeout = "1m"
	fx, s := heldPool(t, rcfg)
	arm := armTheLandDeadline(t, s, landVerifyRepoKey(fx.repoProto))

	slot, err := s.verifyPool.Acquire(fx.ctx, landVerifyRepoKey(fx.repoProto))
	require.NoError(t, err)
	arm()
	_, err = fx.client.UserLand(fx.ctx, fx.request("Land"))
	requireMergeConflict(t, err)
	pkg := landConflictDetail(err)
	require.NotNil(t, pkg, "the package, not a bare deadline")
	for _, p := range []string{"a", "b"} {
		f := packageFile(t, pkg, p)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, f.GetOutcome(), p)
		require.Contains(t, f.GetReason(), "could not verify before the deadline of "+busyDeadline, p)
	}
	require.Equal(t, 0, verifyRuns(t, vdir), "the command did not run: the slot of its repository was taken")
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)

	// With the slot free the same land passes the verification and lands, signed by what ran.
	slot.Release()
	resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	require.NoError(t, err)
	commit, _ := landedCommit(t, fx, resp)
	require.Contains(t, string(commit.GetBody()), "Graft-Checked-By: verify:verify.sh\n")
	require.Equal(t, 1, verifyRuns(t, vdir))
}

// TestUserLand_anAgentsResolutionIsVerifiedThroughThePool: the same for the resolution of an agent, with no
// AI resolver at all: its verification runs before the lane with a slot of the pool, and a deadline ends the
// land with the package and the reason, the command not run.
func TestUserLand_anAgentsResolutionIsVerifiedThroughThePool(t *testing.T) {
	t.Parallel()

	vdir, vargv := countingVerify(t, "grep -qx agent-a a")
	rcfg := withVerify(config.GraftResolver{Timeout: "1m"}, vargv, "1m")
	fx, s := heldPool(t, rcfg)
	arm := armTheLandDeadline(t, s, landVerifyRepoKey(fx.repoProto))

	slot, err := s.verifyPool.Acquire(fx.ctx, landVerifyRepoKey(fx.repoProto))
	require.NoError(t, err)
	req := fx.request("Land")
	req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
	arm()
	_, err = fx.client.UserLand(fx.ctx, req)
	pkg := landConflictDetail(err)
	require.NotNil(t, pkg)
	require.Contains(t, pkg.GetReason(), "the resolution could not be verified: could not verify before the deadline of "+busyDeadline)
	require.Equal(t, 0, verifyRuns(t, vdir))
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)

	slot.Release()
	resp, err := fx.client.UserLand(fx.ctx, req)
	require.NoError(t, err)
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, git.ObjectID(resp.GetLandedCommitId()))
	require.Equal(t, 1, verifyRuns(t, vdir))
}

// TestUserLand_aRulesLandWhoseTipMovedIsVerifiedAgain: the tree is what is verified. While the first
// verification of a rules land runs, a writer adds a file to the branch: the tree to land is another one,
// the lane hands the land back, the same verdicts are verified on the new tree out of the lane (the resolver
// is not called again), and that is what lands.
func TestUserLand_aRulesLandWhoseTipMovedIsVerifiedAgain(t *testing.T) {
	t.Parallel()

	fx, rdir, vdir := verifiedFixture(t, validAnswer(), moveBranchBody, nil)
	writer := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip),
		gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n", "e", "e\n")...))
	writeVerifyMoves(t, vdir, fx, writer)

	resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	require.NoError(t, err)
	commit, landed := landedCommit(t, fx, resp)
	require.Equal(t, []string{writer.String(), fx.commit.String()}, commit.GetParentIds())
	require.Equal(t, "e\n", fx.blobAt(t, landed, "e"))
	require.Equal(t, 1, resolverCalls(t, rdir), "no resolver call: the files did not change")
	require.Equal(t, 2, verifyRuns(t, vdir), "the tree of the new tip is verified, out of the lane")
}

// TestUserLand_aFailingVerificationRejectsTheRulesFiles: a command that fails on the tree of a rules land
// rejects its files with the output, before the lane, and the branch stays.
func TestUserLand_aFailingVerificationRejectsTheRulesFiles(t *testing.T) {
	t.Parallel()

	fx, _, vdir := verifiedFixture(t, validAnswer(), `echo "2 tests failed"; exit 3`, nil)
	_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	requireMergeConflict(t, err)
	for _, f := range landConflictDetail(err).GetFiles() {
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, f.GetOutcome())
		require.Equal(t, "verification failed: exit status 3: 2 tests failed", f.GetReason())
	}
	require.Equal(t, 1, verifyRuns(t, vdir))
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
}
