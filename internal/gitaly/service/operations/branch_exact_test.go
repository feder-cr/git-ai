package operations

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests of this file check that the operations RPCs read their branches by their exact names,
// as TestUserCommitFilesResolvesBranchesExactly and TestUserLandResolvesBranchesExactly check for
// UserCommitFiles and UserLand. git resolves a revision with its DWIM rules:
//   - a missing refs/heads/x resolves through refs/refs/heads/x, refs/tags/refs/heads/x,
//     refs/heads/refs/heads/x or refs/remotes/refs/heads/x (exactBranchDecoys), so a request for a
//     missing branch x would decide against the commit of such a reference (a tag an agent may
//     create);
//   - a bare branch name x resolves to refs/tags/x before refs/heads/x, so a tag named like an
//     existing branch would answer for the branch (UserUpdateSubmodule took the parent of its
//     commit from the bare name).
//
// A request must behave as if the decoy did not exist.

// exactBranchDecoys are the references that git's DWIM rules resolve refs/heads/x through when
// refs/heads/x does not exist.
var exactBranchDecoys = []git.ReferenceName{
	"refs/refs/heads/x", "refs/tags/refs/heads/x", "refs/heads/refs/heads/x", "refs/remotes/refs/heads/x",
}

// setupExactBranchService starts the operations service for a test of this file.
func setupExactBranchService(t *testing.T) (context.Context, config.Cfg, gitalypb.OperationServiceClient) {
	t.Helper()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	return setupOperationsService(t, ctx)
}

// writeDecoy writes reference at a commit of its own, unrelated to the history of the branches,
// whose tree holds the given entries and a file named decoy, and returns the commit.
func writeDecoy(t *testing.T, cfg config.Cfg, repoPath string, reference git.ReferenceName, entries ...gittest.TreeEntry) git.ObjectID {
	t.Helper()

	entries = append(entries, gittest.TreeEntry{Path: "decoy", Mode: "100644", Content: "decoy\n"})
	commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("decoy"), gittest.WithTreeEntries(entries...))
	gittest.WriteRef(t, cfg, repoPath, reference, commit)
	return commit
}

// exactReferenceValue returns the value of reference, read by its exact name, or an empty string
// when it does not exist.
func exactReferenceValue(t *testing.T, cfg config.Cfg, repoPath string, reference git.ReferenceName) string {
	t.Helper()

	output := gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref", "--format=%(refname) %(objectname)", reference.String())
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if name, value, ok := strings.Cut(line, " "); ok && name == reference.String() {
			return value
		}
	}
	return ""
}

// requireDecoyUntouched checks that the request left the decoy where it was and created no branch x.
func requireDecoyUntouched(t *testing.T, cfg config.Cfg, repoPath string, decoy git.ReferenceName, decoyCommit git.ObjectID) {
	t.Helper()

	require.Equal(t, decoyCommit.String(), exactReferenceValue(t, cfg, repoPath, decoy), "the decoy is untouched")
	require.Empty(t, exactReferenceValue(t, cfg, repoPath, "refs/heads/x"), "no branch x")
}

// parentsOf returns the parents of commit.
func parentsOf(t *testing.T, cfg config.Cfg, repoPath string, commit string) string {
	t.Helper()

	return strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "log", "-1", "--format=%P", commit)))
}

// TestUserFFBranchResolvesBranchesExactly checks that UserFFBranch onto a missing branch fails as
// for a branch that does not exist, whatever decoy git's DWIM rules would take for it.
func TestUserFFBranchResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	for _, decoy := range exactBranchDecoys {
		t.Run(decoy.String(), func(t *testing.T) {
			t.Parallel()

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
			decoyCommit := writeDecoy(t, cfg, repoPath, decoy)
			// A fast-forward of the decoy's commit.
			commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(decoyCommit))

			_, err := client.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{
				Repository: repo,
				User:       gittest.TestUser,
				CommitId:   commit.String(),
				Branch:     []byte("x"),
			})
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
			require.ErrorContains(t, err, "reference not found")
			requireDecoyUntouched(t, cfg, repoPath, decoy, decoyCommit)
		})
	}

	t.Run("a symbolic branch", func(t *testing.T) {
		t.Parallel()

		// A symbolic branch reads as the commit of its target, and the update moves the target.
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
		gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "refs/heads/sym", git.DefaultRef.String())
		commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

		_, err := client.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{
			Repository: repo,
			User:       gittest.TestUser,
			CommitId:   commit.String(),
			Branch:     []byte("sym"),
		})
		require.NoError(t, err)
		require.Equal(t, commit.String(), exactReferenceValue(t, cfg, repoPath, git.DefaultRef))
	})

	t.Run("glob characters in the name", func(t *testing.T) {
		t.Parallel()

		// x* is not a branch name, and never a pattern that matches xa.
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		xa := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("xa"))
		commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(xa))

		_, err := client.UserFFBranch(ctx, &gitalypb.UserFFBranchRequest{
			Repository: repo,
			User:       gittest.TestUser,
			CommitId:   commit.String(),
			Branch:     []byte("x*"),
		})
		testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
		require.ErrorContains(t, err, "reference not found")
		require.Equal(t, xa.String(), exactReferenceValue(t, cfg, repoPath, "refs/heads/xa"))
	})
}

// TestUserMergeBranchResolvesBranchesExactly checks that UserMergeBranch into a missing branch is
// not found, whatever decoy git's DWIM rules would take for it.
func TestUserMergeBranchResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	for _, decoy := range exactBranchDecoys {
		t.Run(decoy.String(), func(t *testing.T) {
			t.Parallel()

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
			decoyCommit := writeDecoy(t, cfg, repoPath, decoy)
			commit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(decoyCommit),
				gittest.WithTreeEntries(gittest.TreeEntry{Path: "merged", Mode: "100644", Content: "merged\n"}))

			stream, err := client.UserMergeBranch(ctx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(&gitalypb.UserMergeBranchRequest{
				Repository: repo,
				User:       gittest.TestUser,
				CommitId:   commit.String(),
				Branch:     []byte("x"),
				Message:    []byte("merge into x"),
			}))
			_, err = stream.Recv()
			testhelper.RequireGrpcCode(t, err, codes.NotFound)
			requireDecoyUntouched(t, cfg, repoPath, decoy, decoyCommit)
		})
	}
}

// TestUserDeleteBranchResolvesBranchesExactly checks that UserDeleteBranch of a missing branch
// finds no branch, whatever decoy git's DWIM rules would take for it.
func TestUserDeleteBranchResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	for _, decoy := range exactBranchDecoys {
		t.Run(decoy.String(), func(t *testing.T) {
			t.Parallel()

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
			decoyCommit := writeDecoy(t, cfg, repoPath, decoy)

			_, err := client.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
				Repository: repo,
				User:       gittest.TestUser,
				BranchName: []byte("x"),
			})
			testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
			require.ErrorContains(t, err, `branch not found: "x"`)
			requireDecoyUntouched(t, cfg, repoPath, decoy, decoyCommit)
		})
	}

	t.Run("a branch whose object is missing", func(t *testing.T) {
		t.Parallel()

		// The branch exists under its exact name, so it can be deleted, whatever it points to.
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
		doomed := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("doomed"))
		gittest.WriteRef(t, cfg, repoPath, "refs/heads/broken", doomed)
		require.NoError(t, os.Remove(filepath.Join(repoPath, "objects", doomed.String()[:2], doomed.String()[2:])))

		_, err := client.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
			Repository: repo,
			User:       gittest.TestUser,
			BranchName: []byte("broken"),
		})
		require.NoError(t, err)
		require.Empty(t, exactReferenceValue(t, cfg, repoPath, "refs/heads/broken"), "the branch is deleted")
	})

	t.Run("glob characters in the name", func(t *testing.T) {
		t.Parallel()

		// x* is not a branch name, and never a pattern that matches xa.
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		xa := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("xa"))

		_, err := client.UserDeleteBranch(ctx, &gitalypb.UserDeleteBranchRequest{
			Repository: repo,
			User:       gittest.TestUser,
			BranchName: []byte("x*"),
		})
		testhelper.RequireGrpcCode(t, err, codes.FailedPrecondition)
		require.ErrorContains(t, err, "branch not found")
		require.Equal(t, xa.String(), exactReferenceValue(t, cfg, repoPath, "refs/heads/xa"))
	})
}

