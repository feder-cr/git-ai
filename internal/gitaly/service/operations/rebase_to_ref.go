package operations

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// UserRebaseToRef overwrites the given TargetRef with the result of rebasing
// SourceSHA on top of FirstParentRef, and returns the SHA of the HEAD of
// TargetRef.
func (s *Server) UserRebaseToRef(ctx context.Context, request *gitalypb.UserRebaseToRefRequest) (*gitalypb.UserRebaseToRefResponse, error) {
	if err := validateUserRebaseToRefRequest(ctx, s.locator, request); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, request.GetRepository())
	if err != nil {
		return nil, structerr.NewInternal("creating repo quarantine: %w", err)
	}
	defer cleanup()

	// The first parent is read as a start point is: a missing refs/heads/x is missing, whatever
	// reference git's DWIM rules would take for it.
	oid, err := resolveStartBranchLocally(ctx, quarantineRepo, string(request.GetFirstParentRef()))
	if err != nil {
		return nil, structerr.NewInvalidArgument("invalid FirstParentRef")
	}

	sourceOID, err := quarantineRepo.ResolveRevision(ctx, git.Revision(request.GetSourceSha()))
	if err != nil {
		return nil, structerr.NewInvalidArgument("invalid SourceSha")
	}

	if err := refuseSymbolicTargetRef(ctx, quarantineRepo, request.GetTargetRef()); err != nil {
		return nil, err
	}

	// Resolve the current state of the target reference. We do not care whether it
	// exists or not, but what we do want to assert is that the target reference doesn't
	// change while we compute the merge commit as a small protection against races.
	objectHash, err := quarantineRepo.ObjectHash(ctx)
	if err != nil {
		return nil, structerr.NewInternal("detecting object hash: %w", err)
	}

	var oldTargetOID git.ObjectID
	if expectedOldOID := request.GetExpectedOldOid(); expectedOldOID != "" {
		oldTargetOID, err = objectHash.FromHex(expectedOldOID)
		if err != nil {
			return nil, structerr.NewInvalidArgument("invalid expected old object ID: %w", err).WithMetadata("old_object_id", expectedOldOID)
		}

		oldTargetOID, err = resolveRevision(ctx, quarantineRepo, oldTargetOID)
		if err != nil {
			return nil, structerr.NewInvalidArgument("cannot resolve expected old object ID: %w", err).
				WithMetadata("old_object_id", expectedOldOID)
		}
	} else if targetRef, err := quarantineRepo.GetReference(ctx, git.ReferenceName(request.GetTargetRef())); err == nil {
		oid, err := objectHash.FromHex(targetRef.Target)
		if err != nil {
			return nil, structerr.NewInternal("invalid target revision: %w", err)
		}

		oldTargetOID = oid
	} else if errors.Is(err, git.ErrReferenceAmbiguous) {
		return nil, structerr.NewInvalidArgument("target reference is ambiguous: %w", err)
	} else if errors.Is(err, git.ErrReferenceNotFound) {
		oldTargetOID = objectHash.ZeroOID
	} else {
		return nil, structerr.NewInternal("could not read target reference: %w", err)
	}

	committerSignature, err := git.SignatureFromRequest(request)
	if err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	rebasedOID, err := quarantineRepo.Rebase(
		ctx,
		oid.String(),
		sourceOID.String(),
		localrepo.RebaseWithCommitter(committerSignature),
	)
	if err != nil {
		var conflictErr *localrepo.RebaseConflictError
		if errors.As(err, &conflictErr) {
			return nil, structerr.NewFailedPrecondition("failed to rebase %s on %s while preparing %s due to conflict",
				sourceOID, oid, string(request.GetTargetRef()))
		}

		return nil, structerr.NewInternal("rebasing commits: %w", err)
	}

	if err := quarantineDir.Migrate(ctx); err != nil {
		return nil, structerr.NewInternal("migrating quarantined objects: %w", err)
	}

	repo := s.localRepoFactory.Build(request.GetRepository())

	// The reference-transaction hook enforces the standalone policy, but Git only
	// reports that it failed. Checking the policy here returns the denial itself.
	if err := hook.CheckAgentPolicy(ctx, s.graftInForce(ctx), fmt.Sprintf("%s %s %s\n", oldTargetOID, rebasedOID, request.GetTargetRef())); err != nil {
		return nil, err
	}

	if err := repo.UpdateRef(ctx, git.ReferenceName(request.GetTargetRef()), rebasedOID, oldTargetOID); err != nil {
		return nil, structerr.NewFailedPrecondition("could not update %s. Please refresh and try again", string(request.GetTargetRef()))
	}

	return &gitalypb.UserRebaseToRefResponse{
		CommitId: rebasedOID.String(),
	}, nil
}
