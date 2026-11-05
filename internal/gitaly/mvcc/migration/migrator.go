package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	gitmvcc "github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/safe"
	"github.com/feder-cr/git-ai/internal/structerr"
)

const mvccDir = "mvcc"

// oldRefsBackupDir is where swapRefs moves the live refs/ aside before
// promoting the staged one; a leftover after a crash is what repairMissingRefs
// looks for.
const oldRefsBackupDir = gitmvcc.MigrationOldRefsDir

// migrationStageGlob matches the staging directory, which is moved into the
// repository so repairMissingRefs can find it again after a crash.
const migrationStageGlob = gitmvcc.MigrationStageGlob

// errPackedRefsModified is transient: a concurrent pack-refs already did the
// write. The next attempt is not penalized with a cooldown.
var errPackedRefsModified = errors.New("packed-refs modified")

// errAlreadyMigrated reports that the repository already uses the MVCC
// backend, so nothing was migrated.
var errAlreadyMigrated = errors.New("already migrated")

// errPostSwapCleanup wraps a failure after the config change is committed: the
// repository uses the MVCC backend despite the error.
var errPostSwapCleanup = errors.New("post-swap cleanup failed")

// nameRegex extracts the staging directory from git-refs-migrate(1)'s dry-run
// output. Anchoring on the known prefix tolerates a preceding warning line or a
// non-ASCII path.
var nameRegex = regexp.MustCompile(`Finished dry-run migration of refs, the result can be found at '([^']*)'`)

// mvccContext selects the bundled MVCC Git binary explicitly, since the
// repository still declares the files backend and can't imply it.
func mvccContext(ctx context.Context) context.Context {
	return gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)
}

func migrateWithDryRun(ctx context.Context, repo *localrepo.Repo) (string, error) {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(mvccContext(ctx), gitcmd.Command{
		Name:   "refs",
		Action: "migrate",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "--ref-format=" + git.ReferenceBackendMVCC.Name},
			gitcmd.Flag{Name: "--no-reflog"},
			gitcmd.Flag{Name: "--dry-run"},
		},
	}, gitcmd.WithStdout(&stdout), gitcmd.WithStderr(&stderr)); err != nil {
		return "", structerr.New("running refs migrate: %w", err).WithMetadata("stderr", stderr.String())
	}

	matches := nameRegex.FindStringSubmatch(stdout.String())
	if len(matches) != 2 {
		return "", errors.New("couldn't find migration directory")
	}

	return matches[1], nil
}

// rollbackPromotedStore undoes promoting mvccDir after swapBackend failed, so a
// repository that stays on its old backend isn't left with a store it doesn't
// use. The config commit can fail after the new config is already in place,
// for example when removing its lock, so the store is only removed once the
// config is known to still name the old backend. If it names MVCC, the store is
// live and the failure is reported as errPostSwapCleanup.
func rollbackPromotedStore(ctx context.Context, repoPath string, swapErr error) error {
	backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
	switch {
	case err != nil:
		// Keep the store rather than risk removing a live one.
		return errors.Join(swapErr, fmt.Errorf("detecting reference backend: %w", err))
	case backend.Name == git.ReferenceBackendMVCC.Name:
		return fmt.Errorf("%w: %w", errPostSwapCleanup, swapErr)
	}

	if err := os.RemoveAll(filepath.Join(repoPath, mvccDir)); err != nil {
		return errors.Join(swapErr, fmt.Errorf("removing promoted %q folder: %w", mvccDir, err))
	}
	return swapErr
}

