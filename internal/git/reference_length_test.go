package git

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateReferenceNameLength(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1024, MaxReferenceNameLength)
	require.NoError(t, ValidateReferenceNameLength("refs/heads/main"))
	require.NoError(t, ValidateReferenceNameLength(strings.Repeat("x", 1024)))

	err := ValidateReferenceNameLength(strings.Repeat("x", 1025))
	require.EqualError(t, err, "reference name is 1025 bytes long, over the limit of 1024 bytes")

	// The limit counts bytes, not characters.
	require.Error(t, ValidateReferenceNameLength(strings.Repeat("é", 513)))
}

// TestValidateReference_doesNotLimitTheLength pins that ValidateReference, which also judges existing
// names read back from the storage core's database, accepts a long name: the limit applies to writes.
func TestValidateReference_doesNotLimitTheLength(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateReference("refs/heads/"+strings.Repeat("x", 2000)))
}

func TestValidateRepositoryReferenceNameLength(t *testing.T) {
	t.Parallel()

	require.Equal(t, 15, ObjectPoolReferenceNameAllowance)

	name := func(prefix string, length int) string { return prefix + strings.Repeat("x", length-len(prefix)) }

	require.NoError(t, ValidateRepositoryReferenceNameLength(name("refs/remotes/origin/", 1039), true))
	require.Error(t, ValidateRepositoryReferenceNameLength(name("refs/remotes/origin/", 1040), true))
	require.Error(t, ValidateRepositoryReferenceNameLength(name("refs/remotes/origin/", 1025), false))
	require.Error(t, ValidateRepositoryReferenceNameLength(name("refs/heads/", 1025), true))
}

func TestIsReferenceNameLengthRefusal(t *testing.T) {
	t.Parallel()

	refusal := ValidateReferenceNameLength(strings.Repeat("x", 1025))
	require.True(t, IsReferenceNameLengthRefusal([]byte("error: "+refusal.Error()+"\nfatal: ref updates aborted by hook\n")))
	require.False(t, IsReferenceNameLengthRefusal([]byte("fatal: ref updates aborted by hook\n")))
	require.False(t, IsReferenceNameLengthRefusal([]byte("remote: "+refusal.Error()+"\nfatal: ref updates aborted by hook\n")))
	require.False(t, IsReferenceNameLengthRefusal([]byte("remote: progress\rremote: "+refusal.Error()+"\r\n")))
	require.True(t, IsReferenceNameLengthRefusal([]byte("remote: progress\n"+refusal.Error()+"\n")))
}
