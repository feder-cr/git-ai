package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// A resolution that a model made lands only through the operator's verification (PLAN10 14, 15.9):
// when verify.command ran and passed on the exact tree that lands. Otherwise it is a proposal, which
// the agent reviews, edits or replaces, and lands as its own. The server keeps the proposals of a
// failed land under a hidden reference, which the agent reads by object ID (GetObjects).

// landProposalPruneErrors counts the prunings of proposal references that failed after a land.
var landProposalPruneErrors = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_land_proposal_prune_errors_total",
	Help: "Prunings of the proposal references of a branch that failed after a land.",
})

// landProposalSweepTimeouts counts the sweeps of a branch's proposals that a land gave up on after
// proposals.SweepTimeout. A later sweep or the housekeeping pass removes what they left.
var landProposalSweepTimeouts = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_land_proposal_sweep_timeouts_total",
	Help: "Sweeps of the proposal references of a branch that a land gave up on because they ran out of time.",
})

// landProposalWriteErrors counts the answers that could not be kept as proposals: the land still
// fails with its package, whose files say "proposal not stored".
var landProposalWriteErrors = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_land_proposal_write_errors_total",
	Help: "Resolver answers of a failed land that could not be kept as proposals.",
})

// landProposedReason is the reason of a file that a model resolved and that did not land: a model's
// resolution lands only after verify.command passed on the merged tree, and the reason that it did
// not follows it (landNoVerifyReason, the output of the verification, or why it could not run).
const landProposedReason = "a model resolved the file: it is proposed (see proposal), and lands by itself only after verify.command passed on the merged tree; land it, edited or not, as your own resolution"

// landVerifyDisabledReason follows landProposedReason when a verify.command is configured and
// verify.enabled is false.
const landVerifyDisabledReason = "verification is disabled (verify.enabled is false), so nothing verifies a model's resolution"

// landNoVerifyReason follows landProposedReason when no verify.command is configured.
const landNoVerifyReason = "no verify.command is configured, so nothing verifies a model's resolution"

// landHeldReason is the reason of a file that the resolver holds back: the second model of the llm
// strategy's agree_with gate did not agree with the first, so the resolution is a proposal that
// does not land by itself, even on a tree that verify.command passed on. The reason starts with
// "held:" and the resolver's own words (hold), and says what that means for the land.
func landHeldReason(hold string) string {
	if !strings.HasPrefix(hold, "held: ") {
		hold = "held: " + hold
	}
	return hold + " (the proposal does not land by itself, even if verify.command passes)"
}

// landAnswerHolds reports whether a resolver's answer holds back a file (ResponseFile.Hold): such an
// answer never lands, whatever verify.command says.
func landAnswerHolds(answer *resolver.Response) bool {
	for _, f := range answer.Files {
		if f.Hold != "" {
			return true
		}
	}
	return false
}

// landNormalizeOrigins gives each file of an answer its origin, as the server reads it: OriginRule
// only for a file that says it is a rule's and is not marked as a proposal, OriginModel for any
// other. It is done when the answer arrives, so that the answers kept by their keys and the rounds
// that reuse them never depend on how a resolver spelled the mark, nor on its name.
func landNormalizeOrigins(answer *resolver.Response) {
	for i := range answer.Files {
		if answer.Files[i].FromModel() {
			answer.Files[i].Origin = resolver.OriginModel
		} else {
			answer.Files[i].Origin = resolver.OriginRule
		}
	}
}

// landAnswerProposes reports whether a resolver's answer holds a model's resolution: a file that is
// not marked as a rule's. The name of the resolver decides nothing: a list of names joined over
// rounds, cut to ",+N", or spelled in a way nobody foresaw, can neither hide a model's file nor
// make a rule's one a model's. This is fail-safe: a file that says nothing is a model's.
func landAnswerProposes(answer *resolver.Response) bool {
	for _, f := range answer.Files {
		if f.FromModel() {
			return true
		}
	}
	return false
}

