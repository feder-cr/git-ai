package operations

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
)

// TestUserCommitFilesProcessCount counts the git processes of one UserCommitFiles request, by
// subcommand (PLAN9, WP96): one cat-file for every read, one hash-object per blob, one hash-object
// for every tree, commit-tree and update-ref, and one more for the branches: show-ref, the exact
// read of the branch that resolved, or, when none did (the orphan commit into an empty repository),
// for-each-ref, the check for branches. A process that comes back fails the test.
func TestUserCommitFilesProcessCount(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc        string
		emptyRepo   bool
		newBranch   bool
		paths       []string
		expected    map[string]uint64
		repoCreated bool
	}{
		{
			desc:     "existing branch, top level",
			paths:    []string{"file.txt"},
			expected: map[string]uint64{"cat-file": 1, "hash-object": 2, "commit-tree": 1, "show-ref": 1, "update-ref": 1},
		},
		{
			desc:     "existing branch, nested",
			paths:    []string{"a/b/file.txt"},
			expected: map[string]uint64{"cat-file": 1, "hash-object": 2, "commit-tree": 1, "show-ref": 1, "update-ref": 1},
		},
		{
			desc:      "new branch, top level",
			newBranch: true,
			paths:     []string{"file.txt"},
			expected:  map[string]uint64{"cat-file": 1, "hash-object": 2, "commit-tree": 1, "show-ref": 1, "update-ref": 1},
		},
		{
			desc:      "new branch, nested",
			newBranch: true,
			paths:     []string{"a/b/file.txt"},
			expected:  map[string]uint64{"cat-file": 1, "hash-object": 2, "commit-tree": 1, "show-ref": 1, "update-ref": 1},
		},
		{
			desc:     "three files",
			paths:    []string{"one.txt", "a/two.txt", "a/b/three.txt"},
			expected: map[string]uint64{"cat-file": 1, "hash-object": 4, "commit-tree": 1, "show-ref": 1, "update-ref": 1},
		},
		{
			desc:        "orphan commit into an empty repository",
			emptyRepo:   true,
			paths:       []string{"a/file.txt"},
			expected:    map[string]uint64{"cat-file": 1, "hash-object": 2, "commit-tree": 1, "for-each-ref": 1, "update-ref": 1},
			repoCreated: true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
			cfg := testcfg.Build(t)
			counting := gittest.NewCountingCommandFactory(t, cfg)
			ctx, cfg, client := setupOperationsServiceWithCfg(t, ctx, cfg, testserver.WithGitCommandFactory(counting))

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			if !tc.emptyRepo {
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"),
					gittest.WithTreeEntries(gittest.TreeEntry{Path: "README", Mode: "100644", Content: "readme\n"}))
			}

			branch, start := "master", ""
			if tc.newBranch {
				branch, start = "feature", "master"
			}

			counting.ResetCount()
			stream, err := client.UserCommitFiles(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(headerRequest(repo, gittest.TestUser, branch, []byte("message"), start, "")))
			for i, path := range tc.paths {
				require.NoError(t, stream.Send(createFileHeaderRequest(path)))
				require.NoError(t, stream.Send(actionContentRequest(fmt.Sprintf("content %d\n", i))))
			}
			resp, err := stream.CloseAndRecv()
			require.NoError(t, err)
			require.Empty(t, resp.GetIndexError())
			require.Empty(t, resp.GetPreReceiveError())
			require.Equal(t, tc.repoCreated, resp.GetBranchUpdate().GetRepoCreated())

			// The update reads which reference it writes (localrepo.WrittenReference). The files
			// backend answers from the file of the reference; only git reads a reftable.
			if testhelper.IsReftableEnabled() {
				tc.expected["symbolic-ref"] = 1
			}
			require.Equal(t, tc.expected, counting.Counts())
		})
	}
}
