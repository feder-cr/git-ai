package voter

import (
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
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

			_, err := parseInput(tc.changes, hash, false)
			if tc.refused {
				require.ErrorContains(t, err, "over the limit of 1024 bytes")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestParseInput_objectPoolAllowance(t *testing.T) {
	t.Parallel()

	hash := git.ObjectHashSHA1
	line := func(prefix string, length int) []byte {
		return []byte(fmt.Sprintf("%s %s %s%s\n", hash.ZeroOID, strings.Repeat("1", hash.EncodedLen()), prefix, strings.Repeat("x", length-len(prefix))))
	}

	_, err := parseInput(line("refs/remotes/origin/", 1039), hash, true)
	require.NoError(t, err)
	_, err = parseInput(line("refs/remotes/origin/", 1040), hash, true)
	require.ErrorContains(t, err, "over the limit of 1039 bytes")
	_, err = parseInput(line("refs/remotes/origin/", 1025), hash, false)
	require.ErrorContains(t, err, "over the limit of 1024 bytes")
	_, err = parseInput(line("refs/heads/", 1025), hash, true)
	require.ErrorContains(t, err, "over the limit of 1024 bytes")
}

func TestParsePayloadInput_objectPool(t *testing.T) {
	t.Parallel()

	hash := git.ObjectHashSHA1
	changes := []byte(fmt.Sprintf("%s %s refs/remotes/origin/%s\n", hash.ZeroOID, strings.Repeat("1", hash.EncodedLen()), strings.Repeat("x", 1039-len("refs/remotes/origin/"))))

	_, err := parsePayloadInput(gitcmd.HooksPayload{Repo: &gitalypb.Repository{RelativePath: "@cluster/pools/ab/cd/1"}}, changes, hash)
	require.NoError(t, err)

	_, err = parsePayloadInput(gitcmd.HooksPayload{Repo: &gitalypb.Repository{RelativePath: "repository.git"}}, changes, hash)
	require.ErrorContains(t, err, "over the limit of 1024 bytes")
}
