package repoutil

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/trace"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	housekeepingcfg "github.com/feder-cr/git-ai/internal/git/housekeeping/config"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/stats"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/safe"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/tempdir"
	"github.com/feder-cr/git-ai/internal/transaction/voting"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

type createConfig struct {
	gitOptions []gitcmd.Option
	skipInit   bool
}

// CreateOption is an option that can be passed to Create.
type CreateOption func(cfg *createConfig)

// WithBranchName overrides the default branch name that is to be used when creating the repository.
// If called with an empty string then the default branch name will not be changed.
func WithBranchName(branch string) CreateOption {
	return func(cfg *createConfig) {
		if branch == "" {
			return
		}

		cfg.gitOptions = append(cfg.gitOptions, gitcmd.ValueFlag{Name: "--initial-branch", Value: branch})
	}
}

// WithObjectHash overrides the default object hash of the created repository.
func WithObjectHash(hash git.ObjectHash) CreateOption {
	return func(cfg *createConfig) {
		cfg.gitOptions = append(cfg.gitOptions, gitcmd.ValueFlag{Name: "--object-format", Value: hash.Format})
	}
}

// WithSkipInit causes Create to skip calling git-init(1) so that the seeding function will be
// called with a nonexistent target directory. This can be useful when using git-clone(1) to seed
// the repository.
func WithSkipInit() CreateOption {
	return func(cfg *createConfig) {
		cfg.skipInit = true
	}
}

// WithReferenceBackend sets the reference backend for the new repository.
func WithReferenceBackend(refBackend git.ReferenceBackend) CreateOption {
	return func(cfg *createConfig) {
		cfg.gitOptions = append(cfg.gitOptions, gitcmd.ValueFlag{Name: "--ref-format", Value: refBackend.Name})
	}
}