// stagePackedRefs stages only the repository's packed refs, by running the
// dry-run in a stand-in repository that holds nothing but a hard link to
// packed-refs. Loose refs are left to the replay under the write lock: had they
// been staged, one deleted before the lock was taken would have to be tracked
// down and removed again, or the swap would resurrect it.
//
// It returns the staged store, moved into the repository, and the packed-refs
// that was staged, or nil if there was none. The returned function removes the
// stand-in. Until then the hard link keeps that packed-refs from being freed, so
// comparing it against the live one detects any rewrite.
func stagePackedRefs(ctx context.Context, repo *localrepo.Repo, repoPath string) (_ string, _ os.FileInfo, _ func(), returnedErr error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", nil, nil, fmt.Errorf("reading object hash: %w", err)
	}

	standIn, standInPath, removeStandIn, err := repo.NewTempRepository(ctx)
	if err != nil {
		return "", nil, nil, fmt.Errorf("creating stand-in: %w", err)
	}
	defer func() {
		if returnedErr != nil {
			removeStandIn()
		}
	}()

	// Housekeeping doesn't pack refs in repositories with only a few of them, so
	// a missing packed-refs is treated as empty.
	var packedRefs os.FileInfo
	switch err := os.Link(filepath.Join(repoPath, "packed-refs"), filepath.Join(standInPath, "packed-refs")); {
	case err == nil:
		if packedRefs, err = os.Stat(filepath.Join(standInPath, "packed-refs")); err != nil {
			return "", nil, nil, fmt.Errorf("stat packed-refs: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", nil, nil, fmt.Errorf("linking packed-refs: %w", err)
	}

	for _, dir := range []string{"objects/info", "refs"} {
		if err := os.MkdirAll(filepath.Join(standInPath, dir), mode.Directory); err != nil {
			return "", nil, nil, fmt.Errorf("creating stand-in %q: %w", dir, err)
		}
	}
	// Point the stand-in at the real repo's objects so git can peel annotated
	// tags when staging them. Without this, peel_object() fails (the reftable
	// writer calls it to compute peeled values), the tag is stored as VAL1
	// (no cached peel), and upload-pack has to re-read the tag object on every
	// clone or fetch rather than returning the peel directly from the reftable.
	if err := os.WriteFile(
		filepath.Join(standInPath, "objects", "info", "alternates"),
		[]byte(filepath.Join(repoPath, "objects")+"\n"),
		mode.File,
	); err != nil {
		return "", nil, nil, fmt.Errorf("writing stand-in alternates: %w", err)
	}
	for name, content := range map[string]string{
		"HEAD":   "ref: refs/heads/main\n",
		"config": "[core]\n\trepositoryformatversion = 1\n\tbare = true\n[extensions]\n\tobjectformat = " + objectHash.Format + "\n",
	} {
		if err := os.WriteFile(filepath.Join(standInPath, name), []byte(content), mode.File); err != nil {
			return "", nil, nil, fmt.Errorf("writing stand-in %q: %w", name, err)
		}
	}

	stagePath, err := migrateWithDryRun(ctx, standIn)
	if err != nil {
		return "", nil, nil, err
	}

	movedStagePath := filepath.Join(repoPath, filepath.Base(stagePath))
	if err := os.Rename(stagePath, movedStagePath); err != nil {
		return "", nil, nil, fmt.Errorf("moving staged store: %w", err)
	}

	return movedStagePath, packedRefs, removeStandIn, nil
}

// isLockFile skips git-update-ref(1) lock files, which would otherwise be
// treated as references and fail migration deterministically.
func isLockFile(name string) bool {
	return strings.HasSuffix(name, ".lock")
}

// applyLooseRef writes the loose ref stored at path into the staged store. Git
// never writes a loose ref it can't parse back, so one that doesn't parse is
// reported rather than skipped: skipping it would silently bring back an older
// packed value, or drop the ref.
func applyLooseRef(objectHash git.ObjectHash, updater *updateref.Updater, refname git.ReferenceName, path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading file: %w", err)
	}
	content = bytes.TrimSpace(content)

	if target, ok := bytes.CutPrefix(content, []byte("ref: ")); ok {
		return updater.UpdateSymbolicReference(refname, git.ReferenceName(target))
	}

	if err := objectHash.ValidateHex(string(content)); err != nil {
		return fmt.Errorf("invalid loose reference %q: %w", refname, err)
	}

	return updater.Update(refname, git.ObjectID(content), "")
}

