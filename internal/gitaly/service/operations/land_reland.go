package operations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// landResolutionSyntaxUncheckedTotal counts the files of agents' resolutions that landed without a
// syntax verdict: the resolver's grammar cannot read a side of them.
var landResolutionSyntaxUncheckedTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_land_resolution_syntax_unchecked_total",
	Help: "Files of agents' resolutions that landed without a syntax verdict, because a side does not parse with the resolver's grammar.",
})

// landAgentResolver is the resolver name of an agent's resolution in the Graft-Resolved-By trailer.
func landAgentResolver(actor string) string {
	if actor == "" {
		return "agent"
	}
	return "agent/" + actor
}

// landReview is an agent's resolution held against the merge it resolves, as landReviewResolution
// leaves it: the conflicting files, what resolves each, and whether a rule of an answer rejected a
// file (its outcome says which).
type landReview struct {
	listing  landListing
	resolved map[string]resolver.ResponseFile
	actor    string
	rejected bool
}

// landReviewResolution holds the resolution of the request against the merge of tip and commitID:
// it is accepted only when it was made against this tip and this commit and resolves exactly the
// paths that conflict, each once, and every file gets the acceptance rules of a resolver answer.
// A refusal is the error (a *landUnresolvedError with its package, or a gRPC error); a file that
// breaks a rule of an answer is REJECTED in the review.
func (s *Server) landReviewResolution(ctx context.Context, in *gitalypb.UserLandRequest, repo *localrepo.Repo, tip, commitID git.ObjectID) (landReview, error) {
	r := in.GetResolution()
	if r.GetCommit() != commitID.String() {
		return landReview{}, structerr.NewInvalidArgument("resolution is for commit %q, not commit_id %q", r.GetCommit(), commitID)
	}

	l, err := landListConflicts(ctx, repo, tip, commitID, s.resolveCfg.MaxFileBytesOrDefault())
	if errors.Is(err, errLandNoConflicts) {
		return landReview{}, structerr.NewFailedPrecondition("the merge of tip %s and commit %s has no conflicts, so there is nothing to resolve: land without a resolution", tip, commitID)
	}
	if err != nil {
		return landReview{}, err
	}
	if len(l.files) == 0 {
		return landReview{}, l.notAttempted(landNoConflictingFiles, nil)
	}
	// A merge with more conflicting files than a package lists cannot be resolved through a land,
	// whatever the resolution: its package says so, and the land refuses it before reading anything.
	if len(l.files) > landMaxPackageFiles {
		return landReview{}, l.notAttempted(fmt.Sprintf("the merge has %d conflicting files, more than the %d that a conflict package lists: it cannot be resolved through a land", len(l.files), landMaxPackageFiles), nil)
	}
	if r.GetTip() != tip.String() {
		return landReview{}, l.notAttempted(fmt.Sprintf("the branch tip moved from %s to %s since the resolution was made: resolve the conflicts of this package", r.GetTip(), tip), nil)
	}
	if l.bases > 1 {
		return landReview{}, l.notAttempted(fmt.Sprintf("the change has %d merge bases (a criss-cross history), which are not resolved", l.bases), nil)
	}

	// The merge commit names the actor and every resolved path in its trailers: a value that cannot
	// be one trailer line is refused before anything else, and resolving the path again cannot help.
	actor := identity.Actor(ctx, "")
	if !landTrailerValueValid(landAgentResolver(actor)) {
		return landReview{}, structerr.NewFailedPrecondition("the actor %q cannot be recorded in the %s trailer: it must be one line of printable UTF-8", actor, landTrailerResolvedBy)
	}
	for _, f := range l.files {
		if !landTrailerValueValid(f.conflict.path) {
			return landReview{}, structerr.NewFailedPrecondition("the conflicting path %q cannot be recorded in the %s trailer: it must be one line of printable UTF-8, so its conflict cannot be resolved through a land", f.conflict.path, landTrailerResolvedPath)
		}
	}

	resolved, reason := landResolutionFiles(r, l.files)
	if reason != "" {
		return landReview{}, l.notAttempted(reason, nil)
	}

	review := landReview{listing: l, resolved: resolved, actor: actor}
	maxBytes := s.resolveCfg.MaxFileBytesOrDefault()
	for _, f := range l.files {
		rf := resolved[f.conflict.path]
		err := resolver.ValidateFile(rf, maxBytes)
		if errors.Is(err, resolver.ErrConflictPointer) {
			// A pointer line is the agent's own when a side has one too: read the sides only then.
			err = landValidateWithSides(ctx, repo, f.conflict, rf, maxBytes, err)
		}
		if err == nil {
			err = landModeOfSides(f.conflict, rf)
		}
		if err != nil {
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, err.Error()
			review.rejected = true
		}
	}
	return review, nil
}

