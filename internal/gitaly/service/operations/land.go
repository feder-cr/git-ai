package operations

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	gitupdateref "github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/storagemgr/partition/conflict/refdb"
	"github.com/feder-cr/git-ai/internal/graft/ancestry"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/derived"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// defaultLandMaxAttempts is the number of compare-and-swap attempts UserLand makes when the
// request does not set max_attempts.
const defaultLandMaxAttempts = 20

const (
	// landBackoffBase is the backoff ceiling after the first lost race. The ceiling doubles with
	// every further attempt up to landBackoffMax.
	landBackoffBase = 2 * time.Millisecond
	// landBackoffMax caps the backoff ceiling.
	landBackoffMax = 100 * time.Millisecond
)

// landConflict reports that the compare-and-swap of the branch lost a race against a concurrent
// writer. The attempt is discarded and UserLand retries from the top.
type landConflict struct {
	reference      git.ReferenceName
	oldOID, newOID git.ObjectID
	cause          error
}

// Error returns the error message.
func (e landConflict) Error() string {
	return fmt.Sprintf("reference %s moved concurrently: %v", e.reference, e.cause)
}

// Unwrap returns the underlying reference update error.
func (e landConflict) Unwrap() error {
	return e.cause
}

// UserLand lands commit_id onto a branch. See the proto documentation of the RPC for the
// semantics. Every attempt works in its own quarantine directory: the tip is read, the new
// history is computed and the branch is updated with a compare-and-swap against the tip that
// was read. If the branch moved in the meantime, the attempt is retried.
//
// On MVCC repositories, the attempt runs against a snapshot that is pinned for the whole RPC.
// Retrying within the RPC would read the same tip again, so the conflict is reported to the
// client instead and the client has to retry the landing.
func (s *Server) UserLand(ctx context.Context, in *gitalypb.UserLandRequest) (*gitalypb.UserLandResponse, error) {
	if err := validateUserLandRequest(ctx, s.locator, in); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	committer, err := git.SignatureFromRequest(in)
	if err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	// The check of an agent's resolution runs an external process: it runs here, before the land
	// enters its lane, so that no branch is held while it runs (landPrecheckResolution).
	var precheck *landPrecheck
	if in.GetResolution() != nil && (s.conflictResolver != nil || s.resolveCfg.Verify.Enabled) {
		if precheck, err = s.landPrecheckInQuarantine(ctx, in); err != nil {
			return nil, err
		}
	}

	// A land that asks the server to resolve its conflicts is resolved out of the lane (PLAN10
	// 15.8).
	if in.GetResolveConflicts() && in.GetResolution() == nil {
		return s.landResolving(ctx, in, committer)
	}
	return s.landOnce(ctx, in, committer, precheck, nil)
}

// landOnce lands the request once through its lane, in a batch or alone, with the precheck of an
// agent's resolution or the pre-resolution of the server's, if any, and prunes the branch's
// proposals once it landed.
func (s *Server) landOnce(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature, precheck *landPrecheck, pre *landPreResolution) (*gitalypb.UserLandResponse, error) {
	if s.landBatchable(ctx, in) {
		resp, err := s.userLandBatched(ctx, in, committer, precheck, pre)
		if err != nil {
			return nil, err
		}
		s.landPruneAfterLand(ctx, in)
		return resp, nil
	}

	resp, err := s.landWithRetries(ctx, in, committer, precheck, pre)
	if err != nil {
		return nil, err
	}
	s.landPruneAfterLand(ctx, in)

	// A request that lands alone is a batch of one.
	resp.BatchSize = 1
	resp.BatchPosition = 1

	return resp, nil
}

// landPrecheckInQuarantine runs the precheck in a quarantine that it discards, even on a panic:
// the precheck merges the tip and the change, which writes objects, and it leaves nothing in the
// repository, whatever the storage mode.
func (s *Server) landPrecheckInQuarantine(ctx context.Context, in *gitalypb.UserLandRequest) (*landPrecheck, error) {
	_, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, in.GetRepository())
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return s.landPrecheckResolution(ctx, in, quarantineRepo)
}