// applyLooseRefs writes every loose reference under refs/ and HEAD into the
// staged store, and returns how many loose references it wrote.
func applyLooseRefs(ctx context.Context, repo *localrepo.Repo, repoPath string, updater *updateref.Updater) (int, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading object hash: %w", err)
	}

	numLooseRefs := 0
	err = filepath.WalkDir(filepath.Join(repoPath, "refs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Housekeeping can remove an empty directory mid-walk (see below).
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}

		if isLockFile(d.Name()) {
			return nil
		}

		refname, err := filepath.Rel(repoPath, path)
		if err != nil {
			return fmt.Errorf("parsing refname: %w", err)
		}

		// Git ignores entries starting with a dot, so a valid ref never has
		// one. Report them rather than guess what they are.
		if strings.HasPrefix(d.Name(), ".") {
			return fmt.Errorf("unexpected entry %q in refs/", refname)
		}

		if d.IsDir() {
			return nil
		}

		switch err := applyLooseRef(objectHash, updater, git.ReferenceName(refname), path); {
		case errors.Is(err, os.ErrNotExist):
			// Safety net for external writers or unexpected filesystem
			// conditions; within Gitaly the write lock and the housekeeping
			// lock prevent any in-process actor from removing a ref here.
			return nil
		case err != nil:
			return err
		}
		numLooseRefs++

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walking refs folder: %w", err)
	}

	// The stand-in's HEAD is a placeholder, so always replay the repository's own.
	if err := applyLooseRef(objectHash, updater, "HEAD", filepath.Join(repoPath, "HEAD")); err != nil {
		return 0, fmt.Errorf("applying HEAD: %w", err)
	}

	return numLooseRefs, nil
}

// swapBackend promotes the staged MVCC reference store into the repository and
// then switches config to it. Both config keys are written in a single locked
// file operation so a crash cannot leave the repository half-configured.
func swapBackend(ctx context.Context, repo *localrepo.Repo, repoPath, stagePath string) (err error) {
	if err := os.Rename(
		filepath.Join(stagePath, mvccDir),
		filepath.Join(repoPath, mvccDir),
	); err != nil {
		return fmt.Errorf("moving the %q folder: %w", mvccDir, err)
	}
	defer func() {
		if err != nil {
			err = rollbackPromotedStore(ctx, repoPath, err)
		}
	}()

	configPath := filepath.Join(repoPath, "config")

	writer, err := safe.NewLockingFileWriter(configPath, safe.LockingFileWriterConfig{
		SeedContents: true,
	})
	if err != nil {
		return fmt.Errorf("creating config writer: %w", err)
	}
	defer func() { _ = writer.Close() }()

	// Write via git-config(1) so quoting and section merging are handled.
	for _, kv := range [][2]string{
		{"core.repositoryformatversion", "1"},
		{"extensions.refstorage", git.ReferenceBackendMVCC.Name},
	} {
		var stderr bytes.Buffer
		if err := repo.ExecAndWait(mvccContext(ctx), gitcmd.Command{
			Name: "config",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "--replace-all"},
				gitcmd.ValueFlag{Name: "--file", Value: writer.Path()},
			},
			Args: []string{kv[0], kv[1]},
		}, gitcmd.WithStderr(&stderr)); err != nil {
			return structerr.New("writing %q config: %w", kv[0], err).WithMetadata("stderr", stderr.String())
		}
	}

	if err := writer.Lock(); err != nil {
		return fmt.Errorf("locking config file: %w", err)
	}

	if err := writer.Commit(ctx); err != nil {
		return fmt.Errorf("committing config file: %w", err)
	}

	return nil
}

// swapRefs moves the staged HEAD and refs/ into place. refs/ is moved aside
// rather than removed outright so that, on a crash between the two renames,
// repairMissingRefs can restore it from refs.mvcc-old rather than having to
// create an empty stub.
func swapRefs(repoPath, stagePath string) error {
	if err := os.Rename(
		filepath.Join(stagePath, "HEAD"),
		filepath.Join(repoPath, "HEAD"),
	); err != nil {
		return fmt.Errorf("replace HEAD: %w", err)
	}

	oldRefsPath := filepath.Join(repoPath, oldRefsBackupDir)
	if err := os.Rename(filepath.Join(repoPath, "refs"), oldRefsPath); err != nil {
		return fmt.Errorf("moving old refs aside: %w", err)
	}

	if err := os.Rename(
		filepath.Join(stagePath, "refs"),
		filepath.Join(repoPath, "refs"),
	); err != nil {
		// refs/ is missing now; restore the old one before a concurrent RPC sees it.
		if restoreErr := os.Rename(oldRefsPath, filepath.Join(repoPath, "refs")); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restoring old refs: %w", restoreErr))
		}
		return fmt.Errorf("move dry-run refs folder: %w", err)
	}

	return nil
}

