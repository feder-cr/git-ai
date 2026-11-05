package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/helper/env"
	"github.com/feder-cr/git-ai/internal/stream"
	"github.com/feder-cr/git-ai/internal/tracing"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
	"github.com/sirupsen/logrus"
	labkitcorrelation "gitlab.com/gitlab-org/labkit/correlation/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type hookError struct {
	returnCode int
	err        error
	clientMsg  string
}

func (e hookError) Error() string {
	return fmt.Sprintf("hook returned error code %d", e.returnCode)
}

type hookCommand struct {
	exec     func(context.Context, gitcmd.HooksPayload, gitalypb.HookServiceClient, []string) error
	hookType gitcmd.Hook
}

var hooksBySubcommand = map[string]hookCommand{
	"update": {
		exec:     updateHook,
		hookType: gitcmd.UpdateHook,
	},
	"pre-receive": {
		exec:     preReceiveHook,
		hookType: gitcmd.PreReceiveHook,
	},
	"post-receive": {
		exec:     postReceiveHook,
		hookType: gitcmd.PostReceiveHook,
	},
	"reference-transaction": {
		exec:     referenceTransactionHook,
		hookType: gitcmd.ReferenceTransactionHook,
	},
}

func main() {
	ctx, _ := tracing.PropagateFromEnv(os.Environ())

	logger, logCloser, err := command.NewSubprocessLogger(ctx, os.Getenv, "gitaly-hooks")
	if err != nil {
		fmt.Printf("new subprocess logger: %q", err)
		os.Exit(1)
	}

	defer func() {
		if err := logCloser.Close(); err != nil {
			fmt.Printf("close log: %q", err)
			os.Exit(1)
		}
	}()

	if err := run(ctx, os.Args); err != nil {
		var hookError hookError
		if errors.As(err, &hookError) {
			if hookError.err != nil {
				notifyError("error executing git hook")
				logger.WithError(hookError.err).Error("error executing git hook")
			}
			if hookError.clientMsg != "" {
				notifyError(hookError.clientMsg)
			}
			os.Exit(hookError.returnCode)
		}

		notifyError("error executing git hook")
		logger.WithError(err).Error("error executing git hook")
		os.Exit(1)
	}
}

// Both stderr and stdout of gitaly-hooks are streamed back to clients. stdout is processed by client
// git process transparently. stderr is dumped directly to client's stdout. Thus, we must be cautious
// what to write into stderr.
func notifyError(msg string) {
	fmt.Fprintln(os.Stderr, msg)
}

func run(ctx context.Context, args []string) error {
	switch filepath.Base(args[0]) {
	case "gitaly-hooks":
		if len(args) < 2 {
			return fmt.Errorf("requires hook name. args: %v", args)
		}

		subCmd := args[1]

		switch subCmd {
		case "git":
			return executeHook(ctx, hookCommand{
				exec:     packObjectsHook,
				hookType: gitcmd.PackObjectsHook,
			}, args[2:])
		}

		return fmt.Errorf("subcommand name invalid: %q", subCmd)
	default:
		hookName := filepath.Base(args[0])
		hookCommand, ok := hooksBySubcommand[hookName]
		if !ok {
			return fmt.Errorf("subcommand name invalid: %q", hookName)
		}

		return executeHook(ctx, hookCommand, args[1:])
	}
}

func executeHook(ctx context.Context, cmd hookCommand, args []string) error {
	payload, err := gitcmd.HooksPayloadFromEnv(os.Environ())
	if err != nil {
		return fmt.Errorf("error when getting hooks payload: %w", err)
	}

	// If the hook wasn't requested, then we simply skip executing any
	// logic.
	if !payload.IsHookRequested(cmd.hookType) {
		return nil
	}

	ctx = injectMetadataIntoOutgoingCtx(ctx, payload)

	tracerCloser := initializeTracing()
	defer func() { _ = tracerCloser.Close() }()

	conn, err := dialGitaly(ctx, payload)
	if err != nil {
		return fmt.Errorf("error when connecting to gitaly: %w", err)
	}
	defer conn.Close()

	hookClient := gitalypb.NewHookServiceClient(conn)

	return cmd.exec(ctx, payload, hookClient, args)
}