// landWithRetries lands the request alone, retrying the compare-and-swap while the branch moves. It
// makes at most max_attempts attempts.
//
// On MVCC repositories, the attempt runs against a snapshot that is pinned for the whole RPC.
// Retrying within the RPC would read the same tip again, so the conflict is reported to the
// client instead and the client has to retry the landing.
func (s *Server) landWithRetries(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature, precheck *landPrecheck, pre *landPreResolution) (*gitalypb.UserLandResponse, error) {
	snapshotPinned := landSnapshotPinned(ctx)

	maxAttempts := int(in.GetMaxAttempts())
	if maxAttempts == 0 {
		maxAttempts = defaultLandMaxAttempts
	}

	for attempt := 1; ; attempt++ {
		resp, err := s.landAttempt(ctx, in, committer, precheck, pre)
		if err == nil {
			resp.Attempts = int32(attempt)
			return resp, nil
		}

		var conflict landConflict
		if !errors.As(err, &conflict) {
			return nil, err
		}

		if err := landLostRace(in, pre, conflict, attempt, maxAttempts, snapshotPinned); err != nil {
			return nil, err
		}

		if err := waitLandBackoff(ctx, attempt); err != nil {
			return nil, structerr.NewCanceled("waiting to retry landing: %w", err)
		}

		// In authoritative storage mode the branch may have moved on another node: the next
		// attempt must read the tip that the database has (see doc/graft/STORAGE.md).
		if err := s.ensureStorageFresh(ctx, in.GetRepository()); err != nil {
			return nil, err
		}
	}
}

// landAncestry returns the ancestry checker of one landing attempt. The graph index is used only
// when [graft.land] graph_ancestry is set; otherwise the checker runs git. The index is stored in
// the derived-data store unless [graft.derived] disabled is set. The returned function
// releases the object reader of the checker and must be called once the attempt is over.
func (s *Server) landAncestry(ctx context.Context, quarantineRepo *localrepo.Repo) (ancestry.Checker, func(), error) {
	if !s.graft.Land.GraphAncestry {
		return ancestry.New(ctx, false, quarantineRepo, nil, nil, s.logger), func() {}, nil
	}

	// The quarantine repository reads the objects of the request, which the landed commit may be
	// one of.
	reader, release, err := s.catfileCache.ObjectReader(ctx, quarantineRepo)
	if err != nil {
		return nil, nil, structerr.NewInternal("reading objects for ancestry: %w", err)
	}

	// The quarantine shares the path of the repository, so the stored entries are those of the
	// repository itself.
	repoPath, err := quarantineRepo.Path(ctx)
	if err != nil {
		release()
		return nil, nil, structerr.NewInternal("getting repository path for ancestry: %w", err)
	}

	// With [graft.derived] disabled = true nothing is written to the derived-data store: the
	// graph entries are computed in memory for this attempt only.
	var store *derived.Store
	if !s.graft.Derived.Disabled {
		store = derived.NewStore(repoPath)
	}

	checker := ancestry.New(ctx, true, quarantineRepo, reader, store, s.logger)
	return checker, release, nil
}

// landAttempt performs a single compare-and-swap attempt of UserLand. It returns a landConflict
// if the branch was moved by a concurrent writer, in which case the caller retries.
func (s *Server) landAttempt(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature, precheck *landPrecheck, pre *landPreResolution) (*gitalypb.UserLandResponse, error) {
	attempt, cleanup, err := s.newLandingAttempt(ctx, in.GetRepository(), landPublishLoose, ignoreLandPhase)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	reference := git.NewReferenceNameFromBranchName(string(in.GetBranch()))
	tip, err := attempt.Tip(ctx, reference)
	if err != nil {
		return nil, err
	}

	// The request is decided as a member of a batch is (landingAttempt.decide). The policy is not
	// evaluated here: the update runs with the identity of the request, so the hooks judge it.
	req := &landBatchRequest{ctx: ctx, in: in, committer: committer, actor: landBatchActor(ctx, in), precheck: precheck, preResolution: pre}
	step, err := attempt.decide(ctx, tip, req)
	if err != nil {
		return nil, err
	}

	oldTip := tip
	switch {
	case step.alreadyLanded:
		resp := landResponse(tip, tip, false, true, false)
		if step.earlier != nil {
			step.earlier.setOn(resp)
		}
		return resp, nil
	case step.created:
		oldTip = attempt.objectHash.ZeroOID
	case step.tip == tip:
		// The change became empty on top of the tip: the landing leaves the branch unchanged.
		return landResponse(tip, tip, false, false, false), nil
	}

	if err := attempt.publish(ctx); err != nil {
		return nil, err
	}

	lands := []changelog.Land{{Original: step.commit.String(), Landed: step.tip.String()}}
	if err := s.updateLandedReference(ctx, in, attempt.quarantineDir, attempt.repo, reference, oldTip, step.tip, step.rewritten, lands); err != nil {
		return nil, err
	}

	resp := landResponse(oldTip, step.tip, step.rewritten, false, step.created)
	if step.resolution != nil {
		resp.ResolvedPaths = step.resolution.resolvedPaths
		resp.Resolver = step.resolution.resolver
		step.resolution.landed()
	}
	return resp, nil
}

