package gittest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// ReferenceNameOfLength returns a reference name of exactly length bytes that starts with prefix. It
// is split into components short enough for the files backend, whose loose references are files.
func ReferenceNameOfLength(tb testing.TB, prefix string, length int) git.ReferenceName {
	tb.Helper()
	require.Greater(tb, length, len(prefix))

	const componentLength = 200

	var name strings.Builder
	name.WriteString(prefix)
	for remaining := length - name.Len(); remaining > 0; remaining = length - name.Len() {
		if remaining <= componentLength+1 {
			name.WriteString(strings.Repeat("x", remaining))
			break
		}
		name.WriteString(strings.Repeat("x", componentLength) + "/")
	}

	return git.ReferenceName(name.String())
}

// SeedPackedReference makes a repository hold a reference without any git process writing it, by writing
// the packed-refs file directly: the way to get a reference over git.MaxReferenceNameLength that existed
// before the limit. The repository must have no packed-refs file yet. Only the files backend has one, so
// the test is skipped on reftable.
func SeedPackedReference(tb testing.TB, repoPath string, name git.ReferenceName, oid git.ObjectID) {
	tb.Helper()
	testhelper.SkipWithReftable(tb, "a reftable cannot be seeded without git writing the name")

	file, err := os.OpenFile(filepath.Join(repoPath, "packed-refs"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	require.NoError(tb, err)
	defer testhelper.MustClose(tb, file)

	_, err = fmt.Fprintf(file, "%s %s\n", oid, name)
	require.NoError(tb, err)
}

// referenceNames returns the names of the references of a repository. They are split on newlines only,
// as a name may hold whitespace other than the ASCII space.
func referenceNames(tb testing.TB, cfg config.Cfg, repoPath string) []string {
	tb.Helper()

	output := strings.TrimSuffix(string(Exec(tb, cfg, "-C", repoPath, "for-each-ref", "--format=%(refname)")), "\n")
	if output == "" {
		return nil
	}

	return strings.Split(output, "\n")
}

// RequireReferenceNameLengths asserts the lengths of the names of all the references of a repository,
// in any order, so that a failure does not print names of a thousand bytes.
func RequireReferenceNameLengths(tb testing.TB, cfg config.Cfg, repoPath string, expected ...int) {
	tb.Helper()

	lengths := []int{}
	for _, name := range referenceNames(tb, cfg, repoPath) {
		lengths = append(lengths, len(name))
	}

	require.ElementsMatch(tb, append([]int{}, expected...), lengths)
}

// RequireNoReferenceNameOverLimit asserts that a repository holds no reference name over
// git.MaxReferenceNameLength.
func RequireNoReferenceNameOverLimit(tb testing.TB, cfg config.Cfg, repoPath string) {
	tb.Helper()

	for _, name := range referenceNames(tb, cfg, repoPath) {
		require.LessOrEqual(tb, len(name), git.MaxReferenceNameLength, "a reference name over the limit was written")
	}
}
