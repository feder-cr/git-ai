package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// This file implements the conflict resolution of UserLand (resolve protocol v1, see
// doc/graft/RESOLVE.md). When a landing hits a merge conflict and the request sets
// resolve_conflicts, the conflicting files are sent to the configured resolver (the client of
// internal/graft/resolver, which enforces the path and actor policy, the limits and every
// acceptance rule of the answer). The answer is written into a merge commit with parents
// [tip, commit]. Unless every file is resolved, the land fails with the original merge conflict
// and the conflict package of land_package.go.

// maxResolverNameBytes bounds the name of the resolver, which is written into a commit trailer.
const maxResolverNameBytes = resolver.MaxResolverNameBytes

// maxLoggedExplanationBytes bounds the explanation of the resolver kept in the log of a
// resolution.
const maxLoggedExplanationBytes = 4 << 10

// landResolution is the merge commit that resolves a landing.
type landResolution struct {
	commit        git.ObjectID
	resolvedPaths [][]byte
	resolver      string
	// syntaxUnchecked is the number of files of an agent's resolution that the check accepted
	// without a syntax verdict.
	syntaxUnchecked int
}

// landed records a resolution whose land succeeded: the metric of files landed without a syntax
// verdict counts them here, once per landed request, and not when they were checked, since the
// land may still fail or be decided again.
func (r *landResolution) landed() {
	if r != nil && r.syntaxUnchecked > 0 {
		landResolutionSyntaxUncheckedTotal.Add(float64(r.syntaxUnchecked))
	}
}

// landBlob is one side of a conflicted file as merge-tree reports it: the mode and the blob.
type landBlob struct {
	mode string
	oid  git.ObjectID
}

// landFileConflict is a conflicted file of the merge. The sides are the stage entries of merge-tree,
// so they are the versions of the file that the merge actually compared. A rename changes the
// path of a side, which the stage entries still follow: the name of the conflict is the path of
// the merged tree, and the base may have lived under another name. A nil side is absent.
type landFileConflict struct {
	path   string
	base   *landBlob
	ours   *landBlob
	theirs *landBlob
}