// landValidateWithSides validates rf again with the sides of its conflict, which exempt a pointer
// line when one of them has one (resolver.ValidateFileWithSides). A side over maxBytes, or a
// gitlink, cannot be one of the agent's files and is not read; a side that cannot be read keeps
// the rejection, with why.
func landValidateWithSides(ctx context.Context, repo *localrepo.Repo, c landFileConflict, rf resolver.ResponseFile, maxBytes int64, rejection error) error {
	var sides [][]byte
	for _, b := range []*landBlob{c.base, c.ours, c.theirs} {
		if b == nil || b.mode == landModeGitlink {
			continue
		}
		info, err := repo.ReadObjectInfo(ctx, b.oid.Revision())
		if err != nil {
			return fmt.Errorf("%w (reading its sides for a pointer line of their own: %v)", rejection, err)
		}
		if info.Size > maxBytes {
			continue
		}
		content, err := repo.ReadObject(ctx, b.oid)
		if err != nil {
			return fmt.Errorf("%w (reading its sides for a pointer line of their own: %v)", rejection, err)
		}
		sides = append(sides, content)
	}
	return resolver.ValidateFileWithSides(rf, maxBytes, sides...)
}

// landPrecheck is the check of an agent's resolution, run before the land enters its lane
// (landPrecheckResolution): for the tip and the commit it was run against, the result; or, when
// no check was run because the lane refuses or answers the land without one, why (skipped).
type landPrecheck struct {
	tip, commit git.ObjectID
	result      landCheckResult
	skipped     string
	// verified is the tree that verify.command passed on, out of the lane (15.9), when a
	// verification is configured and the resolution passed the checks; verifyErr is why it was not
	// verified: a *landVerifyError (the command failed, the checkout was refused) or a
	// plain error (the deadline). The lane only compares the tree it would write with
	// verified.
	verified  git.ObjectID
	verifyErr error
}