// cherryPickOrRevertSetup is a repository whose branch main is at base, with commit, a commit on
// base that adds the file picked, to cherry-pick, and commit's parent revert, which adds the file
// reverted, so that reverting commit's child is clean.
type cherryPickOrRevertSetup struct {
	repo     *gitalypb.Repository
	repoPath string
	base     git.ObjectID
	commit   *gitalypb.GitCommit
}

func setupCherryPickOrRevert(t *testing.T, ctx context.Context, cfg config.Cfg, entries ...gittest.TreeEntry) cherryPickOrRevertSetup {
	t.Helper()

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(append([]gittest.TreeEntry{{Path: "base", Mode: "100644", Content: "base\n"}}, entries...)...))
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(
		append([]gittest.TreeEntry{
			{Path: "base", Mode: "100644", Content: "base\n"},
			{Path: "picked", Mode: "100644", Content: "picked\n"},
		}, entries...)...))
	commit, err := localrepo.NewTestRepo(t, cfg, repo).ReadCommit(ctx, commitID.Revision())
	require.NoError(t, err)

	return cherryPickOrRevertSetup{repo: repo, repoPath: repoPath, base: base, commit: commit}
}

// TestUserCherryPickResolvesBranchesExactly checks that UserCherryPick reads its target branch by its
// exact name: onto a missing branch x it creates x from the start branch, whatever decoy git's DWIM
// rules would take for x.
func TestUserCherryPickResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	for _, decoy := range exactBranchDecoys {
		t.Run("missing branch, "+decoy.String(), func(t *testing.T) {
			t.Parallel()

			setup := setupCherryPickOrRevert(t, ctx, cfg)
			decoyCommit := writeDecoy(t, cfg, setup.repoPath, decoy)

			resp, err := client.UserCherryPick(ctx, &gitalypb.UserCherryPickRequest{
				Repository:      setup.repo,
				User:            gittest.TestUser,
				Commit:          setup.commit,
				BranchName:      []byte("x"),
				StartBranchName: []byte(git.DefaultBranch),
				Message:         []byte("cherry-pick onto x"),
			})
			require.NoError(t, err)
			require.True(t, resp.GetBranchUpdate().GetBranchCreated())
			created := resp.GetBranchUpdate().GetCommitId()
			require.Equal(t, created, exactReferenceValue(t, cfg, setup.repoPath, "refs/heads/x"))
			require.Equal(t, setup.base.String(), parentsOf(t, cfg, setup.repoPath, created), "x starts from the start branch")
			require.Equal(t, decoyCommit.String(), exactReferenceValue(t, cfg, setup.repoPath, decoy), "the decoy is untouched")
		})
	}
}

// TestUserRevertResolvesBranchesExactly is TestUserCherryPickResolvesBranchesExactly for
// UserRevert.
func TestUserRevertResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	// revertSetup moves main to the commit of the setup, which the request then reverts.
	revertSetup := func(t *testing.T) cherryPickOrRevertSetup {
		setup := setupCherryPickOrRevert(t, ctx, cfg)
		gittest.WriteRef(t, cfg, setup.repoPath, git.DefaultRef, git.ObjectID(setup.commit.GetId()))
		return setup
	}

	for _, decoy := range exactBranchDecoys {
		t.Run("missing branch, "+decoy.String(), func(t *testing.T) {
			t.Parallel()

			setup := revertSetup(t)
			decoyCommit := writeDecoy(t, cfg, setup.repoPath, decoy)

			resp, err := client.UserRevert(ctx, &gitalypb.UserRevertRequest{
				Repository:      setup.repo,
				User:            gittest.TestUser,
				Commit:          setup.commit,
				BranchName:      []byte("x"),
				StartBranchName: []byte(git.DefaultBranch),
				Message:         []byte("revert onto x"),
			})
			require.NoError(t, err)
			require.True(t, resp.GetBranchUpdate().GetBranchCreated())
			created := resp.GetBranchUpdate().GetCommitId()
			require.Equal(t, created, exactReferenceValue(t, cfg, setup.repoPath, "refs/heads/x"))
			require.Equal(t, setup.commit.GetId(), parentsOf(t, cfg, setup.repoPath, created), "x starts from the start branch")
			require.Equal(t, decoyCommit.String(), exactReferenceValue(t, cfg, setup.repoPath, decoy), "the decoy is untouched")
		})
	}
}

// TestUserUpdateSubmoduleResolvesBranchesExactly checks that UserUpdateSubmodule reads its branch by
// its exact name: a missing branch is not found whatever decoy git's DWIM rules would take for it,
// and a tag named like the branch is neither the parent nor the tree of the new commit.
func TestUserUpdateSubmoduleResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	setup := func(t *testing.T) (*gitalypb.Repository, string, git.ObjectID, git.ObjectID, gittest.TreeEntry) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		subCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("sub"))
		submodule := gittest.TreeEntry{OID: subCommit, Mode: "160000", Path: "sub"}
		branchCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(submodule))
		newSubCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("new sub"), gittest.WithParents(subCommit))
		return repo, repoPath, branchCommit, newSubCommit, submodule
	}

	for _, decoy := range exactBranchDecoys {
		t.Run("missing branch, "+decoy.String(), func(t *testing.T) {
			t.Parallel()

			repo, repoPath, _, newSubCommit, submodule := setup(t)
			decoyCommit := writeDecoy(t, cfg, repoPath, decoy, submodule)

			_, err := client.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{
				Repository:    repo,
				User:          gittest.TestUser,
				CommitSha:     newSubCommit.String(),
				Branch:        []byte("x"),
				Submodule:     []byte("sub"),
				CommitMessage: []byte("update sub on x"),
			})
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
			require.ErrorContains(t, err, "Cannot find branch")
			requireDecoyUntouched(t, cfg, repoPath, decoy, decoyCommit)
		})

		t.Run("missing branch with an expected old object ID, "+decoy.String(), func(t *testing.T) {
			t.Parallel()

			// The expected old object ID passes its own check; the branch, read for the tree and the
			// parent, is still missing, whatever decoy git's DWIM rules would take for it.
			repo, repoPath, branchCommit, newSubCommit, submodule := setup(t)
			decoyCommit := writeDecoy(t, cfg, repoPath, decoy, submodule)

			_, err := client.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{
				Repository:     repo,
				User:           gittest.TestUser,
				CommitSha:      newSubCommit.String(),
				Branch:         []byte("x"),
				Submodule:      []byte("sub"),
				CommitMessage:  []byte("update sub on x"),
				ExpectedOldOid: branchCommit.String(),
			})
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
			require.ErrorContains(t, err, legacyErrPrefixInvalidSubmodulePath)
			requireDecoyUntouched(t, cfg, repoPath, decoy, decoyCommit)
		})
	}

	t.Run("branch with a tag of its name", func(t *testing.T) {
		t.Parallel()

		repo, repoPath, branchCommit, newSubCommit, submodule := setup(t)
		writeDecoy(t, cfg, repoPath, git.ReferenceName("refs/tags/"+git.DefaultBranch), submodule)

		resp, err := client.UserUpdateSubmodule(ctx, &gitalypb.UserUpdateSubmoduleRequest{
			Repository:    repo,
			User:          gittest.TestUser,
			CommitSha:     newSubCommit.String(),
			Branch:        []byte(git.DefaultBranch),
			Submodule:     []byte("sub"),
			CommitMessage: []byte("update sub"),
		})
		require.NoError(t, err)
		updated := resp.GetBranchUpdate().GetCommitId()
		require.Equal(t, branchCommit.String(), parentsOf(t, cfg, repoPath, updated), "the branch, not the tag of its name, is the parent")
		require.Equal(t, "sub\n", string(gittest.Exec(t, cfg, "-C", repoPath, "ls-tree", "--name-only", updated)), "the tree is the branch's")
	})
}

