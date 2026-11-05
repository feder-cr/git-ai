package repository

import (
	"context"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func requireLongNameRefused(t *testing.T, err error) {
	t.Helper()

	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "over the limit of 1024 bytes")
}

func TestRepositoryRPCs_longReferenceNames(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, client := setupRepositoryService(t)
	long := []byte(gittest.ReferenceNameOfLength(t, "refs/heads/", 1025))
	limit := []byte(gittest.ReferenceNameOfLength(t, "refs/heads/", 1024))

	t.Run("WriteRef", func(t *testing.T) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		commitID := gittest.WriteCommit(t, cfg, repoPath)

		_, err := client.WriteRef(ctx, &gitalypb.WriteRefRequest{Repository: repo, Ref: long, Revision: []byte(commitID)})
		requireLongNameRefused(t, err)

		_, err = client.WriteRef(ctx, &gitalypb.WriteRefRequest{Repository: repo, Ref: []byte("HEAD"), Revision: long})
		requireLongNameRefused(t, err)
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)

		_, err = client.WriteRef(ctx, &gitalypb.WriteRefRequest{Repository: repo, Ref: limit, Revision: []byte(commitID)})
		require.NoError(t, err)
		gittest.RequireReferenceNameLengths(t, cfg, repoPath, 1024)

		_, err = client.WriteRef(ctx, &gitalypb.WriteRefRequest{Repository: repo, Ref: []byte("HEAD"), Revision: limit})
		require.NoError(t, err)
	})

	t.Run("WriteRef deletes an existing long reference", func(t *testing.T) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		commitID := gittest.WriteCommit(t, cfg, repoPath)
		gittest.SeedPackedReference(t, repoPath, git.ReferenceName(long), commitID)

		_, err := client.WriteRef(ctx, &gitalypb.WriteRefRequest{Repository: repo, Ref: long, Revision: []byte(gittest.DefaultObjectHash.ZeroOID)})
		require.NoError(t, err)
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})

	t.Run("FetchSourceBranch", func(t *testing.T) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

		_, err := client.FetchSourceBranch(ctx, &gitalypb.FetchSourceBranchRequest{Repository: repo, SourceRepository: repo, SourceBranch: []byte("main"), TargetRef: long})
		requireLongNameRefused(t, err)
		gittest.RequireReferenceNameLengths(t, cfg, repoPath, len("refs/heads/main"))
	})

	t.Run("CreateRepository", func(t *testing.T) {
		repo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: gittest.NewRepositoryName(t)}

		_, err := client.CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{Repository: repo, DefaultBranch: long[len("refs/heads/"):]})
		requireLongNameRefused(t, err)
	})
}

func TestFetchRemote_longReferenceNames(t *testing.T) {
	t.Parallel()
	testhelper.NewFeatureSets(featureflag.FetchRemoteProactiveAuth).Run(t, testFetchRemoteLongReferenceNames)
}

func testFetchRemoteLongReferenceNames(t *testing.T, ctx context.Context) {
	t.Parallel()

	cfg, client := setupRepositoryService(t)
	long := gittest.ReferenceNameOfLength(t, "refs/heads/", 1025)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	_, remotePath := gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, remotePath)
	gittest.SeedPackedReference(t, remotePath, long, commitID)

	// The fetch is a dry run, so the only process that would write the name is the updater's, and the
	// updater refuses the name before that process sees it.
	_, err := client.FetchRemote(ctx, &gitalypb.FetchRemoteRequest{Repository: repo, RemoteParams: &gitalypb.Remote{Url: remotePath}})
	requireLongNameRefused(t, err)
	gittest.RequireReferenceNameLengths(t, cfg, repoPath)
}
