package localrepo

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

func TestRepo_ContainsRef(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cfg, repo, repoPath := setupRepo(t)
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))

	testcases := []struct {
		desc      string
		ref       string
		contained bool
	}{
		{
			desc:      "unqualified master branch",
			ref:       "master",
			contained: true,
		},
		{
			desc:      "fully qualified master branch",
			ref:       "refs/heads/master",
			contained: true,
		},
		{
			desc:      "nonexistent branch",
			ref:       "refs/heads/nonexistent",
			contained: false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			contained, err := repo.HasRevision(ctx, git.Revision(tc.ref))
			require.NoError(t, err)
			require.Equal(t, tc.contained, contained)
		})
	}
}

func TestRepo_ResolveRevision(t *testing.T) {
	t.Parallel()

	testcases := []struct {
		desc        string
		setup       func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID
		revision    string
		expectedErr error
	}{
		{
			desc: "branch",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID {
				return gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
			},
			revision: "main",
		},
		{
			desc: "tag",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID {
				return gittest.WriteCommit(t, cfg, repoPath, gittest.WithReference("refs/tags/1.0.0"))
			},
			revision: "1.0.0",
		},
		{
			desc: "branch parent",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID {
				parent := gittest.WriteCommit(t, cfg, repoPath)

				gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithParents(parent),
					gittest.WithBranch("main"),
				)

				return parent
			},
			revision: "main^",
		},
		{
			desc: "peel branch to tree",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.ObjectID {
				treeID := gittest.WriteTree(t, cfg, repoPath,
					[]gittest.TreeEntry{
						{Path: "foo.bar", Mode: "100644", Content: "Hello world"},
					})

				gittest.WriteCommit(t, cfg, repoPath,
					gittest.WithTree(treeID),
					gittest.WithBranch("main"))

				return treeID
			},
			revision: "main^{tree}",
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg, repo, repoPath := setupRepo(t)
			expectedID := tc.setup(t, cfg, repoPath)

			ref, err := repo.ResolveRevision(ctx, git.Revision(tc.revision))
			require.Equal(t, tc.expectedErr, err)
			if err == nil {
				require.Equal(t, expectedID, ref)
			}
		})
	}
}

func TestRepo_GetReference(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cfg, repo, repoPath := setupRepo(t)
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))

	testcases := []struct {
		desc        string
		ref         string
		expected    git.Reference
		expectedErr error
	}{
		{
			desc:     "fully qualified master branch",
			ref:      "refs/heads/master",
			expected: git.NewReference("refs/heads/master", commitID),
		},
		{
			desc:        "unqualified master branch fails",
			ref:         "master",
			expectedErr: git.ErrReferenceNotFound,
		},
		{
			desc:        "nonexistent branch",
			ref:         "refs/heads/nonexistent",
			expectedErr: git.ErrReferenceNotFound,
		},
		{
			desc:        "prefix returns an error",
			ref:         "refs/heads",
			expectedErr: fmt.Errorf("%w: conflicts with %q", git.ErrReferenceAmbiguous, "refs/heads/master"),
		},
		{
			desc:        "nonexistent branch",
			ref:         "nonexistent",
			expectedErr: git.ErrReferenceNotFound,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ref, err := repo.GetReference(ctx, git.ReferenceName(tc.ref))
			require.Equal(t, tc.expectedErr, err)
			require.Equal(t, tc.expected, ref)
		})
	}
}