// landResolveAt resolves, against tip, the conflicting files of the land that have no verdict in
// pre yet (PLAN10 15.8). It runs out of the lane: the policy first (an agent that may not
// update the branch gets nothing resolved), the listing, the classification of every file, one
// resolver request for the files to resolve, taken with a resolve slot, and the validation; then,
// for every resolution that needs it (a model's always, any when a verification is configured),
// the verification of the exact tree that would land (15.9), with a verify slot; then decideLand
// over every file, the verdicts reused included.
// A request carries only what a file's verdict may depend on: the sides, the path and modes, the
// change's message, the configuration; not the tip, its merge base or its message, so that a
// verdict can be reused at another tip (15.8). When the land is landable, the new verdicts are
// added to pre, the verified tree is recorded there, and the listing and the answer to write are
// returned. Otherwise the answer is kept as proposals (14) and the land fails with a
// *landUnresolvedError, whose package says what happened to each file, and why a model's
// resolution did not land; a request the server does not attempt at all also gets a package, with
// the reason. A verification that the context ended is a *landVerifyUnfinishedError. Any other
// error means the conflicts could not be listed, and the caller reports the merge conflict alone.
func (s *Server) landResolveAt(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	tip, commitID git.ObjectID,
	committer git.Signature,
	pre *landPreResolution,
) (landListing, *resolver.Response, error) {
	// An agent that may not update the branch gets nothing resolved, and no proposal is kept for
	// it: the policy decides on the actor and the reference alone, so it is checked first.
	reference := git.NewReferenceNameFromBranchName(string(in.GetBranch()))
	if err := hook.CheckAgentPolicy(ctx, s.graft, fmt.Sprintf("%s %s %s\n", tip, commitID, reference)); err != nil {
		return landListing{}, nil, &landPolicyDeniedError{err: err}
	}
	l, err := landListConflicts(ctx, repo, tip, commitID, s.resolveCfg.MaxFileBytesOrDefault())
	if err != nil {
		return l, nil, err
	}
	base, conflicts, files := l.base, l.conflicts, l.files
	// max_files bounds the request to the resolver only: the package lists every conflicting file.
	maxFiles := s.resolveCfg.MaxFilesOrDefault()
	notAttempted := func(reason string) error { return l.notAttempted(reason, nil) }
	criss := l.bases

	// The actor is the authenticated identity only. The name in the request is chosen by the
	// caller, so it must not decide who may resolve: without an identity the actor is empty and
	// only an actor list that is empty (allows everyone) admits the request.
	actor := identity.Actor(ctx, "")
	switch {
	case len(files) == 0:
		return l, nil, notAttempted(landNoConflictingFiles)
	case s.conflictResolver == nil && s.resolverErr != nil:
		return l, nil, notAttempted(fmt.Sprintf("conflict resolution is configured, but its resolver cannot be built: %v", s.resolverErr))
	case s.conflictResolver == nil:
		return l, nil, notAttempted("conflict resolution is not enabled on this server")
	case !s.conflictResolver.ActorAllowed(actor):
		return l, nil, notAttempted(fmt.Sprintf("actor %q may not resolve conflicts", actor))
	case criss > 1:
		return l, nil, notAttempted(fmt.Sprintf("the change has %d merge bases (a criss-cross history), which are not resolved", criss))
	case len(conflicts) > maxFiles:
		return l, nil, notAttempted(fmt.Sprintf("%d conflicting files exceed max_files %d", len(conflicts), maxFiles))
	}

	if err := s.landClassify(ctx, repo, files, base, tip, commitID, newLandClassifyCache(files, tip)); err != nil {
		return l, nil, err
	}
	pre.setCurrent(files)

	// A file the classification sends and that has a verdict with its key is not resolved again.
	// mine are the files with an answer, reused or fresh, whose outcome the answer decides.
	reused := map[string]resolver.ResponseFile{}
	var reusedNames []string
	var sent []resolver.File
	var mine []*landFileState
	// A land that ran out of time waiting for its turn on its files (land_tickets.go) is ended with the
	// package, and does not land past the lands that hold the tickets: its files are not attempted, and
	// say why.
	queuedBehind := false
	if pre.unverifiable != "" {
		paths := make([]string, 0, len(files))
		for _, f := range files {
			paths = append(paths, f.conflict.path)
		}
		scope := landTicketScope(in.GetRepository().GetStorageName(), in.GetRepository().GetRelativePath(), in.GetBranch())
		queuedBehind = landTickets.blocked(landTicketKeys(scope, paths, s.resolveCfg.Verify.Enabled), pre.ticket)
	}
	for _, f := range files {
		if !f.sent() {
			continue
		}
		if v, ok := pre.verdicts[landKeyOf(f.conflict)]; ok && !queuedBehind {
			mine = append(mine, f)
			reused[f.conflict.path] = v.file
			reusedNames = append(reusedNames, v.resolver)
			f.outcome = gitalypb.LandConflictFile_OUTCOME_RESOLVED
			continue
		}
		if pre.unverifiable != "" {
			// The land is being ended (landFinalPass): the resolver is not called, and a file with no
			// verdict at this tip is reported as not attempted, with why.
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, pre.unverifiable
			continue
		}
		mine = append(mine, f)
		sent = append(sent, landRequestFile(f))
	}

	// answer is the resolver's accepted answer, the only thing ever written. A refusal is never
	// written, whatever its outcomes say: answer stays nil, and the land fails with the package.
	// An answer that holds a model's resolution is never landed: it is kept as proposals for the
	// agent, who lands its own resolution (PLAN10 14).
	var answer *resolver.Response
	var resolveErr error
	if len(sent) > 0 {
		req := resolver.Request{
			Version: resolver.ProtocolVersion,
			Repository: resolver.Repository{
				Storage:      in.GetRepository().GetStorageName(),
				RelativePath: in.GetRepository().GetRelativePath(),
			},
			Branch:   string(in.GetBranch()),
			Commit:   commitID.String(),
			Messages: resolver.Messages{Commit: l.commitMessage},
			Actor:    actor,
			Files:    sent,
			// What this server does with the answer, so that a graft-resolve gives a model's resolution
			// only to a server that holds and verifies it.
			Capabilities: resolver.ServerCapabilities(),
		}

		// The client applies the timeout, the request policy and the acceptance rules of the
		// answer; the request waits for a resolve slot within the deadline of ctx.
		resp, err := s.landResolveSlotted(ctx, req)
		resolveErr = err
		switch {
		case err != nil:
			landRefusalOutcomes(files, err)
		case !landResolverNameValid(resp.Resolver):
			// The name is written into the commit trailers, so it must be a plain single-line name.
			landFailSent(files, gitalypb.LandConflictFile_OUTCOME_FAILED, fmt.Sprintf("the resolver's answer names an invalid resolver %q", resp.Resolver))
		default:
			answer = resp
			landNormalizeOrigins(answer)
		}
	}

	combined := landCombinedAnswer(answer, reused, reusedNames, len(sent) > 0)
	if combined != nil {
		landSetOutcomes(combined, mine)
	}
	// The verdicts of an answer that nothing refuses are kept by their keys before it is verified or
	// decided: the lane and the next round reuse them, whatever becomes of this verification.
	if answer != nil && landAllAnswered(files) {
		for _, f := range files {
			for _, rf := range answer.Files {
				if rf.Path == f.conflict.path {
					pre.verdicts[landKeyOf(f.conflict)] = landVerdict{file: rf, resolver: answer.Resolver}
				}
			}
		}
	}

	// A resolution lands only through the operator's verification (15.9): verify.command runs
	// here, out of the lane, on the exact tree that would land, once nothing else stops the land,
	// whoever made the resolution: a model's, whose files stay proposals without one, and a rule's,
	// when a verification is configured. Whatever keeps a file from landing is said in its reason.
	verifyRequired := s.resolveCfg.Verify.Enabled
	var tree, verified git.ObjectID
	if combined != nil && (landAnswerProposes(combined) || verifyRequired) {
		switch {
		case landAnswerHolds(combined):
			// A file is held back: the land cannot land, so the tree is not verified (that would hold
			// the repository's verify slot for nothing). The files say why in their reasons.
		case pre.unverifiable != "":
			// The land has run out of time to verify. A tree that passed in an earlier round needs no
			// verification, and lands (the lane only compares): check it before giving up.
			if verifyRequired && landAllAnswered(files) {
				var err error
				if tree, err = s.landResolvedTree(ctx, repo, l.mergedTree, combined); err != nil {
					return l, nil, err
				}
				if pre.isVerified(tree) {
					verified = tree
					break
				}
			}
			landAnnotateUnverified(files, combined, pre.unverifiable)
		case !verifyRequired && len(s.resolveCfg.Verify.Command) > 0:
			landAnnotateProposed(files, landVerifyDisabledReason)
		case !verifyRequired:
			landAnnotateProposed(files, landNoVerifyReason)
		case landAllAnswered(files):
			var err error
			if tree, err = s.landResolvedTree(ctx, repo, l.mergedTree, combined); err != nil {
				return l, nil, err
			}
			// The command may already have passed on this very tree, in an earlier round: whatever brought
			// the land back (a snapshot that reads the same tip, a tip that moved to the same tree),
			// verifying it again would hold the repository's slot for minutes for the same answer.
			if pre.isVerified(tree) {
				verified = tree
				break
			}
			var verifyErr *landVerifyError
			current := func(ctx context.Context) (bool, error) {
				// A missing branch reads as the empty ID, which is never the tip.
				now, err := s.landBranchTip(ctx, repo, git.NewReferenceNameFromBranchName(string(in.GetBranch())))
				return now == tip, err
			}
			switch err := s.landVerifyOutOfLane(ctx, in.GetRepository(), repo, tree, current); {
			case err == nil:
				verified = tree
				pre.markVerified(tree)
				s.logger.WithFields(log.Fields{"branch": string(in.GetBranch()), "tip": tip.String(), "commit": commitID.String(), "tree": tree.String()}).
					InfoContext(ctx, "graft: a resolution passed the verification out of the lane")
			case errors.As(err, &verifyErr):
				if landAnswerProposes(combined) {
					landAnnotateProposed(files, verifyErr.Error())
				} else {
					landRejectResolved(files, verifyErr.Error())
				}
			default:
				return l, nil, err
			}
		}
	}
	landed := decideLand(combined, files, tree, verified, verifyRequired)
	// An answer that does not land, because a model made part of it or another file is left, is
	// kept for the agent: the files that rules resolved can be landed as they are. The proposals
	// are an aid and the package is the contract: when they cannot be kept, the package still goes
	// to the agent, each file of the answer saying that its proposal was not stored, and the
	// failure is logged and counted.
	if combined != nil && !landed {
		ref, err := s.landWriteProposals(ctx, in, l, combined, committer)
		if err != nil {
			landProposalWriteErrors.Inc()
			s.logger.WithError(err).WithField("branch", string(in.GetBranch())).ErrorContext(ctx, "graft: the resolver's answer could not be kept as proposals")
			landProposalsNotStored(files, combined, err)
		} else {
			l.proposalRef = ref
		}
	}
	if !landed {
		return l, nil, l.unresolved("", landUnresolvedCause(files), resolveErr)
	}

	return l, combined, nil
}

