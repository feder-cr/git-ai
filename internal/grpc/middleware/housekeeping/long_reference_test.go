package housekeeping

import (
	"context"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// failingStream refuses the first message, as an interceptor below this one does.
type failingStream struct {
	grpc.ServerStream
	ctx context.Context
	err error
}

func (s failingStream) Context() context.Context { return s.ctx }
func (s failingStream) RecvMsg(any) error        { return s.err }

func TestStreamInterceptor_logLevelOfARefusedRequest(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	for desc, tc := range map[string]struct {
		err   error
		level logrus.Level
	}{
		"invalid request": {err: structerr.NewInvalidArgument("refused"), level: logrus.InfoLevel},
		"anything else":   {err: structerr.NewInternal("broken"), level: logrus.ErrorLevel},
	} {
		t.Run(desc, func(t *testing.T) {
			logger := testhelper.NewLogger(t)
			hook := testhelper.AddLoggerHook(logger)

			m := NewHousekeepingMiddleware(logger, protoregistry.GitalyProtoPreregistered, localrepo.Factory{}, nil, MiddlewareConfig{})
			defer m.WaitForWorkers()

			err := m.StreamServerInterceptor()(nil, failingStream{ctx: ctx, err: tc.err},
				&grpc.StreamServerInfo{FullMethod: gitalypb.OperationService_UserCommitFiles_FullMethodName},
				func(any, grpc.ServerStream) error { return nil })
			require.NoError(t, err)

			var levels []logrus.Level
			for _, entry := range hook.AllEntries() {
				if entry.Message == "lookup target repository for housekeeping" {
					levels = append(levels, entry.Level)
				}
			}
			require.Equal(t, []logrus.Level{tc.level}, levels)
		})
	}
}
