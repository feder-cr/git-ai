package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func hunk(b0, b1, o0, o1, t0, t1 uint32) *gitalypb.LandConflictHunk {
	return &gitalypb.LandConflictHunk{
		Base:   &gitalypb.LineRange{Start: b0, End: b1},
		Ours:   &gitalypb.LineRange{Start: o0, End: o1},
		Theirs: &gitalypb.LineRange{Start: t0, End: t1},
	}
}

func TestLandConflictHunks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name               string
		base, ours, theirs string
		want               []*gitalypb.LandConflictHunk
	}{
		{
			name: "one line changed on both sides",
			base: "1\n2\n3\n", ours: "1\nO\n3\n", theirs: "1\nT\n3\n",
			want: []*gitalypb.LandConflictHunk{hunk(2, 3, 2, 3, 2, 3)},
		},
		{
			// Ours adds two lines at the top, which moves the conflict two lines down in ours only.
			name: "ranges count the lines of their own side",
			base: "1\n2\n3\n4\n", ours: "a\nb\n1\n2\nO\n4\n", theirs: "1\n2\nT\nT2\n4\n",
			want: []*gitalypb.LandConflictHunk{hunk(3, 4, 5, 6, 3, 5)},
		},
		{
			name: "non-overlapping changes merge without a hunk",
			base: "1\n2\n3\n4\n5\n", ours: "O\n2\n3\n4\n5\n", theirs: "1\n2\n3\n4\nT\n",
		},
		{
			name: "the same change on both sides is no conflict",
			base: "1\n2\n", ours: "1\nX\n", theirs: "1\nX\n",
		},
		{
			name: "both insert at the same point",
			base: "1\n2\n", ours: "1\nO\n2\n", theirs: "1\nT\n2\n",
			want: []*gitalypb.LandConflictHunk{hunk(2, 2, 2, 3, 2, 3)},
		},
		{
			name: "add/add: no base",
			base: "", ours: "o1\no2\n", theirs: "t1\n",
			want: []*gitalypb.LandConflictHunk{hunk(1, 1, 1, 3, 1, 2)},
		},
		{
			name: "two conflicts",
			base: "1\n2\n3\n4\n5\n", ours: "O1\n2\n3\n4\nO5\n", theirs: "T1\n2\n3\n4\nT5\n",
			want: []*gitalypb.LandConflictHunk{hunk(1, 2, 1, 2, 1, 2), hunk(5, 6, 5, 6, 5, 6)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := landConflictHunks([]byte(tc.base), []byte(tc.ours), []byte(tc.theirs))
			require.True(t, ok)
			require.Len(t, got, len(tc.want))
			for i := range tc.want {
				require.Equal(t, tc.want[i].String(), got[i].String(), "hunk %d", i)
			}
		})
	}
}

// landConflictDetail returns the LandConflict detail of err, or nil.
func landConflictDetail(err error) *gitalypb.LandConflict {
	for _, detail := range status.Convert(err).Details() {
		if pkg, ok := detail.(*gitalypb.LandConflict); ok {
			return pkg
		}
	}
	return nil
}

// builtPackage builds the conflict package of an unresolved land, as landWithPackage does.
func builtPackage(t *testing.T, ctx context.Context, e *landUnresolvedError) *gitalypb.LandConflict {
	t.Helper()
	pkg, err := e.listing.conflictPackage(ctx, e.reason)
	require.NoError(t, err)
	return pkg
}

// writeRawAnswer sets any JSON as the answer the resolver script of dir gives.
func writeRawAnswer(t *testing.T, dir string, answer any) {
	t.Helper()
	raw, err := json.Marshal(answer)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "answer.json"), raw, 0o600))
}

func packageFile(t *testing.T, pkg *gitalypb.LandConflict, path string) *gitalypb.LandConflictFile {
	t.Helper()
	for _, f := range pkg.GetFiles() {
		if string(f.GetPath()) == path {
			return f
		}
	}
	require.FailNow(t, "no file in the package", "path %q", path)
	return nil
}

