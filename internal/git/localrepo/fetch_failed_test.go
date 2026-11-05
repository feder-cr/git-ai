package localrepo

import (
	"errors"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestFetchOpts_fetchFailed(t *testing.T) {
	t.Parallel()

	failure := errors.New("exit status 128")
	refusal := git.ValidateReferenceNameLength(strings.Repeat("x", 1025)).Error()

	code := func(opts FetchOpts, stderr string) codes.Code {
		err := opts.fetchFailed(failure, []byte(stderr))
		require.ErrorAs(t, err, &FetchFailedError{})

		var coded interface{ GRPCStatus() *status.Status }
		if errors.As(err, &coded) {
			return coded.GRPCStatus().Code()
		}
		return codes.Unknown
	}

	require.Equal(t, codes.InvalidArgument, code(FetchOpts{}, refusal+"\nfatal: ref updates aborted by hook\n"))
	require.Equal(t, codes.Unknown, code(FetchOpts{}, "remote: "+refusal+"\nfatal: the remote hung up\n"), "the remote cannot choose the error")
	require.Equal(t, codes.Unknown, code(FetchOpts{DisableTransactions: true}, refusal+"\n"), "no hook ran")
	require.Equal(t, codes.Unknown, code(FetchOpts{}, "fatal: the remote hung up\n"))
}
