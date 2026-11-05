package hook

import (
	"context"
	"errors"

	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
)

func validateReferenceTransactionHookRequest(ctx context.Context, locator storage.Locator, in *gitalypb.ReferenceTransactionHookRequest) error {
	return locator.ValidateRepository(ctx, in.GetRepository())
}

func (s *server) ReferenceTransactionHook(stream gitalypb.HookService_ReferenceTransactionHookServer) error {
	request, err := stream.Recv()
	if err != nil {
		return structerr.NewInternal("receiving first request: %w", err)
	}

	if err := validateReferenceTransactionHookRequest(stream.Context(), s.locator, request); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	var state hook.ReferenceTransactionState
	switch request.GetState() {
	case gitalypb.ReferenceTransactionHookRequest_PREPARING:
		state = hook.ReferenceTransactionPreparing
	case gitalypb.ReferenceTransactionHookRequest_PREPARED:
		state = hook.ReferenceTransactionPrepared
	case gitalypb.ReferenceTransactionHookRequest_COMMITTED:
		state = hook.ReferenceTransactionCommitted
	case gitalypb.ReferenceTransactionHookRequest_ABORTED:
		state = hook.ReferenceTransactionAborted
	default:
		return structerr.NewInvalidArgument("invalid hook state")
	}

	stdin := streamio.NewReader(func() ([]byte, error) {
		req, err := stream.Recv()
		return req.GetStdin(), err
	})

	if err := s.manager.ReferenceTransactionHook(
		stream.Context(),
		state,
		request.GetEnvironmentVariables(),
		stdin,
	); err != nil {
		// The storage core refused a reference git cannot hold next to another one, after
		// git's transaction began, so git refuses the whole transaction. git gives this hook
		// no side band: on SSH its stderr reaches the client as is, over HTTP it does not.
		var nameConflict updateref.FileDirectoryConflictError
		if errors.As(err, &nameConflict) {
			if err := stream.Send(&gitalypb.ReferenceTransactionHookResponse{
				Stderr: []byte(hook.RefNameConflictMessage(nameConflict) + "\n"),
			}); err != nil {
				return structerr.NewInternal("sending response: %w", err)
			}
			if err := stream.Send(&gitalypb.ReferenceTransactionHookResponse{
				ExitStatus: &gitalypb.ExitStatus{Value: 1},
			}); err != nil {
				return structerr.NewInternal("sending response: %w", err)
			}
			return nil
		}

		switch {
		case errors.Is(err, transaction.ErrTransactionAborted):
			return structerr.NewAborted("reference-transaction hook: %w", err)
		case errors.Is(err, transaction.ErrTransactionStopped):
			return structerr.NewFailedPrecondition("reference-transaction hook: %w", err)
		default:
			return structerr.NewInternal("reference-transaction hook: %w", err)
		}
	}

	if err := stream.Send(&gitalypb.ReferenceTransactionHookResponse{
		ExitStatus: &gitalypb.ExitStatus{Value: 0},
	}); err != nil {
		return structerr.NewInternal("sending response: %w", err)
	}

	return nil
}
