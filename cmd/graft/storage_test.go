package main

import (
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/sdk"
)

// TestCLIStorageAgainstServer drives graft storage against a real server whose
// storage core runs in shadow mode. It runs serially because the CLI tests
// share the package-level backend factory.
func TestCLIStorageAgainstServer(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	core := coretest.Open(t, coretest.Options{Mode: corestore.ModeShadow})
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll, testserver.WithGraftStorage(core))

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	main := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	target := []string{"-address", addr, "-storage", repo.GetStorageName(), "-repo", repo.GetRelativePath()}
	cli := func(args ...string) (int, string, string) {
		return runCLI(t, append(append([]string{"storage"}, args...), target...)...)
	}

	code, stdout, stderr := cli("state")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "mode\tshadow\n")
	require.Contains(t, stdout, "tracked\tfalse\n")
	require.Contains(t, stdout, "node_id\tnode-a\n")

	code, _, stderr = cli("verify")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "not imported")

	code, stdout, stderr = cli("import", "--json")
	require.Equal(t, exitOK, code, stderr)
	var imported sdk.StorageImport
	require.NoError(t, json.Unmarshal([]byte(stdout), &imported))
	require.EqualValues(t, 1, imported.Sequence)
	require.EqualValues(t, 2, imported.References)
	require.Positive(t, imported.PackBytes)

	// Importing a tracked repository again brings only the history written
	// since the import (none here) and leaves the references alone
	// (doc/graft/PLAN6.md, LOCK AND HISTORY RULE): it succeeds and is idempotent.
	code, stdout, stderr = cli("import", "--json")
	require.Equal(t, exitOK, code, stderr)
	var again sdk.StorageImport
	require.NoError(t, json.Unmarshal([]byte(stdout), &again))
	require.EqualValues(t, 1, again.Sequence, "the current head")
	require.Zero(t, again.References, "no reference is imported again")
	require.Zero(t, again.ChangelogEntries)
	require.Zero(t, again.Reviews)

	// The text output of a history import says what it did: it is not an
	// import changeset without references.
	code, stdout, stderr = cli("import")
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "already imported at changeset 1: imported 0 changelog entries and 0 reviews written since\n", stdout)

	code, stdout, stderr = cli("state", "--json")
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, 1, strings.Count(stdout, "\n"), "one object, one line")
	var state sdk.StorageState
	require.NoError(t, json.Unmarshal([]byte(stdout), &state))
	require.Equal(t, "shadow", state.Mode)
	require.True(t, state.Tracked)
	require.EqualValues(t, 1, state.HeadSequence)

	code, stdout, stderr = cli("verify", "--connectivity")
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "head sequence 1\nconnectivity ok\nconsistent\n", stdout)

	// A local change the database never saw is reported, and fails the command.
	gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/rogue", main.String())

	code, stdout, stderr = cli("verify")
	require.Equal(t, exitError, code)
	require.Equal(t, "head sequence 1\ndivergence\trefs/heads/rogue\tlocal="+main.String()+"\tdatabase=absent\n", stdout)
	require.Contains(t, stderr, "1 reference(s) diverge")

	code, stdout, _ = cli("verify", "--json")
	require.Equal(t, exitError, code)
	var verify sdk.StorageVerify
	require.NoError(t, json.Unmarshal([]byte(stdout), &verify))
	require.Equal(t, []sdk.StorageDivergence{{Reference: "refs/heads/rogue", LocalTarget: main.String()}}, verify.Divergences)
}

