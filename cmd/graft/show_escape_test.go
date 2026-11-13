package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/stretchr/testify/require"
)

// TestCLIShowEscapesServerText checks that graft show prints the author, the
// subject and the file paths of a commit as displayText shows them. A commit's
// author, subject and file names come from other clients: printed raw, an
// escape in them would move the cursor of the terminal and overwrite the lines
// above, for example to forge which files a commit changed.
func TestCLIShowEscapesServerText(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a.txt", []byte("one"))
	want := commitDetails{
		OID:      strings.Repeat("c", 40),
		Parents:  []string{strings.Repeat("b", 40)},
		Author:   "Ada\x1b[2K <ada@example.com>",
		Subject:  "fix\x1b[1A\x1b[2Kforged",
		Message:  "fix\x1b[1A\x1b[2Kforged",
		Trailers: map[string][]string{},
		Files:    []fileDetails{{Path: "ledger/a\x1b[2J.txt", Additions: 2, Deletions: 1}},
	}
	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) {
		return detailBackend{MemoryBackend: mem, details: want}, nil
	}
	t.Cleanup(func() { backendFactory = previous })

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "show", "-C", dir, "main")
	require.Equal(t, exitOK, code, stderr)
	require.NotContains(t, stdout, "\x1b", "no raw escape reaches the terminal")

	require.Contains(t, stdout, "Author: Ada"+`\x1b[2K`+" <ada@example.com>")
	require.Contains(t, stdout, "\n"+`fix\x1b[1A\x1b[2Kforged`+"\n")
	require.Contains(t, stdout, "+2 -1\tledger/a"+`\x1b[2J`+".txt")
}