// updateLandedReference performs the compare-and-swap of the branch, recording lands, the
// identities of the changes it lands (doc/graft/PLAN11.md), in its changelog entry. Hook rejections
// are returned as PermissionDenied errors, lost races as landConflict and invalid requests or
// conflicting references as InvalidArgument or FailedPrecondition. Everything else is Internal.
func (s *Server) updateLandedReference(
	ctx context.Context,
	in *gitalypb.UserLandRequest,
	quarantineDir *quarantine.Dir,
	quarantineRepo *localrepo.Repo,
	reference git.ReferenceName,
	oldOID, newOID git.ObjectID,
	rewritten bool,
	lands []changelog.Land,
) error {
	if rewritten {
		ctx = changelog.WithRewritten(ctx)
	}

	ctx, err := landWithRecord(ctx, reference, lands)
	if err != nil {
		return err
	}

	// On MVCC repositories a concurrent writer is detected when the snapshot is committed rather
	// than by the compare-and-swap above. The commit then returns the registered error.
	if cache := mvcc.CacheFromContext(ctx); cache != nil {
		cache.SetCommitConflictError(
			structerr.NewFailedPrecondition("unexpected old value").WithDetail(
				landReferenceUpdateError(reference, oldOID, newOID),
			),
		)
	}

	err = s.updateReferenceWithHooks(ctx, in.GetRepository(), in.GetUser(), quarantineDir, reference, newOID, oldOID)
	if err == nil {
		return nil
	}

	var customHookErr updateref.CustomHookError
	if errors.As(err, &customHookErr) {
		return structerr.NewPermissionDenied("%w", customHookErr).WithDetail(&gitalypb.UserLandError{
			Error: &gitalypb.UserLandError_CustomHook{
				CustomHook: customHookErr.Proto(),
			},
		})
	}

	var notAllowedErr hook.NotAllowedError
	if errors.As(err, &notAllowedErr) {
		return structerr.NewPermissionDenied("%w", notAllowedErr).WithDetail(&gitalypb.UserLandError{
			Error: &gitalypb.UserLandError_AccessCheck{
				AccessCheck: &gitalypb.AccessCheckError{
					ErrorMessage: notAllowedErr.Message,
					UserId:       notAllowedErr.UserID,
					Protocol:     notAllowedErr.Protocol,
					Changes:      notAllowedErr.Changes,
				},
			},
		})
	}

	// Only lost races are retried. The other errors do not change when the attempt is repeated,
	// so retrying would only run the hooks again.
	var (
		mismatchingStateErr gitupdateref.MismatchingStateError
		alreadyLockedErr    gitupdateref.AlreadyLockedError
		alreadyExistsErr    gitupdateref.ReferenceAlreadyExistsError
		unexpectedOldValErr refdb.UnexpectedOldValueError
	)
	if errors.As(err, &mismatchingStateErr) ||
		errors.As(err, &alreadyLockedErr) ||
		errors.As(err, &alreadyExistsErr) ||
		errors.As(err, &unexpectedOldValErr) ||
		errors.Is(err, gitupdateref.ErrPackedRefsLocked) {
		return landConflict{
			reference: reference,
			oldOID:    oldOID,
			newOID:    newOID,
			cause:     err,
		}
	}

	var invalidFormatErr gitupdateref.InvalidReferenceFormatError
	if errors.As(err, &invalidFormatErr) {
		return structerr.NewInvalidArgument("invalid branch reference: %w", err)
	}

	var nonCommitErr gitupdateref.NonCommitObjectError
	if errors.As(err, &nonCommitErr) {
		return structerr.NewInvalidArgument("commit ID does not refer to a commit: %w", err)
	}

	var fileDirectoryErr gitupdateref.FileDirectoryConflictError
	if errors.As(err, &fileDirectoryErr) {
		return structerr.NewFailedPrecondition("reference conflicts with existing reference %q: %w", fileDirectoryErr.ExistingReferenceName, err)
	}

	// Git does not report every lost race in a form that is classified above. A branch that
	// was deleted concurrently fails with "unable to resolve reference", for example. When
	// locking or committing the reference failed and the branch no longer points to the tip
	// that was read, a concurrent writer moved it, so the attempt is retried. The next
	// attempt sees the current state of the branch.
	var updateErr updateref.Error
	if errors.As(err, &updateErr) {
		moved, resolveErr := s.landReferenceMoved(ctx, quarantineRepo, reference, oldOID)
		if resolveErr != nil {
			return structerr.NewInternal("updating ref with hooks: %w", errors.Join(err, resolveErr))
		}
		if moved {
			return landConflict{
				reference: reference,
				oldOID:    oldOID,
				newOID:    newOID,
				cause:     err,
			}
		}
	}

	return structerr.NewInternal("updating ref with hooks: %w", err)
}