// TestUserMergeToRefResolvesBranchesExactly checks that a first_parent_ref refs/heads/x that does not
// exist fails as when no decoy is there, and that it is the merge's first parent when it does.
func TestUserMergeToRefResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	merge := func(repo *gitalypb.Repository, source git.ObjectID, firstParentRef string) (*gitalypb.UserMergeToRefResponse, error) {
		return client.UserMergeToRef(ctx, &gitalypb.UserMergeToRefRequest{
			Repository:     repo,
			User:           gittest.TestUser,
			SourceSha:      source.String(),
			TargetRef:      []byte("refs/merge-requests/1/merge"),
			FirstParentRef: []byte(firstParentRef),
			Message:        []byte("merge"),
		})
	}

	for _, decoy := range exactBranchDecoys {
		t.Run(decoy.String(), func(t *testing.T) {
			t.Parallel()

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			source := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
			decoyCommit := writeDecoy(t, cfg, repoPath, decoy)

			_, err := merge(repo, source, "refs/heads/x")
			testhelper.RequireGrpcError(t, structerr.NewInvalidArgument("Invalid merge source"), err)
			requireDecoyUntouched(t, cfg, repoPath, decoy, decoyCommit)
			require.Empty(t, exactReferenceValue(t, cfg, repoPath, "refs/merge-requests/1/merge"))
		})
	}

	t.Run("the first parent exists", func(t *testing.T) {
		t.Parallel()

		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
		x := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithMessage("x"), gittest.WithBranch("x"))
		source := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithMessage("source"))
		for _, decoy := range exactBranchDecoys {
			writeDecoy(t, cfg, repoPath, decoy)
		}

		response, err := merge(repo, source, "refs/heads/x")
		require.NoError(t, err)
		require.Equal(t, x.String()+" "+source.String(), parentsOf(t, cfg, repoPath, response.GetCommitId()))
	})

	t.Run("a tag does not answer for a branch of the name", func(t *testing.T) {
		t.Parallel()

		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
		x := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithMessage("x"), gittest.WithBranch("x"))
		source := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithMessage("source"))
		writeDecoy(t, cfg, repoPath, "refs/tags/x")

		response, err := client.UserMergeToRef(ctx, &gitalypb.UserMergeToRefRequest{
			Repository: repo,
			User:       gittest.TestUser,
			SourceSha:  source.String(),
			TargetRef:  []byte("refs/merge-requests/1/merge"),
			Branch:     []byte("x"),
			Message:    []byte("merge"),
		})
		require.NoError(t, err)
		require.Equal(t, x.String()+" "+source.String(), parentsOf(t, cfg, repoPath, response.GetCommitId()))
	})
}

// TestUserRebaseToRefResolvesBranchesExactly checks that a first_parent_ref refs/heads/x that does
// not exist fails as when no decoy is there, and that it is what the rebase goes onto when it does.
func TestUserRebaseToRefResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	rebase := func(repo *gitalypb.Repository, source git.ObjectID, firstParentRef string) (*gitalypb.UserRebaseToRefResponse, error) {
		return client.UserRebaseToRef(ctx, &gitalypb.UserRebaseToRefRequest{
			Repository:     repo,
			User:           gittest.TestUser,
			SourceSha:      source.String(),
			TargetRef:      []byte("refs/merge-requests/1/rebase"),
			FirstParentRef: []byte(firstParentRef),
			Timestamp:      timestamppb.Now(),
		})
	}

	for _, decoy := range exactBranchDecoys {
		t.Run(decoy.String(), func(t *testing.T) {
			t.Parallel()

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			source := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
			decoyCommit := writeDecoy(t, cfg, repoPath, decoy)

			_, err := rebase(repo, source, "refs/heads/x")
			testhelper.RequireGrpcError(t, structerr.NewInvalidArgument("invalid FirstParentRef"), err)
			requireDecoyUntouched(t, cfg, repoPath, decoy, decoyCommit)
			require.Empty(t, exactReferenceValue(t, cfg, repoPath, "refs/merge-requests/1/rebase"))
		})
	}

	t.Run("the first parent exists", func(t *testing.T) {
		t.Parallel()

		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
		x := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithMessage("x"), gittest.WithBranch("x"),
			gittest.WithTreeEntries(gittest.TreeEntry{Path: "x", Mode: "100644", Content: "x"}))
		source := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithMessage("source"),
			gittest.WithTreeEntries(gittest.TreeEntry{Path: "source", Mode: "100644", Content: "source"}))
		for _, decoy := range exactBranchDecoys {
			writeDecoy(t, cfg, repoPath, decoy)
		}

		response, err := rebase(repo, source, "refs/heads/x")
		require.NoError(t, err)
		require.Equal(t, x.String(), parentsOf(t, cfg, repoPath, response.GetCommitId()))
	})
}

// TestUserCreateBranchStartPointResolvesBranchesFirst checks that start_point is read by the rule of
// resolveStartBranch, against what each form of it names.
func TestUserCreateBranchStartPointResolvesBranchesFirst(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithMessage("base"))
	branch := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithMessage("the branch"), gittest.WithBranch("x"))
	tag := writeDecoy(t, cfg, repoPath, "refs/tags/x")
	onlyTag := writeDecoy(t, cfg, repoPath, "refs/tags/only-tag")
	other := writeDecoy(t, cfg, repoPath, "refs/x")
	missingDecoy := writeDecoy(t, cfg, repoPath, "refs/tags/refs/heads/missing")
	require.NotEqual(t, tag, branch)
	require.NotEqual(t, other, branch)
	annotated := gittest.WriteTag(t, cfg, repoPath, "annotated", branch.Revision(), gittest.WriteTagConfig{Message: "annotated"})
	require.NotEqual(t, annotated, branch)
	require.NotEmpty(t, missingDecoy)

	for i, tc := range []struct {
		startPoint string
		expected   git.ObjectID
	}{
		{startPoint: "x", expected: branch},
		{startPoint: "x~1", expected: base},
		{startPoint: "heads/x", expected: branch},
		{startPoint: "tags/x", expected: tag},
		{startPoint: "refs/heads/x", expected: branch},
		{startPoint: "refs/tags/x", expected: tag},
		{startPoint: "refs/x", expected: other},
		{startPoint: "only-tag", expected: onlyTag},
		{startPoint: "annotated", expected: branch},
		{startPoint: branch.String(), expected: branch},
		{startPoint: branch.String()[:12], expected: branch},
		{startPoint: strings.ToUpper(tag.String()), expected: tag},
		{startPoint: "HEAD", expected: gittest.ResolveRevision(t, cfg, repoPath, "HEAD")},
		{startPoint: "refs/heads/missing"},
		{startPoint: "missing"},
		{startPoint: "x:file"},
		{startPoint: ":/the branch"},
	} {
		t.Run(tc.startPoint, func(t *testing.T) {
			name := fmt.Sprintf("created-%d", i)
			response, err := client.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
				Repository: repo,
				User:       gittest.TestUser,
				BranchName: []byte(name),
				StartPoint: []byte(tc.startPoint),
			})
			if tc.expected == "" {
				testhelper.RequireGrpcError(t, structerr.NewFailedPrecondition("revspec '%s' not found", tc.startPoint), err)
				require.Empty(t, exactReferenceValue(t, cfg, repoPath, git.NewReferenceNameFromBranchName(name)))
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expected.String(), response.GetBranch().GetTargetCommit().GetId())
			require.Equal(t, tc.expected.String(), exactReferenceValue(t, cfg, repoPath, git.NewReferenceNameFromBranchName(name)))
		})
	}
}