func TestUserLand_conflictPackage(t *testing.T) {
	t.Parallel()

	t.Run("a refusal gives every file its outcome", func(t *testing.T) {
		t.Parallel()

		// The resolver refuses with the outcome of each file on stdout, as graft-resolve does.
		dir, argv := resolverScript(t, `cat "$DIR/answer.json"; exit 1`)
		writeRawAnswer(t, dir, resolver.ErrorResponse{
			Error: "unresolved",
			Files: []resolver.FileOutcome{
				{Path: "a", Outcome: resolver.OutcomeResolved},
				{Path: "b", Outcome: resolver.OutcomeRejected, Reason: "syntax check: b does not parse"},
			},
		})
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg, "the merge conflict carries the package")
		require.Equal(t, fx.tip.String(), pkg.GetTip())
		require.Equal(t, fx.commit.String(), pkg.GetCommit())
		require.Equal(t, fx.base.String(), pkg.GetMergeBase())
		require.Empty(t, pkg.GetReason())

		a, b := packageFile(t, pkg, "a"), packageFile(t, pkg, "b")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, a.GetOutcome())
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, b.GetOutcome())
		require.Equal(t, "syntax check: b does not parse", b.GetReason())
		require.Equal(t, gitalypb.LandConflictFile_KIND_CONTENT, b.GetKind())
		require.NotEmpty(t, b.GetOurs().GetOid())
		require.Len(t, b.GetHunks(), 1)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("a malformed outcome list is not trusted in part", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, `cat "$DIR/answer.json"; exit 1`)
		writeRawAnswer(t, dir, resolver.ErrorResponse{
			Error: "unresolved",
			Files: []resolver.FileOutcome{{Path: "a", Outcome: resolver.OutcomeResolved}}, // b is missing
		})
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_FAILED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "unresolved", p)
		}
	})

	t.Run("a refusal that marks every file resolved never writes", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, `cat "$DIR/answer.json"; exit 1`)
		writeRawAnswer(t, dir, resolver.ErrorResponse{
			Error: "refused",
			Files: []resolver.FileOutcome{{Path: "a", Outcome: resolver.OutcomeResolved}, {Path: "b", Outcome: resolver.OutcomeResolved}},
		})
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_FAILED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "refused", p)
		}
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("a lockfile is not sent, the other file is", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, resolver.Response{
			Version:  resolver.ProtocolVersion,
			Resolver: "graft-resolve/fake",
			Files:    []resolver.ResponseFile{{Origin: resolver.OriginRule, Path: "a", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged-a\n")}},
		})
		fx := newResolveFixtureWith(t, enabledResolver(argv),
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "base\n"}, {Mode: "100644", Path: "go.sum", Content: "base\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "ours\n"}, {Mode: "100644", Path: "go.sum", Content: "ours\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "theirs\n"}, {Mode: "100644", Path: "go.sum", Content: "theirs\n"}},
		)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, packageFile(t, pkg, "a").GetOutcome())
		lock := packageFile(t, pkg, "go.sum")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_SENT, lock.GetOutcome())
		require.Equal(t, resolver.NotSentLockfile, lock.GetReason())

		req := resolverRequest(t, dir, 1)
		require.Len(t, req.Files, 1, "the lockfile never reaches the resolver")
		require.Equal(t, "a", req.Files[0].Path)
	})

	t.Run("a disabled resolver gives a package that was not attempted", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Contains(t, pkg.GetReason(), "not enabled")
		for _, f := range pkg.GetFiles() {
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, f.GetOutcome())
			// The agent gets the hunks even though no file was read for a resolver.
			require.Len(t, f.GetHunks(), 1, string(f.GetPath()))
		}
	})

	t.Run("without resolve_conflicts the package helps the caller resolve", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.ResolveConflicts = false
		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, fx.tip.String(), pkg.GetTip())
		require.Equal(t, fx.commit.String(), pkg.GetCommit())
		require.Equal(t, fx.base.String(), pkg.GetMergeBase())
		require.Equal(t, "conflict resolution was not requested", pkg.GetReason())
		require.Len(t, pkg.GetFiles(), 2)
		for _, f := range pkg.GetFiles() {
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, f.GetOutcome())
			require.Len(t, f.GetHunks(), 1)
			require.NotEmpty(t, f.GetBase().GetOid())
			require.NotEmpty(t, f.GetOurs().GetOid())
			require.NotEmpty(t, f.GetTheirs().GetOid())
		}
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})
}

// resolutionOf returns the resolution of the fixture's conflicts a and b with the given contents.
func resolutionOf(fx resolveFixture, a, b string) *gitalypb.LandResolution {
	return &gitalypb.LandResolution{
		Tip:    fx.tip.String(),
		Commit: fx.commit.String(),
		Files: []*gitalypb.LandResolvedFile{
			{Path: []byte("a"), Action: gitalypb.LandResolvedFile_ACTION_WRITE, Mode: "100644", Content: []byte(a)},
			{Path: []byte("b"), Action: gitalypb.LandResolvedFile_ACTION_WRITE, Mode: "100644", Content: []byte(b)},
		},
	}
}

