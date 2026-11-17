package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
)

// TestCLILandConflictsAndResolved drives the agent's way through a conflict the server does not
// resolve (it has no resolver here): graft land --conflicts-dir writes the package, the
// agent edits the file with the markers, and graft land --resolved lands it.
func TestCLILandConflictsAndResolved(t *testing.T) {
	for _, strategy := range []string{"rebase", "merge"} {
		for _, resolve := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/resolve=%t", strategy, resolve), func(t *testing.T) {
				testCLILandConflictsAndResolved(t, strategy, resolve)
			})
		}
	}
}

func testCLILandConflictsAndResolved(t *testing.T, strategy string, resolve bool) {
	t.Helper()

	env := startServer(t)
	base := env.seedMain(t, map[string]string{"x.txt": "1\nbase\n3\n", "y.txt": "y\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	// Another writer changes x.txt and y.txt on main; the local change edits x.txt too.
	gittest.WriteCommit(t, env.cfg, env.repoPath,
		gittest.WithTree(env.writeTree(t, map[string]string{"x.txt": "1\ntip\n3\n", "y.txt": "y2\n"})),
		gittest.WithParents(base), gittest.WithBranch("main"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.txt"), []byte("1\nlocal\n3\n"), 0o644))
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "local x")
	require.Equal(t, exitOK, code, stderr)

	conflicts := filepath.Join(t.TempDir(), "conflicts")
	args := []string{"land", "-C", dir, "-author", "Dev <dev@example.com>", "-strategy", strategy, "-conflicts-dir", conflicts}
	if resolve {
		args = append(args, "-resolve")
	}
	code, stdout, stderr := runCLI(t, args...)
	require.Equal(t, exitError, code, stdout)
	require.Contains(t, stderr, "merge conflict")
	require.Contains(t, stdout, "the land did not resolve 1 conflicting files")
	require.Contains(t, stdout, "x.txt: not_attempted")
	if resolve {
		require.Contains(t, stdout, "conflict resolution is not enabled")
	} else {
		require.Contains(t, stdout, "conflict resolution was not requested")
	}
	require.Contains(t, stdout, "graft land --resolved "+conflicts)

	// The file to edit holds the merge with git's diff3 markers; the sides are beside it.
	marked, err := os.ReadFile(filepath.Join(conflicts, "files", "x.txt"))
	require.NoError(t, err)
	require.Equal(t, "1\n<<<<<<< ours (the branch tip)\ntip\n||||||| base\nbase\n=======\nlocal\n>>>>>>> theirs (the change)\n3\n", string(marked))
	for side, want := range map[string]string{"base": "1\nbase\n3\n", "ours": "1\ntip\n3\n", "theirs": "1\nlocal\n3\n"} {
		got, err := os.ReadFile(filepath.Join(conflicts, "sides", side, "x.txt"))
		require.NoError(t, err)
		require.Equal(t, want, string(got), side)
	}
	require.FileExists(t, filepath.Join(conflicts, "conflict.json"))

	// The agent resolves the file and lands the resolution.
	require.NoError(t, os.WriteFile(filepath.Join(conflicts, "files", "x.txt"), []byte("1\ntip and local\n3\n"), 0o644))
	// Repeating the failed land must not overwrite the agent's edits.
	code, _, stderr = runCLI(t, args...)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "already holds a conflicts directory")
	edited, err := os.ReadFile(filepath.Join(conflicts, "files", "x.txt"))
	require.NoError(t, err)
	require.Equal(t, "1\ntip and local\n3\n", string(edited))

	code, stdout, stderr = runCLI(t, "land", "-C", dir, "-author", "Dev <dev@example.com>", "-resolved", conflicts)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "landed at ")
	require.Contains(t, stdout, "(resolved 1 conflicting files: x.txt)")

	code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:x.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, "1\ntip and local\n3\n", stdout)
	code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:y.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, "y2\n", stdout, "the other writer's change is kept")
	code, stdout, _ = runCLI(t, "show", "-C", dir, "-json", "main")
	require.Equal(t, exitOK, code)
	var shown struct {
		Trailers map[string][]string `json:"trailers"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &shown), stdout)
	require.Equal(t, []string{"agent"}, shown.Trailers["Graft-Resolved-By"])
	require.Equal(t, []string{"x.txt"}, shown.Trailers["Graft-Resolved-Path"])
	require.NotContains(t, shown.Trailers, "Graft-Checked-By", "no resolver checked it")
}

// TestCLILandConflictsDirectoryNamespacesAreDisjoint: conflicting paths named like the package
// or like a side of another file are written and resolved like any other.
func TestCLILandConflictsDirectoryNamespacesAreDisjoint(t *testing.T) {
	env := startServer(t)
	names := []string{"conflict.json", "x.txt", "sides/x.txt.base", "files/x.txt"}
	seed := map[string]string{}
	for _, n := range names {
		seed[n] = "base\n"
	}
	base := env.seedMain(t, seed)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	remote := map[string]string{}
	for _, n := range names {
		remote[n] = "remote " + n + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, filepath.FromSlash(n)), []byte("local "+n+"\n"), 0o644))
	}
	gittest.WriteCommit(t, env.cfg, env.repoPath, gittest.WithTree(env.writeTree(t, remote)), gittest.WithParents(base), gittest.WithBranch("main"))
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "local")
	require.Equal(t, exitOK, code, stderr)

	conflicts := filepath.Join(t.TempDir(), "conflicts")
	code, stdout, _ := runCLI(t, "land", "-C", dir, "-author", "Dev <dev@example.com>", "-resolve", "-conflicts-dir", conflicts)
	require.Equal(t, exitError, code, stdout)
	for _, n := range names {
		marked, err := os.ReadFile(filepath.Join(conflicts, "files", filepath.FromSlash(n)))
		require.NoError(t, err, n)
		require.Contains(t, string(marked), "local "+n, n)
		require.FileExists(t, filepath.Join(conflicts, "sides", "theirs", filepath.FromSlash(n)), n)
		require.NoError(t, os.WriteFile(filepath.Join(conflicts, "files", filepath.FromSlash(n)), []byte("resolved "+n+"\n"), 0o644))
	}
	require.FileExists(t, filepath.Join(conflicts, "conflict.json"))

	code, _, stderr = runCLI(t, "land", "-C", dir, "-author", "Dev <dev@example.com>", "-resolved", conflicts)
	require.Equal(t, exitOK, code, stderr)
	for _, n := range names {
		code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:"+n)
		require.Equal(t, exitOK, code, n)
		require.Equal(t, "resolved "+n+"\n", stdout, n)
	}
}

func TestCLILandConflictFlagsAreChecked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "land", "-C", dir, "-resolved", "x", "-resolve")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "--resolved takes neither")

	code, _, stderr = runCLI(t, "land", "-C", dir, "-resolved", "x", "-conflicts-dir", "y")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "--resolved takes neither")
}

// TestCLILandConflictsWithAnUnavailableSide: a side that the merge wrote for the conflict (the
// content merge of a rename) is not kept by the server, so the package gives no object ID for it.
// The conflicts directory is still written: that side is said to be unavailable and left out, the
// file to edit is the side there is, and the resolution lands.
func TestCLILandConflictsWithAnUnavailableSide(t *testing.T) {
	lines := func(replace map[int]string) string {
		out := ""
		for i := 1; i <= 20; i++ {
			if r, ok := replace[i]; ok {
				out += r + "\n"
			} else {
				out += fmt.Sprintf("%d\n", i)
			}
		}
		return out
	}
	env := startServer(t)
	base := env.seedMain(t, map[string]string{"a.txt": lines(nil)})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	// Main renames a.txt to c.txt and changes line 19; the local change edits line 2 of a.txt and
	// adds a c.txt of its own.
	gittest.WriteCommit(t, env.cfg, env.repoPath,
		gittest.WithTree(env.writeTree(t, map[string]string{"c.txt": lines(map[int]string{19: "nineteen-ours"})})),
		gittest.WithParents(base), gittest.WithBranch("main"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte(lines(map[int]string{2: "two-theirs"})), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.txt"), []byte("added by theirs\n"), 0o644))
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "local")
	require.Equal(t, exitOK, code, stderr)

	conflicts := filepath.Join(t.TempDir(), "conflicts")
	code, stdout, _ := runCLI(t, "land", "-C", dir, "-author", "Dev <dev@example.com>", "-strategy", "merge", "-resolve", "-conflicts-dir", conflicts)
	require.Equal(t, exitError, code, stdout)
	require.Contains(t, stdout, "c.txt: not_attempted")
	require.Contains(t, stdout, "ours side unavailable")
	require.Contains(t, stdout, "wrote the conflicts to "+conflicts)

	require.NoFileExists(t, filepath.Join(conflicts, "sides", "ours", "c.txt"))
	theirs, err := os.ReadFile(filepath.Join(conflicts, "sides", "theirs", "c.txt"))
	require.NoError(t, err)
	require.Equal(t, "added by theirs\n", string(theirs))
	edit, err := os.ReadFile(filepath.Join(conflicts, "files", "c.txt"))
	require.NoError(t, err)
	require.Contains(t, string(edit), "<<<<<<< ours (the branch tip): unavailable, ", "the unavailable side is said so")
	require.Contains(t, string(edit), "=======\nadded by theirs\n>>>>>>> theirs (the change)\n", "the side there is, between markers")

	// Landed unedited, the file is refused by the marker rule: the tip's side is never dropped.
	code, _, stderr = runCLI(t, "land", "-C", dir, "-author", "Dev <dev@example.com>", "-resolved", conflicts)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "merge conflict")
	code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:c.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, lines(map[int]string{19: "nineteen-ours"}), stdout, "the tip is unchanged")

	resolved := lines(map[int]string{2: "two-theirs", 19: "nineteen-ours"}) + "added by theirs\n"
	require.NoError(t, os.WriteFile(filepath.Join(conflicts, "files", "c.txt"), []byte(resolved), 0o644))
	code, _, stderr = runCLI(t, "land", "-C", dir, "-author", "Dev <dev@example.com>", "-resolved", conflicts)
	require.Equal(t, exitOK, code, stderr)
	code, stdout, _ = runCLI(t, "cat", "-C", dir, "main:c.txt")
	require.Equal(t, exitOK, code)
	require.Equal(t, resolved, stdout)
}