// landSetOutcomes sets the outcome of each of files from the answer that covers them: RESOLVED, or
// REJECTED when the resolution writes a mode no side has, or PROPOSED when a model made the file,
// that is when the file is not marked as a rule's (the mark may be missing, never the other way
// round). The lane sets them the same way from the verdicts it applies, so that both ask decideLand
// the same question.
func landSetOutcomes(answer *resolver.Response, files []*landFileState) {
	written := make(map[string]resolver.ResponseFile, len(answer.Files))
	for _, rf := range answer.Files {
		written[rf.Path] = rf
	}
	for _, f := range files {
		rf, ok := written[f.conflict.path]
		f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_RESOLVED, ""
		switch err := landModeOfSides(f.conflict, rf); {
		case !ok:
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_FAILED, "the resolver gave no answer for the file"
		case err != nil:
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, err.Error()
		case rf.Hold != "":
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_PROPOSED, landCutString(landHeldReason(rf.Hold), landPackageReasonBytes)
		case rf.FromModel():
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_PROPOSED, landProposedReason
		}
	}
}

// landAllAnswered reports whether every file is resolved or proposed: nothing else keeps the land
// from landing, so that a model's resolution is worth verifying.
func landAllAnswered(files []*landFileState) bool {
	for _, f := range files {
		if f.outcome != gitalypb.LandConflictFile_OUTCOME_RESOLVED && f.outcome != gitalypb.LandConflictFile_OUTCOME_PROPOSED {
			return false
		}
	}
	return true
}

// landAnnotateProposed adds why a model's files did not land to the reason of each.
// landAnnotateUnverified says why a land that needs the verification could not be verified in time:
// a model's files are PROPOSED with the reason, and the files of an answer made by rules only are
// NOT_ATTEMPTED with it, since nothing of them lands without the verification.
func landAnnotateUnverified(files []*landFileState, answer *resolver.Response, why string) {
	if landAnswerProposes(answer) {
		landAnnotateProposed(files, why)
		return
	}
	for _, f := range files {
		if f.outcome == gitalypb.LandConflictFile_OUTCOME_RESOLVED {
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, why
		}
	}
}

// landRejectResolved marks every resolved file of a rules' answer REJECTED with the reason of the
// verification that failed: the land does not happen, and the package says why.
func landRejectResolved(files []*landFileState, why string) {
	for _, f := range files {
		if f.outcome == gitalypb.LandConflictFile_OUTCOME_RESOLVED {
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, why
		}
	}
}

func landAnnotateProposed(files []*landFileState, why string) {
	for _, f := range files {
		if f.outcome != gitalypb.LandConflictFile_OUTCOME_PROPOSED {
			continue
		}
		if f.reason == "" {
			f.reason = why
		} else {
			f.reason += "; " + why
		}
	}
}

// landResolveSlotted sends the resolve request with a resolve slot. The slot is given back when the
// call returns, even on a panic: a lost slot would refuse every later resolution of the process.
func (s *Server) landResolveSlotted(ctx context.Context, req resolver.Request) (*resolver.Response, error) {
	slot, err := s.conflictResolver.AcquireResolveSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer slot.Release()
	if s.landResolveHook != nil {
		s.landResolveHook()
	}
	return slot.Resolve(ctx, req)
}

// landCombinedAnswer is the answer of a land whose files are resolved in rounds: the files the
// resolver answered now and the verdicts reused, under the names of the resolvers that made them.
// It is nil when the resolver was asked and did not answer, or when there is nothing at all.
func landCombinedAnswer(answer *resolver.Response, reused map[string]resolver.ResponseFile, reusedNames []string, asked bool) *resolver.Response {
	if asked && answer == nil {
		return nil
	}
	if answer == nil && len(reused) == 0 {
		return nil
	}
	combined := &resolver.Response{Version: resolver.ProtocolVersion}
	var names []string
	if answer != nil {
		combined.Files = append(combined.Files, answer.Files...)
		combined.Explanation = answer.Explanation
		names = append(names, answer.Resolver)
	}
	paths := make([]string, 0, len(reused))
	for p := range reused {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		combined.Files = append(combined.Files, reused[p])
	}
	names = append(names, reusedNames...)
	combined.Resolver = landJoinResolverNames(names)
	return combined
}

// landJoinResolverNames joins the distinct resolver names of an answer made in rounds, within
// the bound of a resolver name: the names that fit, and "+N" for the others.
func landJoinResolverNames(names []string) string {
	seen := map[string]bool{}
	var distinct []string
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			distinct = append(distinct, n)
		}
	}
	joined := strings.Join(distinct, ",")
	if len(joined) <= maxResolverNameBytes {
		return joined
	}
	for kept := len(distinct) - 1; kept > 0; kept-- {
		cut := strings.Join(distinct[:kept], ",") + fmt.Sprintf(",+%d", len(distinct)-kept)
		if len(cut) <= maxResolverNameBytes {
			return cut
		}
	}
	return distinct[0][:min(len(distinct[0]), maxResolverNameBytes)]
}