// TestRepo_ResolveReferenceExactly checks that a reference resolves only by its exact name, to its
// own value: a missing refs/heads/x never resolves through a reference that git's DWIM rules try
// after it, nor through a reference below its name; a reference at an annotated tag resolves to the
// tag, as rev-parse of the reference does; and a symbolic branch to the commit of its target.
func TestRepo_ResolveReferenceExactly(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, repo, repoPath := setupRepo(t, withDisabledHooks())

	commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	other := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("other"))
	for _, ref := range []git.ReferenceName{
		"refs/refs/heads/x", "refs/tags/refs/heads/x", "refs/heads/refs/heads/x", "refs/remotes/refs/heads/x", "refs/heads/x/y",
	} {
		gittest.WriteRef(t, cfg, repoPath, ref, other)
	}
	tag := gittest.WriteTag(t, cfg, repoPath, "annotated", commit.Revision(), gittest.WriteTagConfig{Message: "annotated"})
	// git writes only commits to branches; another reference may hold the tag itself.
	gittest.WriteRef(t, cfg, repoPath, "refs/keep/at-tag", tag)
	gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/symbolic", "refs/heads/main")
	// A branch whose object is missing still has its value.
	broken := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("broken"))
	gittest.WriteRef(t, cfg, repoPath, "refs/heads/broken", broken)
	require.NoError(t, os.Remove(filepath.Join(repoPath, "objects", broken.String()[:2], broken.String()[2:])))

	for _, tc := range []struct {
		reference   git.ReferenceName
		expected    git.ObjectID
		expectedErr error
	}{
		{reference: "refs/heads/main", expected: commit},
		{reference: "refs/heads/x", expectedErr: git.ErrReferenceNotFound},
		// Not reference names: never a pattern, never a revision.
		{reference: "refs/heads/x*", expectedErr: git.ErrReferenceNotFound},
		{reference: "refs/heads/main~1", expectedErr: git.ErrReferenceNotFound},
		{reference: "refs/keep/at-tag", expected: tag},
		{reference: "refs/heads/broken", expected: broken},
		{reference: "refs/heads/symbolic", expected: commit},
	} {
		t.Run(tc.reference.String(), func(t *testing.T) {
			oid, err := repo.ResolveReferenceExactly(ctx, tc.reference)
			require.Equal(t, tc.expectedErr, err)
			require.Equal(t, tc.expected, oid)
		})
	}
}

// TestRepo_WrittenReference checks which reference an update of a name writes, by asking git: the
// answer is compared with what `git update-ref` then moves, so that the two cannot disagree.
func TestRepo_WrittenReference(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, repo, repoPath := setupRepo(t, withDisabledHooks())

	commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	next := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(commit))
	gittest.WriteRef(t, cfg, repoPath, "refs/heads/target", commit)
	gittest.WriteRef(t, cfg, repoPath, "refs/heads/refs/heads/missing", commit)
	gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/one", "refs/heads/target")
	gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/chain", "refs/heads/one")
	gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/dangling", "refs/graft/absent")

	for _, tc := range []struct {
		reference git.ReferenceName
		expected  git.ReferenceName
	}{
		{reference: "refs/heads/main", expected: "refs/heads/main"},
		{reference: "refs/heads/one", expected: "refs/heads/target"},
		{reference: "refs/heads/chain", expected: "refs/heads/target"},
		{reference: "refs/heads/dangling", expected: "refs/graft/absent"},
		// A missing name is that name, not one of git's DWIM rules.
		{reference: "refs/heads/missing", expected: "refs/heads/missing"},
		// Not a name git can write: it is returned for the update to refuse.
		{reference: "refs/heads/not..valid", expected: "refs/heads/not..valid"},
	} {
		t.Run(tc.reference.String(), func(t *testing.T) {
			written, err := repo.WrittenReference(ctx, tc.reference)
			require.NoError(t, err)
			require.Equal(t, tc.expected, written)
		})
	}

	t.Run("packed references", func(t *testing.T) {
		gittest.Exec(t, cfg, "-C", repoPath, "pack-refs", "--all")

		for reference, expected := range map[git.ReferenceName]git.ReferenceName{
			"refs/heads/main":  "refs/heads/main",
			"refs/heads/one":   "refs/heads/target",
			"refs/heads/chain": "refs/heads/target",
			// A name below a branch, and a directory of references, are not symbolic either.
			"refs/heads/main/below": "refs/heads/main/below",
			"refs/heads":            "refs/heads",
		} {
			written, err := repo.WrittenReference(ctx, reference)
			require.NoError(t, err)
			require.Equal(t, expected, written, reference)
		}
	})

	t.Run("what git update-ref moves", func(t *testing.T) {
		written, err := repo.WrittenReference(ctx, "refs/heads/chain")
		require.NoError(t, err)

		gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/chain", next.String(), commit.String())
		require.Equal(t, next.String(), text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "--verify", written.String())))
		gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/chain", commit.String(), next.String())
	})

	t.Run("a cycle", func(t *testing.T) {
		gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/cycle-a", "refs/heads/cycle-b")
		gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/cycle-b", "refs/heads/cycle-a")

		_, err := repo.WrittenReference(ctx, "refs/heads/cycle-a")
		testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
	})

	t.Run("a name too long for the file system", func(t *testing.T) {
		long := git.ReferenceName("refs/heads/" + strings.Repeat("x", 300))
		written, err := repo.WrittenReference(ctx, long)
		require.NoError(t, err)
		require.Equal(t, long, written)
	})

	t.Run("a chain of symbolic references up to what git follows", func(t *testing.T) {
		gittest.WriteRef(t, cfg, repoPath, "refs/heads/end", commit)
		previous := "refs/heads/end"
		for hops := 1; hops <= 7; hops++ {
			name := fmt.Sprintf("refs/heads/hop-%d", hops)
			gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", name, previous)
			previous = name

			written, err := repo.WrittenReference(ctx, git.ReferenceName(name))
			if hops <= maxSymbolicHops {
				require.NoError(t, err, "%d hops", hops)
				require.Equal(t, git.ReferenceName("refs/heads/end"), written, "%d hops", hops)
			} else {
				testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
				require.Empty(t, written, "%d hops", hops)
			}
		}
	})
}