// landReferenceMoved reports whether the reference no longer points to expectedOID. A missing
// reference has moved unless the zero OID was expected.
func (s *Server) landReferenceMoved(ctx context.Context, repo *localrepo.Repo, reference git.ReferenceName, expectedOID git.ObjectID) (bool, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return false, fmt.Errorf("detecting object hash: %w", err)
	}

	current, err := s.landBranchTip(ctx, repo, reference)
	if err != nil {
		return false, fmt.Errorf("resolving branch after failed update: %w", err)
	}
	if current == "" {
		return expectedOID != objectHash.ZeroOID, nil
	}

	return current != expectedOID, nil
}

// landBranchTip returns the commit of the branch reference, read by its exact name, or an empty ID
// when the branch does not exist. git resolves refs/heads/<x> with its DWIM rules: when the branch
// is missing, refs/refs/heads/<x>, refs/tags/refs/heads/<x>, refs/heads/refs/heads/<x> or
// refs/remotes/refs/heads/<x> answers for it, and a land would decide, and answer already_landed,
// against the commit such a reference (a tag an agent may create) chose. UserCommitFiles reads its
// branches the same way (localrepo.ResolveBranchesExactly). A symbolic branch resolves to the commit
// of its target.
func (s *Server) landBranchTip(ctx context.Context, repo *localrepo.Repo, reference git.ReferenceName) (git.ObjectID, error) {
	reader, release, err := s.catfileCache.ObjectInfoReader(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("creating object info reader: %w", err)
	}
	defer release()

	branches, err := repo.ResolveBranchesExactly(ctx, reader, reference)
	if err != nil {
		return "", err
	}

	return branches[reference], nil
}

// landMergeConflictError returns the FailedPrecondition error for a change that does not apply
// cleanly onto the branch tip.
func landMergeConflictError(tip, commitID git.ObjectID, conflictingFiles [][]byte, cause error) error {
	return structerr.NewFailedPrecondition("landing commit: %w", cause).WithDetail(&gitalypb.UserLandError{
		Error: &gitalypb.UserLandError_MergeConflict{
			MergeConflict: &gitalypb.MergeConflictError{
				ConflictingFiles:     conflictingFiles,
				ConflictingCommitIds: []string{tip.String(), commitID.String()},
			},
		},
	})
}

// landSnapshotPinned reports whether the RPC reads a snapshot that is pinned for all of it (MVCC).
func landSnapshotPinned(ctx context.Context) bool {
	return mvcc.CacheFromContext(ctx) != nil
}

// landLostRace decides what a landing attempt that lost the compare-and-swap does next: nil to
// attempt again, or the error that ends the land. On a pinned snapshot a new attempt in the RPC
// would read the same tip and build the same tree, so the land ends with the retryable
// FailedPrecondition for the client to retry, whatever the land holds: not even a model's
// resolution is handed back to be verified again, as that would verify the same tree once more.
// Otherwise the attempts run out at max_attempts and the land ends as landKeptMoving says.
func landLostRace(in *gitalypb.UserLandRequest, pre *landPreResolution, conflict landConflict, attempt, maxAttempts int, pinned bool) error {
	switch {
	case pinned:
		return landKeptMovingError(in, conflict, attempt)
	case attempt >= maxAttempts:
		return landKeptMoving(in, pre, conflict, attempt)
	}
	return nil
}

// landKeptMoving is the error of a landing that gave up because the branch kept moving. A model's
// resolution is handed back instead, as a land that needs its new tree verified (it is the same
// cause: the branch moved on): UserLand goes out again, and at its deadline the agent gets the
// proposals, not an error about a branch it has nothing to do with. The others get
// landKeptMovingError.
func landKeptMoving(in *gitalypb.UserLandRequest, pre *landPreResolution, conflict landConflict, attempts int) error {
	if pre.mustVerify() {
		return &landNeedsResolution{tip: conflict.newOID, commit: git.ObjectID(in.GetCommitId()), moved: true}
	}
	return landKeptMovingError(in, conflict, attempts)
}

