package operations

import (
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// TestLandPruneProposals_aFailedSweepIsCounted: a sweep of the land that loses to a rewrite of a
// reference fails nothing of the land: it is counted, the rewritten proposal survives, and the next
// sweep cleans up the surplus.
func TestLandPruneProposals_aFailedSweepIsCounted(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, modelAnswer())
	fx := newResolveFixture(t, enabledResolver(argv))

	old := time.Now().Add(-8 * 24 * time.Hour)
	branchDir := proposals.BranchDir(git.DefaultBranch)
	raced := git.ReferenceName(branchDir + fx.commit.String())
	surplus := git.ReferenceName(branchDir + fx.base.String())
	target := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage("raced"), gittest.WithCommitterDate(old))
	other := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage("surplus"), gittest.WithCommitterDate(old))
	rewritten := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage("rewritten"), gittest.WithCommitterDate(time.Now()))
	zero := gittest.DefaultObjectHash.ZeroOID
	require.NoError(t, proposals.Update(fx.ctx, fx.repo, []proposals.Op{
		{Name: raced, NewOID: target, OldOID: zero},
		{Name: surplus, NewOID: other, OldOID: zero},
	}))

	var once sync.Once
	srv := &Server{
		logger: testhelper.SharedLogger(t),
		proposalSweeper: proposals.NewSweeperWithHook(func() {
			once.Do(func() {
				require.NoError(t, proposals.Update(fx.ctx, fx.repo, []proposals.Op{{Name: raced, NewOID: rewritten, OldOID: target}}))
			})
		}),
	}

	before := testutil.ToFloat64(landProposalPruneErrors)
	srv.landPruneProposals(fx.ctx, fx.repo, git.DefaultBranch)
	require.GreaterOrEqual(t, testutil.ToFloat64(landProposalPruneErrors)-before, 1.0, "the failed sweep is counted")
	require.Equal(t, rewritten.String(), proposalRefTarget(t, fx), "the rewritten proposal survives")

	srv.landPruneProposals(fx.ctx, fx.repo, git.DefaultBranch)
	require.Equal(t, rewritten.String(), proposalRefTarget(t, fx))
	_, err := fx.repo.GetReference(fx.ctx, surplus)
	require.ErrorIs(t, err, git.ErrReferenceNotFound, "the later sweep cleaned up")
}
