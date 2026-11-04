package gitcmd_test

import (
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/grpc/metadata"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	grpcmetadata "google.golang.org/grpc/metadata"
)

func TestWithRefHook(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("refs/heads/master"))

	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	objectHash, err := repo.ObjectHash(ctx)
	require.NoError(t, err)

	opt := gitcmd.WithRefTxHook(objectHash, repo)
	subCmd := gitcmd.Command{Name: "update-ref", Args: []string{"refs/heads/master", gittest.DefaultObjectHash.ZeroOID.String()}}

	for _, tt := range []struct {
		name string
		fn   func() (*command.Command, error)
	}{
		{
			name: "NewCommand",
			fn: func() (*command.Command, error) {
				return gittest.NewCommandFactory(t, cfg, gitcmd.WithSkipHooks()).New(ctx, repoProto, subCmd, opt)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := tt.fn()
			require.NoError(t, err)
			require.Contains(t, cmd.Env(), "GOTRACEBACK=none")

			require.NoError(t, cmd.Wait())

			var actualEnvVars []string
			for _, env := range cmd.Env() {
				kv := strings.SplitN(env, "=", 2)
				require.Len(t, kv, 2)
				key, val := kv[0], kv[1]

				if strings.HasPrefix(key, "GL_") || strings.HasPrefix(key, "GITALY_") {
					require.NotEmptyf(t, strings.TrimSpace(val),
						"env var %s value should not be empty string", key)
					actualEnvVars = append(actualEnvVars, key)
				}
			}

			expectedEnv := []string{
				"GITALY_HOOKS_PAYLOAD",
				command.EnvLogConfiguration,
			}

			require.EqualValues(t, expectedEnv, actualEnvVars)
		})
	}
}

func TestWithRefTxHookChangelogInvocation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	invocation := func(t *testing.T, disabled bool, storageMode ...string) string {
		cfg := testcfg.Build(t)
		cfg.Graft.Changelog.Disabled = disabled
		if len(storageMode) > 0 {
			cfg.Graft.Storage.Mode = storageMode[0]
		}

		repoProto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
			SkipCreationViaService: true,
		})
		repo := localrepo.NewTestRepo(t, cfg, repoProto)
		objectHash, err := repo.ObjectHash(ctx)
		require.NoError(t, err)

		cmd, err := gittest.NewCommandFactory(t, cfg, gitcmd.WithSkipHooks()).New(ctx, repoProto,
			gitcmd.Command{Name: "rev-parse", Flags: []gitcmd.Option{gitcmd.Flag{Name: "--git-dir"}}}, gitcmd.WithRefTxHook(objectHash, repo))
		require.NoError(t, err)
		require.NoError(t, cmd.Wait())

		meta, err := gitcmd.ChangelogMetaFromEnv(cmd.Env())
		require.NoError(t, err)

		return meta.Invocation
	}

	t.Run("each command gets its own invocation", func(t *testing.T) {
		first, second := invocation(t, false), invocation(t, false)
		require.NotEmpty(t, first)
		require.NotEmpty(t, second)
		require.NotEqual(t, first, second)
	})

	t.Run("no invocation without the changeset log", func(t *testing.T) {
		require.Empty(t, invocation(t, true))
	})

	t.Run("the authoritative storage core needs one without the changeset log", func(t *testing.T) {
		require.NotEmpty(t, invocation(t, true, "authoritative"))
	})
}

func TestWithPackObjectsHookEnv(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.PackObjectsCache.Enabled = true

	repoProto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	userID := "user-123"
	username := "username"
	protocol := "protocol"
	remoteIP := "1.2.3.4"

	objectHash, err := repo.ObjectHash(ctx)
	require.NoError(t, err)

	opt := gitcmd.WithPackObjectsHookEnv(objectHash, repoProto, protocol)
	subCmd := gitcmd.Command{Name: "upload-pack", Args: []string{"a/b/c"}}

	ctx = grpcmetadata.AppendToOutgoingContext(ctx, "user_id", userID, "username", username, "remote_ip", remoteIP)
	ctx = metadata.OutgoingToIncoming(ctx)

	cmd, err := gittest.NewCommandFactory(t, cfg, gitcmd.WithSkipHooks()).New(ctx, repo, subCmd, opt)
	require.NoError(t, err)

	payload, err := gitcmd.HooksPayloadFromEnv(cmd.Env())
	require.NoError(t, err)

	require.Equal(t, userID, payload.UserDetails.UserID)
	require.Equal(t, username, payload.UserDetails.Username)
	require.Equal(t, protocol, payload.UserDetails.Protocol)
	require.Equal(t, remoteIP, payload.UserDetails.RemoteIP)
}

func TestWithReceivePackHooksRemoteIP(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repoProto, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	objectHash, err := repo.ObjectHash(ctx)
	require.NoError(t, err)

	glID := "user-123"
	glUsername := "username"
	protocol := "protocol"
	remoteIP := "5.6.7.8"

	req := &gitalypb.PostReceivePackRequest{
		Repository: repoProto,
		GlId:       glID,
		GlUsername: glUsername,
	}

	opt := gitcmd.WithReceivePackHooks(objectHash, req, protocol)
	subCmd := gitcmd.Command{Name: "receive-pack", Args: []string{"a/b/c"}}

	ctx = grpcmetadata.AppendToOutgoingContext(ctx, "remote_ip", remoteIP)
	ctx = metadata.OutgoingToIncoming(ctx)

	cmd, err := gittest.NewCommandFactory(t, cfg, gitcmd.WithSkipHooks()).New(ctx, repo, subCmd, opt)
	require.NoError(t, err)

	payload, err := gitcmd.HooksPayloadFromEnv(cmd.Env())
	require.NoError(t, err)

	require.Equal(t, glID, payload.UserDetails.UserID)
	require.Equal(t, glUsername, payload.UserDetails.Username)
	require.Equal(t, protocol, payload.UserDetails.Protocol)
	require.Equal(t, remoteIP, payload.UserDetails.RemoteIP)
}
