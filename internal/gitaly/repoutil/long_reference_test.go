package repoutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

	createWithHead := func(t *testing.T, targetLength int) (string, error) {
		t.Helper()

		repo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: gittest.NewRepositoryName(t)}
		err := Create(ctx, logger, locator, gitCmdFactory, catfileCache, &transaction.MockManager{}, counter.NewRepositoryCounter(cfg.Storages), repo,
			func(seed *gitalypb.Repository) error {
				seedPath, err := locator.GetRepoPath(ctx, seed, storage.WithRepositoryVerificationSkipped())
				require.NoError(t, err)

				// An unborn HEAD, as git-clone(1) copies it from a remote: no reference exists.
				target := gittest.ReferenceNameOfLength(t, "refs/heads/", targetLength)
				return os.WriteFile(filepath.Join(seedPath, "HEAD"), []byte("ref: "+target.String()+"\n"), 0o644)
			},
		)

		return filepath.Join(cfg.Storages[0].Path, repo.GetRelativePath()), err
	}

	t.Run("an unborn HEAD over the limit lands nothing", func(t *testing.T) {
		path, err := createWithHead(t, 1025)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
		require.ErrorContains(t, err, "over the limit of 1024 bytes")

		_, statErr := os.Stat(path)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("an unborn HEAD of exactly the limit lands", func(t *testing.T) {
		_, err := createWithHead(t, 1024)
		require.NoError(t, err)
	})

	t.Run("a pool keeps its allowance", func(t *testing.T) {
		repo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: gittest.NewObjectPoolName(t)}
		err := Create(ctx, logger, locator, gitCmdFactory, catfileCache, &transaction.MockManager{}, counter.NewRepositoryCounter(cfg.Storages), repo,
			func(seed *gitalypb.Repository) error {
				seedPath, err := locator.GetRepoPath(ctx, seed, storage.WithRepositoryVerificationSkipped())
				require.NoError(t, err)

				commitID := gittest.WriteCommit(t, cfg, seedPath)
				gittest.SeedPackedReference(t, seedPath, gittest.ReferenceNameOfLength(t, "refs/remotes/origin/", 1039), commitID)
				return nil
			},
		)
		require.NoError(t, err)
	})

	t.Run("a name of exactly the limit lands", func(t *testing.T) {
		path, err := create(t, 1024)
		require.NoError(t, err)
		gittest.RequireReferenceNameLengths(t, cfg, path, 1024)
	})
}

// TestValidateForEachRefLine judges lines as text: no git process sees these names.
func TestValidateForEachRefLine(t *testing.T) {
	t.Parallel()

	const refused = "over the limit of 1024 bytes"
	short := "refs/heads/main"
	limit := gittest.ReferenceNameOfLength(t, "refs/heads/", 1024).String()
	long := gittest.ReferenceNameOfLength(t, "refs/heads/", 1025).String()

	// git accepts these in a name. Each splits the name into parts under the limit when it is
	// taken for a separator.
	for _, whitespace := range []string{"\u00a0", "\u0085", "\u2003", "\u3000"} {
		name := "refs/heads/" + strings.Repeat(strings.Repeat("x", 500)+whitespace, 3)
		require.Greater(t, len(name), 1024)

		require.ErrorContains(t, validateForEachRefLine(name+" \n", false), refused)
		require.ErrorContains(t, validateForEachRefLine("refs/heads/alias "+name+"\n", false), refused)
	}

	require.NoError(t, validateForEachRefLine(limit+" "+limit+"\n", false))
	require.NoError(t, validateForEachRefLine(short+" \n", false))
	require.NoError(t, validateForEachRefLine("", false))
	require.ErrorContains(t, validateForEachRefLine(long+" \n", false), refused)
	require.ErrorContains(t, validateForEachRefLine(short+" "+long+"\n", false), refused)
	require.ErrorContains(t, validateForEachRefLine(long, false), refused, "the last line may have no newline")
}