// landApplyVerdicts lands, in the lane, the verdicts that the pre-resolution made out of it
// (PLAN10 15.8). It lists the conflicts at the lane's tip and classifies every file again (the
// classification reads the tip), and writes the merge commit only when every conflicting file is
// sent and has a verdict with its key; the resolver is never called. A file without one makes it
// return *landNeedsResolution, which UserLand resolves out of the lane. A clean merge of the tip
// and the change is errLandNoConflicts, as before.
func (s *Server) landApplyVerdicts(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	tip, commitID git.ObjectID,
	committer git.Signature,
	pre *landPreResolution,
) (landResolution, error) {
	if s.landLaneHook != nil {
		s.landLaneHook()
	}
	l, err := landListConflicts(ctx, repo, tip, commitID, s.resolveCfg.MaxFileBytesOrDefault())
	if err != nil {
		return landResolution{}, err
	}
	var missing []string
	if pre == nil || s.conflictResolver == nil || l.bases > 1 {
		for _, f := range l.files {
			missing = append(missing, f.conflict.path)
		}
	} else {
		if err := s.landClassify(ctx, repo, l.files, l.base, tip, commitID, newLandClassifyCache(l.files, tip)); err != nil {
			return landResolution{}, err
		}
		pre.setCurrent(l.files)
		for _, f := range l.files {
			if _, ok := pre.verdicts[landKeyOf(f.conflict)]; !ok || !f.sent() {
				missing = append(missing, f.conflict.path)
			}
		}
	}
	if len(l.files) == 0 {
		// A merge that is unclean without a conflicting file (a directory rename split) at this tip.
		// The pre-resolution refuses it with the same reason when it sees it at its own tip; here the
		// tip moved to it. No file can be resolved, so asking for files again would only repeat the
		// round: the land is refused, with the package that says why.
		s.logger.WithField("branch", string(in.GetBranch())).WithField("tip", tip.String()).
			Warn("graft: the merge at the lane's tip has conflicts without a conflicting file")
		return landResolution{}, l.notAttempted(landNoConflictingFiles, nil)
	}
	// The rules are resolved here, in the lane, within a bound of time (land_inline.go): the member rides
	// the batch, as it did before the resolution left the lane. What a rule cannot resolve goes out.
	inline := landInlineNotTried
	if len(missing) > 0 && pre != nil && s.conflictResolver != nil && l.bases <= 1 {
		// A land that lost a round out of the lane holds the files with priority: resolving them here would
		// change them under its verdict once more, so the lands that would give way and wait their turn.
		if s.landInlineEnabled() && landTickets.priorityBlocked(s.landListingKeys(in, l), pre.ticket) {
			return landResolution{}, &landNeedsResolution{tip: tip, commit: commitID, paths: landListingPaths(l), queued: true}
		}
		// The holder itself does not resolve inline: it comes back with the verdict of its slow rule, and asking
		// the rule again would only stall the batch.
		if !pre.ticket.hasPriority() {
			inline = s.landResolveInline(ctx, in, tip, commitID, l, pre)
		}
		if inline == landInlineResolved {
			missing = missing[:0]
			for _, f := range l.files {
				if _, ok := pre.verdicts[landKeyOf(f.conflict)]; !ok || !f.sent() {
					missing = append(missing, f.conflict.path)
				}
			}
		}
	}
	// What the rules cannot resolve, without a verification, is resolved out of the lane for the proposals
	// and ends there: what a model resolves never lands, so no ticket protects anything.
	rulesCannot := inline == landInlineRulesCannot && !s.resolveCfg.Verify.Enabled
	// A land that lost a round holds the ticket of its files and is the one whose verdict lands: any other
	// land on the same files, whose verdicts were made out of the lane, waits for its turn
	// (land_tickets.go), instead of landing first and invalidating the holder's verdict once more. A land
	// that the lane has just resolved inline needs no turn: the rules do not wait for the tickets.
	if pre != nil && s.conflictResolver != nil && l.bases <= 1 && inline != landInlineResolved && !rulesCannot {
		paths := make([]string, 0, len(l.files))
		for _, f := range l.files {
			paths = append(paths, f.conflict.path)
		}
		scope := landTicketScope(in.GetRepository().GetStorageName(), in.GetRepository().GetRelativePath(), in.GetBranch())
		if landTickets.blocked(landTicketKeys(scope, paths, s.resolveCfg.Verify.Enabled), pre.ticket) {
			return landResolution{}, &landNeedsResolution{tip: tip, commit: commitID, paths: paths, queued: true}
		}
	}
	if len(missing) > 0 {
		return landResolution{}, &landNeedsResolution{tip: tip, commit: commitID, paths: missing, rulesCannot: rulesCannot}
	}

	reused := map[string]resolver.ResponseFile{}
	var names []string
	for _, f := range l.files {
		v := pre.verdicts[landKeyOf(f.conflict)]
		reused[f.conflict.path] = v.file
		names = append(names, v.resolver)
	}
	answer := landCombinedAnswer(nil, reused, names, false)
	landSetOutcomes(answer, l.files)

	// The one rule, asked of the tree this tip would land: it lands only if verify.command passed on
	// this very tree (tree OIDs, not tips), whoever made the resolution, whenever a verification is
	// configured, and a model's resolution on no other. When the tip moved, the tree is another one,
	// and the land goes back out of the lane to be verified again. The lane never runs a
	// verification: it compares.
	verifyRequired := s.resolveCfg.Verify.Enabled
	var tree, verified git.ObjectID
	if landAnswerProposes(answer) || verifyRequired {
		if tree, err = s.landResolvedTree(ctx, repo, l.mergedTree, answer); err != nil {
			return landResolution{}, err
		}
		if pre.isVerified(tree) {
			verified = tree
		}
	}
	if !decideLand(answer, l.files, tree, verified, verifyRequired) {
		need := &landNeedsResolution{tip: tip, commit: commitID, unverified: tree}
		if tree == "" {
			// Nothing to verify, yet not landable: a file the answer rejects. Resolving again says why.
			for _, f := range l.files {
				need.paths = append(need.paths, f.conflict.path)
			}
		}
		return landResolution{}, need
	}
	checkedBy := ""
	if verified != "" {
		checkedBy = s.landVerifyTrailer()
	}
	res, err := s.landWriteResolution(ctx, in, repo, tip, commitID, l.mergedTree, committer, identity.Actor(ctx, ""), answer, checkedBy, tree, verified)
	return res, err
}

// verifyRejected turns a resolution that the verification rejected into the unresolved land with
// its package, every file rejected with the reason of the verification. Any other error is
// returned as is.
func (l landListing) verifyRejected(err error) error {
	var verifyErr *landVerifyError
	if !errors.As(err, &verifyErr) {
		return err
	}
	for _, f := range l.files {
		f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, verifyErr.Error()
	}
	return l.unresolved("", verifyErr.Error(), err)
}

// landListing is the conflicting files of the merge of a tip and a change, with what the conflict
// package needs.
type landListing struct {
	repo          *localrepo.Repo
	maxFileBytes  int64
	tip, commitID git.ObjectID
	base          git.ObjectID
	hasBase       bool
	// mergeBases are all the merge bases, and bases their number; more than one is a criss-cross
	// history.
	mergeBases                []git.ObjectID
	bases                     int
	mergedTree                git.ObjectID
	conflicts                 []landFileConflict
	files                     []*landFileState
	tipMessage, commitMessage string
	// messages are merge-tree's messages about the merge. A merge can be unclean without a
	// conflicting file (a directory rename split): then files is empty and only messages
	// describe it.
	messages []landMergeMessage
	// proposalRef is the reference that keeps the resolver's answer of a land that did not
	// happen (landWriteProposals), empty when there is none.
	proposalRef git.ReferenceName
}

// landNoConflictingFiles is the reason of a merge that is unclean without a conflicting file.
const landNoConflictingFiles = "the merge has conflicts without a conflicting file (see the messages of the package), which a resolution of files cannot resolve"

// errLandNoConflicts is the error of landListConflicts when the merge is clean.
var errLandNoConflicts = errors.New("the merge of the tip and the change is clean")

// landListConflicts lists the files that conflict between tip and commitID. A clean merge is an
// error: when rebasing, the conflict may come from an intermediate commit that a later commit of
// the change undoes, so the merge of the two ends is clean and the landing reports the merge
// conflict. This is a documented limitation.
func landListConflicts(ctx context.Context, repo *localrepo.Repo, tip, commitID git.ObjectID, maxFileBytes int64) (landListing, error) {
	l := landListing{repo: repo, maxFileBytes: maxFileBytes, tip: tip, commitID: commitID}
	var err error
	if l.base, l.hasBase, l.mergeBases, err = landMergeBase(ctx, repo, tip, commitID); err != nil {
		return l, err
	}
	l.bases = len(l.mergeBases)
	var clean bool
	if l.mergedTree, l.conflicts, l.messages, clean, err = landMergeTree(ctx, repo, tip, commitID, l.base, l.hasBase); err != nil {
		return l, err
	}
	if clean {
		return l, errLandNoConflicts
	}
	if l.tipMessage, err = landCommitBody(ctx, repo, tip); err != nil {
		return l, err
	}
	if l.commitMessage, err = landCommitBody(ctx, repo, commitID); err != nil {
		return l, err
	}
	for _, c := range l.conflicts {
		l.files = append(l.files, &landFileState{conflict: c, kind: landKindOf(c)})
	}
	return l, nil
}

