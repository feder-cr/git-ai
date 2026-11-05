package updateref

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A name over the limit never reaches git-update-ref(1) in these tests: a test stops at the first
// failed assertion, and the updater's cleanup then aborts the transaction, which writes nothing.

func requireTooLong(t *testing.T, err error) {
	t.Helper()

	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "over the limit of 1024 bytes")
}

func TestUpdater_longReferenceNames(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	long := gittest.ReferenceNameOfLength(t, "refs/heads/", 1025)
	limit := gittest.ReferenceNameOfLength(t, "refs/heads/", 1024)

	t.Run("create over the limit is refused and aborts what is queued", func(t *testing.T) {
		t.Parallel()

		cfg, _, repoPath, updater := setupUpdater(t, ctx)
		commitID := gittest.WriteCommit(t, cfg, repoPath)

		require.NoError(t, updater.Start())
		require.NoError(t, updater.Create("refs/heads/queued", commitID))
		requireTooLong(t, updater.Create(long, commitID))

		// The updater is closed, and stays so.
		requireTooLong(t, updater.Commit())
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})

	t.Run("forced update over the limit is refused", func(t *testing.T) {
		t.Parallel()

		cfg, _, repoPath, updater := setupUpdater(t, ctx)
		commitID := gittest.WriteCommit(t, cfg, repoPath)

		require.NoError(t, updater.Start())
		requireTooLong(t, updater.Update(long, commitID, ""))
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})

	t.Run("symbolic reference named over the limit is refused", func(t *testing.T) {
		t.Parallel()

		cfg, _, repoPath, updater := setupUpdater(t, ctx)

		require.NoError(t, updater.Start())
		requireTooLong(t, updater.UpdateSymbolicReference(long, "refs/heads/main"))
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})

	t.Run("symbolic reference target over the limit is refused", func(t *testing.T) {
		t.Parallel()

		cfg, _, repoPath, updater := setupUpdater(t, ctx)

		require.NoError(t, updater.Start())
		requireTooLong(t, updater.UpdateSymbolicReference("refs/heads/alias", long))
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})

	t.Run("exactly the limit is written", func(t *testing.T) {
		t.Parallel()

		cfg, _, repoPath, updater := setupUpdater(t, ctx)
		commitID := gittest.WriteCommit(t, cfg, repoPath)

		require.NoError(t, updater.Start())
		require.NoError(t, updater.Create(limit, commitID))
		require.NoError(t, updater.Commit())
		gittest.RequireReferenceNameLengths(t, cfg, repoPath, 1024)
	})

	t.Run("symbolic reference target of exactly the limit is written", func(t *testing.T) {
		t.Parallel()

		cfg, repo, repoPath, _ := setupUpdater(t, ctx)
		commitID := gittest.WriteCommit(t, cfg, repoPath)

		updater, err := New(ctx, repo, WithNoDeref())
		require.NoError(t, err)
		defer func() { _ = updater.Close() }()

		require.NoError(t, updater.Start())
		require.NoError(t, updater.Create(limit, commitID))
		require.NoError(t, updater.UpdateSymbolicReference("refs/heads/alias", limit))
		require.NoError(t, updater.Commit())
		gittest.RequireReferenceNameLengths(t, cfg, repoPath, 1024, len("refs/heads/alias"))
	})

	t.Run("an existing long reference can be deleted", func(t *testing.T) {
		t.Parallel()

		cfg, _, repoPath, updater := setupUpdater(t, ctx)
		commitID := gittest.WriteCommit(t, cfg, repoPath)
		gittest.SeedPackedReference(t, repoPath, long, commitID)
		gittest.RequireReferenceNameLengths(t, cfg, repoPath, 1025)

		require.NoError(t, updater.Start())
		require.NoError(t, updater.Delete(long))
		require.NoError(t, updater.Commit())
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})

	t.Run("deleting through Update with the zero object ID is not refused", func(t *testing.T) {
		t.Parallel()

		cfg, _, repoPath, updater := setupUpdater(t, ctx)
		commitID := gittest.WriteCommit(t, cfg, repoPath)
		gittest.SeedPackedReference(t, repoPath, long, commitID)

		require.NoError(t, updater.Start())
		require.NoError(t, updater.Update(long, gittest.DefaultObjectHash.ZeroOID, commitID))
		require.NoError(t, updater.Commit())
		gittest.RequireReferenceNameLengths(t, cfg, repoPath)
	})

	t.Run("a copy of existing references is not refused", func(t *testing.T) {
		t.Parallel()

		cfg, repo, repoPath, _ := setupUpdater(t, ctx)
		commitID := gittest.WriteCommit(t, cfg, repoPath)

		updater, err := New(ctx, repo, WithExistingReferences())
		require.NoError(t, err)
		defer func() { _ = updater.Close() }()

		// Queued only: the deferred Close aborts it, so that git writes nothing.
		require.NoError(t, updater.Start())
		require.NoError(t, updater.Create(git.ReferenceName(long), commitID))
	})
}
