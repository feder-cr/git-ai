package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/housekeeping/config"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/git/stats"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/tracing"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
	"gitlab.com/gitlab-org/labkit/correlation"
	"google.golang.org/grpc/codes"
)

// OptimizeRepositoryConfig is the configuration used by OptimizeRepository that is computed by
// applying all the OptimizeRepositoryOption modifiers.
type OptimizeRepositoryConfig struct {
	StrategyConstructor OptimizationStrategyConstructor
}

// OptimizeRepositoryOption is an option that can be passed to OptimizeRepository.
type OptimizeRepositoryOption func(cfg *OptimizeRepositoryConfig)

// OptimizationStrategyConstructor is a constructor for an OptimizationStrategy that is being
// informed by the passed-in RepositoryInfo.
type OptimizationStrategyConstructor func(stats.RepositoryInfo) housekeeping.OptimizationStrategy

// WithOptimizationStrategyConstructor changes the constructor for the optimization strategy.that is
// used to determine which parts of the repository will be optimized. By default the
// HeuristicalOptimizationStrategy is used.
func WithOptimizationStrategyConstructor(strategyConstructor OptimizationStrategyConstructor) OptimizeRepositoryOption {
	return func(cfg *OptimizeRepositoryConfig) {
		cfg.StrategyConstructor = strategyConstructor
	}
}

// OptimizeRepository performs optimizations on the repository. Whether optimizations are performed
// or not depends on a set of heuristics.
func (m *RepositoryManager) OptimizeRepository(
	ctx context.Context,
	repo *localrepo.Repo,
	opts ...OptimizeRepositoryOption,
) error {
	var cfg OptimizeRepositoryConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	span, ctx := tracing.StartSpanIfHasParent(ctx, "housekeeping.OptimizeRepository", nil)
	defer span.End()

	if err := m.maybeStartTransaction(ctx, repo, func(ctx context.Context, tx storage.Transaction, repo *localrepo.Repo) error {
		originalRepo := &gitalypb.Repository{
			StorageName:  repo.GetStorageName(),
			RelativePath: repo.GetRelativePath(),
		}
		if tx != nil {
			originalRepo = tx.OriginalRepository(originalRepo)
		}

		// tryRunningHousekeeping acquires a lock on the repository to prevent other concurrent housekeeping calls on the repository.
		// As we may be in a transaction, the repository's relative path may have been rewritten. We use the original unrewritten relative
		// path here to ensure we hit the same key regardless if we run in different transactions where the snapshot prefixes in the
		// relative paths may differ.
		ok, cleanup := m.repositoryStates.tryRunningHousekeeping(originalRepo)
		// If we didn't succeed to set the state to "running" because of a concurrent housekeeping run
		// we exit early.
		if !ok {
			return nil
		}
		defer cleanup()

		// A background commit-graph write (WriteCommitGraphIfIdle) may have started before
		// housekeeping. It holds the commit-graph chain lock, so the commit-graph step of
		// housekeeping would fail if it ran concurrently. Such writes are short, and none can
		// start anymore now that housekeeping is marked as running.
		if err := m.repositoryStates.waitForBackgroundWrite(ctx, originalRepo); err != nil {
			return fmt.Errorf("waiting for background commit-graph write: %w", err)
		}

		if m.optimizeFunc != nil {
			return m.withMVCCCache(ctx, repo, func(ctx context.Context) error {
				strategy, err := m.validate(ctx, repo, cfg)
				if err != nil {
					return err
				}
				return m.optimizeFunc(ctx, repo, strategy)
			})
		}

		if tx != nil {
			if m.proposalTTL > 0 {
				// A reference update here would bypass the transaction: the proposals then expire
				// only through the lands of their branch (PLAN10 14).
				m.logger.InfoContext(ctx, "graft: proposal pruning skipped: the repository is housekept in a transaction")
			}
			return m.optimizeRepositoryWithTransaction(ctx, repo, cfg)
		}

		return m.withMVCCCache(ctx, repo, func(ctx context.Context) error {
			err := m.optimizeRepository(ctx, repo, cfg)
			m.pruneGraftProposals(ctx, repo)
			return err
		})
	}); err != nil {
		return err
	}

	return nil
}