func injectMetadataIntoOutgoingCtx(ctx context.Context, payload gitcmd.HooksPayload) context.Context {
	if payload.UserDetails != nil {
		ctx = metadata.AppendToOutgoingContext(
			ctx,
			"user_id",
			payload.UserDetails.UserID,
			"username",
			payload.UserDetails.Username,
			"remote_ip",
			payload.UserDetails.RemoteIP,
		)
	}

	for _, flag := range payload.FeatureFlagsWithValue {
		ctx = featureflag.OutgoingCtxWithFeatureFlag(ctx, flag.Flag, flag.Enabled)
	}
	return ctx
}

func noopSender(c chan error) {}

func dialGitaly(ctx context.Context, payload gitcmd.HooksPayload) (*grpc.ClientConn, error) {
	var dialOpts []grpc.DialOption
	if payload.InternalSocketToken != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(payload.InternalSocketToken)))
	}

	// Propagate correlation ID
	unaryInterceptors := []grpc.UnaryClientInterceptor{
		labkitcorrelation.UnaryClientCorrelationInterceptor(
			labkitcorrelation.WithClientName("gitaly-hooks"),
		),
	}
	streamInterceptors := []grpc.StreamClientInterceptor{
		labkitcorrelation.StreamClientCorrelationInterceptor(
			labkitcorrelation.WithClientName("gitaly-hooks"),
		),
	}

	if spanContext, err := tracing.PropagateFromEnv(os.Environ()); err == nil {
		unaryInterceptors = append(unaryInterceptors, tracing.UnaryPassthroughInterceptor(spanContext))
		streamInterceptors = append(streamInterceptors, tracing.StreamPassthroughInterceptor(spanContext))
	}

	dialOpts = append(dialOpts,
		grpc.WithChainUnaryInterceptor(unaryInterceptors...),
		grpc.WithChainStreamInterceptor(streamInterceptors...),
	)
	conn, err := client.New(ctx, "unix://"+payload.InternalSocket, client.WithGrpcOptions(dialOpts))
	if err != nil {
		return nil, fmt.Errorf("error when dialing: %w", err)
	}

	return conn, nil
}

func initializeTracing() io.Closer {
	// All stdout and stderr are captured by Gitaly process. They may be sent back to users.
	// We don't want to bother them with these redundant logs. As a result, all logs should be
	// suppressed while tracing is in initialization phase.
	//
	//nolint:forbidigo
	output := logrus.StandardLogger().Out
	logrus.SetOutput(io.Discard)
	defer logrus.SetOutput(output)

	// This is a sanitized environment. It does not expose any spans. Instead, it
	// transfers incoming metadata from ENV to gRPC outgoing metadata without
	// modification or starting new spans. This connects the parent span in the
	// Gitaly process with remote spans from subsequent gRPC calls issued by
	// this hook. Tracing is non-critical, so initialization errors are
	// intentionally discarded.
	closer, _ := tracing.InitializeTracerProvider(context.Background(), "gitaly-hooks")
	return closer
}

func gitPushOptions() []string {
	var gitPushOptions []string

	gitPushOptionCount, err := env.GetInt("GIT_PUSH_OPTION_COUNT", 0)
	if err != nil {
		return gitPushOptions
	}

	for i := 0; i < gitPushOptionCount; i++ {
		gitPushOptions = append(gitPushOptions, os.Getenv(fmt.Sprintf("GIT_PUSH_OPTION_%d", i)))
	}

	return gitPushOptions
}

func sendFunc(reqWriter io.Writer, stream grpc.ClientStream, stdin io.Reader) func(errC chan error) {
	return func(errC chan error) {
		_, errSend := io.Copy(reqWriter, stdin)
		errClose := stream.CloseSend()
		if errSend != nil {
			errC <- errSend
		} else {
			errC <- errClose
		}
	}
}