// Create will create a new repository in a race-free way with proper transactional semantics. The
// repository will only be created if it doesn't yet exist and if nodes which take part in the
// transaction reach quorum. Otherwise, the target path of the new repository will not be modified.
// The repository can optionally be seeded with contents
func Create(
	ctx context.Context,
	logger log.Logger,
	locator storage.Locator,
	gitCmdFactory gitcmd.CommandFactory,
	catfileCache catfile.Cache,
	txManager transaction.Manager,
	repoCounter *counter.RepositoryCounter,
	repository storage.Repository,
	seedRepository func(repository *gitalypb.Repository) error,
	options ...CreateOption,
) error {
	targetPath, err := locator.GetRepoPath(ctx, repository, storage.WithRepositoryVerificationSkipped())
	if err != nil {
		return structerr.NewInvalidArgument("locate repository: %w", err)
	}

	// The repository must not exist on disk already, or otherwise we won't be able to
	// create it with atomic semantics.
	if _, err := os.Stat(targetPath); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			return structerr.NewAlreadyExists("repository exists already")
		}

		return fmt.Errorf("pre-lock stat: %w", err)
	}

	newRepoProto, newRepoDir, cleanup, err := tempdir.NewRepository(ctx, repository.GetStorageName(), logger, locator)
	if err != nil {
		return fmt.Errorf("creating temporary repository: %w", err)
	}
	defer cleanup()

	// Note that we do not create the repository directly in its target location, but
	// instead create it in a temporary directory, first. This is done such that we can
	// guarantee atomicity and roll back the change easily in case an error happens.

	var cfg createConfig
	for _, option := range options {
		option(&cfg)
	}

	if !cfg.skipInit {
		stderr := &bytes.Buffer{}
		cmd, err := gitCmdFactory.NewWithoutRepo(ctx, gitcmd.Command{
			Name: "init",
			Flags: append([]gitcmd.Option{
				gitcmd.Flag{Name: "--bare"},
				gitcmd.Flag{Name: "--quiet"},
			}, cfg.gitOptions...),
			Args: []string{newRepoDir.Path()},
		}, gitcmd.WithStderr(stderr))
		if err != nil {
			return fmt.Errorf("spawning git-init: %w", err)
		}

		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("creating repository: %w, stderr: %q", err, stderr.String())
		}
	} else {
		if err := os.Remove(newRepoDir.Path()); err != nil {
			return fmt.Errorf("removing precreated directory: %w", err)
		}
	}

	if err := seedRepository(newRepoProto); err != nil {
		// Return the error returned by the callback function as-is so we don't clobber any
		// potential returned gRPC error codes.
		return err
	}

	newRepo := localrepo.New(logger, locator, gitCmdFactory, catfileCache, newRepoProto)

	// The seed can bring references from a URL, a bundle, a snapshot or another repository, and git
	// clone runs no hook that could refuse them, so the names are checked here. The repository is
	// still the temporary one: when it is refused nothing lands.
	if err := validateReferenceNameLengths(ctx, newRepo, storage.IsPoolRepository(repository)); err != nil {
		return err
	}

	// In order to guarantee that the repository is going to be the same across all
	// Gitalies in case we're behind Praefect, we vote on the logical state of the
	// repository.
	voteHash := voting.NewVoteHash()

	if err := VoteRepository(ctx, &voteHash, newRepo); err != nil {
		return err
	}

	vote, err := voteHash.Vote()
	if err != nil {
		return fmt.Errorf("computing vote: %w", err)
	}

	// Create the full-repack timestamp in the new repository. This is done so that we don't
	// consider new repositories to have never been repacked yet, which would cause repository
	// housekeeping to perform a full repack right away. And in general, this would not really
	// be needed as the end result for most of the repository-creating RPCs would be a either an
	// empty or a neatly-packed repository anyway.
	//
	// As this timestamp should never impact the user-observable state of a repository we do not
	// include it in the voting hash.
	if err := stats.UpdateFullRepackTimestamp(newRepoDir.Path(), time.Now()); err != nil {
		return fmt.Errorf("creating full-repack timestamp: %w", err)
	}

	// We're now entering the critical section where we want to have exclusive access
	// over creation of the repository. So we:
	//
	// 1. Lock the repository path such that no other process can create it at the same
	//    time.
	// 2. Vote on the new repository's state.
	// 3. Move the repository into place.
	// 4. Do another confirmatory vote to signal that we performed the change.
	// 5. Unlock the repository again.
	//
	// This sequence guarantees that the change is atomic and can trivially be rolled
	// back in case we fail to either lock the repository or reach quorum in the initial
	// vote.
	unlock, err := Lock(ctx, logger, locator, repository)
	if err != nil {
		// Another request is creating or removing the repository (or one left a
		// stale lock): a conflict of the caller, as in Remove, not an internal
		// error.
		if errors.Is(err, safe.ErrFileAlreadyLocked) {
			return structerr.NewFailedPrecondition("repository is already locked")
		}
		return fmt.Errorf("locking repository: %w", err)
	}
	defer unlock()

	// Now that the repository is locked, we must assert that it _still_ doesn't exist.
	// Otherwise, it could have happened that a concurrent RPC call created it while we created
	// and seeded our temporary repository. While we would notice this at the point of moving
	// the repository into place, we want to be as sure as possible that the action will succeed
	// previous to the first transactional vote.
	if _, err := os.Stat(targetPath); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			return structerr.NewAlreadyExists("repository exists already")
		}

		return fmt.Errorf("post-lock stat: %w", err)
	}
	if err := transaction.VoteOnContext(ctx, txManager, vote, voting.Preparing); err != nil {
		return structerr.NewFailedPrecondition("preparatory vote: %w", err)
	}

	if err := transaction.VoteOnContext(ctx, txManager, vote, voting.Prepared); err != nil {
		return structerr.NewFailedPrecondition("preparatory vote: %w", err)
	}

	syncer := safe.NewSyncer()
	if storage.NeedsSync(ctx) {
		if err := syncer.SyncRecursive(ctx, newRepoDir.Path()); err != nil {
			return fmt.Errorf("sync recursive: %w", err)
		}
	}

	// Now that we have locked the repository and all Gitalies have agreed that they
	// want to do the same change we can move the repository into place.
	if err := os.Rename(newRepoDir.Path(), targetPath); err != nil {
		return fmt.Errorf("moving repository into place: %w", err)
	}

	storagePath, err := locator.GetStorageByName(ctx, repository.GetStorageName())
	if err != nil {
		return fmt.Errorf("get storage by name: %w", err)
	}

	if storage.NeedsSync(ctx) {
		if err := syncer.SyncHierarchy(ctx, storagePath, repository.GetRelativePath()); err != nil {
			return fmt.Errorf("sync hierarchy: %w", err)
		}
	}

	if err := transaction.VoteOnContext(ctx, txManager, vote, voting.Committed); err != nil {
		return structerr.NewFailedPrecondition("committing vote: %w", err)
	}

	repoCounter.Increment(repository)

	if tx := storage.ExtractTransaction(ctx); tx != nil {
		// Git allows writing unreachable objects into the repository that are missing their dependencies. The reachable
		// ones are checked through connectivity checks but unreachable ones are not.
		//
		// Transactions rely on a property that all objects in the repository have all of their dependencies met. This allows
		// us to skip full connectivity checks, and simply check that the immediate dependencies of the newly written objects
		// are satisfied. Repository creations are used in various contexts and not all of them guarantee this property. Perform
		// a full repack to drop all unreachable objects. This way we're certain all of the objects committed through a repository
		// creation have their dependencies satisified. Ideally we would only perform a connectivity check of the new objects,
		// and record the dependencies that must exist in the repository already. Repository creations should generally include
		// all objects so the rewriting should not be needed. Issue: https://gitlab.com/gitlab-org/gitaly/-/issues/5969
		if err := performFullRepack(ctx, localrepo.New(logger, locator, gitCmdFactory, catfileCache, &gitalypb.Repository{
			StorageName:  repository.GetStorageName(),
			RelativePath: repository.GetRelativePath(),
		})); err != nil {
			return fmt.Errorf("perform full repack: %w", err)
		}

		originalRelativePath, err := filepath.Rel(tx.FS().Root(), targetPath)
		if err != nil {
			return fmt.Errorf("original relative path: %w", err)
		}

		if err := storage.RecordDirectoryCreation(tx.FS(), originalRelativePath); err != nil {
			return fmt.Errorf("record directory creation: %w", err)
		}

		if err := tx.KV().Set(storage.RepositoryKey(originalRelativePath), nil); err != nil {
			return fmt.Errorf("store repository key: %w", err)
		}
	}

	// We unlock the repository implicitly via the deferred `Close()` call.
	return nil
}