// withMVCCCache runs fn within an MVCC cache lifecycle for MVCC-backed
// repositories, so background housekeeping (which does not pass through the mvcc
// gRPC middleware) operates on the current manifest snapshot and publishes the
// repacked result as a new manifest. This wrapper hydrates the base snapshot
// beforehand and commits the pointer afterward.
//
// It is a passthrough (just runs fn) when:
//   - a cache is already active on the context. This happens for the
//     OptimizeRepository RPC, whose handler runs inside the mvcc middleware: the
//     middleware owns that lifecycle and its Commit, so we must not wrap again.
//     Housekeeping triggered by an arbitrary RPC does not end up here, because the
//     housekeeping middleware is registered as the outer interceptor and its
//     detached goroutine therefore never sees the mvcc cache;
//   - MVCC cache support was not configured (see WithMVCCCacheSupport); or
//   - the repository does not use the MVCC reference backend.
func (m *RepositoryManager) withMVCCCache(ctx context.Context, repo *localrepo.Repo, fn func(context.Context) error) error {
	if mvcc.CacheFromContext(ctx) != nil || m.cacheFactory == nil {
		return fn(ctx)
	}

	backend, err := repo.ReferenceBackend(ctx)
	if err != nil {
		return fmt.Errorf("detect reference backend: %w", err)
	}
	if backend.Name != git.ReferenceBackendMVCC.Name {
		return fn(ctx)
	}

	// read-write cache: housekeeping repacks and publishes a new manifest.
	cache, err := m.cacheFactory.NewCache(ctx, repo.GetStorageName(), repo.GetRelativePath(), false)
	if err != nil {
		return fmt.Errorf("new mvcc cache: %w", err)
	}
	cache.Prepare(ctx)
	defer cache.Cleanup()

	// Wait for the cache to be hydrated before running the optimization. On the
	// RPC path readiness is awaited lazily by the first Git command, but an
	// optimization run may spawn no Git command at all (the strategy can decide
	// nothing needs to be done), in which case Commit would race with the
	// asynchronous preparation. Housekeeping gains nothing from the asynchronous
	// warm-up anyway, so wait here and fail early with a clear error.
	if err := cache.WaitUntilReady(ctx); err != nil {
		return fmt.Errorf("prepare mvcc cache: %w", err)
	}

	ctx = mvcc.ContextWithCache(ctx, cache)
	ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendMVCC)

	if err := fn(ctx); err != nil {
		return err
	}

	return cache.Commit(ctx)
}

func (m *RepositoryManager) maybeStartTransaction(ctx context.Context, repo *localrepo.Repo, run func(context.Context, storage.Transaction, *localrepo.Repo) error) error {
	if m.node == nil {
		return run(ctx, nil, repo)
	}

	return m.runInTransaction(ctx, "housekeeping/heuristics", true, repo, run)
}

func (m *RepositoryManager) runInTransaction(ctx context.Context, transactionName string, readOnly bool, repo *localrepo.Repo, run func(context.Context, storage.Transaction, *localrepo.Repo) error) (returnedErr error) {
	originalRepo := &gitalypb.Repository{
		StorageName:  repo.GetStorageName(),
		RelativePath: repo.GetRelativePath(),
	}
	if tx := storage.ExtractTransaction(ctx); tx != nil {
		originalRepo = tx.OriginalRepository(originalRepo)
	}

	storageHandle, err := m.node.GetStorage(repo.GetStorageName())
	if err != nil {
		return fmt.Errorf("get storage: %w", err)
	}

	tx, err := storageHandle.Begin(ctx, storage.TransactionOptions{
		ReadOnly:     readOnly,
		RelativePath: originalRepo.GetRelativePath(),
	})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	if err := run(
		storage.ContextWithTransaction(ctx, tx),
		tx,
		localrepo.NewFrom(repo, tx.RewriteRepository(originalRepo)),
	); err != nil {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: %w", rollbackErr))
		}

		return fmt.Errorf("run: %w", err)
	}

	commitLSN, err := tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// No need to log the read-only transaction gathering the heuristics as
	// we're only interested in logging write transactions to identify conflicts.
	if !readOnly {
		storage.LogTransactionCommit(ctx, m.logger, commitLSN, transactionName)
	}

	return nil
}

