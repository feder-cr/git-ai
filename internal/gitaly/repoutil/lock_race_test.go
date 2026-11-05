package repoutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/safe"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// raceBeforeLock makes Lock run race on the path of repoPath, as a concurrent request would between
// the moment Lock reads the path and the moment it takes the lock. The tests that use it do not run
// in parallel: the hook is global, and Go runs the parallel tests of the package after them.
func raceBeforeLock(t *testing.T, repoPath string, race func(t *testing.T)) {
	t.Helper()
	beforeLock = func(path string) {
		if path == repoPath {
			race(t)
		}
	}
	t.Cleanup(func() { beforeLock = nil })
}

func TestLockLosesARaceOfItsPath(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)

	for _, tc := range []struct {
		desc     string
		existing bool
		race     func(t *testing.T, repoPath string)
		// cause is the error of the race; locked tells that Lock reports it as a held lock.
		cause  error
		locked bool
	}{
		{
			desc:  "another request created the repository",
			race:  func(t *testing.T, repoPath string) { require.NoError(t, os.Mkdir(repoPath, mode.Directory)) },
			cause: safe.ErrFileConcurrentlyCreated, locked: true,
		},
		{
			desc:     "another request removed the repository",
			existing: true,
			race:     func(t *testing.T, repoPath string) { require.NoError(t, os.Remove(repoPath)) },
			cause:    safe.ErrFileConcurrentlyDeleted, locked: true,
		},
		{
			// Not a creation or a removal: a write into the repository's directory.
			desc:     "the repository's directory changed",
			existing: true,
			race: func(t *testing.T, repoPath string) {
				require.NoError(t, os.WriteFile(filepath.Join(repoPath, "new-file"), nil, mode.File))
			},
			cause: safe.ErrFileConcurrentlyModified,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			repo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: gittest.NewRepositoryName(t)}
			repoPath, err := locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(repoPath), mode.Directory))
			if tc.existing {
				require.NoError(t, os.Mkdir(repoPath, mode.Directory))
				// The kernel stamps the directory with a coarse clock: a write in the same
				// tick as the mkdir could leave its modification time as it was.
				past := time.Now().Add(-time.Hour)
				require.NoError(t, os.Chtimes(repoPath, past, past))
			}
			raceBeforeLock(t, repoPath, func(t *testing.T) { tc.race(t, repoPath) })

			unlock, err := Lock(ctx, logger, locator, repo)
			require.Nil(t, unlock)
			require.ErrorIs(t, err, tc.cause)
			require.Equal(t, tc.locked, errors.Is(err, safe.ErrFileAlreadyLocked), "reported as a held lock: %v", err)
			require.NoFileExists(t, repoPath+".lock", "the losing request leaves no lock")
		})
	}
}

// TestCreateAndRemoveLoseARaceAsALockedRepository checks that Create and Remove answer a creation or
// a removal of the repository by another request between their read of the path and their lock with
// the error of a held lock, FailedPrecondition "repository is already locked", not Internal.
func TestCreateAndRemoveLoseARaceAsALockedRepository(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)
	catfileCache := catfile.NewCache(cfg)
	defer catfileCache.Stop()
	locked := structerr.NewFailedPrecondition("repository is already locked")

	t.Run("create", func(t *testing.T) {
		repo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: gittest.NewRepositoryName(t)}
		repoPath, err := locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())
		require.NoError(t, err)
		raceBeforeLock(t, repoPath, func(t *testing.T) { require.NoError(t, os.Mkdir(repoPath, mode.Directory)) })

		err = Create(ctx, logger, locator, gittest.NewCommandFactory(t, cfg), catfileCache, &transaction.MockManager{},
			counter.NewRepositoryCounter(cfg.Storages), repo, func(*gitalypb.Repository) error { return nil })
		require.Equal(t, locked, err)
		require.NoFileExists(t, repoPath+".lock")
	})

	t.Run("remove", func(t *testing.T) {
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		raceBeforeLock(t, repoPath, func(t *testing.T) { require.NoError(t, os.RemoveAll(repoPath)) })

		err := Remove(ctx, logger, locator, &transaction.MockManager{}, counter.NewRepositoryCounter(cfg.Storages), repo)
		require.Equal(t, locked, err)
		require.NoFileExists(t, repoPath+".lock")
	})
}
