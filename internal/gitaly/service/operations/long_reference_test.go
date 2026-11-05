package operations

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestOperations_longReferenceNames checks the refusal through the real server, its interceptors
// included. The server refuses each name before it runs a handler, a hook or git.
func TestOperations_longReferenceNames(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupOperationsService(t, testhelper.Context(t))
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	user := gittest.TestUser
	const branchPrefix = "refs/heads/"

	long := []byte(gittest.ReferenceNameOfLength(t, branchPrefix, 1025).String()[len(branchPrefix):])
	longTag := []byte(gittest.ReferenceNameOfLength(t, "refs/tags/", 1025).String()[len("refs/tags/"):])
	longFull := []byte(gittest.ReferenceNameOfLength(t, "refs/merge-requests/", 1025))
	message := []byte("message")

	for desc, call := range map[string]func() error{
		"UserCreateBranch": func() error {
			_, err := client.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{Repository: repo, User: user, BranchName: long, StartPoint: []byte(commitID)})
			return err
		},
		"UserUpdateBranch": func() error {
			_, err := client.UserUpdateBranch(ctx, &gitalypb.UserUpdateBranchRequest{Repository: repo, User: user, BranchName: long, Newrev: []byte(commitID), Oldrev: []byte(commitID)})
			return err
		},
		"UserCreateTag": func() error {
			_, err := client.UserCreateTag(ctx, &gitalypb.UserCreateTagRequest{Repository: repo, User: user, TagName: longTag, TargetRevision: []byte(commitID)})
			return err
		},
		"UserMergeToRef": func() error {
			_, err := client.UserMergeToRef(ctx, &gitalypb.UserMergeToRefRequest{Repository: repo, User: user, TargetRef: longFull, SourceSha: commitID.String(), Message: message})
			return err
		},
		"UserRebaseToRef": func() error {
			_, err := client.UserRebaseToRef(ctx, &gitalypb.UserRebaseToRefRequest{Repository: repo, User: user, TargetRef: longFull, SourceSha: commitID.String()})
			return err
		},
		"UserFFBranch": func() error {
			_, err := client.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{Repository: repo, User: user, Branch: long, CommitId: commitID.String()})
			return err
		},
		"UserCherryPick": func() error {
			_, err := client.UserCherryPick(ctx, &gitalypb.UserCherryPickRequest{Repository: repo, User: user, BranchName: long, Message: message})
			return err
		},
		"UserRevert": func() error {
			_, err := client.UserRevert(ctx, &gitalypb.UserRevertRequest{Repository: repo, User: user, BranchName: long, Message: message})
			return err
		},
		"UserUpdateSubmodule": func() error {
			_, err := client.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{Repository: repo, User: user, Branch: long, CommitSha: commitID.String(), Submodule: []byte("sub"), CommitMessage: message})
			return err
		},
		"UserLand": func() error {
			_, err := client.UserLand(ctx, &gitalypb.UserLandRequest{Repository: repo, User: user, Branch: long, CommitId: commitID.String()})
			return err
		},
		"UserCommitFiles": func() error {
			stream, err := client.UserCommitFiles(ctx)
			require.NoError(t, err)
			if err := stream.Send(&gitalypb.UserCommitFilesRequest{UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{
				Header: &gitalypb.UserCommitFilesRequestHeader{Repository: repo, User: user, BranchName: long, CommitMessage: message},
			}}); err != nil {
				return recvCommitFiles(stream)
			}
			return recvCommitFiles(stream)
		},
		"UserMergeBranch": func() error {
			stream, err := client.UserMergeBranch(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{Repository: repo, User: user, Branch: long, CommitId: commitID.String(), Message: message}))
			_, err = stream.Recv()
			return err
		},
		"UserRebaseConfirmable": func() error {
			stream, err := client.UserRebaseConfirmable(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UserRebaseConfirmableRequest{UserRebaseConfirmableRequestPayload: &gitalypb.UserRebaseConfirmableRequest_Header_{
				Header: &gitalypb.UserRebaseConfirmableRequest_Header{Repository: repo, User: user, Branch: long, BranchSha: commitID.String()},
			}}))
			_, err = stream.Recv()
			return err
		},
		"UserApplyPatch": func() error {
			stream, err := client.UserApplyPatch(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{
				Header: &gitalypb.UserApplyPatchRequest_Header{Repository: repo, User: user, TargetBranch: long},
			}}))
			_, err = stream.CloseAndRecv()
			return err
		},
	} {
		t.Run(desc+" refuses a name over the limit", func(t *testing.T) {
			err := call()
			require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
			require.ErrorContains(t, err, "over the limit of 1024 bytes")
		})
	}

	// Only the default branch: nothing above was written.
	gittest.RequireReferenceNameLengths(t, cfg, repoPath, len(git.DefaultRef))

	t.Run("a name of exactly the limit is written", func(t *testing.T) {
		atLimit := []byte(gittest.ReferenceNameOfLength(t, branchPrefix, 1024).String()[len(branchPrefix):])
		_, err := client.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{Repository: repo, User: user, BranchName: atLimit, StartPoint: []byte(commitID)})
		require.NoError(t, err)
		gittest.RequireReferenceNameLengths(t, cfg, repoPath, len(git.DefaultRef), 1024)

		_, err = client.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{Repository: repo, User: user, BranchName: atLimit})
		require.NoError(t, err)
		gittest.RequireReferenceNameLengths(t, cfg, repoPath, len(git.DefaultRef))
	})

	t.Run("an existing long branch can be deleted", func(t *testing.T) {
		seededRepo, seededPath := gittest.CreateRepository(t, ctx, cfg)
		seededCommit := gittest.WriteCommit(t, cfg, seededPath)
		gittest.SeedPackedReference(t, seededPath, git.ReferenceName(branchPrefix+string(long)), seededCommit)
		gittest.RequireReferenceNameLengths(t, cfg, seededPath, 1025)

		_, err := client.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{Repository: seededRepo, User: user, BranchName: long})
		require.NoError(t, err)
		gittest.RequireReferenceNameLengths(t, cfg, seededPath)
	})
}

func recvCommitFiles(stream gitalypb.OperationService_UserCommitFilesClient) error {
	_, err := stream.CloseAndRecv()
	return err
}
