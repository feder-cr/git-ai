package repoutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/safe"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// beforeLock, when set by a test, is called by Lock with the repository path
// after the path was read and before the lock is taken, where a concurrent
// request may create or remove the repository.
var beforeLock func(path string)

// Lock attempts to lock the entire repository directory such that only one
// process can obtain the lock at a time.
//
// The repositories parent directory will be created if it does not exist.
//
// Returns an error that is safe.ErrFileAlreadyLocked if the repository is
// already locked, and also if another request created or removed the
// repository between the moment Lock read its path and the moment it took the
// lock: that request held the lock meanwhile, and the caller lost the same race
// it loses when it finds the lock taken.
func Lock(ctx context.Context, logger log.Logger, locator storage.Locator, repository storage.Repository) (func(), error) {
	path, err := locator.GetRepoPath(ctx, repository, storage.WithRepositoryVerificationSkipped())
	if err != nil {
		return nil, err
	}

	// Create the parent directory in case it doesn't exist yet.
	if err := os.MkdirAll(filepath.Dir(path), mode.Directory); err != nil {
		return nil, structerr.NewInternal("create directories: %w", err)
	}

	// We're somewhat abusing this file writer given that we simply want to assert that
	// the target directory doesn't exist and isn't created while we want to move the
	// new repository into place. We thus only use the locking semantics of the writer,
	// but will never commit it.
	locker, err := safe.NewLockingFileWriter(path)
	if err != nil {
		return nil, err
	}

	unlock := func() {
		if err := locker.Close(); err != nil {
			logger.WithError(err).ErrorContext(ctx, "closing repository locker failed")
		}
	}

	if beforeLock != nil {
		beforeLock(path)
	}

	if err := locker.Lock(); err != nil {
		unlock()

		if errors.Is(err, safe.ErrFileConcurrentlyCreated) || errors.Is(err, safe.ErrFileConcurrentlyDeleted) {
			return nil, fmt.Errorf("%w: %w", safe.ErrFileAlreadyLocked, err)
		}
		return nil, err
	}

	return unlock, nil
}
