package hook

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// These tests call the hooks directly, so no git process sees a name over the limit.

func TestUpdateHook_longReferenceNames(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	commitID := gittest.WriteCommit(t, cfg, repoPath)
	zero := gittest.DefaultObjectHash.ZeroOID.String()

	marker := filepath.Join(testhelper.TempDir(t), "custom-hook-ran")
	gittest.WriteCustomHook(t, repoPath, "update", []byte("#!/bin/sh\ntouch '"+marker+"'\n"))

	hookManager := NewManager(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t), gittest.NewCommandFactory(t, cfg), nil, transaction.NewTrackingManager(),
		gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive), NewPostReceiveRegistry())

	env, err := gitcmd.NewHooksPayload(ctx, cfg, repo, gittest.DefaultObjectHash, nil,
		&gitcmd.UserDetails{UserID: "1234", Username: "user", Protocol: "web"},
		gitcmd.UpdateHook, featureflag.FromContext(ctx),
	).Env()
	require.NoError(t, err)

	run := func(ref, newValue string) (string, error) {
		var stdout, stderr bytes.Buffer
		err := hookManager.UpdateHook(ctx, repo, ref, zero, newValue, []string{env}, &stdout, &stderr)
		return stderr.String(), err
	}

	long := gittest.ReferenceNameOfLength(t, "refs/heads/", 1025).String()

	stderr, err := run(long, commitID.String())
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "over the limit of 1024 bytes")
	require.Contains(t, stderr, "over the limit of 1024 bytes", "the client must be told why")
	require.NoFileExists(t, marker, "the custom hook must not run for a refused reference")

	_, err = run(gittest.ReferenceNameOfLength(t, "refs/heads/", 1024).String(), commitID.String())
	require.NoError(t, err)
	require.FileExists(t, marker)
	require.NoError(t, os.Remove(marker))

	// Deleting an existing long reference is not refused.
	_, err = run(long, zero)
	require.NoError(t, err)
	require.FileExists(t, marker)
}

func TestReferenceTransactionHook_longReferenceNames(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	hookManager := NewManager(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t), gittest.NewCommandFactory(t, cfg), nil, transaction.NewTrackingManager(),
		gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive), NewPostReceiveRegistry())

	env, err := gitcmd.NewHooksPayload(ctx, cfg, repo, gittest.DefaultObjectHash, nil,
		&gitcmd.UserDetails{UserID: "1234", Username: "user", Protocol: "web"},
		gitcmd.ReferenceTransactionHook, featureflag.FromContext(ctx),
	).Env()
	require.NoError(t, err)

	zero := gittest.DefaultObjectHash.ZeroOID.String()
	oid := strings.Repeat("1", gittest.DefaultObjectHash.EncodedLen())
	long := gittest.ReferenceNameOfLength(t, "refs/heads/", 1025).String()
	limit := gittest.ReferenceNameOfLength(t, "refs/heads/", 1024).String()

	line := func(oldValue, newValue, name string) string {
		return fmt.Sprintf("%s %s %s\n", oldValue, newValue, name)
	}

	for _, tc := range []struct {
		desc    string
		changes string
		refused bool
	}{
		{desc: "create over the limit", changes: line(zero, oid, long), refused: true},
		{desc: "update over the limit", changes: line(oid, oid, long), refused: true},
		{desc: "among other updates", changes: line(zero, oid, "refs/heads/a") + line(zero, oid, long), refused: true},
		{desc: "symbolic reference over the limit", changes: line(zero, "ref:refs/heads/a", long), refused: true},
		{desc: "symbolic target over the limit", changes: line(zero, "ref:"+long, "refs/heads/alias"), refused: true},
		{desc: "exactly the limit", changes: line(zero, oid, limit)},
		{desc: "symbolic target of exactly the limit", changes: line(zero, "ref:"+limit, "refs/heads/alias")},
		{desc: "deletion of an existing long reference", changes: line(oid, zero, long)},
		{desc: "forced deletion of an existing long reference", changes: line(zero, zero, long)},
	} {
		// The prepared phase is the one that gitaly-hooks always forwards; the preparing phase only
		// reaches the server with the storage core.
		for phase, state := range map[string]ReferenceTransactionState{
			"prepared":  ReferenceTransactionPrepared,
			"preparing": ReferenceTransactionPreparing,
		} {
			t.Run(tc.desc+" "+phase, func(t *testing.T) {
				err := hookManager.ReferenceTransactionHook(ctx, state, []string{env}, strings.NewReader(tc.changes))
				if !tc.refused {
					require.NoError(t, err)
					return
				}

				require.Equal(t, codes.InvalidArgument, status.Code(err))
				require.ErrorContains(t, err, "over the limit of 1024 bytes")
			})
		}
	}
}

// TestValidateReferenceNameLengths_objectPool decides on names only: no git process sees them.
func TestValidateReferenceNameLengths_objectPool(t *testing.T) {
	t.Parallel()

	pool := &gitalypb.Repository{RelativePath: gittest.NewObjectPoolName(t)}
	repository := &gitalypb.Repository{RelativePath: "repository.git"}
	zero := gittest.DefaultObjectHash.ZeroOID.String()
	oid := strings.Repeat("1", gittest.DefaultObjectHash.EncodedLen())

	validate := func(repo *gitalypb.Repository, prefix string, length int) error {
		line := fmt.Sprintf("%s %s %s\n", zero, oid, gittest.ReferenceNameOfLength(t, prefix, length))
		return ValidateReferenceNameLengths(repo, gittest.DefaultObjectHash, []byte(line))
	}

	// The longest name of a source, 1024 bytes, is 1039 bytes under the refspec of its pool.
	require.NoError(t, validate(pool, "refs/remotes/origin/", 1024+15))
	require.ErrorContains(t, validate(pool, "refs/remotes/origin/", 1024+16), "over the limit of 1039 bytes")
	// The allowance is for that namespace of a pool only.
	require.ErrorContains(t, validate(pool, "refs/heads/", 1025), "over the limit of 1024 bytes")
	require.ErrorContains(t, validate(repository, "refs/remotes/origin/", 1025), "over the limit of 1024 bytes")
}