func (m *RepositoryManager) validate(
	ctx context.Context,
	repo *localrepo.Repo,
	cfg OptimizeRepositoryConfig,
) (housekeeping.OptimizationStrategy, error) {
	repositoryInfo, err := stats.RepositoryInfoForRepository(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("deriving repository info: %w", err)
	}

	repositoryInfo.Log(ctx, m.logger)
	m.metrics.ReportRepositoryInfo(repositoryInfo)

	var strategy housekeeping.OptimizationStrategy
	if cfg.StrategyConstructor == nil {
		strategy = housekeeping.NewHeuristicalOptimizationStrategy(repositoryInfo)
	} else {
		strategy = cfg.StrategyConstructor(repositoryInfo)
	}

	return strategy, nil
}

func (m *RepositoryManager) optimizeRepository(
	ctx context.Context,
	repo *localrepo.Repo,
	cfg OptimizeRepositoryConfig,
) error {
	strategy, err := m.validate(ctx, repo, cfg)
	if err != nil {
		return err
	}

	var cleanStaleDataTime, cleanWorktreesTime, repackTime, packRefsTime,
		pruneTime, commitGraphTime time.Duration

	finishTotalTimer := m.metrics.ReportTaskLatency("total", "apply")
	totalStatus := "failure"

	optimizations := make(map[string]string)
	defer func() {
		totalTime := finishTotalTimer()

		m.logger.WithFields(logrus.Fields{
			"correlation_id":        correlation.ExtractFromContext(ctx),
			"optimizations":         optimizations,
			"total_time":            totalTime,
			"clean_stale_data_time": cleanStaleDataTime,
			"clean_work_trees_time": cleanWorktreesTime,
			"repack_time":           repackTime,
			"pack_refs_time":        packRefsTime,
			"prune_time":            pruneTime,
			"commit_graph_time":     commitGraphTime,
			"storage_name":          repo.Repository.GetStorageName(),
			"relative_path":         repo.Repository.GetRelativePath(),
			"gl_project_path":       repo.Repository.GetGlProjectPath(),
			"gl_repo":               repo.Repository.GetGlRepository(),
		}).Info("optimized repository")

		for task, status := range optimizations {
			m.metrics.TasksTotal.WithLabelValues(task, status).Inc()
		}

		m.metrics.TasksTotal.WithLabelValues("total", totalStatus).Add(1)
	}()

	cleanupCfg := housekeeping.DefaultStaleDataCleanup()

	finishTimer := m.metrics.ReportTaskLatency("clean-stale-data", "apply")
	if err := m.CleanStaleData(ctx, repo, cleanupCfg); err != nil {
		return fmt.Errorf("could not execute housekeeping: %w", err)
	}
	cleanStaleDataTime = finishTimer()

	finishTimer = m.metrics.ReportTaskLatency("clean-worktrees", "apply")
	if err := housekeeping.CleanupWorktrees(ctx, repo); err != nil {
		return fmt.Errorf("could not clean up worktrees: %w", err)
	}
	cleanWorktreesTime = finishTimer()

	finishTimer = m.metrics.ReportTaskLatency("repack", "apply")
	didRepack, repackCfg, err := repackIfNeeded(ctx, repo, strategy)
	if err != nil {
		optimizations["packed_objects_"+string(repackCfg.Strategy)] = "failure"
		if repackCfg.WriteBitmap {
			optimizations["written_bitmap"] = "failure"
		}
		if repackCfg.WriteMultiPackIndex {
			optimizations["written_multi_pack_index"] = "failure"
		}

		return fmt.Errorf("could not repack: %w", err)
	}
	if didRepack {
		optimizations["packed_objects_"+string(repackCfg.Strategy)] = "success"

		if repackCfg.WriteBitmap {
			optimizations["written_bitmap"] = "success"
		}
		if repackCfg.WriteMultiPackIndex {
			optimizations["written_multi_pack_index"] = "success"
		}
	}
	repackTime = finishTimer()

	finishTimer = m.metrics.ReportTaskLatency("prune", "apply")
	didPrune, err := pruneIfNeeded(ctx, repo, strategy)
	if err != nil {
		optimizations["pruned_objects"] = "failure"
		return fmt.Errorf("could not prune: %w", err)
	} else if didPrune {
		optimizations["pruned_objects"] = "success"
	}
	pruneTime = finishTimer()

	finishTimer = m.metrics.ReportTaskLatency("pack-refs", "apply")
	didPackRefs, err := m.packRefsIfNeeded(ctx, repo, strategy)
	switch {
	case errors.Is(err, errReftableCompactionContended):
		// Benign: the stack stays consistent and the next housekeeping compacts or cleans what
		// this one could not, as git's own auto-compaction assumes. The rest still runs.
		optimizations["packed_refs"] = "skipped"
		m.logger.WithError(err).WarnContext(ctx, "skipped packing references: reftable stack locked or modified concurrently")
	case err != nil:
		optimizations["packed_refs"] = "failure"
		return fmt.Errorf("could not pack refs: %w", err)
	case didPackRefs:
		optimizations["packed_refs"] = "success"
	}
	packRefsTime = finishTimer()

	finishTimer = m.metrics.ReportTaskLatency("commit-graph", "apply")
	if didWriteCommitGraph, writeCommitGraphCfg, err := writeCommitGraphIfNeeded(ctx, repo, strategy); err != nil {
		optimizations["written_commit_graph_full"] = "failure"
		optimizations["written_commit_graph_incremental"] = "failure"
		return fmt.Errorf("could not write commit-graph: %w", err)
	} else if didWriteCommitGraph {
		if writeCommitGraphCfg.ReplaceChain {
			optimizations["written_commit_graph_full"] = "success"
		} else {
			optimizations["written_commit_graph_incremental"] = "success"
		}
	}
	commitGraphTime = finishTimer()

	totalStatus = "success"

	return nil
}