// TestCLIStorageMaintenanceAgainstServer drives graft storage checkpoint and gc
// against a real server whose storage core runs in authoritative mode. It runs
// serially because the CLI tests share the package-level backend factory.
func TestCLIStorageMaintenanceAgainstServer(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative})
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll, testserver.WithGraftStorage(core))

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	server := []string{"-address", addr, "-storage", repo.GetStorageName()}
	target := append(append([]string{}, server...), "-repo", repo.GetRelativePath())
	cli := func(args ...string) (int, string, string) {
		return runCLI(t, append(append([]string{"storage"}, args...), target...)...)
	}

	code, _, stderr := cli("import")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := cli("checkpoint")
	require.Equal(t, exitOK, code, stderr)
	require.Regexp(t, `^checkpoint created at changeset 1: 2 references, pack [1-9][0-9]* bytes\n$`, stdout)

	code, stdout, stderr = cli("checkpoint", "--json")
	require.Equal(t, exitOK, code, stderr)
	var checkpoint sdk.StorageCheckpoint
	require.NoError(t, json.Unmarshal([]byte(stdout), &checkpoint))
	require.False(t, checkpoint.Created, "a checkpoint at the head exists")
	require.EqualValues(t, 1, checkpoint.Sequence)

	code, stdout, stderr = cli("gc", "--dry-run", "--json")
	require.Equal(t, exitOK, code, stderr)
	var gc sdk.StorageGC
	require.NoError(t, json.Unmarshal([]byte(stdout), &gc))
	require.True(t, gc.DryRun)

	code, stdout, stderr = cli("gc")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "deleted 0 objects (0 bytes)")

	code, stdout, stderr = runCLI(t, append([]string{"storage", "gc", "--removed"}, server...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "deleted 0 objects (0 bytes) of 0 removed repositories of storage "+repo.GetStorageName()+"\n", stdout)

	// Objects of a repository ID the database has not allocated (a database
	// restored from an older backup) are reported, never touched.
	record, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	require.NoError(t, err)
	ahead := corestore.NewPackKey(record.ID+2, sha256.Sum256([]byte("ahead")))
	require.NoError(t, core.Blobs.Put(ctx, ahead, strings.NewReader("ahead")))
	code, stdout, stderr = runCLI(t, append([]string{"storage", "gc", "--removed"}, server...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "deleted 0 objects (0 bytes) of 0 removed repositories of storage "+repo.GetStorageName()+"\n"+
		"warning: 1 repository IDs above the database's allocator have objects in the bucket (a database restored from an older backup?); they are left alone\n", stdout)
	exists, err := core.Blobs.Exists(ctx, ahead)
	require.NoError(t, err)
	require.True(t, exists)
}

// TestCLIStorageDeleteConflictingRef drives the repair of a repository whose
// database holds two references that git cannot hold together: verify names them
// and the way out, and the deletion the operator chooses brings the repository
// back. It runs serially because the CLI tests share the package-level backend
// factory.
func TestCLIStorageDeleteConflictingRef(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative})
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll, testserver.WithGraftStorage(core))

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	main := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	target := []string{"-address", addr, "-storage", repo.GetStorageName(), "-repo", repo.GetRelativePath()}
	cli := func(args ...string) (int, string, string) {
		return runCLI(t, append(append([]string{"storage"}, args...), target...)...)
	}

	code, _, stderr := cli("import")
	require.Equal(t, exitOK, code, stderr)

	record, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	require.NoError(t, err)
	coretest.CommitRefWithoutNameCheck(t, ctx, core.Meta, record.ID, "refs/heads/a", corestore.Target(main))
	coretest.CommitRefWithoutNameCheck(t, ctx, core.Meta, record.ID, "refs/heads/a/b", corestore.Target(main))

	code, _, stderr = cli("verify")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "refs/heads/a/b")
	require.Contains(t, stderr, "delete-conflicting-ref")

	code, _, stderr = cli("delete-conflicting-ref", "refs/heads/main", main.String())
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "not part of a pair")

	code, stdout, stderr := cli("delete-conflicting-ref", "--json", "refs/heads/a", main.String())
	require.Equal(t, exitOK, code, stderr)
	var deleted map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &deleted))
	require.Equal(t, "refs/heads/a", deleted["reference"])
	require.Equal(t, main.String(), deleted["deleted_target"])
	require.EqualValues(t, 4, deleted["sequence"])

	code, stdout, stderr = cli("verify")
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "head sequence 4\nconsistent\n", stdout)
	requireRefs(t, cfg, repoPath, map[string]string{
		"refs/heads/a/b":  main.String(),
		"refs/heads/main": main.String(),
	})
}

// TestCLIStorageCoreOff checks that a server without a storage core reports
// mode off and refuses an import.
func TestCLIStorageCoreOff(t *testing.T) {
	env := startServer(t)
	target := []string{"-address", env.addr, "-storage", env.storage, "-repo", env.relPath}

	code, stdout, stderr := runCLI(t, append([]string{"storage", "state"}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "mode\toff\n")

	code, _, stderr = runCLI(t, append([]string{"storage", "import"}, target...)...)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "storage core is off")
}

