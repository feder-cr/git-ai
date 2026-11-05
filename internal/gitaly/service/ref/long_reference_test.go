package ref

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

func TestUpdateReferences_longReferenceNames(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)
	long := gittest.ReferenceNameOfLength(t, "refs/heads/", 1025)
	limit := gittest.ReferenceNameOfLength(t, "refs/heads/", 1024)

	update := func(repo *gitalypb.Repository, updates ...*gitalypb.UpdateReferencesRequest_Update) error {
		stream, err := client.UpdateReferences(ctx)
		require.NoError(t, err)
		// The server may refuse the stream as soon as it reads the message, so a failed send
		// has its reason in the status that CloseAndRecv returns.
		_ = stream.Send(&gitalypb.UpdateReferencesRequest{Repository: repo, Updates: updates})
		_, err = stream.CloseAndRecv()
		return err
	}

	t.Run("a name over the limit is refused with the others of the request", func(t *testing.T) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		commitID := gittest.WriteCommit(t, cfg, repoPath)

		err := update(repo,
			&gitalypb.UpdateReferencesRequest_Update{Reference: []byte("refs/heads/ok"), NewObjectId: []byte(commitID)},
			&gitalypb.UpdateReferencesRequest_Update{Reference: []byte(long), NewObjectId: []byte(commitID)},
		)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
		require.ErrorContains(t, err, "over the limit of 1024 bytes")
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})

	t.Run("a name of exactly the limit is written", func(t *testing.T) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		commitID := gittest.WriteCommit(t, cfg, repoPath)

		require.NoError(t, update(repo, &gitalypb.UpdateReferencesRequest_Update{Reference: []byte(limit), NewObjectId: []byte(commitID)}))
		gittest.RequireReferenceNameLengths(t, cfg, repoPath, 1024)
	})

	t.Run("an existing long reference can be read and deleted", func(t *testing.T) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		commitID := gittest.WriteCommit(t, cfg, repoPath)
		gittest.SeedPackedReference(t, repoPath, git.ReferenceName(long), commitID)

		exists, err := client.RefExists(ctx, &gitalypb.RefExistsRequest{Repository: repo, Ref: []byte(long)})
		require.NoError(t, err)
		require.True(t, exists.GetValue())

		err = update(repo, &gitalypb.UpdateReferencesRequest_Update{Reference: []byte(long), NewObjectId: []byte(commitID)})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "updating it is a write")

		require.NoError(t, update(repo, &gitalypb.UpdateReferencesRequest_Update{Reference: []byte(long), NewObjectId: []byte(gittest.DefaultObjectHash.ZeroOID)}))
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})

	t.Run("DeleteRefs deletes an existing long reference", func(t *testing.T) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		commitID := gittest.WriteCommit(t, cfg, repoPath)
		gittest.SeedPackedReference(t, repoPath, git.ReferenceName(long), commitID)

		_, err := client.DeleteRefs(ctx, &gitalypb.DeleteRefsRequest{Repository: repo, Refs: [][]byte{[]byte(long)}})
		require.NoError(t, err)
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})
}
