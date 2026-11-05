package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestPolicyHookDecidesForAgentUpdates drives the last layer of the policy on its
// own: the real reference-transaction hook (git runs the gitaly-hooks binary,
// which calls the server's HookService), run by git update-ref with the hooks
// payload that the server builds for a write of an agent, as for an RPC or a
// push. The earlier layers (the request filter, the pre-receive hook,
// CheckAgentPolicy, the agents' receive.hideRefs) do not take part, so the hook
// decides alone:
//   - refs/graft/x is refused although the policy lets the builder update
//     refs/graft/** (startWithAgentPushes), because that namespace is the
//     server's, in standalone mode and outside it;
//   - so is an update through a symbolic reference whose target is under
//     refs/graft/: the hook sees the target;
//   - a branch the policy allows is written;
//   - a branch the policy denies (main for the builder) is refused in standalone
//     mode, and written outside it, where the policy does not apply.
//
// git only reports that the hook aborted the transaction, so the reason of each
// refusal is read from the hook's log entry in the server. TestPolicyHoldsOnAgentPushes
// covers the layers before the hook on a real push.
func TestPolicyHookDecidesForAgentUpdates(t *testing.T) {
	t.Parallel()

	for _, standalone := range []bool{true, false} {
		t.Run(fmt.Sprintf("standalone=%t", standalone), func(t *testing.T) {
			t.Parallel()
			testPolicyHookDecidesForAgentUpdates(t, standalone)
		})
	}
}

func testPolicyHookDecidesForAgentUpdates(t *testing.T, standalone bool) {
	logger := testhelper.NewLogger(t)
	logs := testhelper.AddLoggerHook(logger)
	s := startWithAgentPushes(t, standalone, testserver.WithLogger(logger))
	mainCommit := writeMain(t, s, map[string]string{"README.md": "hook\n"})
	next := gittest.WriteCommit(t, s.cfg, s.repoPath, gittest.WithParents(git.ObjectID(mainCommit)), gittest.WithMessage("next"))

	// The payload of the hook carries the agent of the context, as for a write that
	// the builder makes with its token.
	ctx := identity.NewContext(s.ctx, identity.Identity{Name: "builder", Source: identity.SourceAgent})
	repo := localrepo.NewTestRepo(t, s.cfg, &gitalypb.Repository{StorageName: s.storage, RelativePath: s.relPath})

	update := func(reference git.ReferenceName, newOID, oldOID git.ObjectID) error {
		t.Helper()
		updater, err := updateref.New(ctx, repo)
		require.NoError(t, err)

		require.NoError(t, updater.Start())
		require.NoError(t, updater.Update(reference, newOID, oldOID))
		commitErr := updater.Commit()
		// A refused transaction ends git update-ref with an error, which Close
		// reports again.
		closeErr := updater.Close()
		if commitErr == nil {
			require.NoError(t, closeErr)
		}
		return commitErr
	}
	// hookDenials returns the reasons of the hook's denials logged so far.
	hookDenials := func() []string {
		var reasons []string
		for _, entry := range logs.AllEntries() {
			if entry.Message == "agent reference update denied" {
				reasons = append(reasons, entry.Data["reason"].(string))
			}
		}
		return reasons
	}
	zero := gittest.DefaultObjectHash.ZeroOID

	require.NoError(t, update("refs/heads/feature/hook", next, zero))
	require.Equal(t, next.String(), s.git(t, "rev-parse", "refs/heads/feature/hook"))
	require.Empty(t, hookDenials())

	var want []string
	if standalone {
		require.Error(t, update("refs/heads/main", next, git.ObjectID(mainCommit)))
		require.Equal(t, mainCommit, s.git(t, "rev-parse", "refs/heads/main"))
		want = append(want, `policy denies "builder" to update "refs/heads/main" (rule 3)`)
	} else {
		require.NoError(t, update("refs/heads/main", next, git.ObjectID(mainCommit)))
		require.Equal(t, next.String(), s.git(t, "rev-parse", "refs/heads/main"))
	}
	require.Equal(t, want, hookDenials())

	require.Error(t, update("refs/graft/x", next, zero))
	require.Empty(t, s.git(t, "for-each-ref", "refs/graft/"))

	// A symbolic reference to refs/graft/x, which only the server could have made: the
	// update of the symbolic reference writes its target, and the hook refuses that.
	s.git(t, "symbolic-ref", "refs/heads/alias", "refs/graft/x")
	require.Error(t, update("refs/heads/alias", next, zero))
	require.Empty(t, s.git(t, "for-each-ref", "refs/graft/"))

	denials := hookDenials()
	require.Len(t, denials, len(want)+2)
	for _, denial := range denials[len(want):] {
		require.True(t, strings.HasPrefix(denial, `a reference under refs/graft/ is reserved for the server`), denial)
	}
}
