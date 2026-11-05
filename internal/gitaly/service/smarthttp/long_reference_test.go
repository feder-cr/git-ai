package smarthttp

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// TestPostReceivePack_longReferenceNames crafts the commands of the push, so no client git process sees
// the long name, and the update hook refuses it on the server before git writes it there.
func TestPostReceivePack_longReferenceNames(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.SocketPath = runSmartHTTPServer(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	client := newSmartHTTPClient(t, cfg.SocketPath, cfg.Auth.Token)
	zero := gittest.DefaultObjectHash.ZeroOID
	long := gittest.ReferenceNameOfLength(t, "refs/heads/long/", 1025)
	limit := gittest.ReferenceNameOfLength(t, "refs/heads/limit/", 1024)

	request := func(repo *gitalypb.Repository) *gitalypb.PostReceivePackRequest {
		return &gitalypb.PostReceivePackRequest{Repository: repo, GlUsername: "user", GlId: "123", GlRepository: "project-456"}
	}

	t.Run("the long name alone is refused and the others land", func(t *testing.T) {
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		repoProto.GlProjectPath = "project/path"
		base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("base"))

		push := setupPush(t, ctx, cfg, repoPath, func(pushPath string) []refUpdate {
			commitID := gittest.WriteCommit(t, cfg, pushPath, gittest.WithParents(base))
			return []refUpdate{
				{ref: "refs/heads/first", from: zero, to: commitID},
				{ref: long, from: zero, to: commitID},
				{ref: limit, from: zero, to: commitID},
			}
		})

		stream, err := client.PostReceivePack(ctx)
		require.NoError(t, err)
		response := push.perform(t, stream, request(repoProto))
		gittest.RequireNoReferenceNameOverLimit(t, cfg, repoPath)

		require.Contains(t, response, "over the limit of 1024 bytes", "the client must be told why")
		require.Contains(t, response, "ok refs/heads/first")
		require.Contains(t, response, "ok "+limit.String())
		require.Contains(t, response, "ng "+long.String()+" hook declined")

		gittest.RequireReferenceNameLengths(t, cfg, repoPath, len("refs/heads/first"), 1024)
	})

	t.Run("an existing long reference can be deleted", func(t *testing.T) {
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
		repoProto.GlProjectPath = "project/path"
		commitID := gittest.WriteCommit(t, cfg, repoPath)
		gittest.SeedPackedReference(t, repoPath, git.ReferenceName(long), commitID)

		push := setupPush(t, ctx, cfg, repoPath, func(string) []refUpdate {
			return []refUpdate{{ref: long, from: commitID, to: zero}}
		})

		stream, err := client.PostReceivePack(ctx)
		require.NoError(t, err)
		response := push.perform(t, stream, request(repoProto))

		require.Contains(t, response, "ok "+long.String())
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})
}