// optimizeRepositoryWithTransaction performs optimizations in the context of WAL transaction.
//
// Reference repacking and object repacking are run in two different transactions. This decreases the chance of conflicts as it
// allow reference repacking to commit faster. Reference repacking conflicts with reference deletions but runs relatively fast.
// Object repacking is slower but is conflict-free if no pruning is done.
//
// Note that the strategy is selected in a parent transaction. The repository's state may change in the meanwhile but this shouldn't
// really change things too much. RepositoryManager itself prevents concurrent housekeeping. Even if there was a housekeeping operation
// committed in between, we'd just do redundant repacks.
func (m *RepositoryManager) optimizeRepositoryWithTransaction(
	ctx context.Context,
	repo *localrepo.Repo,
	cfg OptimizeRepositoryConfig,
) error {
	strategy, err := m.validate(ctx, repo, cfg)
	if err != nil {
		return err
	}

	repackNeeded, repackCfg := strategy.ShouldRepackObjects(ctx)
	packRefsNeeded := strategy.ShouldRepackReferences(ctx)

	writeCommitGraphNeeded, writeCommitGraphCfg, err := strategy.ShouldWriteCommitGraph(ctx)
	if err != nil {
		return fmt.Errorf("checking commit graph writing eligibility: %w", err)
	}

	var errPackReferences error
	if packRefsNeeded {
		if err := m.runInTransaction(ctx, "housekeeping/pack-refs", false, repo, func(ctx context.Context, tx storage.Transaction, repo *localrepo.Repo) error {
			tx.PackRefs()
			return nil
		}); err != nil {
			errPackReferences = fmt.Errorf("run reference packing: %w", err)
		}
	}

	var errRepackObjects error
	if repackNeeded || writeCommitGraphNeeded {
		if err := m.runInTransaction(ctx, "housekeeping/pack-objects", false, repo, func(ctx context.Context, tx storage.Transaction, repo *localrepo.Repo) error {
			if repackNeeded {
				tx.Repack(repackCfg)
			}

			if writeCommitGraphNeeded {
				tx.WriteCommitGraphs(writeCommitGraphCfg)
			}
			return nil
		}); err != nil {
			errRepackObjects = fmt.Errorf("run object repacking: %w", err)
		}
	}

	getStatus := func(err error) string {
		if err != nil {
			return "failure"
		}

		return "success"
	}

	repackObjectsStatus := getStatus(errRepackObjects)

	optimizations := make(map[string]string)
	if repackNeeded {
		optimizations["packed_objects_"+string(repackCfg.Strategy)] = repackObjectsStatus
		if repackCfg.WriteBitmap {
			optimizations["written_bitmap"] = repackObjectsStatus
		}
		if repackCfg.WriteMultiPackIndex {
			optimizations["written_multi_pack_index"] = repackObjectsStatus
		}
	}

	if packRefsNeeded {
		optimizations["packed_refs"] = getStatus(errPackReferences)
	}

	if writeCommitGraphNeeded {
		if writeCommitGraphCfg.ReplaceChain {
			optimizations["written_commit_graph_full"] = repackObjectsStatus
		} else {
			optimizations["written_commit_graph_incremental"] = repackObjectsStatus
		}
	}

	m.logger.WithFields(logrus.Fields{
		"correlation_id": correlation.ExtractFromContext(ctx),
		"optimizations":  optimizations,
		"storage_name":   repo.Repository.GetStorageName(),
		"relative_path":  repo.Repository.GetRelativePath(),
	}).Info("optimized repository with WAL")
	for task, status := range optimizations {
		m.metrics.TasksTotal.WithLabelValues(task, status).Inc()
	}

	errCombined := errors.Join(errPackReferences, errRepackObjects)
	m.metrics.TasksTotal.WithLabelValues("total", getStatus(errCombined)).Add(1)

	return errCombined
}