// unresolved returns the error of a land left unresolved, which carries what its conflict package
// is built from (landWithPackage). reason is the package's reason, cause the error's. err is the
// error of the resolver or of the check that caused it, if any.
func (l landListing) unresolved(reason, cause string, err error) error {
	return &landUnresolvedError{listing: l, reason: reason, cause: cause, err: err}
}

// notAttempted returns the error of a land the server did not try to resolve at all, every file
// NOT_ATTEMPTED and the package carrying the reason.
func (l landListing) notAttempted(reason string, err error) error {
	for _, f := range l.files {
		f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, ""
	}
	return l.unresolved(reason, reason, err)
}

// landModeOfSides refuses a written resolution whose mode is none of the modes of the conflict's
// sides: a resolution merges the sides, and turning a file into a symbolic link or an executable
// (or back) that no side asked for is not a merge. A deletion has no mode.
func landModeOfSides(c landFileConflict, rf resolver.ResponseFile) error {
	if rf.Action != resolver.ActionWrite {
		return nil
	}
	var modes []string
	for _, b := range []*landBlob{c.base, c.ours, c.theirs} {
		if b == nil {
			continue
		}
		if b.mode == rf.Mode {
			return nil
		}
		if !slices.Contains(modes, b.mode) {
			modes = append(modes, b.mode)
		}
	}
	return fmt.Errorf("the resolution writes mode %s, which is none of the modes of the sides (%s)", rf.Mode, strings.Join(modes, ", "))
}

// landAllResolved reports whether every file of the land is resolved.
func landAllResolved(files []*landFileState) bool {
	for _, f := range files {
		if f.outcome != gitalypb.LandConflictFile_OUTCOME_RESOLVED {
			return false
		}
	}
	return true
}

// landRequestFile is the file of the resolve request for a classified file that is sent.
func landRequestFile(f *landFileState) resolver.File {
	file := resolver.File{Path: f.conflict.path, Kind: f.kind}
	for i, side := range []struct {
		blob *landBlob
		dst  **resolver.Side
	}{{f.conflict.base, &file.Base}, {f.conflict.ours, &file.Ours}, {f.conflict.theirs, &file.Theirs}} {
		if side.blob != nil {
			*side.dst = &resolver.Side{OID: side.blob.oid.String(), Mode: side.blob.mode, Content: f.sides[i]}
		}
	}
	return file
}

// landFailSent sets the outcome and reason of every file that was sent.
func landFailSent(files []*landFileState, outcome gitalypb.LandConflictFile_Outcome, reason string) {
	for _, f := range files {
		if f.sent() {
			f.outcome, f.reason = outcome, reason
		}
	}
}

// landRefusalOutcomes sets the outcome of the sent files from the resolver's failure. A refusal
// with the outcome of every file (validated by the client) gives each its own; otherwise every
// sent file failed with the error.
func landRefusalOutcomes(files []*landFileState, err error) {
	var refusal *resolver.UnresolvableError
	if !errors.As(err, &refusal) || refusal.Files == nil {
		landFailSent(files, gitalypb.LandConflictFile_OUTCOME_FAILED, fmt.Sprintf("resolver failed: %v", err))
		return
	}
	outcomes := make(map[string]resolver.FileOutcome, len(refusal.Files))
	for _, o := range refusal.Files {
		outcomes[o.Path] = o
	}
	for _, f := range files {
		if !f.sent() {
			continue
		}
		o := outcomes[f.conflict.path]
		switch o.Outcome {
		case resolver.OutcomeResolved:
			f.outcome = gitalypb.LandConflictFile_OUTCOME_RESOLVED
		case resolver.OutcomeRejected:
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_REJECTED, o.Reason
		default:
			f.outcome, f.reason = gitalypb.LandConflictFile_OUTCOME_FAILED, o.Reason
		}
	}
}