// maxSymbolicHops is the longest chain of symbolic references that git-symbolic-ref(1) reads to its
// end. It is git's limit, read by the test of WrittenReference; a new git that moves it fails there.
const maxSymbolicHops = 4

// TestRepo_WrittenReference_looseFiles checks what only the files backend reads by itself.
func TestRepo_WrittenReference_looseFiles(t *testing.T) {
	t.Parallel()
	testhelper.SkipWithReftable(t, "the references are not files")

	ctx := testhelper.Context(t)
	cfg, repo, repoPath := setupRepo(t, withDisabledHooks())
	commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	t.Run("a symbolic link", func(t *testing.T) {
		require.NoError(t, os.Symlink("refs/heads/main", filepath.Join(repoPath, "refs/heads/link")))

		written, err := repo.WrittenReference(ctx, "refs/heads/link")
		require.NoError(t, err)
		require.Equal(t, git.ReferenceName("refs/heads/main"), written)
	})

	t.Run("a failed read does not tell where the repository is", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root reads every file")
		}
		gittest.WriteRef(t, cfg, repoPath, "refs/heads/unreadable", commit)
		path := filepath.Join(repoPath, "refs/heads/unreadable")
		require.NoError(t, os.Chmod(path, 0))
		t.Cleanup(func() { require.NoError(t, os.Chmod(path, 0o644)) })

		_, err := repo.WrittenReference(ctx, "refs/heads/unreadable")
		require.ErrorIs(t, err, os.ErrPermission)
		require.NotContains(t, err.Error(), repoPath)
	})
}