// exactBranchPatch is a patch of the file named file, from "change me" to "changed".
func exactBranchPatch(parent git.ObjectID) []byte {
	return []byte(fmt.Sprintf(`From %s Mon Sep 17 00:00:00 2001
From: Patch User <patchuser@gitlab.org>
Date: Thu, 18 Oct 2018 13:40:35 +0200
Subject: [PATCH] A commit from a patch

---
 file | 2 +-
 1 file changed, 1 insertion(+), 1 deletion(-)

diff --git a/file b/file
index 3742e48..e40a3b9 100644
--- a/file
+++ b/file
@@ -1 +1 @@
-change me
+changed
--
2.19.1
`, parent))
}

// applyExactBranchPatch applies exactBranchPatch onto targetBranch.
func applyExactBranchPatch(t *testing.T, ctx context.Context, client gitalypb.OperationServiceClient, repo *gitalypb.Repository, targetBranch string, parent git.ObjectID) (*gitalypb.UserApplyPatchResponse, error) {
	t.Helper()

	stream, err := client.UserApplyPatch(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
		UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{
			Header: &gitalypb.UserApplyPatchRequest_Header{
				Repository:   repo,
				User:         gittest.TestUser,
				TargetBranch: []byte(targetBranch),
				Timestamp:    &timestamppb.Timestamp{Seconds: 1234512345},
			},
		},
	}))
	require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
		UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Patches{Patches: exactBranchPatch(parent)},
	}))
	return stream.CloseAndRecv()
}

// TestUserApplyPatchResolvesBranchesExactly checks that UserApplyPatch reads its target branch and
// the default branch by their exact names: onto a missing branch x it creates x from the default
// branch, and without a default branch it fails as such, whatever decoy git's DWIM rules would
// take for either.
func TestUserApplyPatchResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)
	file := gittest.TreeEntry{Path: "file", Mode: "100644", Content: "change me\n"}

	for _, decoy := range exactBranchDecoys {
		t.Run("missing branch, "+decoy.String(), func(t *testing.T) {
			t.Parallel()

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			defaultCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(file))
			decoyCommit := writeDecoy(t, cfg, repoPath, decoy, file)

			resp, err := applyExactBranchPatch(t, ctx, client, repo, "x", defaultCommit)
			require.NoError(t, err)
			require.True(t, resp.GetBranchUpdate().GetBranchCreated())
			created := resp.GetBranchUpdate().GetCommitId()
			require.Equal(t, created, exactReferenceValue(t, cfg, repoPath, "refs/heads/x"))
			require.Equal(t, defaultCommit.String(), parentsOf(t, cfg, repoPath, created), "x starts from the default branch")
			require.Equal(t, decoyCommit.String(), exactReferenceValue(t, cfg, repoPath, decoy), "the decoy is untouched")
		})

		decoyRef := git.ReferenceName(strings.Replace(decoy.String(), "/x", "/"+git.DefaultBranch, 1))
		t.Run("missing default branch, "+decoyRef.String(), func(t *testing.T) {
			t.Parallel()

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			// HEAD is refs/heads/main, which does not exist; the decoy is named for it.
			decoyCommit := writeDecoy(t, cfg, repoPath, decoyRef, file)

			resp, err := applyExactBranchPatch(t, ctx, client, repo, "x", decoyCommit)
			if strings.HasPrefix(decoyRef.String(), "refs/heads/") {
				// refs/heads/refs/heads/main is a branch, the repository's only one: the default
				// branch falls back to it, as to the first branch of any repository.
				require.NoError(t, err)
				require.Equal(t, decoyCommit.String(), parentsOf(t, cfg, repoPath, resp.GetBranchUpdate().GetCommitId()))
				return
			}
			require.ErrorContains(t, err, errNoDefaultBranch.Error())
			require.Equal(t, decoyCommit.String(), exactReferenceValue(t, cfg, repoPath, decoyRef), "the decoy is untouched")
			require.Empty(t, exactReferenceValue(t, cfg, repoPath, "refs/heads/x"), "no branch x")
		})

		t.Run("default branch falls back to master, "+decoyRef.String(), func(t *testing.T) {
			t.Parallel()

			repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
			// HEAD is refs/heads/main, which does not exist, and master is the branch of the
			// repository: the default branch is master, whatever decoy is named for main.
			master := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"), gittest.WithTreeEntries(file))
			writeDecoy(t, cfg, repoPath, decoyRef, file)

			resp, err := applyExactBranchPatch(t, ctx, client, repo, "x", master)
			require.NoError(t, err)
			require.True(t, resp.GetBranchUpdate().GetBranchCreated())
			require.Equal(t, master.String(), parentsOf(t, cfg, repoPath, resp.GetBranchUpdate().GetCommitId()), "x starts from master")
		})
	}
}

