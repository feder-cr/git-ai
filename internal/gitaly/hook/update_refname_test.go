package hook

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// conflictVoter is a storage voter whose database holds existing, which conflicts with every
// created name. It counts the reads.
type conflictVoter struct {
	recordingVoter
	existing string
	reads    int
}

func (v *conflictVoter) CreatedRefConflict(context.Context, *gitalypb.Repository, string) (string, error) {
	v.reads++
	return v.existing, nil
}

// TestUpdateHook_storageRefNameConflict covers the update hook's refusal of a name that git
// cannot hold next to a reference of the storage core's database: the message on stderr, which
// does not name a reference the writer cannot see, and no read for an update or a deletion.
func TestUpdateHook_storageRefNameConflict(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	zero := gittest.DefaultObjectHash.ZeroOID.String()
	oid := strings.Repeat("b", gittest.DefaultObjectHash.EncodedLen())
	user := &gitcmd.UserDetails{UserID: "1", Username: "user", Protocol: "ssh"}

	envOf := func(t *testing.T, agent bool) []string {
		payloadCtx := ctx
		if agent {
			payloadCtx = identity.NewContext(ctx, identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
		}
		env, err := gitcmd.NewHooksPayload(payloadCtx, cfg, repo, gittest.DefaultObjectHash, nil, user, gitcmd.UpdateHook, nil).Env()
		require.NoError(t, err)
		return []string{env}
	}
	managerOf := func(voter StorageVoter) Manager {
		return NewManager(
			cfg,
			config.NewLocator(cfg),
			testhelper.SharedLogger(t),
			gittest.NewCommandFactory(t, cfg),
			nil,
			transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannel.NewRegistry()),
			gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
			NewPostReceiveRegistry(),
			WithStorageVoter(voter),
		)
	}

	for _, tc := range []struct {
		desc     string
		existing string
		agent    bool
		want     string
	}{
		{"a visible name, to an agent", "refs/heads/a", true, "refs/heads/a"},
		{"a name under refs/graft/, to an agent", "refs/graft/proposals/1", true, hiddenRefName},
		{"a hidden internal name, to an agent", "refs/keep-around/" + oid, true, hiddenRefName},
		{"a name under refs/graft/, to another writer", "refs/graft/proposals/1", false, "refs/graft/proposals/1"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			voter := &conflictVoter{existing: tc.existing}
			var stdout, stderr bytes.Buffer
			err := managerOf(voter).UpdateHook(ctx, repo, "refs/heads/a/b", zero, oid, envOf(t, tc.agent), &stdout, &stderr)

			var conflict updateref.FileDirectoryConflictError
			require.True(t, errors.As(err, &conflict), "error: %v", err)
			require.Equal(t, "refs/heads/a/b", conflict.ConflictingReferenceName)
			require.Equal(t, tc.want, conflict.ExistingReferenceName)
			require.Equal(t, "refusing refs/heads/a/b: git cannot hold it next to "+tc.want+"; push the other references without it\n", stderr.String())
			if tc.want != tc.existing {
				require.NotContains(t, err.Error(), tc.existing)
			}
		})
	}

	t.Run("names are shown as text", func(t *testing.T) {
		voter := &conflictVoter{existing: "refs/heads/x\u202eb"}
		var stdout, stderr bytes.Buffer
		err := managerOf(voter).UpdateHook(ctx, repo, "refs/heads/x\u202eb/\x1bc", zero, oid, envOf(t, false), &stdout, &stderr)
		var conflict updateref.FileDirectoryConflictError
		require.True(t, errors.As(err, &conflict), "error: %v", err)
		require.Equal(t, `refusing refs/heads/x\u202eb/\x1bc: git cannot hold it next to refs/heads/x\u202eb; push the other references without it`+"\n", stderr.String())
	})

	t.Run("updates and deletions read nothing", func(t *testing.T) {
		voter := &conflictVoter{existing: "refs/heads/a"}
		manager := managerOf(voter)
		other := strings.Repeat("c", gittest.DefaultObjectHash.EncodedLen())
		for _, values := range [][2]string{{oid, other}, {oid, zero}} {
			var stdout, stderr bytes.Buffer
			require.NoError(t, manager.UpdateHook(ctx, repo, "refs/heads/a/b", values[0], values[1], envOf(t, true), &stdout, &stderr))
			require.Empty(t, stderr.String())
		}
		require.Zero(t, voter.reads)
	})

	t.Run("no conflict", func(t *testing.T) {
		voter := &conflictVoter{}
		var stdout, stderr bytes.Buffer
		require.NoError(t, managerOf(voter).UpdateHook(ctx, repo, "refs/heads/a/b", zero, oid, envOf(t, true), &stdout, &stderr))
		require.Equal(t, 1, voter.reads)
	})
}