// landPrecheckResolution runs the resolver's checks of the request's resolution before the land
// enters its lane, so that no branch is held while an external process runs: the lane only applies
// the verdicts (landWithResolution). The check is run against the tip the resolution was made
// against, which is the only tip the land accepts it at; if the branch has moved by the time the
// lane runs the land, the land refuses the resolution with a fresh package, as it would anyway.
// The whole check has one deadline, the resolver's timeout: every check request uses what remains
// of it, and its expiry fails the check closed.
//
// The check runs in the quarantine repo, which the caller discards: the merge it computes leaves no
// object behind. A resolution for a tip the branch has left, or for a change that already landed,
// is not checked (skipped): the lane refuses or answers it. It returns nil when there is neither a
// resolver nor a verification (verify.enabled) to run, and when the configured resolver cannot be
// built (the lane refuses the resolution with that reason before anything is verified). A failure to
// check is the result's failure, with its cause, which the lane reports. The whole precheck has one
// deadline: the resolver's timeout, plus the verification's when verify.enabled is set.
func (s *Server) landPrecheckResolution(ctx context.Context, in *gitalypb.UserLandRequest, repo *localrepo.Repo) (*landPrecheck, error) {
	if s.conflictResolver == nil && (s.resolverErr != nil || !s.resolveCfg.Verify.Enabled) {
		return nil, nil
	}
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, structerr.NewInternal("detecting object hash: %w", err)
	}
	tip, err := objectHash.FromHex(in.GetResolution().GetTip())
	if err != nil {
		return &landPrecheck{skipped: fmt.Sprintf("its tip %q is no object ID", in.GetResolution().GetTip())}, nil
	}
	commitID, err := repo.ResolveRevision(ctx, git.Revision(in.GetCommitId()+"^{commit}"))
	if err != nil {
		return &landPrecheck{skipped: fmt.Sprintf("commit_id %q does not resolve to a commit", in.GetCommitId())}, nil
	}

	// An agent that may not update the branch gets nothing checked or verified: the check and the
	// verification run the operator's code, and the policy decides on the actor and the reference
	// alone, so it comes first, as in landResolveAt. Otherwise a denied agent could hold the
	// repository's verify slot, and its tree would run in the operator's sandbox.
	reference := git.NewReferenceNameFromBranchName(string(in.GetBranch()))
	if err := hook.CheckAgentPolicy(ctx, s.graft, fmt.Sprintf("%s %s %s\n", tip, commitID, reference)); err != nil {
		return nil, err
	}

	// A resolution for a tip the branch has left, or for a change that already landed, needs no
	// check: the lane refuses or answers it at once, as it reads the tip again under the lane.
	// The branch is read as the lane reads it (landingAttempt.Tip): by its exact name, and a
	// symbolic reference resolves to its target's commit.
	branchTip, err := s.landBranchTip(ctx, repo, git.NewReferenceNameFromBranchName(string(in.GetBranch())))
	switch {
	case err != nil:
		return &landPrecheck{tip: tip, commit: commitID, result: landCheckResult{failure: fmt.Sprintf("reading the branch: %v", err), failureErr: err}}, nil
	case branchTip == "":
		return &landPrecheck{skipped: fmt.Sprintf("branch %q does not exist", string(in.GetBranch()))}, nil
	case branchTip != tip:
		return &landPrecheck{skipped: fmt.Sprintf("the branch was at %s, not at the tip of the resolution", branchTip)}, nil
	}
	if landed, err := repo.IsAncestor(ctx, commitID.Revision(), tip.Revision()); err != nil {
		return &landPrecheck{tip: tip, commit: commitID, result: landCheckResult{failure: fmt.Sprintf("checking whether the change already landed: %v", err), failureErr: err}}, nil
	} else if landed {
		return &landPrecheck{skipped: "the change had already landed"}, nil
	}

	review, err := s.landReviewResolution(ctx, in, repo, tip, commitID)
	if err != nil || review.rejected {
		// A refusal of the resolution (its package, or an invalid argument or failed precondition)
		// is repeated by the lane before any check. Any other error is a failure to check it.
		var unresolved *landUnresolvedError
		code := status.Code(err)
		if err == nil || errors.As(err, &unresolved) || code == codes.InvalidArgument || code == codes.FailedPrecondition {
			return &landPrecheck{skipped: "the land refuses the resolution before checking it"}, nil
		}
		return &landPrecheck{tip: tip, commit: commitID, result: landCheckResult{failure: fmt.Sprintf("listing the conflicts: %v", err), failureErr: err}}, nil
	}

	timeout, err := s.resolveCfg.TimeoutOrDefault()
	if err != nil {
		return nil, structerr.NewInternal("resolver timeout: %w", err)
	}
	// One budget for the whole precheck: the check takes the resolver's timeout at most, and the
	// verification what the budget has left, so that the deadline is the one the docs say.
	budget := timeout
	if s.resolveCfg.Verify.Enabled {
		verifyTimeout, err := s.resolveCfg.Verify.TimeoutOrDefault()
		if err != nil {
			return nil, structerr.NewInternal("verify timeout: %w", err)
		}
		budget += verifyTimeout
	}
	budgetCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var result landCheckResult
	if s.conflictResolver != nil {
		checkCtx, cancelCheck := context.WithTimeout(budgetCtx, timeout)
		defer cancelCheck()
		result = s.landCheckResolution(checkCtx, in, repo, review.listing, review.actor, review.resolved)
	}
	pc := &landPrecheck{tip: tip, commit: commitID, result: result}
	if s.resolveCfg.Verify.Enabled && result.failure == "" && !landCheckRejects(result) {
		if err := s.landPrecheckVerify(ctx, budgetCtx, budget, in, repo, review, pc); err != nil {
			return nil, err
		}
	}
	return pc, nil
}

