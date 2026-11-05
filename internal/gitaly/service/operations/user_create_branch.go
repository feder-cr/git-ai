package operations

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func validateUserCreateBranchRequest(ctx context.Context, locator storage.Locator, in *gitalypb.UserCreateBranchRequest) error {
	if err := locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return err
	}
	if len(in.GetBranchName()) == 0 {
		return errors.New("empty branch name")
	}
	if in.GetUser() == nil {
		return errors.New("empty user")
	}
	if len(in.GetStartPoint()) == 0 {
		return errors.New("empty start point")
	}
	return nil
}

// UserCreateBranch creates a single branch in the context of a specific user. It executes
// hooks and contacts Rails to verify that the user is allowed to create the branch.
func (s *Server) UserCreateBranch(ctx context.Context, req *gitalypb.UserCreateBranchRequest) (*gitalypb.UserCreateBranchResponse, error) {
	if err := validateUserCreateBranchRequest(ctx, s.locator, req); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// The start point is read by the rule of every start point: an existing branch beats a tag of
	// the same name.
	startPointOID, err := resolveStartBranchLocally(ctx, quarantineRepo, string(req.GetStartPoint()))
	if err != nil {
		return nil, structerr.NewFailedPrecondition("revspec '%s' not found", req.GetStartPoint())
	}

	startPointCommit, err := quarantineRepo.ReadCommit(ctx, startPointOID.Revision())
	if err != nil {
		return nil, structerr.NewFailedPrecondition("revspec '%s' not found", req.GetStartPoint())
	}

	objectHash, err := quarantineRepo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	referenceName := git.NewReferenceNameFromBranchName(string(req.GetBranchName()))

	if err := s.updateReferenceWithHooks(ctx, req.GetRepository(), req.GetUser(), quarantineDir, referenceName, startPointOID, objectHash.ZeroOID); err != nil {
		var customHookErr updateref.CustomHookError
		var notAllowedError hook.NotAllowedError

		// An access check that refuses the update (the standalone policy, or GitLab)
		// is a denial, as in UserDeleteBranch, not an internal error.
		// UserCreateBranchError has no access check detail.
		if errors.As(err, &notAllowedError) {
			return nil, structerr.NewPermissionDenied("creation denied by access checks: %w", err)
		}

		if errors.As(err, &customHookErr) {
			return nil, structerr.NewPermissionDenied("creation denied by custom hooks: %w", err).WithDetail(
				&gitalypb.UserCreateBranchError{
					Error: &gitalypb.UserCreateBranchError_CustomHook{
						CustomHook: customHookErr.Proto(),
					},
				},
			)
		}

		var updateRefError updateref.Error
		if errors.As(err, &updateRefError) {
			return nil, structerr.NewFailedPrecondition("%w", err)
		}

		return nil, err
	}

	return &gitalypb.UserCreateBranchResponse{
		Branch: &gitalypb.Branch{
			Name:         req.GetBranchName(),
			TargetCommit: startPointCommit,
		},
	}, nil
}