// TestRepo_GetDefaultBranchReadsCandidatesExactly checks that the candidates of the default branch
// (HEAD's branch, main, master) are read by their exact names: a missing refs/heads/main is not the
// default branch because git's DWIM rules would resolve it through a decoy, and the default branch
// is then master.
func TestRepo_GetDefaultBranchReadsCandidatesExactly(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	for _, decoy := range []git.ReferenceName{
		"refs/refs/heads/main", "refs/tags/refs/heads/main", "refs/heads/refs/heads/main", "refs/remotes/refs/heads/main",
	} {
		t.Run(decoy.String(), func(t *testing.T) {
			t.Parallel()

			cfg, repo, repoPath := setupRepo(t, withDisabledHooks())
			gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/main")
			gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))
			gittest.WriteRef(t, cfg, repoPath, decoy, gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("decoy")))

			defaultBranch, err := repo.GetDefaultBranch(ctx)
			require.NoError(t, err)
			require.Equal(t, git.LegacyDefaultRef, defaultBranch)
		})
	}

	t.Run("HEAD's branch at a missing object", func(t *testing.T) {
		t.Parallel()

		// The branch exists, so it is the default branch, whatever it points to.
		cfg, repo, repoPath := setupRepo(t, withDisabledHooks())
		gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/main")
		broken := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
		require.NoError(t, os.Remove(filepath.Join(repoPath, "objects", broken.String()[:2], broken.String()[2:])))

		defaultBranch, err := repo.GetDefaultBranch(ctx)
		require.NoError(t, err)
		require.Equal(t, git.DefaultRef, defaultBranch)
	})

	t.Run("HEAD's branch a dangling symbolic reference", func(t *testing.T) {
		t.Parallel()

		// refs/heads/main points to a branch that does not exist: it has no value, so the
		// default branch falls back to master, whatever decoy is named for main.
		cfg, repo, repoPath := setupRepo(t, withDisabledHooks())
		gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/main")
		gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/main", "refs/heads/nope")
		master := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))
		gittest.WriteRef(t, cfg, repoPath, "refs/refs/heads/main", master)

		defaultBranch, err := repo.GetDefaultBranch(ctx)
		require.NoError(t, err)
		require.Equal(t, git.LegacyDefaultRef, defaultBranch)
	})
}

func TestRepo_GetReferenceWithAmbiguousRefs(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cfg, repo, repoPath := setupRepo(t, withDisabledHooks())

	prevOID := gittest.WriteCommit(t, cfg, repoPath)
	currentOID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(prevOID), gittest.WithBranch("master"))

	for _, ref := range []git.ReferenceName{
		"refs/heads/something/master",
		"refs/heads/master2",
		"refs/heads/masterx",
		"refs/heads/refs/heads/master",
		"refs/heads/heads/master",
		"refs/master",
		"refs/tags/master",
	} {
		require.NoError(t, repo.UpdateRef(ctx, ref, prevOID, gittest.DefaultObjectHash.ZeroOID))
	}

	// core.ignorecase is default-enabled on macOS, causing 'MASTER' to match 'master'
	if runtime.GOOS != "darwin" {
		require.NoError(t, repo.UpdateRef(ctx, "refs/heads/MASTER", prevOID, gittest.DefaultObjectHash.ZeroOID))
	}

	ref, err := repo.GetReference(ctx, "refs/heads/master")
	require.NoError(t, err)
	require.Equal(t, git.Reference{
		Name:   "refs/heads/master",
		Target: currentOID.String(),
	}, ref)
}

func TestRepo_GetReferences(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cfg, repo, repoPath := setupRepo(t)

	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("feature"))
	gittest.WriteTag(t, cfg, repoPath, "v1.0.0", "refs/heads/main")

	mainReference, err := repo.GetReference(ctx, "refs/heads/main")
	require.NoError(t, err)
	featureReference, err := repo.GetReference(ctx, "refs/heads/feature")
	require.NoError(t, err)
	tagReference, err := repo.GetReference(ctx, "refs/tags/v1.0.0")
	require.NoError(t, err)

	testcases := []struct {
		desc         string
		patterns     []string
		expectedRefs []git.Reference
	}{
		{
			desc:     "main branch",
			patterns: []string{"refs/heads/main"},
			expectedRefs: []git.Reference{
				mainReference,
			},
		},
		{
			desc:     "two branches",
			patterns: []string{"refs/heads/main", "refs/heads/feature"},
			expectedRefs: []git.Reference{
				featureReference,
				mainReference,
			},
		},
		{
			desc:     "matching subset is returned",
			patterns: []string{"refs/heads/main", "refs/heads/nonexistent"},
			expectedRefs: []git.Reference{
				mainReference,
			},
		},
		{
			desc: "all references",
			expectedRefs: []git.Reference{
				featureReference,
				mainReference,
				tagReference,
			},
		},
		{
			desc:     "branches",
			patterns: []string{"refs/heads/"},
			expectedRefs: []git.Reference{
				featureReference,
				mainReference,
			},
		},
		{
			desc:     "non-existent branch",
			patterns: []string{"refs/heads/nonexistent"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			refs, err := repo.GetReferences(ctx, tc.patterns...)
			require.NoError(t, err)
			require.Equal(t, tc.expectedRefs, refs)
		})
	}
}