// landCheckRejects reports whether the check rejected a file of the resolution: the lane then
// refuses it, and there is nothing to verify.
func landCheckRejects(r landCheckResult) bool {
	for _, v := range r.verdicts {
		if v.rejected != "" {
			return true
		}
	}
	return false
}

// landAgentResponse is the agent's resolution as a resolver answer, the files in the order of the
// listing: what landWriteResolution writes.
func landAgentResponse(review landReview) *resolver.Response {
	resp := &resolver.Response{Version: resolver.ProtocolVersion, Resolver: landAgentResolver(review.actor)}
	for _, f := range review.listing.files {
		resp.Files = append(resp.Files, review.resolved[f.conflict.path])
	}
	return resp
}

// landPrecheckVerify verifies the tree of the agent's resolution out of the lane (15.9), with a slot
// of the verify pool, before the land enters its lane, and records the outcome in pc. caller is the
// request's context and budgetCtx the one of the precheck's budget (the resolver's timeout plus the
// verification's); a verification that the budget cut is recorded as unfinished, and one that the
// caller cut is the caller's cancellation. The resolution was made against the tip the branch is at,
// which the lane insists on, so the tree it would write there is the tree verified here; the branch
// is read again once the slot is taken, and a tree of a tip the branch has left is not verified.
func (s *Server) landPrecheckVerify(caller, budgetCtx context.Context, budget time.Duration, in *gitalypb.UserLandRequest, repo *localrepo.Repo, review landReview, pc *landPrecheck) error {
	tree, err := s.landResolvedTree(budgetCtx, repo, review.listing.mergedTree, landAgentResponse(review))
	if err != nil {
		if caller.Err() != nil {
			return landCallerGaveUp(caller, err)
		}
		if budgetCtx.Err() != nil {
			pc.verifyErr = fmt.Errorf("could not verify before the deadline of %s", budget)
			return nil
		}
		return structerr.NewInternal("building the tree to verify: %w", err)
	}
	current := func(ctx context.Context) (bool, error) {
		// A missing branch reads as the empty ID, which is never the tip.
		now, err := s.landBranchTip(ctx, repo, git.NewReferenceNameFromBranchName(string(in.GetBranch())))
		return now == pc.tip, err
	}
	var verifyErr *landVerifyError
	var moved *landTipMovedError
	switch err := s.landVerifyOutOfLane(budgetCtx, in.GetRepository(), repo, tree, current); {
	case err == nil:
		pc.verified = tree
	case errors.As(err, &verifyErr):
		pc.verifyErr = err
	case errors.As(err, &moved):
		pc.verifyErr = err
	default:
		var unfinished *landVerifyUnfinishedError
		if errors.As(err, &unfinished) && caller.Err() == nil {
			pc.verifyErr = fmt.Errorf("could not verify before the deadline of %s", budget)
			return nil
		}
		if caller.Err() != nil {
			return landCallerGaveUp(caller, err)
		}
		// A fault of the server (the file system, the read-back of the checkout, the verification
		// directory, a git command), as on the resolve path: Internal, not Unknown.
		return structerr.NewInternal("verifying the resolution: %w", err)
	}
	return nil
}