// repackIfNeeded repacks the repository according to the strategy.
func repackIfNeeded(ctx context.Context, repo *localrepo.Repo, strategy housekeeping.OptimizationStrategy) (bool, config.RepackObjectsConfig, error) {
	repackNeeded, cfg := strategy.ShouldRepackObjects(ctx)
	if !repackNeeded {
		return false, config.RepackObjectsConfig{}, nil
	}

	if err := housekeeping.RepackObjects(ctx, repo, cfg); err != nil {
		return false, cfg, err
	}

	return true, cfg, nil
}

// writeCommitGraphIfNeeded writes the commit-graph if required.
func writeCommitGraphIfNeeded(ctx context.Context, repo *localrepo.Repo, strategy housekeeping.OptimizationStrategy) (bool, config.WriteCommitGraphConfig, error) {
	needed, cfg, err := strategy.ShouldWriteCommitGraph(ctx)
	if !needed || err != nil {
		return false, config.WriteCommitGraphConfig{}, err
	}

	if err := housekeeping.WriteCommitGraph(ctx, repo, cfg); err != nil {
		return true, cfg, fmt.Errorf("writing commit-graph: %w", err)
	}

	return true, cfg, nil
}

// pruneIfNeeded removes objects from the repository which are either unreachable or which are
// already part of a packfile. We use a grace period of two weeks.
func pruneIfNeeded(ctx context.Context, repo *localrepo.Repo, strategy housekeeping.OptimizationStrategy) (bool, error) {
	needed, cfg := strategy.ShouldPruneObjects(ctx)
	if !needed {
		return false, nil
	}

	if err := housekeeping.PruneObjects(ctx, repo, cfg); err != nil {
		return true, fmt.Errorf("pruning objects: %w", err)
	}

	return true, nil
}