// landProposalExplanation is the explanation of a file's proposal: the composition report of the
// resolver (what the proposal keeps, drops and repeats of the two sides' changes, PLAN10 15.3), then
// the explanation of the answer, cut to the size of a reason.
func landProposalExplanation(f resolver.ResponseFile, explanation string) string {
	if len(f.Report) == 0 {
		return landCutString(explanation, landPackageReasonBytes)
	}
	text := "report: " + strings.Join(f.Report, "; ")
	if explanation != "" {
		text += ". " + explanation
	}
	return landCutString(text, landPackageReasonBytes)
}

// landWriteProposals keeps the resolver's answer for the agent: the written contents as blobs, a
// commit whose tree holds them by path (its parents the tip and the landed commit), and the
// proposal reference of (branch, commit) pointing at it, replacing an older one. The objects are
// written into a quarantine of their own, not the quarantine of the land, which a failed land
// discards, and migrated into the repository before the reference update: a loose object written
// straight into the repository would race housekeeping's removal of empty loose object
// directories, which the migration survives. The reference is updated through the reference
// transaction (and so the voter in authoritative storage mode). It sets the proposal of every
// file of the listing and returns the reference. The branch's older proposals are pruned after; a
// failed pruning is logged and counted, and fails nothing.
func (s *Server) landWriteProposals(ctx context.Context, in *gitalypb.UserLandRequest, l landListing, answer *resolver.Response, committer git.Signature) (git.ReferenceName, error) {
	// Who caused the proposals is in their commit, the trace of the reference: the update itself
	// is the server's (landUpdateProposalRefs).
	actor := landBatchActor(ctx, in)
	// The date of the proposals' commit is the server's clock, never the request's timestamp:
	// their age decides when they expire.
	now := time.Now()
	start := now
	repo := s.localRepoFactory.Build(in.GetRepository())
	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, in.GetRepository())
	if err != nil {
		return "", err
	}
	defer cleanup()
	tree := &localrepo.TreeEntry{Type: localrepo.Tree, Mode: "040000"}
	byPath := make(map[string]*gitalypb.LandConflictProposal, len(answer.Files))
	for _, f := range answer.Files {
		p := &gitalypb.LandConflictProposal{Action: f.Action, Resolver: answer.Resolver, Explanation: landProposalExplanation(f, answer.Explanation)}
		if f.Action == resolver.ActionWrite {
			blob, err := quarantineRepo.WriteBlob(ctx, bytes.NewReader(f.Content), localrepo.WriteBlobConfig{Path: f.Path})
			if err != nil {
				return "", fmt.Errorf("writing the proposal of %q: %w", f.Path, err)
			}
			entry := localrepo.TreeEntry{OID: blob, Mode: f.Mode, Path: path.Base(f.Path), Type: localrepo.Blob}
			if err := tree.Add(f.Path, entry, localrepo.WithOverwriteFile()); err != nil {
				return "", fmt.Errorf("adding the proposal of %q: %w", f.Path, err)
			}
			p.Oid, p.Mode = blob.String(), f.Mode
		}
		byPath[f.Path] = p
	}
	if err := tree.Write(ctx, quarantineRepo); err != nil {
		return "", fmt.Errorf("writing the tree of the proposals: %w", err)
	}
	commit, err := quarantineRepo.WriteCommit(ctx, localrepo.WriteCommitConfig{
		TreeID:         tree.OID,
		Message:        fmt.Sprintf("graft: proposals for landing %s onto %s\n\nResolver: %s\nRequested-By: %s\n", l.commitID, l.tip, landTrailerValueOrQuoted(answer.Resolver), landTrailerValueOrQuoted(actor)),
		Parents:        []git.ObjectID{l.tip, l.commitID},
		AuthorName:     committer.Name,
		AuthorEmail:    committer.Email,
		AuthorDate:     now,
		CommitterName:  committer.Name,
		CommitterEmail: committer.Email,
		CommitterDate:  now,
		GitConfig:      s.gitConfig,
	})
	if err != nil {
		return "", fmt.Errorf("writing the commit of the proposals: %w", err)
	}

	ref := proposals.Ref(string(in.GetBranch()), l.commitID)
	old, err := repo.GetReference(ctx, ref)
	oldOID := git.ObjectID("")
	switch {
	case err == nil:
		oldOID = git.ObjectID(old.Target)
	case !errors.Is(err, git.ErrReferenceNotFound):
		return "", fmt.Errorf("reading %s: %w", ref, err)
	}
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", fmt.Errorf("detecting object hash: %w", err)
	}
	if oldOID == "" {
		oldOID = objectHash.ZeroOID
	}
	if err := quarantineDir.Migrate(ctx); err != nil {
		return "", fmt.Errorf("migrating the proposals: %w", err)
	}
	if err := proposals.Update(ctx, repo, []proposals.Op{{Name: ref, NewOID: commit, OldOID: oldOID}}); err != nil {
		return "", err
	}
	s.logger.WithFields(log.Fields{"ref": ref.String(), "actor": actor, "resolver": answer.Resolver, "seconds": time.Since(start).Seconds()}).InfoContext(ctx, "graft: kept the resolver's answer as proposals")

	for _, f := range l.files {
		f.proposal = byPath[f.conflict.path]
	}
	s.landPruneProposals(ctx, repo, string(in.GetBranch()))
	return ref, nil
}