// landUnresolvedCause summarizes for the log why the files were not all resolved.
func landUnresolvedCause(files []*landFileState) string {
	var parts []string
	for _, f := range files {
		if f.outcome == gitalypb.LandConflictFile_OUTCOME_RESOLVED {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s %s", f.conflict.path, f.outcome, f.reason))
	}
	return strings.Join(parts, "; ")
}

// landWriteResolution writes a resolution (the resolver's answer, or the agent's) into the merged
// tree and commits it with parents [tip, commitID]; checkedBy names what checked it, if anything
// did. The commit message carries the trailers. built is the resolved tree when the caller already
// built it (landResolvedTree). verified says that verify.command passed on that very tree, out of
// the lane (15.9), and is the tree itself, so that a caller cannot vouch with a bool. The lane runs no
// verification: when one is configured and the tree is not the one it passed on, the resolution is
// refused here, closed, whatever the caller did.
func (s *Server) landWriteResolution(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	repo *localrepo.Repo,
	tip, commitID, mergedTree git.ObjectID,
	committer git.Signature,
	actor string,
	resp *resolver.Response,
	checkedBy string,
	built git.ObjectID,
	verified git.ObjectID,
) (landResolution, error) {
	paths := make([]string, 0, len(resp.Files))
	for _, f := range resp.Files {
		paths = append(paths, f.Path)
	}
	// The message comes first: a value that cannot be a trailer refuses the resolution before
	// anything is written.
	message, err := landResolveCommitMessage(in, commitID, resp.Resolver, checkedBy, paths)
	if err != nil {
		return landResolution{}, err
	}
	treeID := built
	if treeID == "" {
		var err error
		if treeID, err = s.landResolvedTree(ctx, repo, mergedTree, resp); err != nil {
			return landResolution{}, err
		}
	}
	// The verification ran out of the lane, on this very tree, before it got here (15.9): no
	// resolution lands on a tree it did not pass on, and no code path verifies in the lane.
	if s.resolveCfg.Verify.Enabled && (verified == "" || verified != treeID) {
		return landResolution{}, errors.New("a resolution reached the lane without the verification of its tree")
	}

	newTip, err := repo.WriteCommit(ctx, localrepo.WriteCommitConfig{
		TreeID:         treeID,
		Message:        message,
		Parents:        []git.ObjectID{tip, commitID},
		AuthorName:     committer.Name,
		AuthorEmail:    committer.Email,
		AuthorDate:     committer.When,
		CommitterName:  committer.Name,
		CommitterEmail: committer.Email,
		CommitterDate:  committer.When,
		GitConfig:      s.gitConfig,
	})
	if err != nil {
		return landResolution{}, fmt.Errorf("writing merge commit: %w", err)
	}

	resolvedPaths := make([][]byte, 0, len(paths))
	for _, p := range paths {
		resolvedPaths = append(resolvedPaths, []byte(p))
	}

	// The server keeps its own record of every resolution and of what the resolver said about
	// it, next to the trailers of the commit. The branch update may still fail or be retried, so
	// this records the resolution and not the landing.
	s.logger.WithFields(log.Fields{
		"branch":       string(in.GetBranch()),
		"tip":          tip.String(),
		"commit":       commitID.String(),
		"merge_commit": newTip.String(),
		"actor":        actor,
		"resolver":     resp.Resolver,
		"checked_by":   checkedBy,
		"paths":        paths,
		"explanation":  landTruncate(resp.Explanation, maxLoggedExplanationBytes),
	}).InfoContext(ctx, "graft: conflicts of land resolved")

	return landResolution{commit: newTip, resolvedPaths: resolvedPaths, resolver: resp.Resolver}, nil
}

// landResolvedTree writes a resolution into the merged tree and returns the tree: the exact tree
// that a land of this resolution would commit. Its object ID is what the verification is tied to.
func (s *Server) landResolvedTree(ctx context.Context, repo *localrepo.Repo, mergedTree git.ObjectID, resp *resolver.Response) (git.ObjectID, error) {
	tree, err := repo.ReadTree(ctx, git.Revision(mergedTree.String()), localrepo.WithRecursive())
	if err != nil {
		return "", fmt.Errorf("reading merged tree: %w", err)
	}
	for _, f := range resp.Files {
		if f.Action == resolver.ActionDelete {
			if err := tree.Delete(f.Path); err != nil && !errors.Is(err, localrepo.ErrEntryNotFound) {
				return "", fmt.Errorf("deleting %q: %w", f.Path, err)
			}
			continue
		}

		blob, err := repo.WriteBlob(ctx, bytes.NewReader(f.Content), localrepo.WriteBlobConfig{Path: f.Path})
		if err != nil {
			return "", fmt.Errorf("writing resolved %q: %w", f.Path, err)
		}
		entry := localrepo.TreeEntry{OID: blob, Mode: f.Mode, Path: path.Base(f.Path), Type: localrepo.Blob}
		if err := tree.Add(f.Path, entry, localrepo.WithOverwriteFile()); err != nil {
			return "", fmt.Errorf("adding resolved %q: %w", f.Path, err)
		}
	}
	if err := tree.Write(ctx, repo); err != nil {
		return "", fmt.Errorf("writing merged tree: %w", err)
	}
	return tree.OID, nil
}

// landClassify decides, for every file, whether it may be sent to the resolver, and reads the
// sides of those that may (and of those whose content decided against it, for the hunks of the
// package). The order: the path (a commit trailer must carry it, the protocol must carry it, the
// path policy must allow it and every path its sides come from), the shape of the sides, their
// size (from the object info, before reading), then the content rules of resolver.SendRules, with
// linguist-generated from the tip's attributes.
func (s *Server) landClassify(
	ctx context.Context,
	repo *localrepo.Repo,
	files []*landFileState,
	base, tip, commitID git.ObjectID,
	cache *landClassifyCache,
) error {
	if cache.tip != tip {
		return fmt.Errorf("classifying the conflicts: the cache is for tip %s, not %s", cache.tip, tip)
	}
	rules := s.conflictResolver.SendRules()
	listings := cache.listings
	for _, f := range files {
		c := f.conflict
		switch {
		case !utf8.ValidString(c.path):
			f.notSent(landNotSentInvalidUTF8)
			continue
		case !landTrailerValueValid(c.path):
			f.notSent(landNotSentTrailerUnsafe)
			continue
		case !s.conflictResolver.PathAllowed(c.path):
			f.notSent(landNotSentDenied)
			continue
		case landShapeReason(c) != "":
			f.notSent(landShapeReason(c))
			continue
		}
		// A rename makes a side come from another path than the name of the conflict, so the
		// policy is applied to the path of every side as well.
		denied, err := s.landDeniedSource(ctx, repo, listings, c, base, tip, commitID)
		if err != nil {
			return err
		}
		if denied != "" {
			f.notSent(denied)
			continue
		}

		tooLarge := false
		for _, b := range []*landBlob{c.base, c.ours, c.theirs} {
			if b == nil {
				continue
			}
			switch b.mode {
			case resolver.ModeRegular, resolver.ModeExecutable, resolver.ModeSymlink:
			default:
				f.notSent(landNotSentNotAFile)
			}
			if !f.sent() {
				break
			}
			info, err := repo.ReadObjectInfo(ctx, b.oid.Revision())
			if err != nil {
				return fmt.Errorf("reading size of %q: %w", c.path, err)
			}
			if rules.TooLarge(info.Size) {
				tooLarge = true
			}
		}
		if !f.sent() {
			continue
		}
		if tooLarge {
			f.notSent(resolver.NotSentLarge)
			continue
		}
		for i, b := range []*landBlob{c.base, c.ours, c.theirs} {
			if b == nil {
				continue
			}
			content, err := repo.ReadObject(ctx, b.oid)
			if err != nil {
				return fmt.Errorf("reading blob of %q: %w", c.path, err)
			}
			if content == nil {
				content = []byte{}
			}
			f.sides[i] = content
		}
		f.read = true
	}

	// The content rules, with linguist-generated read for every remaining path at once.
	var candidates []string
	for _, f := range files {
		if f.sent() {
			candidates = append(candidates, f.conflict.path)
		}
	}
	generated, err := cache.generatedPaths(ctx, repo, candidates)
	if err != nil {
		return err
	}
	for _, f := range files {
		if !f.sent() {
			continue
		}
		if reason := rules.Reason(f.conflict.path, generated[f.conflict.path], f.sides[0], f.sides[1], f.sides[2]); reason != "" {
			f.notSent(reason)
		}
	}
	return nil
}

// landClassifyCache holds what landClassify reads from the trees, for every call of one land onto
// one tip: the listings of the trees (by revision) and the linguist-generated attribute of the
// paths of the land, read at the tip once for all of them. Both the tip and the paths are fixed
// when it is made: a call for another tip, or for a path that is not one of the land, is an error.
type landClassifyCache struct {
	listings  map[git.ObjectID]map[git.ObjectID][]string
	tip       git.ObjectID
	paths     []string
	known     map[string]bool
	generated map[string]bool
}

// newLandClassifyCache returns the cache of the land of files onto tip.
func newLandClassifyCache(files []*landFileState, tip git.ObjectID) *landClassifyCache {
	c := &landClassifyCache{listings: map[git.ObjectID]map[git.ObjectID][]string{}, tip: tip, known: map[string]bool{}}
	for _, f := range files {
		c.paths = append(c.paths, f.conflict.path)
		c.known[f.conflict.path] = true
	}
	return c
}

// generatedPaths returns the linguist-generated attribute at the tip of the land of paths, every
// one a path of the land. The attributes of all the paths of the land are read on first use, with
// one git check-attr.
func (c *landClassifyCache) generatedPaths(ctx context.Context, repo *localrepo.Repo, paths []string) (map[string]bool, error) {
	for _, p := range paths {
		if !c.known[p] {
			return nil, fmt.Errorf("classifying the conflicts: %q is not a conflicting path of the land", p)
		}
	}
	if len(paths) == 0 {
		return map[string]bool{}, nil
	}
	if c.generated == nil {
		generated, err := landGeneratedPaths(ctx, repo, c.tip, c.paths)
		if err != nil {
			return nil, err
		}
		c.generated = generated
	}
	return c.generated, nil
}

// landShapeReason returns why the shape of the sides keeps a conflict from the resolver, or "". A
// file/directory conflict has no version of the file on either side under its name. A conflict
// whose base is absent while one side is absent is not a conflict between two versions of a file,
// since no merge produces it without a rename; it cannot be answered reliably, so it fails closed.
func landShapeReason(c landFileConflict) string {
	switch {
	case c.ours == nil && c.theirs == nil:
		return landNotSentNoSide
	case c.base == nil && (c.ours == nil || c.theirs == nil):
		return landNotSentNoBaseOneSide
	}
	return ""
}

// landDeniedSource returns the reason when a side of the conflict comes from a path that may not
// be sent, and "" when every side's path may. merge-tree names a conflict after the path of the
// merged tree, but with a rename a side comes from another path: the base and one side may live
// under the old name. Without this check the content of a denied path could reach the resolver
// under an allowed name.
//
// A side lives at the name of the conflict when the tree of its commit holds the same blob there.
// Otherwise every path of that tree holding the blob is a possible source, and all of them must be
// allowed. A side whose blob is in no path of its tree is not sent, nor is a base when the merge
// has none to look it up in. Only trees are read, no blob.
func (s *Server) landDeniedSource(
	ctx context.Context,
	repo *localrepo.Repo,
	listings map[git.ObjectID]map[git.ObjectID][]string,
	c landFileConflict,
	base, tip, commitID git.ObjectID,
) (string, error) {
	for _, side := range []landSide{{c.base, base}, {c.ours, tip}, {c.theirs, commitID}} {
		if side.blob == nil {
			continue
		}
		if side.rev == "" {
			return "the path has a base side but the merge has no merge base to find it in", nil
		}
		sources, err := landSourcePaths(ctx, repo, listings, side.rev, side.blob.oid, c.path)
		if err != nil {
			return "", err
		}
		if len(sources) == 0 {
			return fmt.Sprintf("a side is in no path of %s", side.rev), nil
		}
		for _, p := range sources {
			if !landTrailerValueValid(p) || !s.conflictResolver.PathAllowed(p) {
				return fmt.Sprintf("a side comes from path %q, which may not be sent to the resolver", p), nil
			}
		}
	}
	return "", nil
}

// landTruncate returns s cut to at most n bytes, with a marker when it was cut. The result is
// valid UTF-8: a rune cut in half and any invalid byte are dropped.
func landTruncate(s string, n int) string {
	if len(s) <= n {
		return strings.ToValidUTF8(s, "")
	}
	return strings.ToValidUTF8(s[:n], "") + " [truncated]"
}

// landResolverNameValid reports whether name can be the name of the resolver, which the answer
// chooses and which is written into a commit trailer and the response. It must be non-empty, at
// most maxResolverNameBytes long, and a valid trailer value (landTrailerValueValid).
func landResolverNameValid(name string) bool {
	return len(name) <= maxResolverNameBytes && landTrailerValueValid(name)
}

// landSide is one present side of a conflicting file and the commit whose tree it comes from.
type landSide struct {
	blob *landBlob
	rev  git.ObjectID
}

// landSourcePaths returns the paths of the tree of rev that the blob of a side of the conflict p
// may come from, none when the blob is in no path. listings caches the listing of every tree
// read, by commit.
func landSourcePaths(
	ctx context.Context,
	repo *localrepo.Repo,
	listings map[git.ObjectID]map[git.ObjectID][]string,
	rev, blob git.ObjectID,
	p string,
) ([]string, error) {
	// The common case: the side was not renamed and lives at the name of the conflict.
	info, err := repo.ReadObjectInfo(ctx, git.Revision(rev.String()+":"+p))
	if err == nil && info.Oid == blob {
		return []string{p}, nil
	}
	var notFound localrepo.InvalidObjectError
	if err != nil && !errors.As(err, &notFound) {
		return nil, fmt.Errorf("looking up %q in %s: %w", p, rev, err)
	}

	listing, ok := listings[rev]
	if !ok {
		if listing, err = landListTree(ctx, repo, rev); err != nil {
			return nil, err
		}
		listings[rev] = listing
	}

	return listing[blob], nil
}

// landListTree lists the tree of rev recursively and returns the paths of every object it holds.
func landListTree(ctx context.Context, repo *localrepo.Repo, rev git.ObjectID) (map[git.ObjectID][]string, error) {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "ls-tree",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "-r"},
				gitcmd.Flag{Name: "-z"},
				gitcmd.Flag{Name: "--full-tree"},
			},
			Args: []string{rev.String()},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, fmt.Errorf("ls-tree %s: %w, stderr: %q", rev, err, stderr.String())
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	paths := map[git.ObjectID][]string{}
	for _, record := range strings.Split(stdout.String(), "\x00") {
		if record == "" {
			continue
		}
		// "<mode> SP <type> SP <oid> TAB <path>"
		meta, p, ok := strings.Cut(record, "\t")
		parts := strings.Fields(meta)
		if !ok || len(parts) != 3 {
			return nil, fmt.Errorf("unexpected ls-tree record %q", record)
		}
		if parts[1] != "blob" {
			continue
		}
		oid, err := objectHash.FromHex(parts[2])
		if err != nil {
			return nil, fmt.Errorf("parsing object ID of %q: %w", p, err)
		}
		paths[oid] = append(paths[oid], p)
	}
	return paths, nil
}

