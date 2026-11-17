package operations

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
)

func TestLandConflictWithoutResolution(t *testing.T) {
	t.Parallel()

	t.Run("the package describes the failed tip after the branch moves", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		conflict := landMergeConflictError(fx.tip, fx.commit, [][]byte{[]byte("a"), []byte("b")}, errors.New("merge conflict"))
		moved := gittest.WriteCommit(t, fx.cfg, fx.repoPath,
			gittest.WithParents(fx.tip), gittest.WithBranch(git.DefaultBranch),
			gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "unrelated", Content: "new tip\n"}))

		err := fx.server(t, config.GraftResolver{}).landConflictWithoutResolution(fx.ctx, fx.request("Land"), conflict)
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, fx.tip.String(), pkg.GetTip())
		require.Equal(t, fx.commit.String(), pkg.GetCommit())
		require.Len(t, pkg.GetFiles(), 2)
		requirePackageOIDsKept(t, fx, pkg)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, moved)
	})

	t.Run("a denied agent gets no package", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		s := fx.server(t, config.GraftResolver{})
		s.graft = config.Graft{Standalone: true, Policy: []config.GraftPolicyRule{
			{Actor: "agent-7", Refs: []string{"refs/heads/other"}, Allow: true},
		}}
		s.graftPolicy = policystore.New(s.graft.Policy)
		ctx := identity.NewContext(fx.ctx, identity.Identity{Name: "agent-7", Source: identity.SourceAgent})
		conflict := landMergeConflictError(fx.tip, fx.commit, nil, errors.New("merge conflict"))

		err := s.landConflictWithoutResolution(ctx, fx.request("Land"), conflict)
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)
		require.Nil(t, landConflictDetail(err))
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("an intermediate rebase conflict keeps its original error", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})
		// The first commit conflicts with the tip, but the second restores the base. The merge
		// of the endpoints is clean, so it has no files for the caller to resolve.
		fx.commit = gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithParents(fx.commit),
			gittest.WithTreeEntries(
				gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base\n"},
				gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"},
				gittest.TreeEntry{Mode: "100644", Path: "c", Content: "c\n"}))
		req := fx.request("Land")
		req.ResolveConflicts = false

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		require.Nil(t, landConflictDetail(err))
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})
}
