package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// UserMergeToRef overwrites the given TargetRef to point to either Branch or
// FirstParentRef. Afterwards, it performs a merge of SourceSHA with either
// Branch or FirstParentRef and updates TargetRef to the merge commit.
func (s *Server) UserMergeToRef(ctx context.Context, request *gitalypb.UserMergeToRefRequest) (*gitalypb.UserMergeToRefResponse, error) {
	if err := validateUserMergeToRefRequest(ctx, s.locator, request); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	repo := s.localRepoFactory.Build(request.GetRepository())

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	//nolint:staticcheck // Branch is marked as deprecated in the protobuf
	revision := git.Revision(request.GetBranch())
	if request.FirstParentRef != nil {
		revision = git.Revision(request.GetFirstParentRef())
	}

	// The first parent is read as a start point is: a missing refs/heads/x is missing, whatever
	// reference git's DWIM rules would take for it.
	oid, err := resolveStartBranchLocally(ctx, repo, revision.String())
	if err != nil {
		return nil, structerr.NewInvalidArgument("Invalid merge source")
	}

	sourceOID, err := repo.ResolveRevision(ctx, git.Revision(request.GetSourceSha()))
	if err != nil {
		return nil, structerr.NewInvalidArgument("Invalid merge source")
	}

	authorSignature, err := git.SignatureFromRequest(request)
	if err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	if err := refuseSymbolicTargetRef(ctx, repo, request.GetTargetRef()); err != nil {
		return nil, err
	}

	// Initialize oldTargetOID from expected_old_oid when provided, otherwise
	// resolve it from target_ref. This will be used as an optimistic lock when
	// we finally update target_ref, to ensure it hasn't changed in the
	// meantime.
	var oldTargetOID git.ObjectID
	if expectedOldOID := request.GetExpectedOldOid(); expectedOldOID != "" {
		objectHash, err := repo.ObjectHash(ctx)
		if err != nil {
			return nil, structerr.NewInternal("detecting object hash: %w", err)
		}

		oldTargetOID, err = objectHash.FromHex(expectedOldOID)
		if err != nil {
			return nil, structerr.NewInvalidArgument("invalid expected old object ID: %w", err).WithMetadata("old_object_id", expectedOldOID)
		}

		oldTargetOID, err = resolveRevision(ctx, repo, oldTargetOID)
		if err != nil {
			return nil, structerr.NewInvalidArgument("cannot resolve expected old object ID: %w", err).
				WithMetadata("old_object_id", expectedOldOID)
		}
	} else if targetRef, err := repo.GetReference(ctx, git.ReferenceName(request.GetTargetRef())); err == nil {
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

	// The merge is written into a quarantine, migrated before the reference update: a loose object
	// written straight into the repository would race housekeeping's removal of empty loose object
	// directories, which the migration survives.
	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, request.GetRepository())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	mergeCommitID, err := s.merge(
		ctx,
		quarantineRepo,
		authorSignature,
		authorSignature,
		string(request.GetMessage()),
		oid.String(),
		sourceOID.String(),
		false,
		request.GetSign(),
	)
	if err != nil {
		var conflictErr *localrepo.MergeTreeConflictError
		if errors.As(err, &conflictErr) {
			return nil, structerr.NewFailedPrecondition("merging commits: %w", err).
				WithMetadata("source_sha", sourceOID.String()).
				WithMetadata("target_sha", oid.String()).
				WithMetadata("target_ref", string(request.GetTargetRef())).
				WithDetail(
					&gitalypb.UserMergeToRefError{
						Error: &gitalypb.UserMergeToRefError_MergeConflict{
							MergeConflict: &gitalypb.MergeConflictError{
								ConflictingFiles: conflictingFileNames(conflictErr),
								ConflictingCommitIds: []string{
									oid.String(),
									sourceOID.String(),
								},
							},
						},
					},
				)
		}

		s.logger.WithError(err).WithFields(
			log.Fields{
				"source_sha": sourceOID,
				"target_sha": oid,
				"target_ref": string(request.GetTargetRef()),
			},
		).ErrorContext(ctx, "unable to create merge commit")

		return nil, structerr.NewFailedPrecondition("Failed to create merge commit for source_sha %s and target_sha %s at %s",
			sourceOID, oid, string(request.GetTargetRef()))
	}

	mergeOID, err := objectHash.FromHex(mergeCommitID)
	if err != nil {
		return nil, structerr.NewInternal("parsing merge commit SHA: %w", err)
	}

	// ... and move branch from target ref to the merge commit. The Ruby
	// implementation doesn't invoke hooks, so we don't either.
	// The reference-transaction hook enforces the standalone policy, but Git only
	// reports that it failed. Checking the policy here returns the denial itself.
	if err := hook.CheckAgentPolicy(ctx, s.graftInForce(ctx), fmt.Sprintf("%s %s %s\n", oldTargetOID, mergeOID, request.GetTargetRef())); err != nil {
		return nil, err
	}

	if err := quarantineDir.Migrate(ctx); err != nil {
		return nil, structerr.NewInternal("migrating quarantined objects: %w", err)
	}

	if err := repo.UpdateRef(ctx, git.ReferenceName(request.GetTargetRef()), mergeOID, oldTargetOID); err != nil {
		return nil, structerr.NewFailedPrecondition("Could not update %s. Please refresh and try again", string(request.GetTargetRef()))
	}

	return &gitalypb.UserMergeToRefResponse{
		CommitId: mergeOID.String(),
	}, nil
}

// refuseSymbolicTargetRef refuses a target reference that writes another reference: the RPCs that
// take one validate its name, and a symbolic reference would take the write outside of what they
// validated. It holds whether or not its target exists and whatever old value the request expects.
func refuseSymbolicTargetRef(ctx context.Context, repo *localrepo.Repo, targetRef []byte) error {
	written, err := repo.WrittenReference(ctx, git.ReferenceName(targetRef))
	if err != nil {
		return err
	}
	if written != git.ReferenceName(targetRef) {
		return structerr.NewFailedPrecondition("target reference is symbolic: %q", targetRef)
	}
	return nil
}

func validateUserMergeToRefRequest(ctx context.Context, locator storage.Locator, in *gitalypb.UserMergeToRefRequest) error {
	if err := locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return err
	}

	//nolint:staticcheck // Branch is marked as deprecated in the protobuf
	if len(in.GetFirstParentRef()) == 0 && len(in.GetBranch()) == 0 {
		return errors.New("empty first parent ref and branch name")
	}

	if in.GetUser() == nil {
		return errors.New("empty user")
	}

	if in.GetSourceSha() == "" {
		return errors.New("empty source SHA")
	}

	if len(in.GetTargetRef()) == 0 {
		return errors.New("empty target ref")
	}

	if !strings.HasPrefix(string(in.GetTargetRef()), "refs/merge-requests") {
		return errors.New("invalid target ref")
	}

	return nil
}
