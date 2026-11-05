package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestHookService_storageRefNameConflict covers how the hook service hands the storage core's
// refusals to gitaly-hooks: a name conflict declines the reference with an exit status (the
// reason was written to stderr), the reference-transaction hook writes the reason to stderr
// itself, and a database that cannot be read stays Unavailable (structerr.NewInternal keeps the
// code of a structerr error it wraps).
func TestHookService_storageRefNameConflict(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	conflict := updateref.FileDirectoryConflictError{ConflictingReferenceName: "refs/heads/a/b", ExistingReferenceName: "refs/heads/a"}
	message := "refusing refs/heads/a/b: git cannot hold it next to refs/heads/a; push the other references without it\n"

	var updateErr error
	manager := hook.NewMockManager(t, nil, nil,
		func(t *testing.T, ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error {
			return updateErr
		},
		func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
			return fmt.Errorf("storage core: %w", conflict)
		},
	)
	cfg.SocketPath = runHooksServer(t, cfg, nil, testserver.WithHookManager(manager))
	repo, _ := gittest.CreateRepository(t, ctx, cfg)
	client, conn := newHooksClient(t, cfg.SocketPath)
	defer conn.Close()

	update := func() (int32, error) {
		stream, err := client.UpdateHook(ctx, &gitalypb.UpdateHookRequest{Repository: repo, Ref: []byte("refs/heads/a/b")})
		require.NoError(t, err)
		for {
			resp, err := stream.Recv()
			if err != nil {
				return 0, err
			}
			if resp.GetExitStatus() != nil {
				return resp.GetExitStatus().GetValue(), nil
			}
		}
	}

	t.Run("update hook, name conflict", func(t *testing.T) {
		updateErr = fmt.Errorf("storage core: %w", conflict)
		code, err := update()
		require.NoError(t, err)
		require.Equal(t, int32(1), code)
	})

	t.Run("update hook, database unavailable", func(t *testing.T) {
		updateErr = structerr.NewUnavailable("storage core: the database is gone")
		_, err := update()
		require.Equal(t, codes.Unavailable, status.Code(err), "error: %v", err)
	})

	t.Run("reference-transaction hook, name conflict", func(t *testing.T) {
		stream, err := client.ReferenceTransactionHook(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.ReferenceTransactionHookRequest{
			Repository: repo, State: gitalypb.ReferenceTransactionHookRequest_PREPARED,
		}))
		require.NoError(t, stream.CloseSend())

		var stderr []byte
		var exit *gitalypb.ExitStatus
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			stderr = append(stderr, resp.GetStderr()...)
			if resp.GetExitStatus() != nil {
				exit = resp.GetExitStatus()
			}
		}
		require.Equal(t, message, string(stderr))
		require.Equal(t, int32(1), exit.GetValue())
	})
}