func TestUserLand_reland(t *testing.T) {
	t.Parallel()

	t.Run("without a resolver the resolution lands with the server's rules only", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		resp, err := fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)
		landed := git.ObjectID(resp.GetLandedCommitId())
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, landed)
		require.Equal(t, "agent-a\n", fx.blobAt(t, landed, "a"))
		require.Equal(t, "agent-b\n", fx.blobAt(t, landed, "b"))
		require.Equal(t, "d\n", fx.blobAt(t, landed, "d"))

		commit, err := fx.repo.ReadCommit(fx.ctx, landed.Revision())
		require.NoError(t, err)
		require.Equal(t, []string{fx.tip.String(), fx.commit.String()}, commit.GetParentIds())
		body := string(commit.GetBody())
		require.Contains(t, body, "Graft-Resolved-By: agent\n")
		require.NotContains(t, body, "Graft-Checked-By:")
		require.Equal(t, "agent", resp.GetResolver())
	})

	t.Run("with a resolver the resolution is checked and the trailer says so", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeRawAnswer(t, dir, resolver.CheckResponse{
			Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: "graft-resolve",
			Files: []resolver.CheckFile{{Path: "a", OK: true, Syntax: resolver.SyntaxNone}, {Path: "b", OK: true, Syntax: resolver.SyntaxNone}},
		})
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		resp, err := fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)
		commit, err := fx.repo.ReadCommit(fx.ctx, git.ObjectID(resp.GetLandedCommitId()).Revision())
		require.NoError(t, err)
		require.Contains(t, string(commit.GetBody()), "Graft-Checked-By: graft-resolve\n")

		check := resolverRequest(t, dir, 1)
		require.Equal(t, resolver.OperationCheck, check.Operation)
		require.Len(t, check.Resolution, 2)
		require.Equal(t, "agent-a\n", string(check.Resolution[0].Content))
	})

	t.Run("actors and max_files bound AI requests only: an actor they exclude re-lands, checked", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeRawAnswer(t, dir, resolver.CheckResponse{
			Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: "graft-resolve",
			Files: []resolver.CheckFile{{Path: "a", OK: true, Syntax: resolver.SyntaxNone}, {Path: "b", OK: true, Syntax: resolver.SyntaxNone}},
		})
		rcfg := enabledResolver(argv)
		rcfg.Actors = []string{"someone-else"}
		rcfg.MaxFiles = 1
		fx := newResolveFixture(t, rcfg)
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		resp, err := fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)
		commit, err := fx.repo.ReadCommit(fx.ctx, git.ObjectID(resp.GetLandedCommitId()).Revision())
		require.NoError(t, err)
		require.Contains(t, string(commit.GetBody()), "Graft-Checked-By: graft-resolve\n")
		require.Equal(t, 1, resolverCalls(t, dir), "the check ran, on both files, whatever the actor and max_files")
		require.Len(t, resolverRequest(t, dir, 1).Resolution, 2)
	})

	t.Run("the check goes in bounded requests, one batch at a time", func(t *testing.T) {
		t.Parallel()

		// A bound of one byte: every file is larger than the bound, so each is a check request of
		// its own, answered in turn.
		dir, argv := perCallChecker(t, checkAnswer("graft-resolve", "a", true, ""), checkAnswer("graft-resolve", "b", true, ""))
		fx, s, req := batchedReland(t, argv)

		pre, err := s.landPrecheckResolution(fx.ctx, req, fx.repo)
		require.NoError(t, err)
		res, err := s.landWithResolution(fx.ctx, req, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
		require.NoError(t, err)
		require.Equal(t, 2, resolverCalls(t, dir))
		for n, p := range []string{"a", "b"} {
			check := resolverRequest(t, dir, n+1)
			require.Len(t, check.Files, 1)
			require.Equal(t, p, check.Files[0].Path)
			require.Empty(t, check.Messages, "the check carries no commit message")
		}
		require.Equal(t, 2, res.syntaxUnchecked, "the verdicts of every batch count")
		commit, err := fx.repo.ReadCommit(fx.ctx, res.commit.Revision())
		require.NoError(t, err)
		require.Contains(t, string(commit.GetBody()), "Graft-Checked-By: graft-resolve\n")
	})

	t.Run("a later batch that rejects keeps the verdict of the earlier one", func(t *testing.T) {
		t.Parallel()

		_, argv := perCallChecker(t, checkAnswer("graft-resolve", "a", true, ""), checkAnswer("graft-resolve", "b", false, "b is wrong"))
		fx, s, req := batchedReland(t, argv)
		pre, err := s.landPrecheckResolution(fx.ctx, req, fx.repo)
		require.NoError(t, err)
		_, err = s.landWithResolution(fx.ctx, req, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
		pkg := builtUnresolved(t, fx, err)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, packageFile(t, pkg, "a").GetOutcome())
		b := packageFile(t, pkg, "b")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, b.GetOutcome())
		require.Equal(t, "b is wrong", b.GetReason())
	})

	t.Run("a later batch that cannot be checked keeps the earlier verdicts and fails closed", func(t *testing.T) {
		t.Parallel()

		for name, second := range map[string]string{
			"an error":                  "",
			"another checker answering": checkAnswer("other-checker", "b", true, ""),
		} {
			_, argv := perCallChecker(t, checkAnswer("graft-resolve", "a", false, "a is wrong"), second)
			fx, s, req := batchedReland(t, argv)
			pre, err := s.landPrecheckResolution(fx.ctx, req, fx.repo)
			require.NoError(t, err, name)
			_, err = s.landWithResolution(fx.ctx, req, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
			pkg := builtUnresolved(t, fx, err)
			require.Contains(t, pkg.GetReason(), "the resolution could not be checked", name)
			a := packageFile(t, pkg, "a")
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, a.GetOutcome(), name)
			require.Equal(t, "a is wrong", a.GetReason(), name)
			b := packageFile(t, pkg, "b")
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, b.GetOutcome(), name)
			require.False(t, b.GetSyntaxUnchecked(), name)
		}
	})

	t.Run("the whole check has one deadline", func(t *testing.T) {
		t.Parallel()

		// No clock decides this test. The check is made of two batches. Every batch must run under the
		// same deadline, which is set once when the check starts and is the resolver's timeout from
		// then (so the second request has what the first left, not a timeout of its own), and a check
		// that ends in its second batch fails closed with the verdicts of the first. The test ends the
		// check from the hook of the second batch, an event; the deadline itself is read from the
		// context of each batch.
		_, argv := perCallChecker(t, checkAnswer("graft-resolve", "a", true, ""), checkAnswer("graft-resolve", "b", true, ""))
		fx, s, req := batchedReland(t, argv)
		s.resolveCfg.Timeout = "1h"
		client, err := resolver.New(s.resolveCfg)
		require.NoError(t, err)
		s.conflictResolver = client

		callerCtx, cancel := context.WithCancel(fx.ctx)
		defer cancel()
		var mu sync.Mutex
		var deadlines []time.Time
		var withoutDeadline int
		// The hook only records: a failed assertion inside it would leave the check stuck with its slot
		// instead of failing the test.
		s.landCheckBatchHook = func(ctx context.Context) {
			deadline, ok := ctx.Deadline()
			mu.Lock()
			defer mu.Unlock()
			if !ok {
				withoutDeadline++
			}
			deadlines = append(deadlines, deadline)
			if len(deadlines) == 2 {
				cancel()
			}
		}

		before := time.Now()
		pre, err := s.landPrecheckResolution(callerCtx, req, fx.repo)
		after := time.Now()
		require.NoError(t, err)
		require.Zero(t, withoutDeadline, "a batch runs under the deadline of the check")
		require.Len(t, deadlines, 2, "the check ran two batches")
		require.Equal(t, deadlines[0], deadlines[1], "one deadline for the whole check")
		require.False(t, deadlines[0].Before(before.Add(time.Hour)), "set when the check started, not before")
		require.False(t, after.Add(time.Hour).Before(deadlines[0]), "set when the check started, not later")
		_, err = s.landWithResolution(fx.ctx, req, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
		pkg := builtUnresolved(t, fx, err)
		require.Contains(t, pkg.GetReason(), "the resolution could not be checked")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, packageFile(t, pkg, "a").GetOutcome())
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, packageFile(t, pkg, "b").GetOutcome())
	})

	t.Run("without a check for its tip and commit the land fails closed, saying why", func(t *testing.T) {
		t.Parallel()

		_, argv := perCallChecker(t, checkAnswer("graft-resolve", "a", true, ""))
		fx, s, req := batchedReland(t, argv)
		for name, tc := range map[string]struct {
			precheck *landPrecheck
			want     string
		}{
			"no check":           {nil, "it was not checked before the land"},
			"a skipped check":    {&landPrecheck{skipped: "the change had already landed"}, "since the change had already landed"},
			"a check of another": {&landPrecheck{tip: fx.base, commit: fx.commit}, fmt.Sprintf("it was checked against tip %s and commit %s, not tip %s", fx.base, fx.commit, fx.tip)},
		} {
			_, err := s.landWithResolution(fx.ctx, req, fx.repo, fx.tip, fx.commit, fx.committer(), tc.precheck)
			pkg := builtUnresolved(t, fx, err)
			require.Contains(t, pkg.GetReason(), "the resolution could not be checked", name)
			require.Contains(t, pkg.GetReason(), tc.want, name)
		}
	})

	t.Run("a configured resolver that cannot be built fails closed with its cause", func(t *testing.T) {
		t.Parallel()

		_, argv := perCallChecker(t)
		fx, s, req := batchedReland(t, argv)
		s.conflictResolver, s.resolverErr = nil, errors.New("token file is gone")

		pre, err := s.landPrecheckResolution(fx.ctx, req, fx.repo)
		require.NoError(t, err)
		require.Nil(t, pre, "no resolver: no check")
		_, err = s.landWithResolution(fx.ctx, req, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
		pkg := builtUnresolved(t, fx, err)
		require.Contains(t, pkg.GetReason(), "the configured resolver cannot be built: token file is gone")

		// A land that asks the server to resolve is refused with the same cause.
		_, err = s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
		pkg = builtUnresolved(t, fx, err)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, packageFile(t, pkg, "a").GetOutcome())
		require.Contains(t, pkg.GetReason(), "conflict resolution is configured, but its resolver cannot be built: token file is gone")
	})

	t.Run("the wait for a check slot counts against the deadline of the check", func(t *testing.T) {
		t.Parallel()

		dir, argv := perCallChecker(t, checkAnswer("graft-resolve", "a", true, ""), checkAnswer("graft-resolve", "b", true, ""))
		fx, s, req := batchedReland(t, argv)
		s.resolveCfg.Timeout = "300ms"
		s.resolveCfg.CheckConcurrency = 1
		client, err := resolver.New(s.resolveCfg)
		require.NoError(t, err)
		s.conflictResolver = client

		// Another check holds the only slot for longer than the deadline.
		held, err := client.AcquireCheckSlot(fx.ctx)
		require.NoError(t, err)
		start := time.Now()
		pre, err := s.landPrecheckResolution(fx.ctx, req, fx.repo)
		require.NoError(t, err)
		require.Less(t, time.Since(start), 2*time.Second, "the wait stops at the deadline")
		held.Release()
		require.Contains(t, pre.result.failure, "waiting for the resolver")
		require.Equal(t, 0, resolverCalls(t, dir))

		_, err = s.landWithResolution(fx.ctx, req, fx.repo, fx.tip, fx.commit, fx.committer(), pre)
		pkg := builtUnresolved(t, fx, err)
		require.Contains(t, pkg.GetReason(), "the resolution could not be checked")
		require.Contains(t, pkg.GetReason(), "context deadline exceeded")
	})

	t.Run("a git error between batches keeps the earlier verdicts and fails closed", func(t *testing.T) {
		t.Parallel()

		// a and b have sides of their own. While the first batch (a) is checked, the blob of b's
		// theirs side disappears: reading the second batch fails, after a got its verdict.
		fx := newResolveFixtureWith(t, config.GraftResolver{},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "base\n"}, {Mode: "100644", Path: "b", Content: "base-b\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "ours\n"}, {Mode: "100644", Path: "b", Content: "ours-b\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "theirs\n"}, {Mode: "100644", Path: "b", Content: "theirs-b\n"}},
		)
		theirsB := text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "rev-parse", fx.commit.String()+":b"))
		loose := filepath.Join(fx.repoPath, "objects", theirsB[:2], theirsB[2:])
		require.FileExists(t, loose)

		dir, argv := resolverScript(t, fmt.Sprintf(`[ "$N" = 1 ] && rm -f '%s'; [ -s "$DIR/answer-$N.json" ] || exit 1; cp "$DIR/answer-$N.json" "$DIR/answer.json"`, loose))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "answer-1.json"), []byte(checkAnswer("graft-resolve", "a", false, "a is wrong")), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "answer-2.json"), []byte(checkAnswer("graft-resolve", "b", true, "")), 0o600))
		s := fx.server(t, enabledResolver(argv))
		s.checkBatchBytes = 1
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		pre, err := s.landPrecheckResolution(fx.ctx, req, fx.repo)
		require.NoError(t, err)
		require.Equal(t, 1, resolverCalls(t, dir), "the second batch was never sent")
		require.Contains(t, pre.result.failure, "checking: ")
		require.Contains(t, pre.result.failure, `of "b": invalid object`)
		require.Equal(t, "a is wrong", pre.result.verdicts["a"].rejected, "the first batch's verdict is kept")
		_, hasB := pre.result.verdicts["b"]
		require.False(t, hasB)
	})

	t.Run("the check of a moved tip is skipped: the lane refuses it at once", func(t *testing.T) {
		t.Parallel()

		dir, argv := perCallChecker(t)
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
		req.Resolution.Tip = fx.base.String()

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Contains(t, pkg.GetReason(), "tip moved")
		require.Equal(t, 0, resolverCalls(t, dir), "nothing was checked")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("the check of a change that already landed is skipped: the lane answers it at once", func(t *testing.T) {
		t.Parallel()

		dir, argv := perCallChecker(t)
		fx := newResolveFixture(t, enabledResolver(argv))
		landed := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip, fx.commit),
			gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(
				gittest.TreeEntry{Mode: "100644", Path: "a", Content: "merged\n"},
				gittest.TreeEntry{Mode: "100644", Path: "b", Content: "merged\n"},
			))
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
		req.Resolution.Tip = landed.String()

		resp, err := fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)
		require.True(t, resp.GetAlreadyLanded())
		require.Equal(t, 0, resolverCalls(t, dir), "nothing was checked")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, landed)
	})

	t.Run("the check leaves no object in the repository", func(t *testing.T) {
		t.Parallel()

		// The check merges the tip and the change, which writes trees and blobs; it runs in a
		// quarantine that is discarded, so a land that the check refuses leaves the objects of the
		// repository as they were.
		dir, argv := resolverScript(t, "")
		writeRawAnswer(t, dir, resolver.CheckResponse{
			Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: "graft-resolve",
			Files: []resolver.CheckFile{{Path: "a", OK: false, Reason: "a is wrong", Syntax: resolver.SyntaxNone}, {Path: "b", OK: true, Syntax: resolver.SyntaxNone}},
		})
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
		before := gittest.ListObjects(t, fx.cfg, fx.repoPath)

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, packageFile(t, pkg, "a").GetOutcome())
		require.Equal(t, 1, resolverCalls(t, dir))
		require.ElementsMatch(t, before, gittest.ListObjects(t, fx.cfg, fx.repoPath))
	})

	t.Run("the check runs outside the lane: another land of the branch is not held", func(t *testing.T) {
		t.Parallel()

		// The checker takes 3 s; meanwhile a fast-forward of the branch lands at once.
		dir, argv := resolverScript(t, "sleep 3")
		writeRawAnswer(t, dir, resolver.CheckResponse{
			Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: "graft-resolve",
			Files: []resolver.CheckFile{{Path: "a", OK: true, Syntax: resolver.SyntaxNone}, {Path: "b", OK: true, Syntax: resolver.SyntaxNone}},
		})
		fx := newResolveFixture(t, enabledResolver(argv))
		forward := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.tip), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "ours\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "ours\n"},
			gittest.TreeEntry{Mode: "100644", Path: "c", Content: "c\n"},
			gittest.TreeEntry{Mode: "100644", Path: "e", Content: "e\n"},
		))
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		var wg sync.WaitGroup
		var relandDone, forwardDone time.Time
		var relandErr, forwardErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, relandErr = fx.client.UserLand(fx.ctx, req)
			relandDone = time.Now()
		}()
		go func() {
			defer wg.Done()
			time.Sleep(300 * time.Millisecond)
			_, forwardErr = fx.client.UserLand(fx.ctx, landRequest(fx.repoProto, git.DefaultBranch, forward))
			forwardDone = time.Now()
		}()
		wg.Wait()
		require.NoError(t, forwardErr)
		require.True(t, forwardDone.Before(relandDone), "the fast-forward landed while the check ran")
		// The branch moved, so the resolution made for the old tip is refused with a fresh package.
		requireMergeConflict(t, relandErr)
		require.Contains(t, landConflictDetail(relandErr).GetReason(), "the branch tip moved")
	})

	t.Run("a merge with more conflicting files than a package lists is refused", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "exit 1")
		var base, tip, commit []gittest.TreeEntry
		for i := 0; i <= landMaxPackageFiles; i++ {
			p := fmt.Sprintf("f%04d", i)
			base = append(base, gittest.TreeEntry{Mode: "100644", Path: p, Content: "base\n"})
			tip = append(tip, gittest.TreeEntry{Mode: "100644", Path: p, Content: "ours\n"})
			commit = append(commit, gittest.TreeEntry{Mode: "100644", Path: p, Content: "theirs\n"})
		}
		fx := newResolveFixtureWith(t, enabledResolver(argv), base, tip, commit)
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = &gitalypb.LandResolution{Tip: fx.tip.String(), Commit: fx.commit.String(), Files: []*gitalypb.LandResolvedFile{
			{Path: []byte("f0000"), Action: gitalypb.LandResolvedFile_ACTION_DELETE},
		}}

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Empty(t, pkg.GetFiles())
		require.Contains(t, pkg.GetReason(), fmt.Sprintf("more than the %d that a conflict package lists", landMaxPackageFiles))
		require.Equal(t, 0, resolverCalls(t, dir), "nothing is read or checked")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("a written mode that no side has is rejected", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
		req.Resolution.Files[0].Mode = "100755"

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		a := packageFile(t, pkg, "a")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, a.GetOutcome())
		require.Contains(t, a.GetReason(), "mode 100755, which is none of the modes of the sides (100644)")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("a failed check rejects the file", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeRawAnswer(t, dir, resolver.CheckResponse{
			Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: "graft-resolve",
			Files: []resolver.CheckFile{{Path: "a", OK: true, Syntax: resolver.SyntaxUnchecked}, {Path: "b", OK: false, Reason: "check failed: tests", Syntax: resolver.SyntaxNone}},
		})
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		a := packageFile(t, pkg, "a")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, a.GetOutcome())
		require.True(t, a.GetSyntaxUnchecked(), "a passed without a syntax verdict, and the package says so")
		b := packageFile(t, pkg, "b")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, b.GetOutcome())
		require.Equal(t, "check failed: tests", b.GetReason())
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("a resolver that does not know the check fails closed", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer()) // an old resolver answers a resolve response
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Contains(t, pkg.GetReason(), "could not be checked")
		require.Contains(t, pkg.GetReason(), "does not support the check")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("a pointer line to a binary side is rejected", func(t *testing.T) {
		t.Parallel()

		// An SDK client gets the rule of graft land --resolved: the server refuses the pointer
		// line that stands for a binary side in the file to edit.
		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "x\n"+resolver.ConflictPointerPrefix+"the binary ours side is sides/ours/b\n")

		_, err := fx.client.UserLand(fx.ctx, req)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		b := packageFile(t, pkg, "b")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, b.GetOutcome())
		require.Contains(t, b.GetReason(), "the pointer to a binary side")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("several files go in one check request within its bound", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeRawAnswer(t, dir, resolver.CheckResponse{
			Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: "graft-resolve",
			Files: []resolver.CheckFile{
				{Path: "a", OK: true, Syntax: resolver.SyntaxNone},
				{Path: "b", OK: true, Syntax: resolver.SyntaxNone},
				{Path: "e", OK: true, Syntax: resolver.SyntaxNone},
			},
		})
		fx := newResolveFixtureWith(t, enabledResolver(argv),
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "base\n"}, {Mode: "100644", Path: "b", Content: "base-b\n"}, {Mode: "100644", Path: "e", Content: "base-e\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "ours\n"}, {Mode: "100644", Path: "b", Content: "ours-b\n"}, {Mode: "100644", Path: "e", Content: "ours-e\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a", Content: "theirs\n"}, {Mode: "100644", Path: "b", Content: "theirs-b\n"}, {Mode: "100644", Path: "e", Content: "theirs-e\n"}},
		)
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
		req.Resolution.Files = append(req.Resolution.Files, &gitalypb.LandResolvedFile{
			Path: []byte("e"), Action: gitalypb.LandResolvedFile_ACTION_WRITE, Mode: "100644", Content: []byte("agent-e\n"),
		})

		resp, err := fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)
		require.ElementsMatch(t, [][]byte{[]byte("a"), []byte("b"), []byte("e")}, resp.GetResolvedPaths())
		require.Equal(t, 1, resolverCalls(t, dir), "one request for the three files")
		check := resolverRequest(t, dir, 1)
		require.Len(t, check.Files, 3)
		require.Len(t, check.Resolution, 3)
		for i, p := range []string{"a", "b", "e"} {
			require.Equal(t, p, check.Files[i].Path)
			require.Equal(t, p, check.Resolution[i].Path)
			require.Equal(t, "agent-"+p+"\n", string(check.Resolution[i].Content))
		}
	})

	t.Run("conflict markers are rejected", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "<<<<<<< ours\nx\n=======\ny\n>>>>>>> theirs\n")

		_, err := fx.client.UserLand(fx.ctx, req)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		b := packageFile(t, pkg, "b")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, b.GetOutcome())
		require.Contains(t, b.GetReason(), "conflict marker")
	})

	t.Run("a moved tip gives a fresh package", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
		req.Resolution.Tip = fx.base.String() // made against another tip

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, fx.tip.String(), pkg.GetTip())
		require.Contains(t, pkg.GetReason(), "tip moved")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("the resolution must resolve exactly the conflicting paths", func(t *testing.T) {
		t.Parallel()

		for name, mutate := range map[string]func(*gitalypb.LandResolution){
			"a path missing":         func(r *gitalypb.LandResolution) { r.Files = r.Files[:1] },
			"a path twice":           func(r *gitalypb.LandResolution) { r.Files[1].Path = []byte("a") },
			"a path not in conflict": func(r *gitalypb.LandResolution) { r.Files[1].Path = []byte("c") },
		} {
			fx := newResolveFixture(t, config.GraftResolver{})
			req := fx.request("Land")
			req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
			mutate(req.Resolution)

			_, err := fx.client.UserLand(fx.ctx, req)
			pkg := landConflictDetail(err)
			require.NotNil(t, pkg, name)
			require.True(t, strings.HasPrefix(pkg.GetReason(), "the resolution "), "%s: reason %q", name, pkg.GetReason())
		}
	})

	t.Run("a resolution for another commit is an invalid argument", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
		req.Resolution.Commit = fx.tip.String()

		_, err := fx.client.UserLand(fx.ctx, req)
		require.Contains(t, err.Error(), "resolution is for commit")
	})

	t.Run("a conflicting path with a line break is refused before anything is written", func(t *testing.T) {
		t.Parallel()

		fx := newLineBreakPathFixture(t, config.GraftResolver{})
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = &gitalypb.LandResolution{
			Tip:    fx.tip.String(),
			Commit: fx.commit.String(),
			Files: []*gitalypb.LandResolvedFile{
				{Path: []byte(lineBreakPath), Action: gitalypb.LandResolvedFile_ACTION_WRITE, Mode: "100644", Content: []byte("agent\n")},
			},
		}

		_, err := fx.client.UserLand(fx.ctx, req)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		require.Contains(t, err.Error(), "Graft-Resolved-Path trailer")
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})
}