func TestRepo_GetRemoteReferences(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cfg := testcfg.Build(t)

	gitCmdFactory, readSSHCommand := captureGitSSHCommand(t, ctx, cfg)

	storagePath, ok := cfg.StoragePath("default")
	require.True(t, ok)

	const relativePath = "repository-1"
	repoPath := filepath.Join(storagePath, relativePath)

	gittest.Exec(t, cfg, "init", repoPath)
	gittest.Exec(t, cfg, "-C", repoPath, "commit", "--allow-empty", "-m", "commit message")
	commit := text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", git.DefaultRef.String()))

	for _, cmd := range [][]string{
		{"update-ref", git.DefaultRef.String(), commit},
		{"tag", "lightweight-tag", commit},
		{"tag", "-m", "tag message", "annotated-tag", git.DefaultRef.String()},
		{"symbolic-ref", "refs/heads/symbolic", git.DefaultRef.String()},
		{"update-ref", "refs/remote/remote-name/remote-branch", commit},
	} {
		gittest.Exec(t, cfg, append([]string{"-C", repoPath}, cmd...)...)
	}

	annotatedTagOID := text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "annotated-tag"))

	catfileCache := catfile.NewCache(cfg)
	defer catfileCache.Stop()

	repo := New(
		testhelper.NewLogger(t),
		config.NewLocator(cfg),
		gitCmdFactory,
		catfileCache,
		&gitalypb.Repository{StorageName: "default", RelativePath: filepath.Join(relativePath, ".git")},
	)
	for _, tc := range []struct {
		desc                  string
		remote                string
		opts                  []GetRemoteReferencesOption
		expected              []git.Reference
		expectedGitSSHCommand string
	}{
		{
			desc:   "not found",
			remote: repoPath,
			opts: []GetRemoteReferencesOption{
				WithPatterns("this-pattern-does-not-match-anything"),
			},
		},
		{
			desc:   "all",
			remote: repoPath,
			expected: []git.Reference{
				{Name: git.DefaultRef, Target: commit},
				{Name: "refs/heads/symbolic", Target: commit, IsSymbolic: true},
				{Name: "refs/remote/remote-name/remote-branch", Target: commit},
				{Name: "refs/tags/annotated-tag", Target: annotatedTagOID},
				{Name: "refs/tags/lightweight-tag", Target: commit},
			},
		},
		{
			desc:   "branches and tags only",
			remote: repoPath,
			opts: []GetRemoteReferencesOption{
				WithPatterns("refs/heads/*", "refs/tags/*"),
			},
			expected: []git.Reference{
				{Name: git.DefaultRef, Target: commit},
				{Name: "refs/heads/symbolic", Target: commit, IsSymbolic: true},
				{Name: "refs/tags/annotated-tag", Target: annotatedTagOID},
				{Name: "refs/tags/lightweight-tag", Target: commit},
			},
		},
		{
			desc:   "with in-memory remote",
			remote: "inmemory",
			opts: []GetRemoteReferencesOption{
				WithPatterns(git.DefaultRef.String()),
				WithConfig(gitcmd.ConfigPair{
					Key:   "remote.inmemory.url",
					Value: repoPath,
				}),
			},
			expected: []git.Reference{
				{Name: git.DefaultRef, Target: commit},
			},
		},
		{
			desc:   "with custom ssh command",
			remote: repoPath,
			opts: []GetRemoteReferencesOption{
				WithPatterns(git.DefaultRef.String()),
				WithSSHCommand("custom-ssh -with-creds"),
			},
			expected: []git.Reference{
				{Name: git.DefaultRef, Target: commit},
			},
			expectedGitSSHCommand: "custom-ssh -with-creds",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			refs, err := repo.GetRemoteReferences(ctx, tc.remote, tc.opts...)
			require.NoError(t, err)
			require.Equal(t, tc.expected, refs)

			gitSSHCommand, err := readSSHCommand()
			if !os.IsNotExist(err) {
				require.NoError(t, err)
			}

			require.Equal(t, tc.expectedGitSSHCommand, string(gitSSHCommand))
		})
	}
}