// landWithResolution lands commitID onto tip with the caller's resolution of the conflicts
// (UserLandRequest.resolution, PLAN10 3.3 and 12), held against the merge by
// landReviewResolution. When a resolver is configured, the files that may be sent to it must pass
// its checks (the check operation: the syntax check and the configured checks), which ran before
// the lane (precheck, for this tip and commit): a failing or failed check fails the land, and a
// resolution without a check for this tip and commit fails closed. Nothing lands partially: an
// unaccepted resolution fails with a fresh conflict package (*landUnresolvedError).
//
// The merge commit carries Graft-Resolved-By: agent/<actor> and, when the resolver checked the
// resolution, Graft-Checked-By: <resolver>.
func (s *Server) landWithResolution(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	tip, commitID git.ObjectID,
	committer git.Signature,
	precheck *landPrecheck,
) (landResolution, error) {
	review, err := s.landReviewResolution(ctx, in, repo, tip, commitID)
	if err != nil {
		return landResolution{}, err
	}
	l, rejected := review.listing, review.rejected

	checkedBy, syntaxUnchecked := "", 0
	if !rejected && (s.conflictResolver != nil || s.resolverErr != nil) {
		switch {
		case s.conflictResolver == nil:
			return landResolution{}, l.notAttempted(fmt.Sprintf("the resolution could not be checked: the configured resolver cannot be built: %v", s.resolverErr), s.resolverErr)
		case precheck == nil:
			return landResolution{}, l.notAttempted("the resolution could not be checked: it was not checked before the land", nil)
		case precheck.skipped != "":
			return landResolution{}, l.notAttempted(fmt.Sprintf("the resolution could not be checked: it was not checked before the land, since %s", precheck.skipped), nil)
		case precheck.tip != tip || precheck.commit != commitID:
			return landResolution{}, l.notAttempted(fmt.Sprintf("the resolution could not be checked: it was checked against tip %s and commit %s, not tip %s and commit %s", precheck.tip, precheck.commit, tip, commitID), nil)
		}
		res := precheck.result
		for _, f := range l.files {
			v, ok := res.verdicts[f.conflict.path]
			if !ok {
				continue
			}
			if v.rejected != "" {
				f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, v.rejected
			}
			f.syntaxUnchecked = v.syntaxUnchecked
		}
		if res.failure != "" {
			// The files checked before the check failed keep their verdict; the others were not
			// attempted.
			for _, f := range l.files {
				if v, ok := res.verdicts[f.conflict.path]; !ok {
					f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, ""
				} else if v.rejected == "" {
					f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_RESOLVED, ""
				}
			}
			reason := "the resolution could not be checked: " + res.failure
			return landResolution{}, l.unresolved(reason, reason, res.failureErr)
		}
		checkedBy = res.checkedBy
		for _, v := range res.verdicts {
			if v.syntaxUnchecked {
				syntaxUnchecked++
			}
		}
	}
	for _, f := range l.files {
		if f.outcome == gitalypb.LandConflictFile_OUTCOME_REJECTED {
			rejected = true
		}
	}
	if rejected {
		for _, f := range l.files {
			if f.outcome != gitalypb.LandConflictFile_OUTCOME_REJECTED {
				f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_RESOLVED, ""
			}
		}
		return landResolution{}, l.unresolved("", "the resolution was rejected: "+landUnresolvedCause(l.files), nil)
	}

	resp := landAgentResponse(review)
	// The verification ran out of the lane (15.9): the lane compares the tree it would write with
	// the one that passed, and writes nothing that did not.
	var built, verified git.ObjectID
	if s.resolveCfg.Verify.Enabled {
		switch reason := landPrecheckUnusable(precheck, tip, commitID); {
		case reason != "":
			return landResolution{}, l.notAttempted("the resolution could not be verified: "+reason, nil)
		case precheck.verifyErr != nil:
			var verifyErr *landVerifyError
			if errors.As(precheck.verifyErr, &verifyErr) {
				return landResolution{}, l.verifyRejected(precheck.verifyErr)
			}
			return landResolution{}, l.notAttempted("the resolution could not be verified: "+precheck.verifyErr.Error(), nil)
		}
		tree, err := s.landResolvedTree(ctx, repo, l.mergedTree, resp)
		if err != nil {
			return landResolution{}, err
		}
		switch {
		case precheck.verified == "":
			return landResolution{}, l.notAttempted("the resolution could not be verified: the precheck did not verify it", nil)
		case precheck.verified != tree:
			return landResolution{}, l.notAttempted("the resolution could not be verified: the tree to land is not the tree that was verified", nil)
		}
		built, verified = tree, precheck.verified
	}
	res, err := s.landWriteResolution(ctx, in, repo, tip, commitID, l.mergedTree, committer, review.actor, resp, checkedBy, built, verified)
	res.syntaxUnchecked = syntaxUnchecked
	return res, err
}