func (m *RepositoryManager) packRefsIfNeeded(ctx context.Context, repo *localrepo.Repo, strategy housekeeping.OptimizationStrategy) (bool, error) {
	backend, err := repo.ReferenceBackend(ctx)
	if err != nil {
		return false, fmt.Errorf("detect reference backend: %w", err)
	}

	// Never repack references of MVCC repositories as reftable compaction will automatically
	// take place during writes.
	if backend.Name == git.ReferenceBackendMVCC.Name {
		return false, nil
	}

	if !strategy.ShouldRepackReferences(ctx) {
		return false, nil
	}

	var stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx, gitcmd.Command{
		Name: "pack-refs",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "--auto"},
			gitcmd.Flag{Name: "--all"},
		},
	}, gitcmd.WithStderr(&stderr)); err != nil {
		err = fmt.Errorf("packing refs: %w, stderr: %q", err, stderr.String())
		if backend.Name == git.ReferenceBackendReftables.Name && isReftableContention(err, stderr.String()) {
			return false, fmt.Errorf("%w: %w", errReftableCompactionContended, err)
		}
		return false, err
	}

	return true, nil
}

// errReftableCompactionContended is returned by packRefsIfNeeded when git-pack-refs(1) failed on a
// reftable repository only because a concurrent writer held the stack's lock or changed the stack.
var errReftableCompactionContended = errors.New("reftable compaction contended")

// Exit codes and stderr of git-pack-refs(1) on a reftable repository when the stack is contended.
// They are git internals, read from git 2.56.0-rc1 (3bbf434) and 2.55.0-rc1 (1ea786d), the bundled
// builds, and pinned by TestPackRefsIfNeeded_reftableContention against the real git:
//
//   - reftable_be_optimize (refs/reftable-backend.c:1699) returns what reftable_stack_clean
//     (reftable/stack.c:1845) returns as is, and git exits with it modulo 256 and an empty stderr:
//     REFTABLE_LOCK_ERROR (-5) when tables.list.lock is held, REFTABLE_OUTDATED_ERROR (-12) when
//     the stack changed under it (reftable/reftable-error.h).
//   - When the compaction itself fails, reftable_be_optimize reports it with error(), exiting 255,
//     and "unable to compact stack: <reftable_error_str>", whose texts for these two errors come
//     from reftable/error.c.
//
// git's own auto-compaction treats both errors as benign (reftable/stack.c:833). A lock left by a
// crashed git is no reason to retry here either: CleanStaleData removes tables.list.lock once it is
// older than housekeeping.ReftableLockfileGracePeriod, before the next pack-refs runs.
const (
	reftableLockErrorExitCode     = 256 - 5
	reftableOutdatedErrorExitCode = 256 - 12
	gitErrorExitCode              = 255

	reftableCompactLockedStderr   = "error: unable to compact stack: data is locked\n"
	reftableCompactOutdatedStderr = "error: unable to compact stack: data concurrently modified\n"
)

// isReftableContention reports whether git-pack-refs(1) failed with err and stderr on a reftable
// repository only because the stack was locked or modified concurrently. Any other failure is a
// real error.
func isReftableContention(err error, stderr string) bool {
	exitCode, ok := command.ExitStatus(err)
	if !ok {
		return false
	}

	switch exitCode {
	case reftableLockErrorExitCode, reftableOutdatedErrorExitCode:
		return stderr == ""
	case gitErrorExitCode:
		return stderr == reftableCompactLockedStderr || stderr == reftableCompactOutdatedStderr
	default:
		return false
	}
}

