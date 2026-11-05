package repoutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestCreate_longReferenceNames seeds the temporary repository by writing its packed-refs file, as a
// clone that brings a long name would leave it, without a git process writing the name.
func TestCreate_longReferenceNames(t *testing.T) {
	t.Parallel()
	testhelper.SkipWithReftable(t, "a reftable cannot be seeded without git writing the name")

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)
	catfileCache := catfile.NewCache(cfg)
	defer catfileCache.Stop()

	create := func(t *testing.T, nameLength int) (string, error) {
		t.Helper()

		repo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: gittest.NewRepositoryName(t)}
		err := Create(ctx, logger, locator, gitCmdFactory, catfileCache, &transaction.MockManager{}, counter.NewRepositoryCounter(cfg.Storages), repo,
			func(seed *gitalypb.Repository) error {
				seedPath, err := locator.GetRepoPath(ctx, seed, storage.WithRepositoryVerificationSkipped())
				require.NoError(t, err)

				commitID := gittest.WriteCommit(t, cfg, seedPath)
				gittest.SeedPackedReference(t, seedPath, gittest.ReferenceNameOfLength(t, "refs/heads/", nameLength), commitID)
				return nil
			},
		)

		return filepath.Join(cfg.Storages[0].Path, repo.GetRelativePath()), err
	}

	t.Run("a name over the limit lands nothing", func(t *testing.T) {
		path, err := create(t, 1025)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
		require.ErrorContains(t, err, "over the limit of 1024 bytes")

		_, statErr := os.Stat(path)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("a name of exactly the limit lands", func(t *testing.T) {
		path, err := create(t, 1024)
		require.NoError(t, err)
		gittest.RequireReferenceNameLengths(t, cfg, path, 1024)
	})
}