// TestStartBranchResolvesTheBranchFirst checks the start branch of UserCherryPick, UserRevert and
// UserRebaseConfirmable (resolveStartBranch): a name is read as the branch of that name first, so a
// tag named like an existing start branch does not answer for it; a name without a branch keeps
// git's resolution, so a SHA or a tag still works as a start point; and a full reference name is
// read exactly as given, so a missing one does not resolve through a decoy.
func TestStartBranchResolvesTheBranchFirst(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	cherryPick := func(setup cherryPickOrRevertSetup, startBranch string) (*gitalypb.UserCherryPickResponse, error) {
		return client.UserCherryPick(ctx, &gitalypb.UserCherryPickRequest{
			Repository:      setup.repo,
			User:            gittest.TestUser,
			Commit:          setup.commit,
			BranchName:      []byte("new"),
			StartBranchName: []byte(startBranch),
			Message:         []byte("cherry-pick onto new"),
		})
	}

	t.Run("cherry-pick, start branch with a tag of its name", func(t *testing.T) {
		t.Parallel()

		setup := setupCherryPickOrRevert(t, ctx, cfg)
		writeDecoy(t, cfg, setup.repoPath, git.ReferenceName("refs/tags/"+git.DefaultBranch))

		resp, err := cherryPick(setup, git.DefaultBranch)
		require.NoError(t, err)
		require.Equal(t, setup.base.String(), parentsOf(t, cfg, setup.repoPath, resp.GetBranchUpdate().GetCommitId()),
			"the start branch, not the tag of its name, is the parent")
	})

	t.Run("cherry-pick, a SHA as the start", func(t *testing.T) {
		t.Parallel()

		setup := setupCherryPickOrRevert(t, ctx, cfg)

		resp, err := cherryPick(setup, setup.base.String())
		require.NoError(t, err)
		require.Equal(t, setup.base.String(), parentsOf(t, cfg, setup.repoPath, resp.GetBranchUpdate().GetCommitId()))
	})

	t.Run("cherry-pick, a SHA as the start with a branch named like the SHA", func(t *testing.T) {
		t.Parallel()

		// git takes a full object ID for the object, not for a reference named like it.
		setup := setupCherryPickOrRevert(t, ctx, cfg)
		writeDecoy(t, cfg, setup.repoPath, git.ReferenceName("refs/heads/"+setup.base.String()),
			gittest.TreeEntry{Path: "base", Mode: "100644", Content: "base\n"})

		resp, err := cherryPick(setup, setup.base.String())
		require.NoError(t, err)
		require.Equal(t, setup.base.String(), parentsOf(t, cfg, setup.repoPath, resp.GetBranchUpdate().GetCommitId()),
			"the SHA, not the branch named like it, is the parent")
	})

	// revisionSetup moves main to the commit of the setup, so that main~1 is base, and tags base as
	// the annotated tag v1.
	revisionSetup := func(t *testing.T) cherryPickOrRevertSetup {
		setup := setupCherryPickOrRevert(t, ctx, cfg)
		gittest.WriteRef(t, cfg, setup.repoPath, git.DefaultRef, git.ObjectID(setup.commit.GetId()))
		gittest.WriteTag(t, cfg, setup.repoPath, "v1", setup.base.Revision(), gittest.WriteTagConfig{Message: "v1"})
		return setup
	}
	for _, start := range []string{
		"refs/heads/" + git.DefaultBranch + "~1",
		git.DefaultBranch + "~1",
		"refs/tags/v1",
		"refs/tags/v1^{}",
		"v1^{}",
	} {
		t.Run("cherry-pick, a revision as the start, "+start, func(t *testing.T) {
			t.Parallel()

			setup := revisionSetup(t)
			// A tag named like the branch: main~1 is the branch's parent, not the tag's.
			writeDecoy(t, cfg, setup.repoPath, git.ReferenceName("refs/tags/"+git.DefaultBranch))

			resp, err := cherryPick(setup, start)
			require.NoError(t, err)
			require.Equal(t, setup.base.String(), parentsOf(t, cfg, setup.repoPath, resp.GetBranchUpdate().GetCommitId()))
		})
	}

	t.Run("cherry-pick, a tag without a branch of its name as the start", func(t *testing.T) {
		t.Parallel()

		setup := setupCherryPickOrRevert(t, ctx, cfg)
		gittest.WriteRef(t, cfg, setup.repoPath, "refs/tags/v1", setup.base)

		resp, err := cherryPick(setup, "v1")
		require.NoError(t, err)
		require.Equal(t, setup.base.String(), parentsOf(t, cfg, setup.repoPath, resp.GetBranchUpdate().GetCommitId()))
	})

	for _, decoy := range exactBranchDecoys {
		t.Run("cherry-pick, a missing full reference name, "+decoy.String(), func(t *testing.T) {
			t.Parallel()

			setup := setupCherryPickOrRevert(t, ctx, cfg)
			decoyCommit := writeDecoy(t, cfg, setup.repoPath, decoy)

			_, err := cherryPick(setup, "refs/heads/x")
			testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
			require.ErrorContains(t, err, "resolve start ref")
			require.Empty(t, exactReferenceValue(t, cfg, setup.repoPath, "refs/heads/new"), "no branch new")
			require.Equal(t, decoyCommit.String(), exactReferenceValue(t, cfg, setup.repoPath, decoy), "the decoy is untouched")
		})
	}

	t.Run("revert, start branch with a tag of its name", func(t *testing.T) {
		t.Parallel()

		setup := setupCherryPickOrRevert(t, ctx, cfg)
		gittest.WriteRef(t, cfg, setup.repoPath, git.DefaultRef, git.ObjectID(setup.commit.GetId()))
		// The tag's tree holds the file that the revert removes, so that the revert applies onto it
		// too: only the parent tells which of the two it started from.
		writeDecoy(t, cfg, setup.repoPath, git.ReferenceName("refs/tags/"+git.DefaultBranch),
			gittest.TreeEntry{Path: "base", Mode: "100644", Content: "base\n"},
			gittest.TreeEntry{Path: "picked", Mode: "100644", Content: "picked\n"})

		resp, err := client.UserRevert(ctx, &gitalypb.UserRevertRequest{
			Repository:      setup.repo,
			User:            gittest.TestUser,
			Commit:          setup.commit,
			BranchName:      []byte("new"),
			StartBranchName: []byte(git.DefaultBranch),
			Message:         []byte("revert onto new"),
		})
		require.NoError(t, err)
		require.Equal(t, setup.commit.GetId(), parentsOf(t, cfg, setup.repoPath, resp.GetBranchUpdate().GetCommitId()),
			"the start branch, not the tag of its name, is the parent")
	})

	t.Run("rebase onto a remote branch with a tag of its name", func(t *testing.T) {
		t.Parallel()

		setup := setupRebasableRepositories(t, ctx, cfg, true)
		writeDecoy(t, cfg, setup.remoteRepoPath, git.ReferenceName("refs/tags/"+setup.remoteBranch))

		stream, err := client.UserRebaseConfirmable(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(buildUserRebaseConfirmableHeaderRequest(setup.localRepo, gittest.TestUser, "1",
			setup.localBranch, setup.localCommit, setup.remoteRepo, setup.remoteBranch)))
		first, err := stream.Recv()
		require.NoError(t, err)
		require.NoError(t, stream.Send(buildUserRebaseConfirmableApplyRequest(true)))
		second, err := stream.Recv()
		require.NoError(t, err)
		require.True(t, second.GetRebaseApplied())
		_, err = stream.Recv()
		require.Equal(t, io.EOF, err)

		require.Equal(t, setup.remoteCommit.String(), parentsOf(t, cfg, setup.localRepoPath, first.GetRebaseSha()),
			"the rebase is onto the remote branch, not onto the tag of its name")
	})

	t.Run("rebase onto a remote SHA with a branch named like the SHA", func(t *testing.T) {
		t.Parallel()

		setup := setupRebasableRepositories(t, ctx, cfg, true)
		writeDecoy(t, cfg, setup.remoteRepoPath, git.ReferenceName("refs/heads/"+setup.remoteCommit.String()))

		stream, err := client.UserRebaseConfirmable(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(buildUserRebaseConfirmableHeaderRequest(setup.localRepo, gittest.TestUser, "1",
			setup.localBranch, setup.localCommit, setup.remoteRepo, setup.remoteCommit.String())))
		first, err := stream.Recv()
		require.NoError(t, err)
		require.NoError(t, stream.Send(buildUserRebaseConfirmableApplyRequest(true)))
		second, err := stream.Recv()
		require.NoError(t, err)
		require.True(t, second.GetRebaseApplied())
		_, err = stream.Recv()
		require.Equal(t, io.EOF, err)

		require.Equal(t, setup.remoteCommit.String(), parentsOf(t, cfg, setup.localRepoPath, first.GetRebaseSha()),
			"the rebase is onto the SHA, not onto the branch named like it")
	})

	// The names git resolves before refs/heads/<name>, each with a branch of that name as a decoy, and
	// a non-branch reference that a branch of the exact name beats.
	baseFile := gittest.TreeEntry{Path: "base", Mode: "100644", Content: "base\n"}
	for _, tc := range []struct {
		desc  string
		start func(setup cherryPickOrRevertSetup) string
		decoy func(setup cherryPickOrRevertSetup) git.ReferenceName
		// branch, when set, is a branch at base that the start names.
		branch string
	}{
		{
			desc:   "feature/login with refs/feature/login",
			start:  func(cherryPickOrRevertSetup) string { return "feature/login" },
			decoy:  func(cherryPickOrRevertSetup) git.ReferenceName { return "refs/feature/login" },
			branch: "feature/login",
		},
		{
			desc:   "a branch ORIG_HEAD with a tag ORIG_HEAD",
			start:  func(cherryPickOrRevertSetup) string { return "ORIG_HEAD" },
			decoy:  func(cherryPickOrRevertSetup) git.ReferenceName { return "refs/tags/ORIG_HEAD" },
			branch: "ORIG_HEAD",
		},
		{
			desc:  "HEAD with a branch named HEAD",
			start: func(cherryPickOrRevertSetup) string { return "HEAD" },
			decoy: func(cherryPickOrRevertSetup) git.ReferenceName { return "refs/heads/HEAD" },
		},
		{
			desc:  "heads/main with a branch named heads/main",
			start: func(cherryPickOrRevertSetup) string { return "heads/" + git.DefaultBranch },
			decoy: func(cherryPickOrRevertSetup) git.ReferenceName { return "refs/heads/heads/" + git.DefaultBranch },
		},
		{
			desc:  "an upper-case SHA with a branch named like it",
			start: func(setup cherryPickOrRevertSetup) string { return strings.ToUpper(setup.base.String()) },
			decoy: func(setup cherryPickOrRevertSetup) git.ReferenceName {
				return git.ReferenceName("refs/heads/" + strings.ToUpper(setup.base.String()))
			},
		},
		{
			desc:  "SHA~0 with a branch named like the SHA",
			start: func(setup cherryPickOrRevertSetup) string { return setup.base.String() + "~0" },
			decoy: func(setup cherryPickOrRevertSetup) git.ReferenceName {
				return git.ReferenceName("refs/heads/" + setup.base.String())
			},
		},
		{
			desc:  "main~0 with a tag named main",
			start: func(cherryPickOrRevertSetup) string { return git.DefaultBranch + "~0" },
			decoy: func(cherryPickOrRevertSetup) git.ReferenceName {
				return git.ReferenceName("refs/tags/" + git.DefaultBranch)
			},
		},
		{
			desc:  "main with refs/main",
			start: func(cherryPickOrRevertSetup) string { return git.DefaultBranch },
			decoy: func(cherryPickOrRevertSetup) git.ReferenceName { return git.ReferenceName("refs/" + git.DefaultBranch) },
		},
		{
			// refs/heads/@ is not a valid reference name, so @ has no branch decoy.
			desc:  "@, which is HEAD",
			start: func(cherryPickOrRevertSetup) string { return "@" },
			decoy: func(cherryPickOrRevertSetup) git.ReferenceName { return "" },
		},
	} {
		t.Run("cherry-pick, "+tc.desc, func(t *testing.T) {
			t.Parallel()

			setup := setupCherryPickOrRevert(t, ctx, cfg)
			if tc.branch != "" {
				gittest.WriteRef(t, cfg, setup.repoPath, git.NewReferenceNameFromBranchName(tc.branch), setup.base)
			}
			if decoy := tc.decoy(setup); decoy != "" {
				writeDecoy(t, cfg, setup.repoPath, decoy, baseFile)
			}

			resp, err := cherryPick(setup, tc.start(setup))
			require.NoError(t, err)
			require.Equal(t, setup.base.String(), parentsOf(t, cfg, setup.repoPath, resp.GetBranchUpdate().GetCommitId()))
		})
	}

	t.Run("cherry-pick, main@{1} with a reflog of its own", func(t *testing.T) {
		t.Parallel()

		// main's reflog holds other, then base: main@{1} is other.
		setup := setupCherryPickOrRevert(t, ctx, cfg)
		other := gittest.WriteCommit(t, cfg, setup.repoPath, gittest.WithMessage("other"), gittest.WithParents(setup.base),
			gittest.WithTreeEntries(baseFile, gittest.TreeEntry{Path: "other", Mode: "100644", Content: "other\n"}))
		gittest.Exec(t, cfg, "-C", setup.repoPath, "update-ref", "--create-reflog", git.DefaultRef.String(), other.String())
		gittest.Exec(t, cfg, "-C", setup.repoPath, "update-ref", "--create-reflog", git.DefaultRef.String(), setup.base.String())

		resp, err := cherryPick(setup, git.DefaultBranch+"@{1}")
		require.NoError(t, err)
		require.Equal(t, other.String(), parentsOf(t, cfg, setup.repoPath, resp.GetBranchUpdate().GetCommitId()))
	})

	t.Run("cherry-pick, main@{1} with only a decoy's reflog", func(t *testing.T) {
		t.Parallel()

		// main has no reflog, and refs/refs/heads/main has one, which git would read for
		// refs/heads/main@{1}.
		setup := setupCherryPickOrRevert(t, ctx, cfg)
		decoy := writeDecoy(t, cfg, setup.repoPath, "refs/refs/heads/main", baseFile)
		gittest.Exec(t, cfg, "-C", setup.repoPath, "update-ref", "--create-reflog", "refs/refs/heads/main", decoy.String())
		gittest.Exec(t, cfg, "-C", setup.repoPath, "update-ref", "--create-reflog", "refs/refs/heads/main", setup.base.String())

		_, err := cherryPick(setup, git.DefaultBranch+"@{1}")
		testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
		require.ErrorContains(t, err, "has no reflog")
	})

	t.Run("cherry-pick, a dangling symbolic branch by its full name", func(t *testing.T) {
		t.Parallel()

		// refs/heads/x points to a branch that does not exist: it has no value, and never resolves
		// through the decoy refs/refs/heads/x that git's rules would try after it.
		setup := setupCherryPickOrRevert(t, ctx, cfg)
		gittest.Exec(t, cfg, "-C", setup.repoPath, "symbolic-ref", "refs/heads/x", "refs/heads/nope")
		writeDecoy(t, cfg, setup.repoPath, "refs/refs/heads/x", baseFile)

		_, err := cherryPick(setup, "refs/heads/x")
		testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
		require.ErrorContains(t, err, "resolve start ref")
	})

	t.Run("rebase onto a remote branch with refs/<branch>", func(t *testing.T) {
		t.Parallel()

		setup := setupRebasableRepositories(t, ctx, cfg, true)
		writeDecoy(t, cfg, setup.remoteRepoPath, git.ReferenceName("refs/"+setup.remoteBranch))

		stream, err := client.UserRebaseConfirmable(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(buildUserRebaseConfirmableHeaderRequest(setup.localRepo, gittest.TestUser, "1",
			setup.localBranch, setup.localCommit, setup.remoteRepo, setup.remoteBranch)))
		first, err := stream.Recv()
		require.NoError(t, err)
		require.NoError(t, stream.Send(buildUserRebaseConfirmableApplyRequest(true)))
		_, err = stream.Recv()
		require.NoError(t, err)
		_, err = stream.Recv()
		require.Equal(t, io.EOF, err)

		require.Equal(t, setup.remoteCommit.String(), parentsOf(t, cfg, setup.localRepoPath, first.GetRebaseSha()),
			"the remote branch beats refs/<branch> of the same name")
	})

	t.Run("cherry-pick, main:base, a blob, fails", func(t *testing.T) {
		t.Parallel()

		setup := setupCherryPickOrRevert(t, ctx, cfg)
		_, err := cherryPick(setup, git.DefaultBranch+":base")
		testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
		require.ErrorContains(t, err, "resolve start ref")
	})

	t.Run("rebase onto heads/<branch> with a remote branch named heads/<branch>", func(t *testing.T) {
		t.Parallel()

		setup := setupRebasableRepositories(t, ctx, cfg, true)
		writeDecoy(t, cfg, setup.remoteRepoPath, git.ReferenceName("refs/heads/heads/"+setup.remoteBranch))

		stream, err := client.UserRebaseConfirmable(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(buildUserRebaseConfirmableHeaderRequest(setup.localRepo, gittest.TestUser, "1",
			setup.localBranch, setup.localCommit, setup.remoteRepo, "heads/"+setup.remoteBranch)))
		first, err := stream.Recv()
		require.NoError(t, err)
		require.NoError(t, stream.Send(buildUserRebaseConfirmableApplyRequest(true)))
		_, err = stream.Recv()
		require.NoError(t, err)
		_, err = stream.Recv()
		require.Equal(t, io.EOF, err)

		require.Equal(t, setup.remoteCommit.String(), parentsOf(t, cfg, setup.localRepoPath, first.GetRebaseSha()),
			"heads/<branch> is the branch, as git resolves it, not the branch named heads/<branch>")
	})
}

