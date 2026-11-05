package gitcmd

import (
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestReferenceNameLengthRefusal(t *testing.T) {
	t.Parallel()

	require.NoError(t, ReferenceNameLengthRefusal([]byte("fatal: ref updates aborted by hook\n")))

	refused := git.ValidateReferenceNameLength(strings.Repeat("x", 1025))
	err := ReferenceNameLengthRefusal([]byte(refused.Error() + "\nfatal: ref updates aborted by hook\n"))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "over the limit of 1024 bytes")
}