// landAfterLand is what a land does once it landed, before it answers. Its tickets go first: the
// land they protected is done, and the lands that wait for their turn on these files need not wait
// for the proposals to be pruned too.
func (s *Server) landAfterLand(ctx context.Context, in *gitalypb.UserLandRequest, pre *landPreResolution) {
	if pre != nil {
		pre.ticket.release()
	}
	s.landPruneAfterLand(ctx, in)
}

// landPruneAfterLand prunes the proposals of the branch after a land that asked for resolution or
// brought one, and drops the proposal of the change that landed. Only those lands prune, so that a
// plain land pays nothing for proposals; the bound of landMaxProposalsPerBranch holds anyway, since
// only those lands write proposals. The land has happened: a failure is logged and counted, and
// the land's result stands.
func (s *Server) landPruneAfterLand(ctx context.Context, in *gitalypb.UserLandRequest) {
	if !in.GetResolveConflicts() && in.GetResolution() == nil {
		return
	}
	repo := s.localRepoFactory.Build(in.GetRepository())
	branch := string(in.GetBranch())
	// The proposal of the change that landed is this land's own, a deletion no other land shares;
	// what the branch has too many of is swept, by one land at a time.
	if commitID, err := repo.ResolveRevision(ctx, git.Revision(in.GetCommitId()+"^{commit}")); err == nil {
		if err := s.proposalSweeper.Drop(ctx, repo, proposals.Ref(branch, commitID)); err != nil {
			landProposalPruneErrors.Inc()
			s.logger.WithError(err).WithField("branch", branch).ErrorContext(ctx, "graft: dropping the proposal of the change that landed failed")
		}
	}
	s.landPruneProposals(ctx, repo, branch)
}

// landPruneProposals sweeps the proposals of a branch (proposals.Sweeper), for at most
// proposals.SweepTimeout where it can wait for another land. A failure is logged and counted, and
// fails nothing: the proposals are an aid, and the next sweep decides anew. Running out of time is
// counted apart, as load and not as a failure.
func (s *Server) landPruneProposals(ctx context.Context, repo *localrepo.Repo, branch string) {
	ttl, err := s.resolveCfg.ProposalTTLOrDefault()
	if err == nil {
		err = s.proposalSweeper.Sweep(ctx, repo, branch, ttl, time.Now())
	}
	switch {
	case err == nil:
	case errors.Is(err, proposals.ErrSweepTimedOut):
		landProposalSweepTimeouts.Inc()
		s.logger.WithError(err).WithField("branch", branch).WarnContext(ctx, "graft: the sweep of the proposals of the branch ran out of time")
	default:
		landProposalPruneErrors.Inc()
		s.logger.WithError(err).WithField("branch", branch).ErrorContext(ctx, "graft: pruning the proposals of the branch failed")
	}
}