// landPrecheckUnusable says why the precheck of a land cannot vouch for the tip and commit the lane
// has, or "" when it can.
func landPrecheckUnusable(pc *landPrecheck, tip, commitID git.ObjectID) string {
	switch {
	case pc == nil:
		return "it was not verified before the land"
	case pc.skipped != "":
		return fmt.Sprintf("it was not verified before the land, since %s", pc.skipped)
	case pc.tip != tip || pc.commit != commitID:
		return fmt.Sprintf("it was verified against tip %s and commit %s, not tip %s and commit %s", pc.tip, pc.commit, tip, commitID)
	}
	return ""
}

// landResolutionFiles maps the files of the resolution to the conflicting paths. It returns a
// reason when the resolution does not resolve exactly the conflicting paths, each once.
func landResolutionFiles(r *gitalypb.LandResolution, files []*landFileState) (map[string]resolver.ResponseFile, string) {
	conflicting := make(map[string]bool, len(files))
	var want []string
	for _, f := range files {
		conflicting[f.conflict.path] = true
		want = append(want, f.conflict.path)
	}
	resolved := make(map[string]resolver.ResponseFile, len(r.GetFiles()))
	for _, rf := range r.GetFiles() {
		p := string(rf.GetPath())
		if !conflicting[p] {
			return nil, fmt.Sprintf("the resolution names %q, which does not conflict at this tip", p)
		}
		if _, dup := resolved[p]; dup {
			return nil, fmt.Sprintf("the resolution names %q more than once", p)
		}
		out := resolver.ResponseFile{Path: p, Mode: rf.GetMode(), Content: rf.GetContent()}
		switch rf.GetAction() {
		case gitalypb.LandResolvedFile_ACTION_WRITE:
			out.Action = resolver.ActionWrite
		case gitalypb.LandResolvedFile_ACTION_DELETE:
			out.Action = resolver.ActionDelete
		default:
			out.Action = rf.GetAction().String()
		}
		resolved[p] = out
	}
	if len(resolved) != len(conflicting) {
		var missing []string
		for _, p := range want {
			if _, ok := resolved[p]; !ok {
				missing = append(missing, p)
			}
		}
		slices.Sort(missing)
		return nil, fmt.Sprintf("the resolution does not resolve %s", strings.Join(missing, ", "))
	}
	return resolved, ""
}

// landCheckVerdict is the verdict of the resolver's check on one file.
type landCheckVerdict struct {
	// rejected is the reason of a check the file failed, empty when it passed.
	rejected        string
	syntaxUnchecked bool
}

