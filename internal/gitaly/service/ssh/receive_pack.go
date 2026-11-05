package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/stats"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/transaction/voting"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
)

func (s *server) SSHReceivePack(stream gitalypb.SSHService_SSHReceivePackServer) error {
	req, err := stream.Recv() // First request contains only Repository, GlId, and GlUsername
	if err != nil {
		return structerr.NewInternal("%w", err)
	}

	s.logger.WithFields(log.Fields{
		"GlID":             req.GetGlId(),
		"GlRepository":     req.GetGlRepository(),
		"GlUsername":       req.GetGlUsername(),
		"GitConfigOptions": req.GetGitConfigOptions(),
		"GitProtocol":      req.GetGitProtocol(),
	}).DebugContext(stream.Context(), "SSHReceivePack")

	if err = validateFirstReceivePackRequest(stream.Context(), s.locator, req); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	if err := s.sshReceivePack(stream, req); err != nil {
		return structerr.NewInternal("%w", err)
	}

	// In cases where all reference updates are rejected by git-receive-pack(1), we would end up
	// with no transactional votes at all. We thus do a final vote which concludes this RPC to
	// ensure there's always at least one vote. In case there was diverging behaviour in
	// git-receive-pack(1) which led to a different outcome across voters, then this final vote
	// would fail because the sequence of votes would be different.
	if err := transaction.VoteOnContext(stream.Context(), s.txManager, voting.Vote{}, voting.Committed); err != nil {
		// When the pre-receive hook failed, git-receive-pack(1) exits with code 0.
		// It's arguable whether this is the expected behavior, but anyhow it means
		// cmd.Wait() did not error out. On the other hand, the gitaly-hooks command did
		// stop the transaction upon failure. So this final vote fails.
		// To avoid this error being presented to the end user, ignore it when the
		// transaction was stopped.
		if !errors.Is(err, transaction.ErrTransactionStopped) {
			return structerr.NewAborted("final transactional vote: %w", err)
		}
	}

	return nil
}

func (s *server) sshReceivePack(stream gitalypb.SSHService_SSHReceivePackServer, req *gitalypb.SSHReceivePackRequest) (returnedErr error) {
	ctx := stream.Context()

	stdin := streamio.NewReader(func() ([]byte, error) {
		request, err := stream.Recv()
		return request.GetStdin(), err
	})

	var m sync.Mutex
	stdout := streamio.NewSyncWriter(&m, func(p []byte) error {
		return stream.Send(&gitalypb.SSHReceivePackResponse{Stdout: p})
	})

	// We both need to listen in on the stderr stream in order to be able to judge what exactly
	// is happening, but also relay the output to the client. We thus create a MultiWriter to
	// enable both at the same time.
	var stderrBuilder strings.Builder
	stderr := streamio.NewSyncWriter(&m, func(p []byte) error {
		return stream.Send(&gitalypb.SSHReceivePackResponse{Stderr: p})
	})
	stderr = io.MultiWriter(&stderrBuilder, stderr)

	repoPath, err := s.locator.GetRepoPath(ctx, req.GetRepository())
	if err != nil {
		return err
	}

	config, err := gitcmd.ConvertConfigOptions(req.GetGitConfigOptions())
	if err != nil {
		return err
	}

	// The client may never close stdin, see command.DetachStdin. Recording happens when the copy
	// ended rather than in a deferred call, so the RPC never waits for stdin to drain.
	var copied func()
	if featureflag.ReceivePackStats.IsEnabled(ctx) {
		statsCapture := &stats.PrefixCapture{}
		copied = func() {
			stats.RecordReceivePack(ctx, s.logger, statsCapture.Bytes(), s.receivePackObjectsMetrics)
		}
		stdin = io.TeeReader(stdin, statsCapture)
	}

	pr, err := command.DetachStdin(stdin, copied)
	if err != nil {
		return fmt.Errorf("detaching stdin: %w", err)
	}
	defer pr.Close()

	repo := s.localRepoFactory.Build(req.GetRepository())

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	cmd, err := repo.Exec(ctx, gitcmd.Command{Name: "receive-pack", Args: []string{repoPath}},
		gitcmd.WithStdin(pr),
		gitcmd.WithStdout(stdout),
		gitcmd.WithStderr(stderr),
		gitcmd.WithReceivePackHooks(objectHash, req, "ssh"),
		gitcmd.WithGitProtocol(s.logger, req),
		gitcmd.WithConfig(config...),
	)
	if err != nil {
		return fmt.Errorf("start cmd: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		status, ok := command.ExitStatus(err)
		if !ok {
			return fmt.Errorf("extracting exit status: %w", err)
		}

		// When the command has failed we both want to send its exit status as well as
		// return an error from this RPC call. Otherwise we'd fail the RPC, but return with
		// an `OK` error code to the client.
		if errSend := stream.Send(&gitalypb.SSHReceivePackResponse{
			ExitStatus: &gitalypb.ExitStatus{Value: int32(status)},
		}); errSend != nil {
			s.logger.WithError(errSend).ErrorContext(ctx, "send final status code")
		}

		// Detect the case where the user has cancelled the push and log it with a proper
		// gRPC error code. We can't do anything about this error anyway and it is a totally
		// valid outcome.
		if stderrBuilder.String() == "fatal: the remote end hung up unexpectedly\n" {
			return structerr.NewCanceled("user canceled the push")
		}

		return fmt.Errorf("cmd wait: %w", err)
	}

	return nil
}

func validateFirstReceivePackRequest(ctx context.Context, locator storage.Locator, req *gitalypb.SSHReceivePackRequest) error {
	if req.GetGlId() == "" {
		return errors.New("empty GlId")
	}
	if req.Stdin != nil {
		return errors.New("non-empty data in first request")
	}
	return locator.ValidateRepository(ctx, req.GetRepository())
}
