package operations

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// TestUserLandResolvesBranchesExactly checks that UserLand reads its branch only by its exact
// name, as TestUserCommitFilesResolvesBranchesExactly checks for UserCommitFiles: when
// refs/heads/x is missing, git's DWIM rules would resolve x through refs/refs/heads/x,
// refs/tags/refs/heads/x, refs/heads/refs/heads/x or refs/remotes/refs/heads/x. A land onto x must
// behave as if x did not exist, unbatched and in standalone mode, where the branch also decides
// whether the land enters a lane: it is not found, or creates x with create_branch, in one attempt.
// It never answers already_landed from the decoy's commit, and never moves the decoy.
func TestUserLandResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	for _, mode := range []struct {
		desc  string
		graft config.Graft
	}{
		{desc: "unbatched", graft: config.Graft{}},
		{desc: "batched", graft: batchingGraft()},
	} {
		t.Run(mode.desc, func(t *testing.T) {
			t.Parallel()

			ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
			ctx, cfg, client := setupLandBatchService(t, ctx, mode.graft)

			for _, decoy := range []git.ReferenceName{"refs/refs/heads/x", "refs/tags/refs/heads/x", "refs/heads/refs/heads/x", "refs/remotes/refs/heads/x"} {
				for _, tc := range []struct {
					desc string
					// onDecoy lands a commit on top of the decoy's commit instead of the decoy's
					// commit itself, which the decoy would answer as already landed.
					onDecoy bool
					create  bool
				}{
					{desc: "the decoy's commit"},
					{desc: "a commit on the decoy's commit", onDecoy: true},
					{desc: "the decoy's commit, with create_branch", create: true},
					{desc: "a commit on the decoy's commit, with create_branch", onDecoy: true, create: true},
				} {
					t.Run(string(decoy)+", "+tc.desc, func(t *testing.T) {
						repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
						decoyCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("decoy"),
							gittest.WithTreeEntries(gittest.TreeEntry{Path: "decoy", Mode: "100644", Content: "decoy\n"}))
						gittest.WriteRef(t, cfg, repoPath, decoy, decoyCommit)

						commit := decoyCommit
						if tc.onDecoy {
							commit = gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(decoyCommit), gittest.WithMessage("change"),
								gittest.WithTreeEntries(gittest.TreeEntry{Path: "change", Mode: "100644", Content: "change\n"}))
						}

						request := landRequest(repo, "x", commit)
						request.CreateBranch = tc.create
						resp, err := client.UserLand(ctx, request)

						if !tc.create {
							testhelper.RequireGrpcCode(t, err, codes.NotFound)
							require.Nil(t, resp)
							_, err = localrepo.NewTestRepo(t, cfg, repo).GetReference(ctx, "refs/heads/x")
							require.ErrorIs(t, err, git.ErrReferenceNotFound)
						} else {
							require.NoError(t, err)
							require.False(t, resp.GetAlreadyLanded())
							require.True(t, resp.GetBranchUpdate().GetBranchCreated())
							require.Equal(t, gittest.DefaultObjectHash.ZeroOID.String(), resp.GetOldTip())
							require.Equal(t, commit.String(), resp.GetLandedCommitId())
							require.EqualValues(t, 1, resp.GetAttempts())
							requireBranchAt(t, cfg, repoPath, "x", commit)
						}

						// The decoy is untouched.
						require.Equal(t, decoyCommit, gittest.ResolveRevision(t, cfg, repoPath, decoy.String()))
					})
				}
			}
		})
	}
}
