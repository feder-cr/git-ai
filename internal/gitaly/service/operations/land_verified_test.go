package operations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests of a model's resolution that lands through the operator's verification (PLAN10 15.9): it
// lands only if verify.command passed, out of the lane, on the exact tree that lands.

// countingVerify writes a verification command into a new temporary directory and returns the
// directory and the argv of the command. Every run appends a line to dir/vlines before body runs,
// and N is the number of runs so far. body may block, move the branch or fail.
func countingVerify(t *testing.T, body string) (string, []string) {
	t.Helper()

	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
DIR='%s'
echo x >> "$DIR/vlines"
N=$(wc -l < "$DIR/vlines" | tr -d ' ')
%s
`, dir, body)
	path := filepath.Join(dir, "verify.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return dir, []string{path}
}

// verifyRuns returns the number of times the verification of dir ran.
func verifyRuns(t *testing.T, dir string) int {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "vlines"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)
	return len(raw) / 2
}

// moveBranchBody makes the n-th run of a countingVerify command move the branch to the commit in
// dir/move-n, when the test wrote one (writeVerifyMoves), before it goes on.
var moveBranchBody = fmt.Sprintf(`if [ -f "$DIR/move-$N" ]; then
  "$(cat "$DIR/git")" -C "$(cat "$DIR/repo")" update-ref refs/heads/%s "$(cat "$DIR/move-$N")"
fi`, git.DefaultBranch)

// writeVerifyMoves lets the n-th run of the verification of dir move the branch of fx to moves[n-1].
func writeVerifyMoves(t *testing.T, dir string, fx resolveFixture, moves ...git.ObjectID) {
	t.Helper()

	writeMoves(t, dir, fx, moves...)
}

// verifiedFixture is a land whose resolver answers with a model's resolution, and whose
// verify.command is the countingVerify command with the given body.
func verifiedFixture(t *testing.T, answer resolver.Response, verifyBody string, tune func(*config.GraftResolver)) (resolveFixture, string, string) {
	t.Helper()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, answer)
	vdir, vargv := countingVerify(t, verifyBody)
	rcfg := withVerify(enabledResolver(rargv), vargv, "")
	if tune != nil {
		tune(&rcfg)
	}
	return newResolveFixture(t, rcfg), rdir, vdir
}

// landCommit returns the commit with the given ID.
func landedCommit(t *testing.T, fx resolveFixture, resp *gitalypb.UserLandResponse) (*gitalypb.GitCommit, git.ObjectID) {
	t.Helper()

	id := git.ObjectID(resp.GetLandedCommitId())
	commit, err := fx.repo.ReadCommit(fx.ctx, id.Revision())
	require.NoError(t, err)
	return commit, id
}

func TestUserLand_verifiedModelResolution(t *testing.T) {
	t.Parallel()

	t.Run("a model's resolution that passes the verification lands, signed", func(t *testing.T) {
		t.Parallel()

		fx, rdir, vdir := verifiedFixture(t, modelAnswer(), "grep -qx merged-a a && grep -qx merged-b b && grep -qx d d", nil)

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land agent work"))
		require.NoError(t, err)
		commit, landed := landedCommit(t, fx, resp)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, landed)
		require.Equal(t, []string{fx.tip.String(), fx.commit.String()}, commit.GetParentIds())
		require.Equal(t, "graft-resolve/llm", resp.GetResolver())
		require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "a"))
		require.Equal(t, "merged-b\n", fx.blobAt(t, landed, "b"))
		body := string(commit.GetBody())
		require.Contains(t, body, "Graft-Resolved-By: graft-resolve/llm\n")
		require.Contains(t, body, "Graft-Checked-By: verify:verify.sh\n")
		require.Equal(t, 1, resolverCalls(t, rdir))
		// Once: out of the lane. The lane lands the verified tree without verifying it again.
		require.Equal(t, 1, verifyRuns(t, vdir))
		require.Empty(t, proposalRefTarget(t, fx), "nothing was proposed")
	})

	// A file that says nothing about what made it is a model's (fail-safe): whatever the name of the
	// resolver, it lands only through the verification.
	for desc, name := range map[string]string{
		"a plain name":     "graft-resolve/trivial",
		"joined names":     "graft-resolve/trivial,graft-resolve/llm",
		"a list cut to +N": "graft-resolve/trivial,+1",
	} {
		t.Run("an answer that says nothing about its files lands only if the verification passes, "+desc, func(t *testing.T) {
			t.Parallel()

			unmarked := validAnswer()
			unmarked.Resolver = name
			for i := range unmarked.Files {
				unmarked.Files[i].Origin = ""
			}

			passing, _, passingDir := verifiedFixture(t, unmarked, "exit 0", nil)
			resp, err := passing.client.UserLand(passing.ctx, passing.request("Land"))
			require.NoError(t, err)
			commit, _ := landedCommit(t, passing, resp)
			require.Equal(t, 1, verifyRuns(t, passingDir))
			require.Contains(t, string(commit.GetBody()), "Graft-Checked-By: verify:verify.sh\n")

			failing, _, failingDir := verifiedFixture(t, unmarked, `echo "tests failed"; exit 3`, nil)
			_, err = failing.client.UserLand(failing.ctx, failing.request("Land"))
			requireMergeConflict(t, err)
			for _, p := range []string{"a", "b"} {
				f := packageFile(t, landConflictDetail(err), p)
				require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
				require.Contains(t, f.GetReason(), "verification failed: exit status 3: tests failed", p)
			}
			require.Equal(t, 1, verifyRuns(t, failingDir))
			requireBranchAt(t, failing.cfg, failing.repoPath, git.DefaultBranch, failing.tip)
		})
	}

	t.Run("a model's file next to a rule's file lands when the tree passes", func(t *testing.T) {
		t.Parallel()

		answer := modelAnswer()
		answer.Resolver = "graft-resolve/trivial,llm"
		answer.Files[1].Proposal = false
		fx, _, vdir := verifiedFixture(t, answer, "exit 0", nil)

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		require.NoError(t, err)
		_, landed := landedCommit(t, fx, resp)
		require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "a"))
		require.Equal(t, "merged-b\n", fx.blobAt(t, landed, "b"))
		require.Equal(t, 1, verifyRuns(t, vdir))
	})

	t.Run("a verification that fails gives a proposal and the output", func(t *testing.T) {
		t.Parallel()

		fx, rdir, vdir := verifiedFixture(t, modelAnswer(), `echo "2 tests failed: TestFoo"; exit 3`, nil)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, proposals.Ref(git.DefaultBranch, fx.commit).String(), pkg.GetProposalRef())
		require.NotEmpty(t, proposalRefTarget(t, fx))
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "verification failed: exit status 3: 2 tests failed: TestFoo", p)
			require.NotNil(t, f.GetProposal(), p)
			require.Equal(t, "merged-"+p+"\n", string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "cat-file", "blob", f.GetProposal().GetOid())))
		}
		require.Equal(t, 1, resolverCalls(t, rdir))
		require.Equal(t, 1, verifyRuns(t, vdir), "once, out of the lane")
	})

	t.Run("a verification that times out leaves the files PROPOSED, with its reason", func(t *testing.T) {
		t.Parallel()

		rdir, rargv := resolverScript(t, "")
		writeAnswer(t, rdir, modelAnswer())
		vdir, vargv := countingVerify(t, "sleep 30")
		fx := newResolveFixture(t, withVerify(enabledResolver(rargv), vargv, "300ms"))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		f := packageFile(t, landConflictDetail(err), "a")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome())
		require.Contains(t, f.GetReason(), "the command did not finish within 300ms")
		require.Equal(t, 1, verifyRuns(t, vdir))
	})

	t.Run("with a verify.command that is disabled the reason says so", func(t *testing.T) {
		t.Parallel()

		rdir, rargv := resolverScript(t, "")
		writeAnswer(t, rdir, modelAnswer())
		vdir, vargv := countingVerify(t, "exit 0")
		rcfg := withVerify(enabledResolver(rargv), vargv, "")
		rcfg.Verify.Enabled = false
		fx := newResolveFixture(t, rcfg)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, landConflictDetail(err), p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "verification is disabled (verify.enabled is false)", p)
			require.NotContains(t, f.GetReason(), "no verify.command is configured", p)
		}
		require.Equal(t, 0, verifyRuns(t, vdir))
	})

	t.Run("without a verify.command a model's resolution is a proposal", func(t *testing.T) {
		t.Parallel()

		rdir, rargv := resolverScript(t, "")
		writeAnswer(t, rdir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(rargv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, landConflictDetail(err), p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "no verify.command is configured", p)
			require.NotNil(t, f.GetProposal(), p)
		}
	})

	t.Run("nothing is verified when another file keeps the land from landing", func(t *testing.T) {
		t.Parallel()

		// b is written with a mode no side has: refused. Verifying the tree would cost the operator's
		// tests for a land that cannot land.
		answer := modelAnswer()
		answer.Files[1].Mode = "100755"
		fx, _, vdir := verifiedFixture(t, answer, "exit 0", nil)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, packageFile(t, pkg, "a").GetOutcome())
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, packageFile(t, pkg, "b").GetOutcome())
		require.Equal(t, 0, verifyRuns(t, vdir))
	})

	t.Run("a tip that moved gets the new tree verified, then lands", func(t *testing.T) {
		t.Parallel()

		// While the first verification runs, a writer adds e to the branch: the tree to land is no
		// longer the verified one, so the land goes back out, rebuilds the tree with the same verdicts
		// (the resolver is not called again), verifies it, and lands that.
		fx, rdir, vdir := verifiedFixture(t, modelAnswer(), moveBranchBody, nil)
		writer := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip),
			gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n", "e", "e\n")...))
		writeVerifyMoves(t, vdir, fx, writer)

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		require.NoError(t, err)
		commit, landed := landedCommit(t, fx, resp)
		require.Equal(t, []string{writer.String(), fx.commit.String()}, commit.GetParentIds())
		require.Equal(t, "e\n", fx.blobAt(t, landed, "e"))
		require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "a"))
		require.Equal(t, 1, resolverCalls(t, rdir), "no resolver call: the files did not change")
		require.Equal(t, 2, verifyRuns(t, vdir), "the tree of the new tip is verified, out of the lane")
		require.Contains(t, string(commit.GetBody()), "Graft-Checked-By: verify:verify.sh\n")
	})

	t.Run("a tip that moved to the same tree keeps the verification", func(t *testing.T) {
		t.Parallel()

		// The tree is what is verified, not the tip: a writer that moves the branch to another commit
		// with the very tree the land started from leaves the verified tree the tree to land.
		fx, _, vdir := verifiedFixture(t, modelAnswer(), moveBranchBody, nil)
		same := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip), gittest.WithMessage("same tree, another commit"),
			gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n")...))
		writeVerifyMoves(t, vdir, fx, same)

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		require.NoError(t, err)
		commit, _ := landedCommit(t, fx, resp)
		require.Equal(t, []string{same.String(), fx.commit.String()}, commit.GetParentIds())
		require.Equal(t, 1, verifyRuns(t, vdir), "the same tree: verified once")
	})

	t.Run("a land the lane hands back for the last round gives a proposal, not a failure", func(t *testing.T) {
		t.Parallel()

		// resolve_rounds is a cap: past it, a tree the branch moved away from is not verified in time
		// any more, whatever the clock says. The model's files are proposals, with the reason that the
		// branch moved again, and the resolver is not called again.
		fx, rdir, vdir := verifiedFixture(t, modelAnswer(), moveBranchBody, func(r *config.GraftResolver) {
			r.ResolveRounds = 1
		})
		writer := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip),
			gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n", "e", "e\n")...))
		writeVerifyMoves(t, vdir, fx, writer)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		require.NotContains(t, err.Error(), "files kept changing")
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, writer.String(), pkg.GetTip(), "the package is about the tip the branch moved to")
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "could not verify before the branch moved again", p)
			require.NotNil(t, f.GetProposal(), p)
		}
		require.Equal(t, 1, resolverCalls(t, rdir))
		require.Equal(t, 1, verifyRuns(t, vdir), "the give-up verifies nothing")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, writer)
	})

	t.Run("churn past the deadline gives a proposal, not a failure", func(t *testing.T) {
		t.Parallel()

		// The branch moves under every verification, faster than the land can verify. At the deadline
		// the model's files become proposals, with the reason, and the agent gets the package.
		fx, rdir, vdir := verifiedFixture(t, modelAnswer(), "sleep 0.4\n"+moveBranchBody, func(r *config.GraftResolver) {
			r.Timeout = "1s"
			r.Verify.Timeout = "2s"
			r.ResolveRounds = 1000
		})
		var moves []git.ObjectID
		parent := fx.tip
		for i := 0; i < 40; i++ {
			parent = gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(parent),
				gittest.WithTreeEntries(entries("a", "ours\n", "b", "ours\n", "c", "c\n", "e", fmt.Sprintf("e-%d\n", i))...))
			moves = append(moves, parent)
		}
		writeVerifyMoves(t, vdir, fx, moves...)

		// The bound is a check that the land does not hang, not a measure: the deadline of the land is
		// 3 s, and a loaded machine takes a multiple of that for the pass that follows it.
		start := time.Now()
		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		took := time.Since(start)
		t.Logf("the land ended after %s (deadline 3s)", took)
		require.Less(t, took, 3*time.Minute)
		requireMergeConflict(t, err)
		require.NotContains(t, err.Error(), "files kept changing")
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.NotEmpty(t, pkg.GetProposalRef())
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "could not verify before", p)
			require.NotNil(t, f.GetProposal(), p)
			require.Equal(t, "merged-"+p+"\n", string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "cat-file", "blob", f.GetProposal().GetOid())))
		}
		require.Equal(t, 1, resolverCalls(t, rdir), "the verdicts are reused: the resolver is called once")
		// One round or several, by the speed of the machine: a loaded one spends the whole deadline on
		// the first. What does not depend on speed is that the verification ran, that the writers did
		// not run out (the deadline ended the land), and that the resolver was called once. The round
		// that goes back out after a tip moved is the test above, deterministic.
		require.GreaterOrEqual(t, verifyRuns(t, vdir), 1)
		require.Less(t, verifyRuns(t, vdir), len(moves), "the deadline ended it, not the writers")
		// The branch was only moved by the writers: the model's resolution did not land.
		tip := gittest.ResolveRevision(t, fx.cfg, fx.repoPath, git.DefaultBranch)
		parents := strings.Fields(text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "rev-list", "--parents", "-n1", tip.String())))
		require.NotContains(t, parents, fx.commit.String())
	})
}

func TestUserLand_verifiedModelResolution_laneIsFree(t *testing.T) {
	t.Parallel()

	// The first verification blocks until the test releases it. While it does, the branch's lane is
	// free: a plain land of another change passes through it. When the verification is released, the
	// tree of the model's land is not the tree that was verified (the plain land moved the branch),
	// and the land verifies the new tree and lands on top of the plain land.
	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, modelAnswer())
	vdir, vargv := countingVerify(t, `if [ "$N" = 1 ]; then
  i=0
  while [ ! -f "$DIR/release" ] && [ "$i" -lt 1200 ]; do sleep 0.1; i=$((i+1)); done
fi`)
	graft := batchingGraft()
	graft.Land.Resolver = withVerify(enabledResolver(rargv), vargv, "")
	fx := newResolveFixtureIn(t, graft,
		entries("a", "base\n", "b", "base\n", "c", "c\n"),
		entries("a", "ours\n", "b", "ours\n", "c", "c\n"),
		entries("a", "theirs\n", "b", "theirs\n", "c", "c\n", "d", "d\n"),
	)
	plain := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
		gittest.WithTreeEntries(entries("a", "base\n", "b", "base\n", "c", "c\n", "z", "z\n")...))

	model := landAsync(fx, fx.commit, "Land the model's")
	require.Eventually(t, func() bool { return verifyRuns(t, vdir) == 1 }, 30*time.Second, 10*time.Millisecond, "the verification never started")

	// The verification holds until the test releases it (or two minutes pass): a plain land that ends
	// before that went through a free lane, whatever the speed of the machine.
	plainReq := fx.request("Land the plain one")
	plainReq.CommitId = plain.String()
	plainReq.ResolveConflicts = false
	plainDone := make(chan landResult, 1)
	go func() {
		resp, err := fx.client.UserLand(fx.ctx, plainReq)
		plainDone <- landResult{resp: resp, err: err}
	}()
	var plainResult landResult
	select {
	case plainResult = <-plainDone:
	case <-time.After(60 * time.Second):
		t.Cleanup(func() { _ = os.WriteFile(filepath.Join(vdir, "release"), nil, 0o600) })
		require.FailNow(t, "the plain land did not end while the verification ran: the lane is held")
	}
	require.NoError(t, plainResult.err, "the lane is free while the verification runs")
	plainResp := plainResult.resp
	_, notReleased := os.Stat(filepath.Join(vdir, "release"))
	require.ErrorIs(t, notReleased, fs.ErrNotExist, "the plain land ended before the release")
	select {
	case r := <-model:
		require.FailNow(t, "the model's land ended before the verification was released", "%v %v", r.resp, r.err)
	default:
	}
	require.Equal(t, 1, verifyRuns(t, vdir))

	require.NoError(t, os.WriteFile(filepath.Join(vdir, "release"), nil, 0o600))
	resp := requireLanded(t, model)
	commit, landed := landedCommit(t, fx, resp)
	require.Equal(t, []string{plainResp.GetLandedCommitId(), fx.commit.String()}, commit.GetParentIds())
	require.Equal(t, "z\n", fx.blobAt(t, landed, "z"))
	require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "a"))
	require.Equal(t, 2, verifyRuns(t, vdir))
	require.Equal(t, 1, resolverCalls(t, rdir))
}

// TestUserLand_verifiedModelResolution_poolBoundsVerifications: two lands of one repository at once
// never run the operator's command at once, though the pool has room for four: one repository takes one
// slot (verify_per_repository). The bound of the pool itself is tested in the resolver package.
func TestUserLand_verifiedModelResolution_poolBoundsVerifications(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, modelAnswer())
	vdir, vargv := countingVerify(t, `if ! mkdir "$DIR/lock" 2>/dev/null; then
  echo x >> "$DIR/overlap"
  sleep 0.3
  exit 0
fi
sleep 0.3
rmdir "$DIR/lock"`)
	rcfg := withVerify(enabledResolver(rargv), vargv, "")
	rcfg.VerifyConcurrency = 4
	graft := batchingGraft()
	graft.Land.Resolver = rcfg
	fx := newResolveFixtureIn(t, graft,
		entries("a", "base\n", "b", "base\n"),
		entries("a", "ours\n", "b", "ours\n"),
		entries("a", "theirs-0\n", "b", "theirs-0\n", "d0", "0\n"),
	)
	second := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base),
		gittest.WithTreeEntries(entries("a", "theirs-1\n", "b", "theirs-1\n", "d1", "1\n")...))

	first := landAsync(fx, fx.commit, "Land 0")
	other := landAsync(fx, second, "Land 1")
	requireLanded(t, first)
	requireLanded(t, other)

	require.GreaterOrEqual(t, verifyRuns(t, vdir), 2)
	_, err := os.Stat(filepath.Join(vdir, "overlap"))
	require.ErrorIs(t, err, fs.ErrNotExist, "two verifications ran at once")
}

// TestLandApplyVerdicts_neverVerifiesAModelsResolution: the lane writes a model's resolution only
// for a tree that verify.command passed on out of it, and never runs the command itself.
func TestLandApplyVerdicts_neverVerifiesAModelsResolution(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, modelAnswer())
	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(enabledResolver(rargv), vargv, "")
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	in := fx.request("Land")

	// Out of the lane: the answer is resolved and its tree verified, once.
	pre := newLandPreResolution()
	_, _, err := s.landResolveAt(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
	require.NoError(t, err)
	require.Equal(t, 1, verifyRuns(t, vdir))
	require.Len(t, pre.verified, 1)

	// In the lane, with the verified tree: it lands, and does not verify.
	res, err := s.landApplyVerdicts(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
	require.NoError(t, err)
	require.Equal(t, "merged-a\n", fx.blobAt(t, res.commit, "a"))
	require.Equal(t, 1, verifyRuns(t, vdir), "the lane did not run the command")
	message := string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "log", "-1", "--format=%B", res.commit.String()))
	require.Contains(t, message, "Graft-Checked-By: verify:verify.sh\n")

	// In the lane, with no tree verified (the tip moved to a tree nobody verified): it writes nothing
	// and hands the land back to be verified; it does not verify either.
	unverified := &landPreResolution{verdicts: pre.verdicts, verified: map[git.ObjectID]bool{}}
	_, err = s.landApplyVerdicts(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), unverified)
	var need *landNeedsResolution
	require.ErrorAs(t, err, &need)
	require.NotEmpty(t, need.unverified, "the tree to verify")
	require.Equal(t, 1, verifyRuns(t, vdir), "the lane did not run the command")

	// Another verified tree is not this one: tree OIDs are compared.
	other := &landPreResolution{verdicts: pre.verdicts, verified: map[git.ObjectID]bool{"0000000000000000000000000000000000000000": true}}
	_, err = s.landApplyVerdicts(fx.ctx, in, fx.repo, fx.tip, fx.commit, fx.committer(), other)
	require.ErrorAs(t, err, &need)
	require.Equal(t, 1, verifyRuns(t, vdir))
}

// TestUserLand_verifiedRulesAreVerifiedOutOfTheLane: a resolution made by rules alone is verified like
// any other (15.9): out of the lane, once, and the trailer names what ran.
func TestUserLand_verifiedRulesAreVerifiedOutOfTheLane(t *testing.T) {
	t.Parallel()

	fx, _, vdir := verifiedFixture(t, validAnswer(), "grep -qx merged-a a", nil)

	resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	require.NoError(t, err)
	commit, _ := landedCommit(t, fx, resp)
	require.Equal(t, 1, verifyRuns(t, vdir))
	require.Contains(t, string(commit.GetBody()), "Graft-Resolved-By: graft-resolve/fake\n")
	require.Contains(t, string(commit.GetBody()), "Graft-Checked-By: verify:verify.sh\n", "the trailer names what ran")

	failing, _, failingDir := verifiedFixture(t, validAnswer(), "exit 1", nil)
	_, err = failing.client.UserLand(failing.ctx, failing.request("Land"))
	requireMergeConflict(t, err)
	for _, f := range landConflictDetail(err).GetFiles() {
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, f.GetOutcome())
	}
	require.Equal(t, 1, verifyRuns(t, failingDir))
}

// TestUserLand_verifiedModelResolution_callerGivesUp: a land whose caller gives up while it verifies
// ends with the caller's cancellation, and what the SERVER does with it is as asserted here: the
// operator's command is killed (it does not run on for a land nobody waits for), nothing is kept as a
// proposal, and the branch is not touched. The Canceled on the client side proves none of it.
func TestUserLand_verifiedModelResolution_callerGivesUp(t *testing.T) {
	t.Parallel()

	fx, _, vdir := verifiedFixture(t, modelAnswer(), `echo $$ > "$DIR/pid"
sleep 60`, func(r *config.GraftResolver) { r.Verify.Timeout = "1m" })
	ctx, cancel := context.WithCancel(fx.ctx)
	req := fx.request("Land")
	done := make(chan error, 1)
	go func() {
		_, err := fx.client.UserLand(ctx, req)
		done <- err
	}()
	var pid int
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(filepath.Join(vdir, "pid"))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil
	}, 30*time.Second, 10*time.Millisecond, "the verification never started")
	require.NoError(t, syscall.Kill(pid, 0), "the verification is running")

	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		require.Equal(t, codes.Canceled, status.Code(err), "%v", err)
	case <-time.After(60 * time.Second):
		require.FailNow(t, "the land did not end when its caller gave up")
	}
	// The server's side: the command is gone, nothing was kept or landed.
	require.Eventually(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) },
		30*time.Second, 20*time.Millisecond, "the verification still runs for a land nobody waits for")
	require.Empty(t, proposalRefTarget(t, fx), "a land that was cancelled keeps no proposal")
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
}

// TestUserLand_verifiedModelResolution_aBusyRepositoryEndsInProposals: at most one verification runs
// for a repository (verify_per_repository). A land whose turn does not come before its deadline is not
// an error for the agent: it ends with the package, the model's files PROPOSED, and says why. The
// operator's command did not run for it.
func TestUserLand_verifiedModelResolution_aBusyRepositoryEndsInProposals(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, modelAnswer())
	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(enabledResolver(rargv), vargv, "1m")
	rcfg.Timeout = "1m"
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	s.locator = config.NewLocator(fx.cfg) // the land quarantines the objects it makes
	arm := armTheLandDeadline(t, s, landVerifyRepoKey(fx.repoProto))

	// Another land of the repository is being verified: its slot is the repository's.
	busy, err := s.verifyPool.Acquire(fx.ctx, landVerifyRepoKey(fx.repoProto))
	require.NoError(t, err)

	arm()
	_, err = s.landResolving(fx.ctx, fx.request("Land"), fx.committer())
	requireMergeConflict(t, err)
	pkg := landConflictDetail(err)
	require.NotNil(t, pkg, "the package, not a deadline")
	require.NotEmpty(t, pkg.GetProposalRef())
	for _, p := range []string{"a", "b"} {
		f := packageFile(t, pkg, p)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
		require.Contains(t, f.GetReason(), "could not verify before the deadline of "+busyDeadline, p)
		require.NotNil(t, f.GetProposal(), p)
	}
	require.Equal(t, 0, verifyRuns(t, vdir), "the command did not run for the land that waited")
	require.Equal(t, 1, resolverCalls(t, rdir))
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	busy.Release()

	// Another repository is not held back by the first one's.
	other, err := s.verifyPool.Acquire(fx.ctx, landVerifyRepoKey(&gitalypb.Repository{StorageName: "default", RelativePath: "another.git"}))
	require.NoError(t, err)
	other.Release()
}

// TestUserLand_verifiedModelResolution_aTreeThatPassedIsNotVerifiedAgain: when a land comes back to the
// pre-resolution with the tree that verify.command already passed on (a snapshot that reads the same tip
// again, a tip that moved to the same tree), the command does not run again: it takes minutes and holds
// the repository's one verification slot. The resolver is not called again either.
func TestUserLand_verifiedModelResolution_aTreeThatPassedIsNotVerifiedAgain(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, modelAnswer())
	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(enabledResolver(rargv), vargv, "")
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	s.locator = config.NewLocator(fx.cfg) // the land quarantines the objects it makes

	pre := newLandPreResolution()
	for round := 1; round <= 3; round++ {
		require.NoError(t, s.landPreResolve(fx.ctx, fx.request("Land"), fx.committer(), pre), "round %d", round)
	}
	require.Equal(t, 1, verifyRuns(t, vdir), "the command ran once for the tree, not once per round")
	require.Equal(t, 1, resolverCalls(t, rdir))
}

// lateDeadlineContext is a context whose deadline passes when the test says so: Err is then
// DeadlineExceeded, as for a caller whose own deadline ran out.
type lateDeadlineContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newLateDeadlineContext(parent context.Context) *lateDeadlineContext {
	c := &lateDeadlineContext{Context: parent, done: make(chan struct{})}
	go func() {
		select {
		case <-parent.Done():
			c.expire()
		case <-c.done:
		}
	}()
	return c
}

func (c *lateDeadlineContext) expire() { c.once.Do(func() { close(c.done) }) }

func (c *lateDeadlineContext) Done() <-chan struct{} { return c.done }

func (c *lateDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// TestUserLand_verifiedModelResolution_aCallerThatGivesUpGetsItsCancellation: a caller that cancels while
// the land verifies, or whose own deadline passes, gets Canceled or DeadlineExceeded from the server, and
// not the bare "verification not finished" of the internal error, nor a package: the land is not the
// deadline of the land (landFinalPass), which belongs to the land and not to the caller.
func TestUserLand_verifiedModelResolution_aCallerThatGivesUpGetsItsCancellation(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		code codes.Code
		ctx  func(parent context.Context) (context.Context, context.CancelFunc, func())
	}{
		"cancelled": {codes.Canceled, func(parent context.Context) (context.Context, context.CancelFunc, func()) {
			ctx, cancel := context.WithCancel(parent)
			return ctx, cancel, cancel
		}},
		// The deadline passes once the verification runs, not a fixed time after the test starts: under
		// load a fixed one can pass before the land has even begun to verify.
		"its own deadline": {codes.DeadlineExceeded, func(parent context.Context) (context.Context, context.CancelFunc, func()) {
			ctx := newLateDeadlineContext(parent)
			return ctx, func() {}, ctx.expire
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rdir, rargv := resolverScript(t, "")
			writeAnswer(t, rdir, modelAnswer())
			vdir, vargv := countingVerify(t, `echo $$ > "$DIR/pid"
sleep 60`)
			rcfg := withVerify(enabledResolver(rargv), vargv, "1m")
			rcfg.Timeout = "1m"
			fx := newResolveFixture(t, rcfg)
			s := fx.server(t, rcfg)
			s.locator = config.NewLocator(fx.cfg)

			ctx, cancelAll, giveUp := tc.ctx(fx.ctx)
			defer cancelAll()
			done := make(chan error, 1)
			go func() {
				_, err := s.landResolving(ctx, fx.request("Land"), fx.committer())
				done <- err
			}()
			require.Eventually(t, func() bool {
				_, err := os.Stat(filepath.Join(vdir, "pid"))
				return err == nil
			}, 30*time.Second, 10*time.Millisecond, "the verification never started")
			giveUp()

			select {
			case err := <-done:
				require.Error(t, err)
				require.Equal(t, tc.code, status.Code(err), "%v", err)
				require.Nil(t, landConflictDetail(err), "no package for a caller that is gone")
			case <-time.After(60 * time.Second):
				require.FailNow(t, "the land did not end")
			}
		})
	}
}

// TestUserLand_finalPass_aLostRaceStillEndsWithThePackage: the lane hands the land back after the pass
// pre-resolved it (the branch moved to a tip where a file has no verdict): the next pass ends with the
// package, the file that changed NOT_ATTEMPTED with the reason and the other one with its verdict. It
// is not a merge conflict without a package.
func TestUserLand_finalPass_aLostRaceStillEndsWithThePackage(t *testing.T) {
	t.Parallel()

	rdir, rargv := resolverScript(t, "")
	writeAnswer(t, rdir, validAnswer())
	rcfg := enabledResolver(rargv)
	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	fx := newResolveFixture(t, rcfg)
	require.NotNil(t, s, "the service's own Server")

	// The rules' verdicts of a and b, at the tip of the fixture.
	pre := newLandPreResolution()
	require.NoError(t, s.landPreResolve(fx.ctx, fx.request("Land"), fx.committer(), pre))
	require.Len(t, pre.verdicts, 2)

	var passes []int
	s.landFinalPassHook = func(pass int) {
		passes = append(passes, pass)
		if pass != 1 {
			return
		}
		// Another land changes a on the branch while this one ends.
		gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip), gittest.WithBranch("main"),
			gittest.WithTreeEntries(entries("a", "ours changed\n", "b", "ours\n", "c", "c\n")...))
	}
	_, err := s.landFinalPass(fx.ctx, fx.request("Land"), fx.committer(), pre, "the time ran out")
	requireMergeConflict(t, err)
	pkg := landConflictDetail(err)
	require.NotNil(t, pkg, "the package, not a bare merge conflict")
	require.Equal(t, []int{1}, passes, "the second pass pre-resolves and ends with the package")
	a := packageFile(t, pkg, "a")
	require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, a.GetOutcome())
	require.Equal(t, "the time ran out", a.GetReason())
	require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, packageFile(t, pkg, "b").GetOutcome())
}

// TestUserLand_aHeldModelResolutionNeverLands: a file the resolver holds back (the second model of
// agree_with did not agree) is a proposal that does not land even when verify.command would pass on
// the tree that would land; the command does not even run. The same answer without the hold lands.
func TestUserLand_aHeldModelResolutionNeverLands(t *testing.T) {
	t.Parallel()

	held := modelAnswer()
	held.Files[0].Hold = "held: the second model (glm) disagreed"
	fx, rdir, vdir := verifiedFixture(t, held, "grep -qx merged-a a && grep -qx merged-b b && grep -qx d d", nil)

	_, err := fx.client.UserLand(fx.ctx, fx.request("Land agent work"))
	requireMergeConflict(t, err)
	pkg := landConflictDetail(err)
	require.NotNil(t, pkg)
	var reasons []string
	for _, f := range pkg.GetFiles() {
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), f.GetPath())
		reasons = append(reasons, f.GetReason())
	}
	require.Contains(t, strings.Join(reasons, "\n"), landHeldReason("held: the second model (glm) disagreed"))
	require.Equal(t, 1, resolverCalls(t, rdir))
	require.Equal(t, 0, verifyRuns(t, vdir), "a land that cannot land is not verified")
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	require.NotEmpty(t, proposalRefTarget(t, fx), "the held resolution is kept as a proposal")

	// The hold is the whole difference: the same answer without it lands through the verification.
	fx2, _, vdir2 := verifiedFixture(t, modelAnswer(), "grep -qx merged-a a && grep -qx merged-b b && grep -qx d d", nil)
	_, err = fx2.client.UserLand(fx2.ctx, fx2.request("Land agent work"))
	require.NoError(t, err)
	require.Equal(t, 1, verifyRuns(t, vdir2))
}