// TestCLIStorageUsageErrorsDoNotContactServer checks the arguments of the
// storage commands before any server call. The workspace does not exist, so a
// server call would fail with a different error.
func TestCLIStorageUsageErrorsDoNotContactServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")

	cases := map[string][]string{
		"no subcommand":             {"storage", "-C", dir},
		"unknown subcommand":        {"storage", "repair", "-C", dir},
		"import with arg":           {"storage", "import", "-C", dir, "extra"},
		"state with arg":            {"storage", "state", "-C", dir, "extra"},
		"verify with arg":           {"storage", "verify", "-C", dir, "extra"},
		"checkpoint with arg":       {"storage", "checkpoint", "-C", dir, "extra"},
		"gc with arg":               {"storage", "gc", "-C", dir, "extra"},
		"delete conflicting no arg": {"storage", "delete-conflicting-ref", "-C", dir},
		"delete conflicting 1 arg":  {"storage", "delete-conflicting-ref", "-C", dir, "refs/heads/a"},
		"delete conflicting dry":    {"storage", "delete-conflicting-ref", "--dry-run", "-C", dir, "refs/heads/a", "x"},
		"gc removed and repo":       {"storage", "gc", "--removed", "-C", dir, "-repo", "a.git"},
		"gc removed without server": {"storage", "gc", "--removed", "-C", dir},
		"import dry run":            {"storage", "import", "--dry-run", "-C", dir},
		"checkpoint dry run":        {"storage", "checkpoint", "--dry-run", "-C", dir},
		"state connectivity":        {"storage", "state", "--connectivity", "-C", dir},
		"verify removed":            {"storage", "verify", "--removed", "-C", dir},
		"checkpoint removed":        {"storage", "checkpoint", "--removed", "-C", dir},
	}
	for name, args := range cases {
		code, _, stderr := runCLI(t, args...)
		require.Equal(t, exitUsage, code, "%s: %s", name, stderr)
	}

	// Without a workspace and without -address the target cannot be found, so
	// the command fails before any server call, with an error that is not a usage error.
	code, _, stderr := runCLI(t, "storage", "verify", "-C", dir, "-repo", "a.git", "--connectivity")
	require.NotEqual(t, exitUsage, code, stderr)
	require.NotEqual(t, 0, code, stderr)
}

// TestCLIStorageHelp checks that graft help lists the storage command and that
// its help and its usage errors show the synopsis of the subcommands.
func TestCLIStorageHelp(t *testing.T) {
	code, stdout, _ := runCLI(t, "help")
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "  storage SUBCOMMAND ")

	code, _, stderr := runCLI(t, "storage", "-h")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stderr, storageSynopsis)
	require.Contains(t, stderr, "-connectivity")

	code, _, stderr = runCLI(t, "storage", "-C", filepath.Join(t.TempDir(), "missing"))
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, storageSynopsis)
}

// TestStorageVerifyJSONKeepsNonUTF8Names checks that a reference name that is
// not valid UTF-8 (which the database accepts) is reproduced exactly in the
// JSON output of verify, next to its lossy JSON string.
func TestStorageVerifyJSONKeepsNonUTF8Names(t *testing.T) {
	res := &sdk.StorageVerify{
		HeadSequence: 3,
		Divergences: []sdk.StorageDivergence{
			{Reference: "refs/heads/\xff", LocalTarget: "1111111111111111111111111111111111111111"},
			{Reference: "refs/heads/main", DatabaseTarget: "2222222222222222222222222222222222222222"},
		},
	}

	var out strings.Builder
	require.NoError(t, writeStorageJSON(&out, storageVerifyView(res)))

	var decoded struct {
		HeadSequence uint64 `json:"head_sequence"`
		Divergences  []struct {
			Reference      string `json:"reference"`
			ReferenceBytes []byte `json:"reference_bytes"`
			LocalTarget    string `json:"local_target"`
			DatabaseTarget string `json:"database_target"`
		} `json:"divergences"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &decoded))
	require.EqualValues(t, 3, decoded.HeadSequence)
	require.Len(t, decoded.Divergences, 2)
	require.Equal(t, []byte("refs/heads/\xff"), decoded.Divergences[0].ReferenceBytes)
	require.Equal(t, "refs/heads/�", decoded.Divergences[0].Reference)
	require.Equal(t, "1111111111111111111111111111111111111111", decoded.Divergences[0].LocalTarget)
	require.Nil(t, decoded.Divergences[1].ReferenceBytes)
	require.NotContains(t, out.String(), `"reference_bytes":null`)
	require.Equal(t, "refs/heads/main", decoded.Divergences[1].Reference)

	// The SDK result decodes the same output unchanged otherwise.
	var verify sdk.StorageVerify
	require.NoError(t, json.Unmarshal([]byte(out.String()), &verify))
	require.Equal(t, "2222222222222222222222222222222222222222", verify.Divergences[1].DatabaseTarget)
}

func TestStorageTargetText(t *testing.T) {
	require.Equal(t, "absent", storageTargetText(""))
	require.Equal(t, "ref:refs/heads/main", storageTargetText("ref:refs/heads/main"))
	require.Equal(t, "1111111111111111111111111111111111111111", storageTargetText("1111111111111111111111111111111111111111"))
}
