package hook

import (
	"context"
	"errors"
	"os/exec"
	"sync"

	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
)

func validateUpdateHookRequest(ctx context.Context, locator storage.Locator, in *gitalypb.UpdateHookRequest) error {
	return locator.ValidateRepository(ctx, in.GetRepository())
}

func (s *server) UpdateHook(in *gitalypb.UpdateHookRequest, stream gitalypb.HookService_UpdateHookServer) error {
	if err := validateUpdateHookRequest(stream.Context(), s.locator, in); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	var m sync.Mutex
	stdout := streamio.NewSyncWriter(&m, func(p []byte) error {
		return stream.Send(&gitalypb.UpdateHookResponse{Stdout: p})
	})
	stderr := streamio.NewSyncWriter(&m, func(p []byte) error {
		return stream.Send(&gitalypb.UpdateHookResponse{Stderr: p})
	})

	if err := s.manager.UpdateHook(
		stream.Context(),
		in.GetRepository(),
		string(in.GetRef()),
		in.GetOldValue(),
		in.GetNewValue(),
		in.GetEnvironmentVariables(),
		stdout,
		stderr,
	); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return updateHookResponse(stream, int32(exitError.ExitCode()))
		}
		// The storage core refused the name and told the client why on stderr: the hook
		// declines the reference, as a custom hook that exits with an error does.
		var nameConflict updateref.FileDirectoryConflictError
		if errors.As(err, &nameConflict) {
			return updateHookResponse(stream, 1)
		}

		return structerr.NewInternal("%w", err)
	}

	return updateHookResponse(stream, 0)
}

func updateHookResponse(stream gitalypb.HookService_UpdateHookServer, code int32) error {
	if err := stream.Send(&gitalypb.UpdateHookResponse{
		ExitStatus: &gitalypb.ExitStatus{Value: code},
	}); err != nil {
		return structerr.NewInternal("sending response: %w", err)
	}

	return nil
}