// TestStartBranchLocalAndRemoteAgree runs the same start points in the repository of the request
// and through a start repository, the same repository read over RPC: a reference that exists but
// does not point to a commit fails the request on both paths, with the same code, instead of being
// skipped for a later reference on one of them.
func TestStartBranchLocalAndRemoteAgree(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupExactBranchService(t)

	for _, tc := range []struct {
		desc string
		// refs writes the references of the case and returns the parent the cherry-pick must have,
		// empty when it must fail.
		refs func(t *testing.T, setup cherryPickOrRevertSetup) string
	}{
		{
			desc: "a branch",
			refs: func(t *testing.T, setup cherryPickOrRevertSetup) string {
				gittest.WriteRef(t, cfg, setup.repoPath, "refs/heads/x", setup.base)
				return setup.base.String()
			},
		},
		{
			// x/y is below the name x, which does not exist: the remote-tracking branch x is taken.
			desc: "a branch below a missing branch, before a remote-tracking branch",
			refs: func(t *testing.T, setup cherryPickOrRevertSetup) string {
				gittest.WriteRef(t, cfg, setup.repoPath, "refs/heads/x/y", git.ObjectID(setup.commit.GetId()))
				gittest.WriteRef(t, cfg, setup.repoPath, "refs/remotes/x", setup.base)
				return setup.base.String()
			},
		},
		{
			// A dangling symbolic branch has no value: the remote-tracking branch x is taken.
			desc: "a dangling symbolic branch, before a remote-tracking branch",
			refs: func(t *testing.T, setup cherryPickOrRevertSetup) string {
				gittest.Exec(t, cfg, "-C", setup.repoPath, "symbolic-ref", "refs/heads/x", "refs/heads/nope")
				gittest.WriteRef(t, cfg, setup.repoPath, "refs/remotes/x", setup.base)
				return setup.base.String()
			},
		},
		{
			desc: "a symbolic branch",
			refs: func(t *testing.T, setup cherryPickOrRevertSetup) string {
				gittest.Exec(t, cfg, "-C", setup.repoPath, "symbolic-ref", "refs/heads/x", git.DefaultRef.String())
				return setup.base.String()
			},
		},
		{
			desc: "a branch and a tag of its name at a tree",
			refs: func(t *testing.T, setup cherryPickOrRevertSetup) string {
				tree := strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", setup.repoPath, "rev-parse", setup.base.String()+"^{tree}")))
				gittest.WriteRef(t, cfg, setup.repoPath, "refs/tags/x", git.ObjectID(tree))
				gittest.WriteRef(t, cfg, setup.repoPath, "refs/heads/x", setup.base)
				return setup.base.String()
			},
		},
		{
			desc: "a tag at a tree, before a remote-tracking branch",
			refs: func(t *testing.T, setup cherryPickOrRevertSetup) string {
				tree := strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", setup.repoPath, "rev-parse", setup.base.String()+"^{tree}")))
				gittest.WriteRef(t, cfg, setup.repoPath, "refs/tags/x", git.ObjectID(tree))
				gittest.WriteRef(t, cfg, setup.repoPath, "refs/remotes/x", setup.base)
				return ""
			},
		},
		{
			desc: "a branch at a missing object",
			refs: func(t *testing.T, setup cherryPickOrRevertSetup) string {
				doomed := gittest.WriteCommit(t, cfg, setup.repoPath, gittest.WithMessage("doomed"))
				gittest.WriteRef(t, cfg, setup.repoPath, "refs/heads/x", doomed)
				require.NoError(t, os.Remove(filepath.Join(setup.repoPath, "objects", doomed.String()[:2], doomed.String()[2:])))
				gittest.WriteRef(t, cfg, setup.repoPath, "refs/remotes/x", setup.base)
				return ""
			},
		},
	} {
		for _, remote := range []bool{false, true} {
			desc := tc.desc + ", local"
			if remote {
				desc = tc.desc + ", through a start repository"
			}
			t.Run(desc, func(t *testing.T) {
				t.Parallel()

				setup := setupCherryPickOrRevert(t, ctx, cfg)
				parent := tc.refs(t, setup)
				request := &gitalypb.UserCherryPickRequest{
					Repository:      setup.repo,
					User:            gittest.TestUser,
					Commit:          setup.commit,
					BranchName:      []byte("new"),
					StartBranchName: []byte("x"),
					Message:         []byte("cherry-pick onto new"),
				}
				if remote {
					request.StartRepository = setup.repo
				}

				resp, err := client.UserCherryPick(ctx, request)
				if parent == "" {
					testhelper.RequireGrpcCode(t, err, codes.InvalidArgument)
					require.ErrorContains(t, err, "resolve start ref")
					return
				}
				require.NoError(t, err)
				require.Equal(t, parent, parentsOf(t, cfg, setup.repoPath, resp.GetBranchUpdate().GetCommitId()))
			})
		}
	}
}