func updateHook(ctx context.Context, payload gitcmd.HooksPayload, hookClient gitalypb.HookServiceClient, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("update hook expects exactly three arguments, got %q", args)
	}
	ref, oldValue, newValue := args[0], args[1], args[2]

	req := &gitalypb.UpdateHookRequest{
		Repository:           payload.Repo,
		EnvironmentVariables: os.Environ(),
		Ref:                  []byte(ref),
		OldValue:             oldValue,
		NewValue:             newValue,
	}

	updateHookStream, err := hookClient.UpdateHook(ctx, req)
	if err != nil {
		return fmt.Errorf("error when starting command for update hook: %w", err)
	}

	if returnCode, err := stream.Handler(func() (stream.StdoutStderrResponse, error) {
		return updateHookStream.Recv()
	}, noopSender, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("error when receiving data for update hook: %w", err)
	} else if returnCode != 0 {
		return hookError{returnCode: int(returnCode)}
	}

	return nil
}

func preReceiveHook(ctx context.Context, payload gitcmd.HooksPayload, hookClient gitalypb.HookServiceClient, args []string) error {
	preReceiveHookStream, err := hookClient.PreReceiveHook(ctx)
	if err != nil {
		return fmt.Errorf("error when getting preReceiveHookStream client for: %w", err)
	}

	if err := preReceiveHookStream.Send(&gitalypb.PreReceiveHookRequest{
		Repository:           payload.Repo,
		EnvironmentVariables: os.Environ(),
		GitPushOptions:       gitPushOptions(),
	}); err != nil {
		return fmt.Errorf("error when sending request for pre-receive hook: %w", err)
	}

	f := sendFunc(streamio.NewWriter(func(p []byte) error {
		return preReceiveHookStream.Send(&gitalypb.PreReceiveHookRequest{Stdin: p})
	}), preReceiveHookStream, os.Stdin)

	if returnCode, err := stream.Handler(func() (stream.StdoutStderrResponse, error) {
		return preReceiveHookStream.Recv()
	}, f, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("error when receiving data for pre-receive hook: %w", err)
	} else if returnCode != 0 {
		return hookError{returnCode: int(returnCode)}
	}

	return nil
}

func postReceiveHook(ctx context.Context, payload gitcmd.HooksPayload, hookClient gitalypb.HookServiceClient, args []string) error {
	postReceiveHookStream, err := hookClient.PostReceiveHook(ctx)
	if err != nil {
		return fmt.Errorf("error when getting stream client for post-receive hook: %w", err)
	}

	if err := postReceiveHookStream.Send(&gitalypb.PostReceiveHookRequest{
		Repository:           payload.Repo,
		EnvironmentVariables: os.Environ(),
		GitPushOptions:       gitPushOptions(),
	}); err != nil {
		return fmt.Errorf("error when sending request for post-receive hook: %w", err)
	}

	f := sendFunc(streamio.NewWriter(func(p []byte) error {
		return postReceiveHookStream.Send(&gitalypb.PostReceiveHookRequest{Stdin: p})
	}), postReceiveHookStream, os.Stdin)

	if returnCode, err := stream.Handler(func() (stream.StdoutStderrResponse, error) {
		return postReceiveHookStream.Recv()
	}, f, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("error when receiving data for post-receive hook: %w", err)
	} else if returnCode != 0 {
		return hookError{returnCode: int(returnCode)}
	}

	return nil
}