// landResolveCommitMessage returns the message of the resolution commit: the merge message or
// the default one, followed by the trailers of landResolutionTrailers. An agent's resolution
// records in Graft-Checked-By whether the resolver's checks ran on it: without a resolver it got
// only the marker rule and the server's structural validation.
func landResolveCommitMessage(in *gitalypb.UserLandRequest, commitID git.ObjectID, resolverName, checkedBy string, paths []string) (string, error) {
	trailers, err := landResolutionTrailers(resolverName, checkedBy, paths)
	if err != nil {
		return "", err
	}
	subject := strings.TrimRight(string(in.GetMergeMessage()), "\n")
	if subject == "" {
		subject = fmt.Sprintf("Merge %s into %s", landShortOID(commitID), string(in.GetBranch()))
	}
	return subject + "\n\n" + trailers, nil
}

// landShortOID returns the abbreviated object ID used in default messages.
func landShortOID(oid git.ObjectID) string {
	const short = 7
	s := oid.String()
	if len(s) > short {
		return s[:short]
	}
	return s
}

// landCommitBody returns the message of a commit.
func landCommitBody(ctx context.Context, repo *localrepo.Repo, commit git.ObjectID) (string, error) {
	c, err := repo.ReadCommit(ctx, commit.Revision())
	if err != nil {
		return "", fmt.Errorf("reading commit message of %s: %w", commit, err)
	}
	return string(c.GetBody()), nil
}