// lineBreakPath is a path that would inject a trailer if it were written as one.
const lineBreakPath = "a\nGraft-Resolved-By: evil"

// newLineBreakPathFixture is a fixture whose only conflict is on lineBreakPath.
func newLineBreakPathFixture(t *testing.T, rcfg config.GraftResolver) resolveFixture {
	return newResolveFixtureWith(t, rcfg,
		[]gittest.TreeEntry{{Mode: "100644", Path: lineBreakPath, Content: "base\n"}},
		[]gittest.TreeEntry{{Mode: "100644", Path: lineBreakPath, Content: "ours\n"}},
		[]gittest.TreeEntry{{Mode: "100644", Path: lineBreakPath, Content: "theirs\n"}},
	)
}

func TestUserLand_lineBreakPathIsNeverSent(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	fx := newLineBreakPathFixture(t, enabledResolver(argv))

	_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	requireMergeConflict(t, err)
	pkg := landConflictDetail(err)
	require.NotNil(t, pkg)
	f := packageFile(t, pkg, lineBreakPath)
	require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_SENT, f.GetOutcome())
	require.Equal(t, landNotSentTrailerUnsafe, f.GetReason())
	require.Equal(t, 0, resolverCalls(t, dir))
	requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
}

// seq returns the lines 1 to n, with the given lines replaced.
func seq(n int, replace map[int]string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if r, ok := replace[i]; ok {
			b.WriteString(r + "\n")
			continue
		}
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}