// VoteRepository calculates a vote of a repository based on its logical state.
// We skip the object database for the sake of scalability, and accept
// references as a suitable measure of repository state.
func VoteRepository(ctx context.Context, voteHash *voting.VoteHash, repo *localrepo.Repo) error {
	if err := voteGitConfig(ctx, voteHash, repo); err != nil {
		return fmt.Errorf("vote on git config: %w", err)
	}

	if err := voteReferences(ctx, voteHash, repo); err != nil {
		return fmt.Errorf("vote on references: %w", err)
	}

	return nil
}

// validateReferenceNameLengths refuses a new repository that holds a reference name, or a symbolic
// reference target, over the limit. git-for-each-ref(1) leaves out a symbolic reference that points at
// nothing, so HEAD, which git-clone(1) copies from the remote even when it is unborn, is read by itself.
func validateReferenceNameLengths(ctx context.Context, repo *localrepo.Repo, inObjectPool bool) error {
	var head, stderr bytes.Buffer
	err := repo.ExecAndWait(ctx, gitcmd.Command{
		Name:  "symbolic-ref",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "--quiet"}, gitcmd.Flag{Name: "--no-recurse"}},
		Args:  []string{"HEAD"},
	}, gitcmd.WithDisabledHooks(), gitcmd.WithStdout(&head), gitcmd.WithStderr(&stderr))
	// git-symbolic-ref(1) with --quiet exits with 1 when HEAD is detached.
	if code, ok := command.ExitStatus(err); err != nil && (!ok || code != 1) {
		return fmt.Errorf("reading HEAD: %w, stderr: %q", err, stderr.String())
	}
	if err := git.ValidateReferenceNameLength(strings.TrimSuffix(head.String(), "\n")); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stderr.Reset()
	cmd, err := repo.Exec(ctx, gitcmd.Command{
		Name:  "for-each-ref",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "--format=%(refname) %(symref)"}},
	}, gitcmd.WithSetupStdout(), gitcmd.WithStderr(&stderr))
	if err != nil {
		return fmt.Errorf("spawning for-each-ref: %w", err)
	}

	for lines := bufio.NewReader(cmd); ; {
		line, readErr := lines.ReadString('\n')
		if err := validateForEachRefLine(line, inObjectPool); err != nil {
			cancel()
			_ = cmd.Wait()
			return structerr.NewInvalidArgument("%w", err)
		}
		if readErr != nil {
			break
		}
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("listing references: %w, stderr: %q", err, stderr.String())
	}

	return nil
}

// validateForEachRefLine judges a "<name> <symbolic target>" line of git-for-each-ref(1). Only the ASCII
// space separates the fields: a name holds no ASCII space, but may hold other whitespace such as U+00A0.
func validateForEachRefLine(line string, inObjectPool bool) error {
	for name := range strings.SplitSeq(strings.TrimSuffix(line, "\n"), " ") {
		if err := git.ValidateRepositoryReferenceNameLength(name, inObjectPool); err != nil {
			return err
		}
	}

	return nil
}

func voteGitConfig(ctx context.Context, voteHash *voting.VoteHash, repo *localrepo.Repo) error {
	config, err := repo.GetConfig(ctx)
	if err != nil {
		return fmt.Errorf("get git config: %w", err)
	}

	_, _ = voteHash.Write(config)

	return nil
}

func voteReferences(ctx context.Context, voteHash *voting.VoteHash, repo *localrepo.Repo) error {
	stderr := &bytes.Buffer{}

	cmd, err := repo.Exec(ctx, gitcmd.Command{
		Name: "for-each-ref",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "--format=%(refname) %(objectname) %(symref)"},
			gitcmd.Flag{Name: "--include-root-refs"},
		},
	}, gitcmd.WithStdout(voteHash), gitcmd.WithStderr(stderr))
	if err != nil {
		return fmt.Errorf("spawning for-each-ref: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("running for-each-ref: %w, stderr: %q", err, stderr.String())
	}

	return nil
}

// performFullRepack performs a full repack and drops all unreachable objects.
func performFullRepack(ctx context.Context, repo *localrepo.Repo) (returnedErr error) {
	defer trace.StartRegion(ctx, "packObjects").End()

	if err := housekeeping.PerformRepack(ctx, repo,
		housekeepingcfg.RepackObjectsConfig{},
		// Do a full repack. By using `-a` instead of `-A` we will immediately discard unreachable
		// objects instead of exploding them into loose objects.
		gitcmd.Flag{Name: "-a"},
		// Don't include objects part of alternate.
		gitcmd.Flag{Name: "-l"},
		// Delete loose objects made redundant by this repack and redundant packfiles.
		gitcmd.Flag{Name: "-d"},
	); err != nil {
		return fmt.Errorf("perform repack: %w", err)
	}

	return nil
}
