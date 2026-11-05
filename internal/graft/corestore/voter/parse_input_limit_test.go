package voter

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
)

func TestParseInput_referenceNameLength(t *testing.T) {
	t.Parallel()

	hash := git.ObjectHashSHA1
	zero := hash.ZeroOID.String()
	oid := strings.Repeat("1", hash.EncodedLen())

	name := func(length int) string {
		return "refs/heads/" + strings.Repeat("x", length-len("refs/heads/"))
	}
	line := func(oldValue, newValue, reference string) []byte {
		return []byte(fmt.Sprintf("%s %s %s\n", oldValue, newValue, reference))
	}

	for _, tc := range []struct {
		desc    string
		changes []byte
		refused bool
	}{
		{desc: "create at the limit", changes: line(zero, oid, name(1024))},
		{desc: "create over the limit", changes: line(zero, oid, name(1025)), refused: true},
		{desc: "update over the limit", changes: line(oid, oid, name(1025)), refused: true},
		{desc: "symbolic target at the limit", changes: line(zero, "ref:"+name(1024), "refs/heads/alias")},
		{desc: "symbolic target over the limit", changes: line(zero, "ref:"+name(1025), "refs/heads/alias"), refused: true},
		{desc: "deletion over the limit", changes: line(oid, zero, name(1025))},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			_, err := parseInput(tc.changes, hash)
			if tc.refused {
				require.ErrorContains(t, err, "over the limit of 1024 bytes")
				return
			}
			require.NoError(t, err)
		})
	}
}