// landTrailerValueOrQuoted returns v when it is a valid trailer value (landTrailerValueValid), and
// v quoted otherwise, so that a value the caller chose (the user ID of the request) can never add
// a line to the proposals' message.
func landTrailerValueOrQuoted(v string) string {
	if landTrailerValueValid(v) {
		return v
	}
	return strconv.QuoteToASCII(v)
}

// decideLand is the one decision point of a land that asked the server to resolve its conflicts
// (PLAN10 15.4): it lands only when the resolver answered and every conflicting file is resolved,
// by a rule (RESOLVED) or by a model (PROPOSED). The one rule: a resolution lands only on a tree
// that verify.command ran and passed on, whenever a verification is configured (verifyRequired), and a
// model's resolution lands on no other, with or without one configured (without, it never lands).
// tree is the tree the land would write and verified the tree the verification passed on, compared
// by object ID, not by the tip they were made at; a verified tree is never the empty ID. Every
// verification, whatever made the resolution, runs out of the lane, so that the lane only compares.
func decideLand(answer *resolver.Response, files []*landFileState, tree, verified git.ObjectID, verifyRequired bool) bool {
	if answer == nil {
		return false
	}
	// A file the resolver holds back never lands: the second model did not agree with the first.
	if landAnswerHolds(answer) {
		return false
	}
	proposes := landAnswerProposes(answer)
	if (proposes || verifyRequired) && (verified == "" || verified != tree) {
		return false
	}
	for _, f := range files {
		switch f.outcome {
		case gitalypb.LandConflictFile_OUTCOME_RESOLVED:
		case gitalypb.LandConflictFile_OUTCOME_PROPOSED:
			// Only an answer that proposes has a PROPOSED file, and it passed the rule above.
			if !proposes {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// landProposalsNotStored marks every file of the answer: its proposal was not stored, and why.
func landProposalsNotStored(files []*landFileState, answer *resolver.Response, cause error) {
	inAnswer := make(map[string]bool, len(answer.Files))
	for _, f := range answer.Files {
		inAnswer[f.Path] = true
	}
	for _, f := range files {
		if !inAnswer[f.conflict.path] {
			continue
		}
		f.proposal = nil
		note := "proposal not stored: " + landProposalWriteCause(cause)
		if f.reason == "" {
			f.reason = note
		} else {
			f.reason += "; " + note
		}
	}
}

// landProposalWriteCause is what the package says of a failed write of the proposals: a class of
// the failure, never the error itself, whose git output may name references or the paths of the
// storage. The error is logged where the write failed.
func landProposalWriteCause(err error) string {
	var (
		locked    updateref.AlreadyLockedError
		mismatch  updateref.MismatchingStateError
		exists    updateref.ReferenceAlreadyExistsError
		directory updateref.FileDirectoryConflictError
		inTx      updateref.InTransactionConflictError
		multiple  updateref.MultipleUpdatesError
	)
	switch {
	case errors.As(err, &locked), errors.As(err, &mismatch), errors.As(err, &exists), errors.As(err, &directory),
		errors.As(err, &inTx), errors.As(err, &multiple), errors.Is(err, updateref.ErrPackedRefsLocked):
		return "reference update conflict"
	case errors.Is(err, storage.ErrRepositoryNotFound), errors.Is(err, storage.ErrStorageNotFound),
		errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT), errors.Is(err, syscall.EROFS), errors.Is(err, syscall.EIO):
		return "storage unavailable"
	default:
		return "write failed"
	}
}

// landPolicyDeniedError is the policy's refusal of a land that asked the server to resolve: the
// agent may not update the branch, so nothing is resolved and no proposal is kept. err is the
// refusal (PermissionDenied).
type landPolicyDeniedError struct{ err error }

func (e *landPolicyDeniedError) Error() string { return e.err.Error() }

func (e *landPolicyDeniedError) Unwrap() error { return e.err }