func TestRepo_GetBranches(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cfg, repo, repoPath := setupRepo(t)

	mainID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithMessage("main"))
	featureID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("feature"), gittest.WithMessage("feature"))
	thirdID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("third"), gittest.WithMessage("third"))

	gittest.WriteCommit(t, cfg, repoPath, gittest.WithReference("refs/different/namespace"))
	gittest.WriteTag(t, cfg, repoPath, "v1.0.0", mainID.Revision())

	refs, err := repo.GetBranches(ctx)
	require.NoError(t, err)
	require.Equal(t, []git.Reference{
		{Name: "refs/heads/feature", Target: featureID.String()},
		{Name: "refs/heads/main", Target: mainID.String()},
		{Name: "refs/heads/third", Target: thirdID.String()},
	}, refs)
}

func TestRepo_UpdateRef(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cfg, repo, repoPath := setupRepo(t, withDisabledHooks())

	// We move this into a function so that we can re-seed the repository for each test.
	seedRepo := func(t *testing.T, repoPath string) {
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithMessage("main"))
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("other"), gittest.WithMessage("other"))
	}
	seedRepo(t, repoPath)

	mainCommitID := gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/main")
	otherRef, err := repo.GetReference(ctx, "refs/heads/other")
	require.NoError(t, err)

	nonexistentOID := git.ObjectID(strings.Repeat("1", gittest.DefaultObjectHash.EncodedLen()))

	testcases := []struct {
		desc     string
		ref      string
		newValue git.ObjectID
		oldValue git.ObjectID
		verify   func(t *testing.T, repo *Repo, err error)
	}{
		{
			desc:     "successfully update main",
			ref:      "refs/heads/main",
			newValue: git.ObjectID(otherRef.Target),
			oldValue: mainCommitID,
			verify: func(t *testing.T, repo *Repo, err error) {
				require.NoError(t, err)
				ref, err := repo.GetReference(ctx, "refs/heads/main")
				require.NoError(t, err)
				require.Equal(t, ref.Target, otherRef.Target)
			},
		},
		{
			desc:     "update fails with stale oldValue",
			ref:      "refs/heads/main",
			newValue: git.ObjectID(otherRef.Target),
			oldValue: nonexistentOID,
			verify: func(t *testing.T, repo *Repo, err error) {
				require.Error(t, err)
				ref, err := repo.GetReference(ctx, "refs/heads/main")
				require.NoError(t, err)
				require.Equal(t, ref.Target, mainCommitID.String())
			},
		},
		{
			desc:     "update fails with invalid newValue",
			ref:      "refs/heads/main",
			newValue: nonexistentOID,
			oldValue: mainCommitID,
			verify: func(t *testing.T, repo *Repo, err error) {
				require.Error(t, err)
				ref, err := repo.GetReference(ctx, "refs/heads/main")
				require.NoError(t, err)
				require.Equal(t, ref.Target, mainCommitID.String())
			},
		},
		{
			desc:     "successfully update main with empty oldValue",
			ref:      "refs/heads/main",
			newValue: git.ObjectID(otherRef.Target),
			oldValue: "",
			verify: func(t *testing.T, repo *Repo, err error) {
				require.NoError(t, err)
				ref, err := repo.GetReference(ctx, "refs/heads/main")
				require.NoError(t, err)
				require.Equal(t, ref.Target, otherRef.Target)
			},
		},
		{
			desc:     "updating unqualified branch fails",
			ref:      "main",
			newValue: git.ObjectID(otherRef.Target),
			oldValue: mainCommitID,
			verify: func(t *testing.T, repo *Repo, err error) {
				require.Error(t, err)
				ref, err := repo.GetReference(ctx, "refs/heads/main")
				require.NoError(t, err)
				require.Equal(t, ref.Target, mainCommitID.String())
			},
		},
		{
			desc:     "deleting main succeeds",
			ref:      "refs/heads/main",
			newValue: gittest.DefaultObjectHash.ZeroOID,
			oldValue: mainCommitID,
			verify: func(t *testing.T, repo *Repo, err error) {
				require.NoError(t, err)
				_, err = repo.GetReference(ctx, "refs/heads/main")
				require.Error(t, err)
			},
		},
		{
			desc:     "creating new branch succeeds",
			ref:      "refs/heads/new",
			newValue: mainCommitID,
			oldValue: gittest.DefaultObjectHash.ZeroOID,
			verify: func(t *testing.T, repo *Repo, err error) {
				require.NoError(t, err)
				ref, err := repo.GetReference(ctx, "refs/heads/new")
				require.NoError(t, err)
				require.Equal(t, ref.Target, mainCommitID.String())
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			// We need to create the repository every time so that we don't carry over
			// the state.
			repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})
			repo := New(repo.logger, repo.locator, repo.gitCmdFactory, repo.catfileCache, repoProto)
			seedRepo(t, repoPath)

			err := repo.UpdateRef(ctx, git.ReferenceName(tc.ref), tc.newValue, tc.oldValue)
			tc.verify(t, repo, err)
		})
	}
}