// landCheckResult is what the checks of an agent's resolution found: the checker's name (for the
// Graft-Checked-By trailer, "" when no file could be sent), the verdict of every file checked, and,
// when a check could not run, why (the check fails closed: the land does not proceed).
type landCheckResult struct {
	checkedBy  string
	verdicts   map[string]landCheckVerdict
	failure    string
	failureErr error
}

// landCheckBatchBytes bounds the file data (the sides and the resolution) of one check request,
// resolver.CheckBatchBytes. A test may set a smaller bound on the server (checkBatchBytes).
func (s *Server) landCheckBatchBytes() int64 {
	if s.checkBatchBytes > 0 {
		return s.checkBatchBytes
	}
	return resolver.CheckBatchBytes
}

// landCheckResolution runs the resolver's checks on the files of the resolution that may be sent
// to the resolver (landClassify). The files go in check requests of at most landCheckBatchBytes of
// file data each: for each, a check slot of the resolver is taken first (resolver.Client bounds the
// checks that run at once, and waiting for a slot counts against the deadline of ctx), then the
// sides of the batch are read, checked and released, so that no more than one bounded request is
// held at a time. Every batch must name the same checker. The listing and the attributes of the
// trees are read once for all batches. The requests carry no commit message: the checks judge the
// files. Anything that stops the checks (a request that fails, a git error, the deadline) is the
// result's failure, with the verdicts found before: the check fails closed.
func (s *Server) landCheckResolution(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	l landListing,
	actor string,
	resolved map[string]resolver.ResponseFile,
) landCheckResult {
	result := landCheckResult{verdicts: map[string]landCheckVerdict{}}
	fail := func(what string, err error) landCheckResult {
		result.failure, result.failureErr = fmt.Sprintf("%s: %v", what, err), err
		return result
	}
	batches, err := landCheckBatches(ctx, repo, l.files, resolved, s.landCheckBatchBytes())
	if err != nil {
		return fail("sizing the check requests", err)
	}
	cache := newLandClassifyCache(l.files, l.tip)

	var unchecked []string
	for _, batch := range batches {
		checked, resp, stage, err := s.landCheckSlotted(ctx, in, repo, l, actor, resolved, batch, cache, &unchecked)
		switch {
		case err != nil:
			return fail(stage, err)
		case resp == nil:
			continue
		case !landResolverNameValid(resp.Resolver):
			result.failure = fmt.Sprintf("the check names an invalid resolver %q", resp.Resolver)
			return result
		case result.checkedBy != "" && resp.Resolver != result.checkedBy:
			result.failure = fmt.Sprintf("its checks named the resolvers %q and %q", result.checkedBy, resp.Resolver)
			return result
		}
		result.checkedBy = resp.Resolver
		for i, v := range resp.Files {
			verdict := landCheckVerdict{syntaxUnchecked: v.OK && v.Syntax == resolver.SyntaxUnchecked}
			if !v.OK {
				verdict.rejected = v.Reason
			}
			result.verdicts[checked[i].conflict.path] = verdict
		}
	}
	if len(result.verdicts) == 0 {
		s.logger.WithField("unchecked", unchecked).InfoContext(ctx, "graft: no file of the agent's resolution may be sent to the resolver's checks")
		return result
	}
	s.logger.WithFields(log.Fields{
		"checked_by":     result.checkedBy,
		"checked":        len(result.verdicts),
		"check_requests": len(batches),
		"not_checked":    unchecked,
	}).InfoContext(ctx, "graft: agent's resolution checked")
	return result
}

