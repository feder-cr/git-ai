package ssh

import (
	"fmt"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestReceivePack_longReferenceNames pushes object IDs, so the client creates no reference of its own,
// and the update hook refuses the long name on the server before git writes it there.
func TestReceivePack_longReferenceNames(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	testcfg.BuildGitalyHooks(t, cfg)
	testcfg.BuildGitalySSH(t, cfg)
	cfg.SocketPath = runSSHServer(t, cfg)

	local := setupRepoWithChange(t, cfg)
	long := gittest.ReferenceNameOfLength(t, "refs/heads/long/", 1025)
	limit := gittest.ReferenceNameOfLength(t, "refs/heads/limit/", 1024)

	push := func(t *testing.T, repo *gitalypb.Repository, refspecs ...string) (string, error) {
		t.Helper()

		payload, err := protojson.Marshal(&gitalypb.SSHReceivePackRequest{
			Repository:   repo,
			GlId:         "123",
			GlUsername:   "user",
			GlRepository: "project-456",
		})
		require.NoError(t, err)

		args := append([]string{"-C", local.path, "push", "git@localhost:test/test.git"}, refspecs...)
		cmd := gittest.NewCommand(t, cfg, args...)
		cmd.Env = append(cmd.Env,
			fmt.Sprintf("GITALY_PAYLOAD=%s", payload),
			fmt.Sprintf("GITALY_ADDRESS=%s", cfg.SocketPath),
			fmt.Sprintf("GIT_SSH_COMMAND=%s receive-pack", cfg.BinaryPath("gitaly-ssh")),
		)
		output, err := cmd.CombinedOutput()
		return string(output), err
	}

	t.Run("the long name alone is refused and the others land", func(t *testing.T) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

		output, err := push(t, repo,
			fmt.Sprintf("%s:refs/heads/first", local.newHead),
			fmt.Sprintf("%s:%s", local.newHead, long),
			fmt.Sprintf("%s:%s", local.newHead, limit),
		)
		gittest.RequireNoReferenceNameOverLimit(t, cfg, repoPath)
		require.Error(t, err, "a push with a refused reference fails")
		require.Contains(t, output, "over the limit of 1024 bytes", "the client must be told why")
		require.Contains(t, output, "hook declined")

		gittest.RequireReferenceNameLengths(t, cfg, repoPath, len("refs/heads/first"), 1024)
	})

	t.Run("an existing long reference can be deleted", func(t *testing.T) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		commitID := gittest.WriteCommit(t, cfg, repoPath)
		gittest.SeedPackedReference(t, repoPath, git.ReferenceName(long), commitID)

		output, err := push(t, repo, ":"+long.String())
		require.NoError(t, err, output)
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})
}