func TestGuessHead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc        string
		setup       func(*testing.T, config.Cfg, string) git.Reference
		expected    git.ReferenceName
		expectedErr error
	}{
		{
			desc: "symbolic",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.Reference {
				return git.NewSymbolicReference("HEAD", "refs/heads/something")
			},
			expected: "refs/heads/something",
		},
		{
			desc: "matching default branch",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.Reference {
				commit1 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithMessage("main"))
				commit2 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("feature"), gittest.WithMessage("feature"))

				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", git.DefaultRef.String(), commit1.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", git.LegacyDefaultRef.String(), commit2.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/apple", commit1.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/feature", commit1.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/zucchini", commit1.String())

				return git.NewReference("HEAD", commit1)
			},
			expected: git.DefaultRef,
		},
		{
			desc: "matching default legacy branch",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.Reference {
				commit1 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithMessage("main"))
				commit2 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("feature"), gittest.WithMessage("feature"))

				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", git.DefaultRef.String(), commit2.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", git.LegacyDefaultRef.String(), commit1.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/apple", commit1.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/feature", commit1.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/zucchini", commit1.String())

				return git.NewReference("HEAD", commit1)
			},
			expected: git.LegacyDefaultRef,
		},
		{
			desc: "matching other branch",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.Reference {
				commit1 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithMessage("main"))
				commit2 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("feature"), gittest.WithMessage("feature"))

				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", git.DefaultRef.String(), commit2.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", git.LegacyDefaultRef.String(), commit2.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/apple", commit1.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/feature", commit1.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/zucchini", commit1.String())
				return git.NewReference("HEAD", commit1)
			},
			expected: "refs/heads/apple",
		},
		{
			desc: "missing default branches",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.Reference {
				commit1 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithMessage("main"))

				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "-d", git.DefaultRef.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "-d", git.LegacyDefaultRef.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/apple", commit1.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/feature", commit1.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/zucchini", commit1.String())
				return git.NewReference("HEAD", commit1)
			},
			expected: "refs/heads/apple",
		},
		{
			desc: "no match",
			setup: func(t *testing.T, cfg config.Cfg, repoPath string) git.Reference {
				commit1 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"), gittest.WithMessage("main"))
				commit2 := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("feature"), gittest.WithMessage("feature"))

				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", git.DefaultRef.String(), commit2.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", git.LegacyDefaultRef.String(), commit2.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/apple", commit2.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/feature", commit2.String())
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "refs/heads/zucchini", commit2.String())

				return git.NewReference("HEAD", commit1)
			},
			expectedErr: fmt.Errorf("guess head: %w", git.ErrReferenceNotFound),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg, repo, repoPath := setupRepo(t)

			head := tc.setup(t, cfg, repoPath)

			guess, err := repo.GuessHead(ctx, head)
			if tc.expectedErr == nil {
				require.NoError(t, err)
			} else {
				require.Equal(t, tc.expectedErr, err)
			}
			require.Equal(t, tc.expected, guess)
		})
	}
}