// landCheckSlotted takes a check slot and checks one batch with it (landCheckBatch). The slot and
// the sides of the batch are released when it returns, even on a panic: a lost slot would refuse
// every later check of the process. stage names what failed, for the result's failure.
func (s *Server) landCheckSlotted(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	l landListing,
	actor string,
	resolved map[string]resolver.ResponseFile,
	batch []*landFileState,
	cache *landClassifyCache,
	unchecked *[]string,
) ([]*landFileState, *resolver.CheckResponse, string, error) {
	slot, err := s.conflictResolver.AcquireCheckSlot(ctx)
	if err != nil {
		return nil, nil, "waiting for the resolver", err
	}
	defer slot.Release()
	// The sides of the batch are released once it is checked: the next batch reads its own, and a
	// package reads again, within its own bounds, what its hunks need.
	defer func() {
		for _, f := range batch {
			f.sides, f.read = [3][]byte{}, false
		}
	}()
	if s.landCheckBatchHook != nil {
		s.landCheckBatchHook(ctx)
	}
	checked, resp, err := s.landCheckBatch(ctx, in, repo, l, actor, resolved, batch, cache, slot, unchecked)
	return checked, resp, "checking", err
}

// landCheckBatch reads and checks one batch of files with a check slot taken. It returns the files
// sent, in the order of the verdicts, and the check's answer, nil when no file of the batch may be
// sent; the files that may not are added to unchecked.
func (s *Server) landCheckBatch(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	l landListing,
	actor string,
	resolved map[string]resolver.ResponseFile,
	batch []*landFileState,
	cache *landClassifyCache,
	slot *resolver.CheckSlot,
	unchecked *[]string,
) ([]*landFileState, *resolver.CheckResponse, error) {
	if err := s.landClassify(ctx, repo, batch, l.base, l.tip, l.commitID, cache); err != nil {
		return nil, nil, err
	}
	req := resolver.Request{
		Repository: resolver.Repository{
			Storage:      in.GetRepository().GetStorageName(),
			RelativePath: in.GetRepository().GetRelativePath(),
		},
		Branch: string(in.GetBranch()),
		Tip:    l.tip.String(),
		Commit: l.commitID.String(),
		Actor:  actor,
	}
	if l.hasBase {
		req.MergeBase = l.base.String()
	}
	var checked []*landFileState
	for _, f := range batch {
		if !f.sent() {
			// The file may not go to the resolver: it gets the server's rules only.
			*unchecked = append(*unchecked, fmt.Sprintf("%s (%s)", f.conflict.path, f.reason))
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_UNSPECIFIED, ""
			continue
		}
		req.Files = append(req.Files, landRequestFile(f))
		req.Resolution = append(req.Resolution, resolved[f.conflict.path])
		checked = append(checked, f)
	}
	if len(checked) == 0 {
		return nil, nil, nil
	}
	resp, err := slot.Check(ctx, req)
	return checked, resp, err
}

// landCheckBatches splits the files into batches of at most budget bytes of file data: the sizes
// of their sides (from the object info, before reading) and of their resolution, plus what a file
// adds to the request around them (resolver.CheckFileOverheadBytes for its path). A file larger
// than the budget is a batch of its own, which fits a check request since max_file_bytes is at
// most resolver.MaxCheckFileBytes.
func landCheckBatches(ctx context.Context, repo *localrepo.Repo, files []*landFileState, resolved map[string]resolver.ResponseFile, budget int64) ([][]*landFileState, error) {
	var batches [][]*landFileState
	var cur []*landFileState
	var size int64
	for _, f := range files {
		n := int64(len(resolved[f.conflict.path].Content)) + resolver.CheckFileOverheadBytes(f.conflict.path)
		for _, b := range []*landBlob{f.conflict.base, f.conflict.ours, f.conflict.theirs} {
			if b == nil || (b.mode != resolver.ModeRegular && b.mode != resolver.ModeExecutable && b.mode != resolver.ModeSymlink) {
				continue
			}
			info, err := repo.ReadObjectInfo(ctx, b.oid.Revision())
			if err != nil {
				return nil, fmt.Errorf("reading size of %q: %w", f.conflict.path, err)
			}
			n += info.Size
		}
		if len(cur) > 0 && size+n > budget {
			batches = append(batches, cur)
			cur, size = nil, 0
		}
		cur = append(cur, f)
		size += n
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches, nil
}