func referenceTransactionHook(ctx context.Context, payload gitcmd.HooksPayload, hookClient gitalypb.HookServiceClient, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("reference-transaction hook is missing required arguments, got %q", args)
	}

	var stdin io.Reader = os.Stdin
	var lengthRefusal error

	var state gitalypb.ReferenceTransactionHookRequest_State
	switch args[0] {
	case "preparing":
		changes, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading the reference updates: %w", err)
		}
		objectHash, err := git.ObjectHashByFormat(payload.ObjectFormat)
		if err != nil {
			return fmt.Errorf("looking up object hash: %w", err)
		}
		lengthRefusal = hook.ValidateReferenceNameLengths(payload.Repo, objectHash, changes)

		// The "preparing" state is forwarded only when the Graft storage core is
		// authoritative: its write path builds and uploads the pack of the
		// transaction before git takes the reference locks. Otherwise the RPC is
		// skipped as upstream does, which keeps the votes of Praefect unchanged, and a
		// name over the limit stops here, before git takes the locks of its long path.
		if !payload.StoragePreparing {
			if lengthRefusal != nil {
				return hookError{returnCode: 1, clientMsg: lengthRefusal.Error()}
			}
			return nil
		}
		// The storage core has to learn of the new transaction even when this one is refused,
		// which the server does before it refuses.
		stdin = bytes.NewReader(changes)
		state = gitalypb.ReferenceTransactionHookRequest_PREPARING
	case "prepared":
		state = gitalypb.ReferenceTransactionHookRequest_PREPARED
	case "committed":
		state = gitalypb.ReferenceTransactionHookRequest_COMMITTED
	case "aborted":
		state = gitalypb.ReferenceTransactionHookRequest_ABORTED
	default:
		return fmt.Errorf("reference-transaction hook has invalid state: %q", args[0])
	}

	referenceTransactionHookStream, err := hookClient.ReferenceTransactionHook(ctx)
	if err != nil {
		return fmt.Errorf("error when getting referenceTransactionHookStream client: %w", err)
	}

	if err := referenceTransactionHookStream.Send(&gitalypb.ReferenceTransactionHookRequest{
		Repository:           payload.Repo,
		EnvironmentVariables: os.Environ(),
		State:                state,
	}); err != nil {
		return fmt.Errorf("error when sending request for reference-transaction hook: %w", err)
	}

	f := sendFunc(streamio.NewWriter(func(p []byte) error {
		return referenceTransactionHookStream.Send(&gitalypb.ReferenceTransactionHookRequest{Stdin: p})
	}), referenceTransactionHookStream, stdin)

	returnCode, err := stream.Handler(func() (stream.StdoutStderrResponse, error) {
		return referenceTransactionHookStream.Recv()
	}, f, os.Stdout, os.Stderr)
	// The refusal goes to stderr, where the server finds it in the output of the git process.
	if lengthRefusal != nil {
		return hookError{returnCode: 1, clientMsg: lengthRefusal.Error()}
	}
	if err != nil {
		return fmt.Errorf("error when receiving data for reference-transaction hook: %w", err)
	} else if returnCode != 0 {
		return hookError{returnCode: int(returnCode)}
	}

	return nil
}

func packObjectsHook(ctx context.Context, payload gitcmd.HooksPayload, hookClient gitalypb.HookServiceClient, args []string) error {
	ctx, wt, err := hook.SetupSidechannel(ctx, payload, func(c *net.UnixConn) error {
		// Buffer stdout: serving a packfile (in particular from the pack-objects
		// cache) is a one-way bulk transfer, so batching the sideband packets
		// reduces the number of writes into the stdout pipe.
		return stream.ProxyPktLine(c, os.Stdin, os.Stdout, os.Stderr, true)
	})
	if err != nil {
		return hookError{returnCode: 1, err: fmt.Errorf("RPC failed: SetupSidechannel: %w", err)}
	}
	defer func() {
		// We already check the error further down.
		_ = wt.Close()
	}()

	var glID, glUsername, gitProtocol, remoteIP string

	if payload.UserDetails != nil {
		glID = payload.UserDetails.UserID
		glUsername = payload.UserDetails.Username
		gitProtocol = payload.UserDetails.Protocol
		remoteIP = payload.UserDetails.RemoteIP
	}

	if _, err := hookClient.PackObjectsHookWithSidechannel(
		ctx,
		&gitalypb.PackObjectsHookWithSidechannelRequest{
			Repository:           payload.Repo,
			EnvironmentVariables: os.Environ(),
			Args:                 args,
			GlId:                 glID,
			GlUsername:           glUsername,
			GitProtocol:          gitProtocol,
			RemoteIp:             remoteIP,
		},
	); err != nil {
		return wrapGRPCError(err)
	}

	if err := wt.Wait(); err != nil {
		return hookError{returnCode: 1, err: fmt.Errorf("RPC failed: %w", err)}
	}

	if err := wt.Close(); err != nil {
		return hookError{returnCode: 1, err: fmt.Errorf("RPC failed: closing sidechannel: %w", err)}
	}

	return nil
}

func wrapGRPCError(err error) error {
	wrappedErr := hookError{
		returnCode: 1,
		err:        fmt.Errorf("RPC failed: %w", err),
	}
	if e, ok := status.FromError(err); ok {
		switch e.Code() {
		case codes.ResourceExhausted:
			wrappedErr.clientMsg = "error resource exhausted, please try again later"
		}
	}
	return wrappedErr
}
