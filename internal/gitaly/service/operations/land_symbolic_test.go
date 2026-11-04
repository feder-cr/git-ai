package operations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// TestUserLandThroughASymbolicBranch checks that a land through a symbolic branch is a land on the
// branch it writes: what the land records and looks up per branch is its own, so that a retry through
// either name answers already landed, in a lane or out of one.
func TestUserLandThroughASymbolicBranch(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, func(t *testing.T, ctx context.Context) {
		t.Parallel()

		for _, tc := range []struct {
			desc  string
			graft config.Graft
		}{
			{desc: "alone", graft: config.Graft{Standalone: true, Land: config.GraftLand{Batch: config.GraftLandBatch{Disabled: true}}}},
			{desc: "in a lane", graft: batchingGraft()},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				t.Parallel()

				ctx, cfg, client := setupAgentLandService(t, ctx, tc.graft)
				fx := newRetryFixture(t, ctx, cfg)
				gittest.Exec(t, cfg, "-C", fx.repoPath, "symbolic-ref", "refs/heads/alias", git.DefaultRef.String())

				first, err := client.UserLand(ctx, landRequest(fx.repoProto, "alias", fx.change))
				require.NoError(t, err)
				require.True(t, first.GetRewritten())
				landed := git.ObjectID(first.GetLandedCommitId())
				requireBranchAt(t, cfg, fx.repoPath, git.DefaultBranch, landed)

				entry := landEntry(t, fx.repoPath, landed)
				require.Equal(t, git.DefaultRef.String(), entry.Reference)
				require.Equal(t, []changelog.Land{{Original: fx.change.String(), Landed: landed.String()}}, entry.Lands)

				for _, branch := range []string{"alias", git.DefaultBranch} {
					again, err := client.UserLand(ctx, landRequest(fx.repoProto, branch, fx.change))
					require.NoError(t, err)
					requireLandedEarlier(t, again, entry, landed, "")
				}
				requireBranchAt(t, cfg, fx.repoPath, git.DefaultBranch, landed)
			})
		}

		t.Run("a target that is no branch is refused", func(t *testing.T) {
			t.Parallel()

			ctx, cfg, client := setupOperationsService(t, ctx)
			fx := newRetryFixture(t, ctx, cfg)
			gittest.WriteRef(t, cfg, fx.repoPath, "refs/keep/target", fx.tip)
			gittest.Exec(t, cfg, "-C", fx.repoPath, "symbolic-ref", "refs/heads/alias", "refs/keep/target")

			_, err := client.UserLand(ctx, landRequest(fx.repoProto, "alias", fx.change))
			testhelper.RequireGrpcError(t, structerr.NewFailedPrecondition(`branch "alias" is a symbolic reference to something that is not a branch`), err)
			require.Equal(t, fx.tip.String(), exactReferenceValue(t, cfg, fx.repoPath, "refs/keep/target"))
		})
	})
}