// requirePackageOIDsKept checks that every object ID of the package is in the repository after the
// land failed, so outside the quarantine of the request, and that a side without one says why.
func requirePackageOIDsKept(t *testing.T, fx resolveFixture, pkg *gitalypb.LandConflict) {
	t.Helper()
	for _, f := range pkg.GetFiles() {
		for _, s := range []*gitalypb.LandConflictSide{f.GetBase(), f.GetOurs(), f.GetTheirs()} {
			if s == nil {
				continue
			}
			if s.GetOid() == "" {
				require.Equal(t, landSideUnavailable, s.GetUnavailable(), string(f.GetPath()))
				continue
			}
			require.Empty(t, s.GetUnavailable())
			gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "cat-file", "-e", s.GetOid())
		}
	}
}

func TestUserLand_conflictPackageSidesAreKept(t *testing.T) {
	t.Parallel()

	t.Run("a rename whose content merge is a side of its own", func(t *testing.T) {
		t.Parallel()

		// The tip renames a.txt to c.txt and changes line 19; the change changes line 2 of a.txt
		// and adds a c.txt of its own. merge-tree merges the rename (the base, the tip's c.txt
		// and the change's a.txt) into a blob it writes for the conflict, the tip's side of the
		// add/add conflict on c.txt. That blob is in no tree, and the failed land discards it.
		fx := newResolveFixtureWith(t, config.GraftResolver{},
			[]gittest.TreeEntry{{Mode: "100644", Path: "a.txt", Content: seq(20, nil)}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "c.txt", Content: seq(20, map[int]string{19: "nineteen-ours"})}},
			[]gittest.TreeEntry{
				{Mode: "100644", Path: "a.txt", Content: seq(20, map[int]string{2: "two-theirs"})},
				{Mode: "100644", Path: "c.txt", Content: "added by theirs\n"},
			},
		)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		c := packageFile(t, pkg, "c.txt")
		require.Empty(t, c.GetOurs().GetOid(), "the merged rename is not kept")
		require.Equal(t, landSideUnavailable, c.GetOurs().GetUnavailable())
		require.Equal(t, "100644", c.GetOurs().GetMode())
		require.NotEmpty(t, c.GetTheirs().GetOid())
		// No hunks: they would give line ranges of a side the agent cannot read.
		require.Empty(t, c.GetHunks())
		require.Equal(t, landHunksUnavailable, c.GetHunksOmitted())
		requirePackageOIDsKept(t, fx, pkg)
	})

	t.Run("a submodule side is not a blob", func(t *testing.T) {
		t.Parallel()

		// The tip turns s into a submodule, the change edits it: the tip's side is a gitlink,
		// which names a commit of another repository.
		fx := newResolveFixture(t, config.GraftResolver{})
		submodule := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "x", Content: "x\n"}))
		baseTree := []gittest.TreeEntry{{Mode: "100644", Path: "s", Content: "base\n"}}
		base := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithTreeEntries(baseTree...))
		tip := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(base), gittest.WithBranch(git.DefaultBranch),
			gittest.WithTreeEntries(gittest.TreeEntry{Mode: "160000", Path: "s", OID: submodule}))
		commit := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(base),
			gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "s", Content: "theirs\n"}))
		fx.base, fx.tip, fx.commit = base, tip, commit

		req := fx.request("Land")
		req.Strategy = gitalypb.UserLandRequest_STRATEGY_MERGE
		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		// git records the distinct types on two paths: s holds the submodule, s~<label> the file.
		s := packageFile(t, pkg, "s")
		require.Equal(t, "160000", s.GetOurs().GetMode())
		require.Empty(t, s.GetOurs().GetOid())
		require.Equal(t, landSideGitlink, s.GetOurs().GetUnavailable())
		require.Nil(t, s.GetTheirs())
		var file *gitalypb.LandConflictFile
		for _, f := range pkg.GetFiles() {
			if strings.HasPrefix(string(f.GetPath()), "s~") {
				file = f
			}
		}
		require.NotNil(t, file, "the file side under its new name")
		require.NotEmpty(t, file.GetBase().GetOid(), "a blob found under another name in a kept tree is kept")
		require.NotEmpty(t, file.GetTheirs().GetOid())
	})

	t.Run("a criss-cross history", func(t *testing.T) {
		t.Parallel()

		// A resolver is enabled, so the request gets as far as the merge bases; it is never called.
		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		// Two branches merge each other: tip and commit have two merge bases, x and y. They agree
		// on a, which tip and commit change differently, so the base side of a is x's (and y's)
		// blob, a tree the repository keeps; they differ on b, which tip and commit both keep as
		// "xy", so b does not conflict.
		x := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "mid\n"}, gittest.TreeEntry{Mode: "100644", Path: "b", Content: "x\n"}))
		y := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "mid\n"}, gittest.TreeEntry{Mode: "100644", Path: "b", Content: "y\n"}))
		tip := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(x, y), gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "tip\n"}, gittest.TreeEntry{Mode: "100644", Path: "b", Content: "xy\n"}))
		commit := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(y, x), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "commit\n"}, gittest.TreeEntry{Mode: "100644", Path: "b", Content: "xy\n"}))
		fx.tip, fx.commit = tip, commit

		req := fx.request("Land")
		req.Strategy = gitalypb.UserLandRequest_STRATEGY_MERGE
		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Contains(t, pkg.GetReason(), "2 merge bases")
		require.Len(t, pkg.GetFiles(), 1)
		a := packageFile(t, pkg, "a")
		require.NotEmpty(t, a.GetBase().GetOid(), "a base side in the trees of the merge bases is kept")
		requirePackageOIDsKept(t, fx, pkg)
		require.Equal(t, 0, resolverCalls(t, dir))
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, tip)
	})

	t.Run("a criss-cross history whose base is virtual", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		// The merge bases x and y disagree on a: merge-tree merges them into a virtual base of its
		// own, a blob in no tree, which the failed land discards.
		x := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "x\n"}, gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"}))
		y := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.base), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "y\n"}, gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"}))
		tip := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(x, y), gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "tip\n"}, gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"}))
		commit := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(y, x), gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "commit\n"}, gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"}))
		fx.tip, fx.commit = tip, commit

		req := fx.request("Land")
		req.Strategy = gitalypb.UserLandRequest_STRATEGY_MERGE
		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		a := packageFile(t, pkg, "a")
		require.Empty(t, a.GetBase().GetOid(), "the virtual base is not kept")
		require.Equal(t, landSideUnavailable, a.GetBase().GetUnavailable())
		require.Empty(t, a.GetHunks())
		requirePackageOIDsKept(t, fx, pkg)
	})

	t.Run("an unclean merge without a conflicting file", func(t *testing.T) {
		t.Parallel()

		// The tip moves d/a to x/a and d/b to y/b; the change adds d/new. Git cannot tell where
		// the directory d went (a directory rename split): the merge is unclean, with no stage
		// entry, and only the merge's message describes it.
		fx := newResolveFixtureWith(t, config.GraftResolver{},
			[]gittest.TreeEntry{{Mode: "100644", Path: "d/a", Content: "1\n"}, {Mode: "100644", Path: "d/b", Content: "2\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "x/a", Content: "1\n"}, {Mode: "100644", Path: "y/b", Content: "2\n"}},
			[]gittest.TreeEntry{{Mode: "100644", Path: "d/a", Content: "1\n"}, {Mode: "100644", Path: "d/b", Content: "2\n"}, {Mode: "100644", Path: "d/new", Content: "new\n"}},
		)
		req := fx.request("Land")
		req.Strategy = gitalypb.UserLandRequest_STRATEGY_MERGE
		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Empty(t, pkg.GetFiles())
		require.Equal(t, landNoConflictingFiles, pkg.GetReason())
		var split bool
		for _, m := range pkg.GetMessages() {
			if m.GetType() == "CONFLICT(directory rename unclear split)" {
				split = true
				require.Equal(t, [][]byte{[]byte("d")}, m.GetPaths())
				require.Contains(t, m.GetMessage(), "CONFLICT (directory rename split): Unclear where to rename d to")
			}
		}
		require.True(t, split, "messages %v", pkg.GetMessages())
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})
}

// checkAnswer is the check response of a checker named checker for one file; an empty answer
// stands for a call that fails.
func checkAnswer(checker, path string, ok bool, reason string) string {
	raw, _ := json.Marshal(resolver.CheckResponse{
		Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: checker,
		Files: []resolver.CheckFile{{Path: path, OK: ok, Reason: reason, Syntax: resolver.SyntaxUnchecked}},
	})
	return string(raw)
}

// perCallChecker is a resolver script that gives answers[N-1] to its N-th call, and fails a call
// whose answer is empty.
func perCallChecker(t *testing.T, answers ...string) (string, []string) {
	t.Helper()
	dir, argv := resolverScript(t, `[ -s "$DIR/answer-$N.json" ] || exit 1; cp "$DIR/answer-$N.json" "$DIR/answer.json"`)
	for n, a := range answers {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("answer-%d.json", n+1)), []byte(a), 0o600))
	}
	return dir, argv
}

// batchedReland is the fixture's re-land of a and b on a server that checks every file in a
// request of its own.
func batchedReland(t *testing.T, argv []string) (resolveFixture, *Server, *gitalypb.UserLandRequest) {
	t.Helper()
	fx := newResolveFixture(t, config.GraftResolver{})
	s := fx.server(t, enabledResolver(argv))
	s.checkBatchBytes = 1
	req := fx.request("Land")
	req.ResolveConflicts = false
	req.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")
	return fx, s, req
}

// builtUnresolved returns the package of an unresolved land.
func builtUnresolved(t *testing.T, fx resolveFixture, err error) *gitalypb.LandConflict {
	t.Helper()
	var unresolved *landUnresolvedError
	require.ErrorAs(t, err, &unresolved)
	return builtPackage(t, fx.ctx, unresolved)
}