// TestResolveStartBranch checks the rule of resolveStartBranch over a fake repository: git's
// resolution, but an existing branch of the exact name beats a reference with the same name
// (refs/tags/x, refs/x), unless the name starts with a namespace git prefixes. read answers v(ref)
// for every reference that exists, and resolve answers the revision it is given, so the expected
// value is the revision git is asked for: the value read, with the suffix. A name that git reads
// before any reference (an object ID, a revision without a name) is never looked up.
func TestResolveStartBranch(t *testing.T) {
	t.Parallel()

	sha := strings.Repeat("ab", 20)
	for _, tc := range []struct {
		name     string
		refs     []string
		expected string
		// noLookup tells that the name must not be looked up as a reference.
		noLookup bool
		// reflogs are the references that have a reflog of their own.
		reflogs []string
	}{
		{name: "main", refs: []string{"refs/heads/main"}, expected: "v(refs/heads/main)^{commit}"},
		{name: "main", refs: []string{"refs/tags/main", "refs/heads/main"}, expected: "v(refs/heads/main)^{commit}"},
		{name: "main", refs: []string{"refs/main", "refs/heads/main"}, expected: "v(refs/heads/main)^{commit}"},
		{name: "main", refs: []string{"refs/tags/main"}, expected: "v(refs/tags/main)^{commit}"},
		{name: "main", refs: []string{"refs/main"}, expected: "v(refs/main)^{commit}"},
		{name: "main", refs: []string{"refs/heads/main", "refs/remotes/main"}, expected: "v(refs/heads/main)^{commit}"},
		// A branch with a slash beats a reference with the same name too.
		{name: "feature/login", refs: []string{"refs/feature/login", "refs/heads/feature/login"}, expected: "v(refs/heads/feature/login)^{commit}"},
		{name: "a/b", refs: []string{"refs/a/b", "refs/heads/a/b"}, expected: "v(refs/heads/a/b)^{commit}"},
		{name: "a/b", refs: []string{"refs/tags/a/b", "refs/heads/a/b"}, expected: "v(refs/heads/a/b)^{commit}"},
		{name: "a/b", refs: []string{"refs/a/b"}, expected: "v(refs/a/b)^{commit}"},
		// A name that starts with a namespace git prefixes names it, and follows git.
		{name: "heads/main", refs: []string{"refs/heads/main", "refs/heads/heads/main"}, expected: "v(refs/heads/main)^{commit}"},
		{name: "tags/v1", refs: []string{"refs/tags/v1"}, expected: "v(refs/tags/v1)^{commit}"},
		{name: "tags/v1", refs: []string{"refs/tags/v1", "refs/heads/tags/v1"}, expected: "v(refs/tags/v1)^{commit}"},
		{
			name:     "remotes/origin/main",
			refs:     []string{"refs/remotes/origin/main", "refs/heads/remotes/origin/main"},
			expected: "v(refs/remotes/origin/main)^{commit}",
		},
		{name: "origin/x", refs: []string{"refs/remotes/origin/x"}, expected: "v(refs/remotes/origin/x)^{commit}"},
		{name: "origin", refs: []string{"refs/remotes/origin/HEAD"}, expected: "v(refs/remotes/origin/HEAD)^{commit}"},
		// The suffix applies to the value read.
		{name: "main~1", refs: []string{"refs/tags/main", "refs/heads/main"}, expected: "v(refs/heads/main)~1^{commit}"},
		// A path is not a start point; a colon inside braces is not a path.
		{name: "main:file", refs: []string{"refs/heads/main"}, expected: "path"},
		{name: ":path", expected: "path", noLookup: true},
		// A search of the commit messages is refused too, by choice.
		{name: ":/fix the bug", expected: "path", noLookup: true},
		{name: "main^{/fix: x}", refs: []string{"refs/heads/main"}, expected: "v(refs/heads/main)^{/fix: x}^{commit}"},
		{
			name:     "main@{2024-01-01 10:00:00}",
			refs:     []string{"refs/heads/main"},
			reflogs:  []string{"refs/heads/main"},
			expected: "refs/heads/main@{2024-01-01 10:00:00}^{commit}",
		},
		{name: "refs/tags/v1^{}", refs: []string{"refs/tags/v1"}, expected: "v(refs/tags/v1)^{}^{commit}"},
		// A reflog suffix needs the name, which is read again afterwards, and a reflog of the
		// reference's own: without one, git would read the reflog of a decoy.
		{
			name:     "main@{1}",
			refs:     []string{"refs/tags/main", "refs/heads/main"},
			reflogs:  []string{"refs/heads/main"},
			expected: "refs/heads/main@{1}^{commit}",
		},
		{name: "main@{1}", refs: []string{"refs/heads/main", "refs/refs/heads/main"}, reflogs: []string{"refs/refs/heads/main"}, expected: "not found"},
		{name: "refs/heads/x", refs: []string{"refs/refs/heads/x"}, expected: "not found"},
		{name: "abc1234", expected: "abc1234^{commit}"},
		{name: "@", expected: "@^{commit}"},
		// A root reference that exists comes first; one that does not, such as a dangling HEAD, follows
		// the rules of a bare name.
		{name: "HEAD", refs: []string{"HEAD", "refs/heads/HEAD"}, expected: "v(HEAD)^{commit}"},
		{name: "HEAD", refs: []string{"refs/heads/HEAD"}, expected: "v(refs/heads/HEAD)^{commit}"},
		{name: "FETCH_HEAD~1", refs: []string{"FETCH_HEAD", "refs/heads/FETCH_HEAD"}, expected: "v(FETCH_HEAD)~1^{commit}"},
		{name: "ORIG_HEAD", refs: []string{"refs/tags/ORIG_HEAD", "refs/heads/ORIG_HEAD"}, expected: "v(refs/heads/ORIG_HEAD)^{commit}"},
		{name: sha, refs: []string{"refs/heads/" + sha}, expected: sha + "^{commit}", noLookup: true},
		{name: strings.ToUpper(sha), refs: []string{"refs/heads/" + strings.ToUpper(sha)}, expected: strings.ToUpper(sha) + "^{commit}", noLookup: true},
		{name: sha + "~0", refs: []string{"refs/heads/" + sha}, expected: sha + "~0^{commit}", noLookup: true},
		{name: "@{-1}", expected: "@{-1}^{commit}", noLookup: true},
		{name: "~1", expected: "~1^{commit}", noLookup: true},
	} {
		t.Run(tc.name+" with "+strings.Join(tc.refs, ","), func(t *testing.T) {
			refs := map[git.ReferenceName]bool{}
			for _, ref := range tc.refs {
				refs[git.ReferenceName(ref)] = true
			}
			var lookups []git.ReferenceName
			read := func(name git.ReferenceName) (git.ObjectID, error) {
				lookups = append(lookups, name)
				if !refs[name] {
					return "", git.ErrReferenceNotFound
				}
				return git.ObjectID("v(" + name + ")"), nil
			}
			resolve := func(revision git.Revision) (git.ObjectID, error) {
				return git.ObjectID(revision), nil
			}

			reflogExists := func(name git.ReferenceName) (bool, error) {
				for _, reflog := range tc.reflogs {
					if git.ReferenceName(reflog) == name {
						return true, nil
					}
				}
				return false, nil
			}

			resolved, err := resolveStartBranch(tc.name, git.ObjectHashSHA1, read, reflogExists, resolve)
			switch tc.expected {
			case "not found":
				require.ErrorIs(t, err, git.ErrReferenceNotFound)
			case "path":
				require.ErrorIs(t, err, errStartPointPath)
			default:
				require.NoError(t, err)
				require.Equal(t, tc.expected, resolved.String())
			}
			if tc.noLookup {
				require.Empty(t, lookups, "the name is not looked up as a reference")
			}
		})
	}

	t.Run("a reference that changes while its reflog is read", func(t *testing.T) {
		reads := 0
		read := func(name git.ReferenceName) (git.ObjectID, error) {
			if name != "refs/heads/main" {
				return "", git.ErrReferenceNotFound
			}
			reads++
			return git.ObjectID(fmt.Sprintf("v%d", reads)), nil
		}
		resolve := func(revision git.Revision) (git.ObjectID, error) {
			return git.ObjectID(revision), nil
		}

		reflogExists := func(git.ReferenceName) (bool, error) { return true, nil }

		_, err := resolveStartBranch("main@{1}", git.ObjectHashSHA1, read, reflogExists, resolve)
		require.ErrorIs(t, err, errStartReferenceChanged)
		// The caller sees a failed precondition, which a retry may not meet, unlike the invalid
		// argument of a start branch that does not resolve.
		testhelper.RequireGrpcCode(t, startRefError(err), codes.FailedPrecondition)
		testhelper.RequireGrpcCode(t, startRefError(git.ErrReferenceNotFound), codes.InvalidArgument)
	})
}

// TestSplitRevisionSuffix checks the split of a start revision into its name and its suffix.
func TestSplitRevisionSuffix(t *testing.T) {
	t.Parallel()

	for revision, expected := range map[string][2]string{
		"main":                {"main", ""},
		"refs/heads/main":     {"refs/heads/main", ""},
		"main~1":              {"main", "~1"},
		"refs/heads/main^2~3": {"refs/heads/main", "^2~3"},
		"refs/tags/v1^{}":     {"refs/tags/v1", "^{}"},
		"main@{1}":            {"main", "@{1}"},
		"main:path/file":      {"main", ":path/file"},
		"feature/x~1":         {"feature/x", "~1"},
		"":                    {"", ""},
	} {
		base, suffix := splitRevisionSuffix(revision)
		require.Equal(t, expected, [2]string{base, suffix}, revision)
	}
}
