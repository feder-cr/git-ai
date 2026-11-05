package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
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

// lengthCheckHookServer records what it gets of the preparing state, and refuses like the real one.
type lengthCheckHookServer struct {
	gitalypb.UnimplementedHookServiceServer
	calls  atomic.Int32
	stdin  atomic.Value
	refuse bool
}

func (s *lengthCheckHookServer) ReferenceTransactionHook(stream gitalypb.HookService_ReferenceTransactionHookServer) error {
	s.calls.Add(1)

	var stdin []byte
	for {
		request, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		stdin = append(stdin, request.GetStdin()...)
	}
	s.stdin.Store(string(stdin))

	if s.refuse {
		return structerr.NewInvalidArgument("refused")
	}
	return stream.Send(&gitalypb.ReferenceTransactionHookResponse{ExitStatus: &gitalypb.ExitStatus{Value: 0}})
}

// TestReferenceTransactionPreparing_authoritative: with the storage core the server gets the transaction
// even when its name is refused, because the core has to learn that it started, and the refusal still
// reaches stderr.
func TestReferenceTransactionPreparing_authoritative(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	zero := gittest.DefaultObjectHash.ZeroOID.String()
	oid := strings.Repeat("1", gittest.DefaultObjectHash.EncodedLen())

	for desc, tc := range map[string]struct {
		length  int
		refused bool
	}{
		"at the limit":   {length: 1024},
		"over the limit": {length: 1025, refused: true},
	} {
		t.Run(desc, func(t *testing.T) {
			t.Parallel()

			cfg := testcfg.Build(t)
			testcfg.BuildGitalyHooks(t, cfg)

			server := &lengthCheckHookServer{refuse: tc.refused}
			testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
				gitalypb.RegisterHookServiceServer(srv, server)
			}, testserver.WithDisablePraefect())

			hookPath := filepath.Join(testhelper.TempDir(t), "reference-transaction")
			require.NoError(t, os.Symlink(cfg.BinaryPath("gitaly-hooks"), hookPath))

			payload := gitcmd.NewHooksPayload(ctx, cfg, &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: "repository.git"},
				gittest.DefaultObjectHash, nil, nil, gitcmd.ReferenceTransactionHook, nil)
			payload.StoragePreparing = true
			env, err := payload.Env()
			require.NoError(t, err)

			line := fmt.Sprintf("%s %s %s\n", zero, oid, gittest.ReferenceNameOfLength(t, "refs/heads/", tc.length))

			var stderr bytes.Buffer
			cmd, err := command.New(ctx, testhelper.SharedLogger(t), []string{hookPath, "preparing"},
				command.WithSubprocessLogger(cfg.Logging.Config),
				command.WithEnvironment([]string{env}),
				command.WithStdin(strings.NewReader(line)),
				command.WithStderr(&stderr),
			)
			require.NoError(t, err)
			err = cmd.Wait()

			require.EqualValues(t, 1, server.calls.Load(), "the server gets the transaction")
			require.Equal(t, line, server.stdin.Load(), "the hook forwards what it read")
			if tc.refused {
				require.Error(t, err)
				require.Contains(t, stderr.String(), "over the limit of 1024 bytes")
				return
			}
			require.NoError(t, err, stderr.String())
		})
	}
}