// cleanupRepo removes what Git stops reading once config names MVCC: the old
// store, packed-refs or reftable/, the refs/ moved aside and the staging
// directory.
func cleanupRepo(repoPath, stagePath, oldStore string) error {
	if err := os.RemoveAll(filepath.Join(repoPath, oldStore)); err != nil {
		return fmt.Errorf("removing %q: %w", oldStore, err)
	}

	if err := os.RemoveAll(filepath.Join(repoPath, oldRefsBackupDir)); err != nil {
		return fmt.Errorf("removing old refs: %w", err)
	}

	if err := os.RemoveAll(stagePath); err != nil {
		return fmt.Errorf("removing migration folder: %w", err)
	}

	return nil
}

// samePackedRefs reports whether packed-refs is unchanged, including still
// being absent. mtime alone can miss a same-tick rewrite, so inode and size are
// compared too.
func samePackedRefs(before, after os.FileInfo) bool {
	if before == nil || after == nil {
		return before == nil && after == nil
	}
	return os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

// repairInterruptedSwap handles the one crash window that can't wait for
// housekeeping: a missing refs/ fails validation on every RPC, which keeps
// housekeeping from ever running. It acts only when refs/ is absent and config
// already declares MVCC; other leftovers are harmless. Config is read directly
// because every verified lookup fails while refs/ is missing.
func repairInterruptedSwap(ctx context.Context, repoPath string) error {
	if _, err := os.Stat(filepath.Join(repoPath, "refs")); err == nil {
		return nil
	}

	// Any error here (most likely a missing repository) is left for the verified
	// lookups below to classify.
	backend, err := gitcmd.DetectReferenceBackend(ctx, repoPath)
	if err != nil || backend.Name != git.ReferenceBackendMVCC.Name {
		return nil
	}

	return repairMissingRefs(repoPath)
}

// repairMissingRefs restores refs/ after a crash between the config commit and
// the on-disk swap. Once the backend is mvcc git never reads refs/ again; it
// only has to exist to satisfy Gitaly's validation, so its contents no longer
// matter. It tries, in order: a leftover staging directory's refs/, the
// pre-migration refs/ moved aside, then an empty directory.
func repairMissingRefs(repoPath string) error {
	refsPath := filepath.Join(repoPath, "refs")

	if _, err := os.Stat(refsPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat refs: %w", err)
	}

	stageMatches, err := filepath.Glob(filepath.Join(repoPath, migrationStageGlob))
	if err != nil {
		return fmt.Errorf("globbing for leftover migration directories: %w", err)
	}

	for _, stagePath := range stageMatches {
		stagedRefs := filepath.Join(stagePath, "refs")
		if _, err := os.Stat(stagedRefs); err != nil {
			continue
		}

		if err := os.Rename(stagedRefs, refsPath); err != nil {
			return fmt.Errorf("completing interrupted refs swap: %w", err)
		}
		if err := os.RemoveAll(stagePath); err != nil {
			return fmt.Errorf("removing leftover migration directory: %w", err)
		}
		// The pre-migration refs/ moved aside is dead weight now.
		if err := os.RemoveAll(filepath.Join(repoPath, oldRefsBackupDir)); err != nil {
			return fmt.Errorf("removing pre-migration refs: %w", err)
		}
		return nil
	}

	oldRefsPath := filepath.Join(repoPath, oldRefsBackupDir)
	if _, err := os.Stat(oldRefsPath); err == nil {
		if err := os.Rename(oldRefsPath, refsPath); err != nil {
			return fmt.Errorf("restoring pre-migration refs: %w", err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %q: %w", oldRefsBackupDir, err)
	}

	if err := os.Mkdir(refsPath, mode.Directory); err != nil {
		return fmt.Errorf("creating empty refs: %w", err)
	}
	return nil
}

// errReftableModified reports that the reftable store changed during the
// dry-run, so the staged store is outdated.
var errReftableModified = errors.New("reftable modified during the dry-run")

// migrateReftable migrates a reftable repository. Reftable has no loose refs to
// replay, but every write rewrites tables.list. So the dry-run runs unlocked and
// the migration gives up if tables.list changed in the meantime, rather than
// redo the whole dry-run while holding the write lock.
// It goes through the same swap rather than an in-place git-refs-migrate(1),
// which removes the old store before updating config and so could leave the
// repository without a usable store after a crash.
func migrateReftable(ctx context.Context, repo *localrepo.Repo, repoPath string, block, unblock func() error) (returnedErr error) {
	tablesList := filepath.Join(repoPath, "reftable", "tables.list")
	before, err := os.ReadFile(tablesList)
	if err != nil {
		return fmt.Errorf("reading tables.list: %w", err)
	}

	stagePath, err := migrateWithDryRun(ctx, repo)
	if err != nil {
		return fmt.Errorf("creating dry-run mvcc store: %w", err)
	}
	defer func() {
		if returnedErr != nil {
			if cleanupErr := os.RemoveAll(stagePath); cleanupErr != nil {
				returnedErr = errors.Join(returnedErr, cleanupErr)
			}
		}
	}()

	if err := func() (returnedErr error) {
		if err := block(); err != nil {
			return fmt.Errorf("failed to block: %w", err)
		}
		defer func() {
			if err := unblock(); err != nil {
				returnedErr = errors.Join(fmt.Errorf("failed to unblock: %w", err), returnedErr)
			}
		}()

		after, err := os.ReadFile(tablesList)
		if err != nil {
			return fmt.Errorf("reading tables.list: %w", err)
		}
		if !bytes.Equal(before, after) {
			return errReftableModified
		}

		if err := swapBackend(ctx, repo, repoPath, stagePath); err != nil {
			return fmt.Errorf("swapping backend: %w", err)
		}

		if err := swapRefs(repoPath, stagePath); err != nil {
			return fmt.Errorf("%w: %w", errPostSwapCleanup, err)
		}
		return nil
	}(); err != nil {
		return err
	}

	// Git no longer reads the old store, so remove it without blocking writes.
	if err := cleanupRepo(repoPath, stagePath, "reftable"); err != nil {
		return fmt.Errorf("%w: %w", errPostSwapCleanup, err)
	}

	return nil
}

type migrationStats struct {
	packedRefsSize int64
	numLooseRefs   int
}

func migrate(
	ctx context.Context,
	logger log.Logger,
	repo *localrepo.Repo,
	block func() error,
	unblock func() error,
) (_ migrationStats, err error) {
	var stats migrationStats

	// A crash can leave refs/ missing (see repairInterruptedSwap). Repair it
	// before any verified lookup below, which would otherwise reject the
	// repository and leave nothing to ever fix it.
	rawRepoPath, err := repo.PathWithoutVerification(ctx)
	if err != nil {
		return stats, fmt.Errorf("getting repo path: %w", err)
	}
	if err := repairInterruptedSwap(ctx, rawRepoPath); err != nil {
		return stats, fmt.Errorf("repairing interrupted swap: %w", err)
	}

	backend, err := repo.ReferenceBackend(ctx)
	if err != nil {
		return stats, fmt.Errorf("reference backend: %w", err)
	}

	if backend.Name == git.ReferenceBackendMVCC.Name {
		return stats, errAlreadyMigrated
	}

	repoPath, err := repo.Path(ctx)
	if err != nil {
		return stats, fmt.Errorf("getting repo path: %w", err)
	}

	// An attempt interrupted after promoting mvcc/ but before the config
	// commit leaves it behind, and promoting onto it would then always fail.
	// This must run only for non-MVCC repos: an already-migrated repo has a
	// live mvcc/ directory and removing it would wipe all its references.
	if _, err := os.Stat(filepath.Join(repoPath, mvccDir)); err == nil {
		logger.WithFields(log.Fields{"storage_name": repo.GetStorageName(), "relative_path": repo.GetRelativePath()}).
			WarnContext(ctx, "removing leftover mvcc directory from a previous migration attempt")
		if err := os.RemoveAll(filepath.Join(repoPath, mvccDir)); err != nil {
			return stats, fmt.Errorf("removing leftover %q folder: %w", mvccDir, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return stats, fmt.Errorf("stat %q folder: %w", mvccDir, err)
	}

	if backend.Name == git.ReferenceBackendReftables.Name {
		return stats, migrateReftable(ctx, repo, repoPath, block, unblock)
	}

	return migrateFilesBackend(ctx, repo, repoPath, block, unblock)
}

func migrateFilesBackend(
	ctx context.Context,
	repo *localrepo.Repo,
	repoPath string,
	block func() error,
	unblock func() error,
) (_ migrationStats, err error) {
	var stats migrationStats

	stagePath, before, removeStandIn, err := stagePackedRefs(ctx, repo, repoPath)
	if err != nil {
		return stats, fmt.Errorf("creating dry-run mvcc store: %w", err)
	}
	defer removeStandIn()
	if before != nil {
		stats.packedRefsSize = before.Size()
	}

	defer func() {
		if err != nil {
			if cleanupErr := os.RemoveAll(stagePath); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()

	updater, err := updateref.New(
		mvccContext(ctx),
		repo,
		updateref.WithNoDeref(),
		updateref.WithDisabledTransactions(),
		updateref.WithExistingReferences(),
		updateref.WithRefURI(git.ReferenceBackendMVCC, stagePath),
	)
	if err != nil {
		return stats, fmt.Errorf("creating updateref: %w", err)
	}
	// The happy path closes the updater inside the critical section (before the
	// swap); this deferred close covers every earlier error return.
	updaterClosed := false
	defer func() {
		if updaterClosed {
			return
		}
		if closeErr := updater.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close update-ref: %w", closeErr)
		}
	}()

	if err := updater.Start(); err != nil {
		return stats, fmt.Errorf("start update-ref: %w", err)
	}

	if err := func() (returnedErr error) {
		if returnedErr = block(); returnedErr != nil {
			return fmt.Errorf("failed to block: %w", returnedErr)
		}

		defer func() {
			if err := unblock(); err != nil {
				returnedErr = errors.Join(fmt.Errorf("failed to unblock: %w", err), returnedErr)
			}
		}()

		// Runs under the write lock so no loose ref can be written between the
		// walk and the swap.
		numLooseRefs, err := applyLooseRefs(ctx, repo, repoPath, updater)
		if err != nil {
			return fmt.Errorf("applying loose-refs: %w", err)
		}
		stats.numLooseRefs = numLooseRefs

		if err := updater.Commit(); err != nil {
			return fmt.Errorf("commit update-ref: %w", err)
		}

		// git-update-ref(1) only flushes the new manifest on exit, so close it
		// before the swap or the replayed refs are silently dropped.
		if err := updater.Close(); err != nil {
			return fmt.Errorf("close update-ref: %w", err)
		}
		updaterClosed = true

		after, err := os.Stat(filepath.Join(repoPath, "packed-refs"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat packed-refs: %w", err)
		}

		if !samePackedRefs(before, after) {
			return errPackedRefsModified
		}

		if err := swapBackend(ctx, repo, repoPath, stagePath); err != nil {
			return fmt.Errorf("swapping backend: %w", err)
		}

		// The config change is already committed, so the repository is on MVCC
		// regardless; errPostSwapCleanup lets the caller not treat this as a
		// migration failure.
		if err := swapRefs(repoPath, stagePath); err != nil {
			return fmt.Errorf("%w: %w", errPostSwapCleanup, err)
		}
		return nil
	}(); err != nil {
		return stats, err
	}

	// Git no longer reads the old store, so remove it without blocking writes.
	if err := cleanupRepo(repoPath, stagePath, "packed-refs"); err != nil {
		return stats, fmt.Errorf("%w: %w", errPostSwapCleanup, err)
	}

	return stats, nil
}