// landKeptMovingError returns the FailedPrecondition error for a landing that lost the
// compare-and-swap on every attempt, or that cannot be retried inside the RPC.
func landKeptMovingError(in *gitalypb.UserLandRequest, conflict landConflict, attempts int) error {
	return structerr.NewFailedPrecondition(
		"branch %q kept moving, giving up after %d attempts: %w",
		string(in.GetBranch()), attempts, conflict,
	).WithDetail(landReferenceUpdateError(conflict.reference, conflict.oldOID, conflict.newOID))
}

// landReferenceUpdateError returns the UserLandError detail for a failed compare-and-swap.
func landReferenceUpdateError(reference git.ReferenceName, oldOID, newOID git.ObjectID) *gitalypb.UserLandError {
	return &gitalypb.UserLandError{
		Error: &gitalypb.UserLandError_ReferenceUpdate{
			ReferenceUpdate: &gitalypb.ReferenceUpdateError{
				ReferenceName: []byte(reference),
				OldOid:        oldOID.String(),
				NewOid:        newOID.String(),
			},
		},
	}
}

// landResponse builds the UserLandResponse. Attempts is set by the caller.
func landResponse(oldTip, landedCommitID git.ObjectID, rewritten, alreadyLanded, created bool) *gitalypb.UserLandResponse {
	return &gitalypb.UserLandResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId:      landedCommitID.String(),
			BranchCreated: created,
		},
		LandedCommitId: landedCommitID.String(),
		Rewritten:      rewritten,
		AlreadyLanded:  alreadyLanded,
		OldTip:         oldTip.String(),
	}
}

// waitLandBackoff sleeps for a jittered, exponentially growing delay before the next attempt. The
// jitter spreads competing landers out so that they do not keep colliding.
func waitLandBackoff(ctx context.Context, attempt int) error {
	ceiling := min(landBackoffBase<<min(attempt-1, 6), landBackoffMax)
	delay := ceiling/2 + time.Duration(rand.Int64N(int64(ceiling/2)+1))

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateUserLandRequest(ctx context.Context, locator storage.Locator, in *gitalypb.UserLandRequest) error {
	if err := locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return err
	}

	if in.GetUser() == nil {
		return errors.New("empty user")
	}

	if len(in.GetUser().GetName()) == 0 {
		return errors.New("empty user name")
	}

	if len(in.GetUser().GetEmail()) == 0 {
		return errors.New("empty user email")
	}

	if len(in.GetBranch()) == 0 {
		return errors.New("empty branch name")
	}

	if strings.HasPrefix(string(in.GetBranch()), "refs/") {
		return errors.New("branch must be a branch name, not a full reference name")
	}

	if err := git.ValidateRevision(in.GetBranch()); err != nil {
		return fmt.Errorf("invalid branch name: %w", err)
	}

	// git refuses to create a branch called HEAD, and ValidateReference does not see it as a
	// branch because it only checks the full name.
	if string(in.GetBranch()) == "HEAD" {
		return errors.New("invalid branch name: HEAD is reserved")
	}

	if err := git.ValidateReference("refs/heads/" + string(in.GetBranch())); err != nil {
		return fmt.Errorf("invalid branch name: %w", err)
	}

	// The land records its identity in its update (doc/graft/PLAN11.md), which bounds the name of
	// the branch. The request is refused here rather than its update, so that it fails alone, also
	// in a batch.
	if err := changelog.CheckLandReference("refs/heads/" + string(in.GetBranch())); err != nil {
		return err
	}

	if in.GetCommitId() == "" {
		return errors.New("empty commit id")
	}

	if _, ok := gitalypb.UserLandRequest_Strategy_name[int32(in.GetStrategy())]; !ok {
		return fmt.Errorf("unknown strategy: %d", in.GetStrategy())
	}

	// A resolution lands as a merge commit of the tip and the change, which fast-forward only
	// forbids: the request contradicts itself.
	if in.GetResolution() != nil && in.GetStrategy() == gitalypb.UserLandRequest_STRATEGY_FF_ONLY {
		return errors.New("a resolution lands as a merge commit, which STRATEGY_FF_ONLY does not allow")
	}

	if in.GetMaxAttempts() < 0 {
		return errors.New("negative max attempts")
	}

	return nil
}
