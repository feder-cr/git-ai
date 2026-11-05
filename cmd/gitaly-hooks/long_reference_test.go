package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestReferenceTransactionPreparing_longReferenceNames runs the binary with no server listening, so a
// name is judged with no RPC. No git process is involved.
func TestReferenceTransactionPreparing_longReferenceNames(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	testcfg.BuildGitalyHooks(t, cfg)

	hookPath := filepath.Join(testhelper.TempDir(t), "reference-transaction")
	require.NoError(t, os.Symlink(cfg.BinaryPath("gitaly-hooks"), hookPath))

	zero := gittest.DefaultObjectHash.ZeroOID.String()
	oid := strings.Repeat("1", gittest.DefaultObjectHash.EncodedLen())
	repository := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: "repository.git"}
	pool := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: gittest.NewObjectPoolName(t)}

	for _, tc := range []struct {
		desc    string
		repo    *gitalypb.Repository
		line    string
		refused string
	}{
		{
			desc: "at the limit", repo: repository,
			line: fmt.Sprintf("%s %s %s", zero, oid, gittest.ReferenceNameOfLength(t, "refs/heads/", 1024)),
		},
		{
			desc: "over the limit", repo: repository,
			line:    fmt.Sprintf("%s %s %s", zero, oid, gittest.ReferenceNameOfLength(t, "refs/heads/", 1025)),
			refused: "over the limit of 1024 bytes",
		},
		{
			desc: "symbolic target over the limit", repo: repository,
			line:    fmt.Sprintf("%s ref:%s refs/heads/alias", zero, gittest.ReferenceNameOfLength(t, "refs/heads/", 1025)),
			refused: "over the limit of 1024 bytes",
		},
		{
			desc: "deletion over the limit", repo: repository,
			line: fmt.Sprintf("%s %s %s", oid, zero, gittest.ReferenceNameOfLength(t, "refs/heads/", 1025)),
		},
		{
			desc: "pool namespace at the pool's limit", repo: pool,
			line: fmt.Sprintf("%s %s %s", zero, oid, gittest.ReferenceNameOfLength(t, "refs/remotes/origin/", 1039)),
		},
		{
			desc: "pool namespace over the pool's limit", repo: pool,
			line:    fmt.Sprintf("%s %s %s", zero, oid, gittest.ReferenceNameOfLength(t, "refs/remotes/origin/", 1040)),
			refused: "over the limit of 1039 bytes",
		},
		{
			desc: "the pool namespace of another repository", repo: repository,
			line:    fmt.Sprintf("%s %s %s", zero, oid, gittest.ReferenceNameOfLength(t, "refs/remotes/origin/", 1039)),
			refused: "over the limit of 1024 bytes",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			payload, err := gitcmd.NewHooksPayload(ctx, cfg, tc.repo, gittest.DefaultObjectHash, nil, nil, gitcmd.ReferenceTransactionHook, nil).Env()
			require.NoError(t, err)

			var stderr bytes.Buffer
			cmd, err := command.New(ctx, testhelper.SharedLogger(t), []string{hookPath, "preparing"},
				command.WithSubprocessLogger(cfg.Logging.Config),
				command.WithEnvironment([]string{payload}),
				command.WithStdin(strings.NewReader(tc.line+"\n")),
				command.WithStderr(&stderr),
			)
			require.NoError(t, err)

			err = cmd.Wait()
			if tc.refused == "" {
				require.NoError(t, err, stderr.String())
				return
			}
			require.Error(t, err)
			require.Contains(t, stderr.String(), tc.refused)
		})
	}
}