// landMergeBase returns the merge base of a and b. Unrelated histories have none. A criss-cross
// history has several merge bases, whose number is returned: the base the resolver would be given
// would differ from the base of the recursive merge strategy, so such landings are not resolved,
// and hasBase is false.
func landMergeBase(ctx context.Context, repo *localrepo.Repo, a, b git.ObjectID) (base git.ObjectID, hasBase bool, bases []git.ObjectID, err error) {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{Name: "merge-base", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--all"}}, Args: []string{a.String(), b.String()}},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		if exitStatus, ok := command.ExitStatus(err); ok && exitStatus == 1 {
			return "", false, nil, nil
		}
		return "", false, nil, fmt.Errorf("merge-base: %w, stderr: %q", err, stderr.String())
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", false, nil, fmt.Errorf("detecting object hash: %w", err)
	}
	for _, hex := range strings.Fields(stdout.String()) {
		oid, err := objectHash.FromHex(hex)
		if err != nil {
			return "", false, nil, fmt.Errorf("parsing merge base: %w", err)
		}
		bases = append(bases, oid)
	}
	if len(bases) != 1 {
		return "", false, bases, nil
	}
	return bases[0], true, bases, nil
}

// landMergeTree runs merge-tree of tip and commitID. It returns the tree of the merge, which holds
// the conflicting files in their conflicted state, and the conflicting files with their stage
// entries.
//
// Without --name-only, merge-tree prints a record per stage of every conflicting file, as
// "<mode> SP <oid> SP <stage> TAB <path>", where stage 1 is the base, 2 ours and 3 theirs. The
// sides are taken from these records and not by reading the path again on each side, because a
// rename makes the path of a side differ from the name of the conflict.
func landMergeTree(
	ctx context.Context,
	repo *localrepo.Repo,
	tip, commitID, base git.ObjectID,
	hasBase bool,
) (git.ObjectID, []landFileConflict, []landMergeMessage, bool, error) {
	flags := []gitcmd.Option{
		gitcmd.Flag{Name: "-z"},
		gitcmd.Flag{Name: "--write-tree"},
		gitcmd.Flag{Name: "--allow-unrelated-histories"},
	}
	if hasBase {
		flags = append(flags, gitcmd.ValueFlag{Name: "--merge-base", Value: base.String()})
	}

	var stdout, stderr bytes.Buffer
	clean := true
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{Name: "merge-tree", Flags: flags, Args: []string{tip.String(), commitID.String()}},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		clean = false
		// Exit status 1 means the merge has conflicts, and the tree is printed regardless.
		if exitStatus, ok := command.ExitStatus(err); !ok || exitStatus != 1 {
			return "", nil, nil, false, fmt.Errorf("merge-tree: %w, stderr: %q", err, stderr.String())
		}
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", nil, nil, false, fmt.Errorf("detecting object hash: %w", err)
	}

	// The output is the tree OID, the conflicted file records and, after an empty field, the
	// informational messages.
	fields := strings.Split(stdout.String(), "\x00")
	treeOID, err := objectHash.FromHex(fields[0])
	if err != nil {
		return "", nil, nil, false, fmt.Errorf("parsing merge tree: %w", err)
	}

	var conflicts []landFileConflict
	index := map[string]int{}
	rest := fields[1:]
	for len(rest) > 0 {
		record := rest[0]
		rest = rest[1:]
		if record == "" {
			break
		}

		meta, p, ok := strings.Cut(record, "\t")
		parts := strings.Fields(meta)
		if !ok || p == "" || len(parts) != 3 {
			return "", nil, nil, false, fmt.Errorf("unexpected conflicted file record %q", record)
		}

		oid, err := objectHash.FromHex(parts[1])
		if err != nil {
			return "", nil, nil, false, fmt.Errorf("parsing blob ID of %q: %w", p, err)
		}

		i, found := index[p]
		if !found {
			i = len(conflicts)
			index[p] = i
			conflicts = append(conflicts, landFileConflict{path: p})
		}
		// The index is used and not a pointer, since appending may move the slice.
		c := &conflicts[i]

		var side **landBlob
		switch parts[2] {
		case "1":
			side = &c.base
		case "2":
			side = &c.ours
		case "3":
			side = &c.theirs
		default:
			return "", nil, nil, false, fmt.Errorf("unexpected stage %q of conflicted path %q", parts[2], p)
		}
		if *side != nil {
			return "", nil, nil, false, fmt.Errorf("conflicted path %q has two entries of stage %s", p, parts[2])
		}
		*side = &landBlob{mode: parts[0], oid: oid}
	}

	messages, err := landMergeMessages(rest)
	if err != nil {
		return "", nil, nil, false, err
	}
	return treeOID, conflicts, messages, clean, nil
}

// landMergeMessage is one of merge-tree's messages about the merge.
type landMergeMessage struct {
	paths   []string
	kind    string
	message string
}

// landMergeMessages parses the informational messages of merge-tree -z, which follow the empty
// field after the conflicted file records: each is the number of paths, the paths, the type (such
// as "CONFLICT (rename/delete)" or "Auto-merging") and the message, NUL-terminated. The output ends
// with a NUL, so the last field is empty.
func landMergeMessages(fields []string) ([]landMergeMessage, error) {
	var messages []landMergeMessage
	for len(fields) > 0 && !(len(fields) == 1 && fields[0] == "") {
		n, err := strconv.Atoi(fields[0])
		if err != nil || n < 0 || len(fields) < n+3 {
			return nil, fmt.Errorf("unexpected merge-tree message %q", fields)
		}
		messages = append(messages, landMergeMessage{
			paths:   slices.Clone(fields[1 : 1+n]),
			kind:    fields[1+n],
			message: strings.TrimRight(fields[2+n], "\n"),
		})
		fields = fields[3+n:]
	}
	return messages, nil
}

// isLandMergeConflict reports whether a landing failed because the change does not apply cleanly
// onto the tip.
func isLandMergeConflict(err error) bool {
	for _, detail := range status.Convert(err).Details() {
		if landErr, ok := detail.(*gitalypb.UserLandError); ok && landErr.GetMergeConflict() != nil {
			return true
		}
	}
	return false
}