// CleanStaleData removes any stale data in the repository as per the provided configuration.
func (m *RepositoryManager) CleanStaleData(ctx context.Context, repo *localrepo.Repo, cfg housekeeping.CleanStaleDataConfig) error {
	span, ctx := tracing.StartSpanIfHasParent(ctx, "housekeeping.CleanStaleData", nil)
	defer span.End()

	repoPath, err := repo.Path(ctx)
	if err != nil {
		m.logger.WithError(err).WarnContext(ctx, "housekeeping failed to get repo path")
		if structerr.GRPCCode(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("housekeeping failed to get repo path: %w", err)
	}

	staleDataByType := map[string]int{}
	defer func() {
		if len(staleDataByType) == 0 {
			return
		}

		logEntry := m.logger
		for staleDataType, count := range staleDataByType {
			logEntry = logEntry.WithField(fmt.Sprintf("stale_data.%s", staleDataType), count)
			m.metrics.PrunedFilesTotal.WithLabelValues(staleDataType).Add(float64(count))
		}
		logEntry.InfoContext(ctx, "removed files")
	}()

	var entriesToPrune []string
	for staleEntryType, staleFileFinder := range cfg.StaleFileFinders {
		staleEntries, err := staleFileFinder(ctx, repoPath)
		if err != nil {
			return fmt.Errorf("housekeeping failed to find %s: %w", staleEntryType, err)
		}

		entriesToPrune = append(entriesToPrune, staleEntries...)
		staleDataByType[staleEntryType] = len(staleEntries)
	}

	for _, path := range entriesToPrune {
		// Remove stale entry (file or directory) - os.RemoveAll handles both cases
		// by first calling Remove before going for recursive removal.
		if err := os.RemoveAll(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			staleDataByType["failures"]++
			m.logger.WithError(err).WithField("path", path).WarnContext(ctx, "unable to remove stale entry")
		}
	}

	for repoCleanupName, repoCleanupFn := range cfg.RepoCleanups {
		cleanupCount, err := repoCleanupFn(ctx, repo)
		staleDataByType[repoCleanupName] = cleanupCount
		if err != nil {
			return fmt.Errorf("housekeeping could not perform cleanup %s: %w", repoCleanupName, err)
		}
	}

	for repoCleanupName, repoCleanupFn := range cfg.RepoCleanupWithTxManagers {
		cleanupCount, err := repoCleanupFn(ctx, repo, m.txManager)
		staleDataByType[repoCleanupName] = cleanupCount
		if err != nil {
			return fmt.Errorf("housekeeping could not perform cleanup (with TxManager) %s: %w", repoCleanupName, err)
		}
	}

	return nil
}

// graftProposalPruneErrors counts the housekeeping passes whose pruning of the land's proposals
// failed; the pass itself does not fail for it.
var graftProposalPruneErrors = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_housekeeping_proposal_prune_errors_total",
	Help: "Housekeeping passes whose pruning of the proposal references of the land failed.",
})

// pruneGraftProposals prunes the proposals of the land on every branch (WithGraftProposalTTL). A
// failure is logged and counted, and fails nothing: the proposals are an aid.
func (m *RepositoryManager) pruneGraftProposals(ctx context.Context, repo *localrepo.Repo) {
	if m.proposalTTL <= 0 {
		return
	}
	n, err := proposals.Prune(ctx, repo, proposals.RefPrefix, m.proposalTTL, "", time.Now())
	if err != nil {
		graftProposalPruneErrors.Inc()
		m.logger.WithError(err).ErrorContext(ctx, "graft: pruning the proposals of the land failed")
		return
	}
	if n > 0 {
		m.logger.WithField("pruned", n).InfoContext(ctx, "graft: pruned the expired proposals of the land")
	}
}
