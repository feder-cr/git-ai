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
