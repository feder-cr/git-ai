package housekeeping

import (
	"context"
	"slices"
	"sync"

	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/housekeeping/config"
	"github.com/feder-cr/git-ai/internal/git/housekeeping/manager"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/git/stats"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/middleware"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// OperationThreshold defines when a specific operation should be triggered.
type OperationThreshold struct {
	// RPCInterval is the number of write RPCs after which this operation should potentially run.
	RPCInterval int
}

// MiddlewareConfig holds configuration for all housekeeping operations.
type MiddlewareConfig struct {
	// OperationThresholds maps each operation type to its triggering thresholds.
	OperationThresholds map[config.OperationType]OperationThreshold
	// DefaultThresholds provides a default threshold for operations which aren't explicitly configured.
	DefaultThresholds OperationThreshold
}

// DefaultMiddlewareConfig returns the default configuration with pack-refs running more frequently.
func DefaultMiddlewareConfig() MiddlewareConfig {
	return MiddlewareConfig{
		DefaultThresholds: OperationThreshold{
			RPCInterval: 20,
		},
		OperationThresholds: map[config.OperationType]OperationThreshold{
			config.OpRepackRefs: {
				RPCInterval: 10,
			},
			// Garbage collection reads every manifest to work out what is
			// reachable, so it runs less often than the operations that work on
			// the repository itself.
			config.OpCollectMVCCGarbage: {
				RPCInterval: 100,
			},
		},
	}
}

// activity tracks housekeeping activity for a specific relative path.
type activity struct {
	writeCount int
	active     bool
	// writeCountAtLastRun tracks the write count at which each operation last ran.
	writeCountAtLastRun map[config.OperationType]int
}

func newActivity() *activity {
	return &activity{
		writeCountAtLastRun: make(map[config.OperationType]int),
	}
}

type repoKey struct {
	storage, relativePath string
}

// Middleware manages scheduling of housekeeping tasks by intercepting gRPC requests.
type Middleware struct {
	config       MiddlewareConfig
	repoActivity map[repoKey]*activity

	mu sync.Mutex
	wg sync.WaitGroup

	logger           log.Logger
	registry         *protoregistry.Registry
	manager          manager.Manager
	localRepoFactory localrepo.Factory

	// shutdownCtx is cancelled by Stop to signal all in-flight housekeeping
	// workers that they should abort. Workers derive their context from it so
	// that shutting down the middleware cancels detached housekeeping work.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

// forceHousekeepingRPCs are all the RPCs that we should force housekeeping right after.
var forceHousekeepingRPCs = map[string]struct{}{
	gitalypb.CleanupService_RewriteHistory_FullMethodName: {},
}

// NewHousekeepingMiddleware returns a new middleware.
func NewHousekeepingMiddleware(logger log.Logger, registry *protoregistry.Registry, factory localrepo.Factory, manager manager.Manager, cfg MiddlewareConfig) *Middleware {
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	return &Middleware{
		config:           cfg,
		logger:           logger,
		registry:         registry,
		localRepoFactory: factory,
		manager:          manager,
		repoActivity:     make(map[repoKey]*activity),
		shutdownCtx:      shutdownCtx,
		shutdownCancel:   shutdownCancel,
	}
}

// getThresholds returns the thresholds for a given operation type.
func (m *Middleware) getThresholds(op config.OperationType) OperationThreshold {
	if thresholds, ok := m.config.OperationThresholds[op]; ok {
		return thresholds
	}
	return m.config.DefaultThresholds
}

// Stop cancels all in-flight housekeeping workers and waits for them to finish.
// Housekeeping workers are detached from the request context so that completing
// an RPC doesn't abort them, which means they would otherwise keep running until
// they finish naturally. During shutdown this can block process termination for
// minutes while a large repository is optimized, so Stop provides a cancellation
// path bounded by how quickly the workers react to context cancellation. The
// middleware must not be reused after Stop has been called.
func (m *Middleware) Stop() {
	m.shutdownCancel()
	m.wg.Wait()
}

// UnaryServerInterceptor returns gRPC unary middleware.
func (m *Middleware) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		methodInfo, err := m.registry.LookupMethod(info.FullMethod)
		if err != nil {
			// The only error that occurs here is the failure to find the method in the registry, which
			// happens for health checks and other implicit RPCs not registered with protoregistry.Registry.
			return handler(ctx, req)
		}

		// Housekeeping works on repositories, so there is nothing to do for storage-scoped
		// RPCs, which carry no target repository.
		if methodInfo.Scope != protoregistry.ScopeRepository {
			return handler(ctx, req)
		}

		targetRepo, err := methodInfo.TargetRepo(req.(proto.Message))
		if err != nil {
			m.logger.WithError(err).ErrorContext(ctx, "lookup target repository for housekeeping")
			return handler(ctx, req)
		}

		key := m.getRepoKey(targetRepo)

		switch methodInfo.Operation {
		case protoregistry.OpMaintenance:
			m.mu.Lock()

			if m.isActive(key) {
				m.mu.Unlock()
				return nil, structerr.NewAlreadyExists("housekeeping already executing for repository")
			}
			m.markHousekeepingActive(key)

			m.mu.Unlock()

			resp, err := handler(ctx, req)

			m.markHousekeepingInactive(key)

			return resp, err
		case protoregistry.OpMutator:
			// Execute the handler first so that housekeeping incorporates the latest writes. We also ensure that
			// the scheduling logic doesn't run for invalid requests.
			resp, err := handler(ctx, req)
			// No point in housekeeping if a mutator request was invalid, rate-limited, or unauthorised.
			// For the rest of the errors, we allow housekeeping execution.
			if err != nil {
				if st, ok := status.FromError(err); ok {
					if slices.Index([]codes.Code{
						codes.FailedPrecondition,
						codes.ResourceExhausted,
						codes.PermissionDenied,
					}, st.Code()) >= 0 {
						return resp, err
					}
				}
			}

			// No point in housekeeping a deleted repo.
			if info.FullMethod == gitalypb.RepositoryService_RemoveRepository_FullMethodName {
				return resp, err
			}

			_, forceHousekeeping := forceHousekeepingRPCs[methodInfo.FullMethodName()]
			m.scheduleHousekeeping(ctx, targetRepo, forceHousekeeping)

			return resp, err
		}

		return handler(ctx, req)
	}
}

// StreamServerInterceptor returns gRPC stream request middleware.
func (m *Middleware) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		methodInfo, err := m.registry.LookupMethod(info.FullMethod)
		if err != nil {
			// The only error that occurs here is the failure to find the method in the registry, which
			// happens for health checks and other implicit RPCs not registered with protoregistry.Registry.
			return handler(srv, ss)
		}

		// Housekeeping works on repositories, so there is nothing to do for storage-scoped
		// RPCs, which carry no target repository.
		if methodInfo.Scope != protoregistry.ScopeRepository {
			return handler(srv, ss)
		}

		req := methodInfo.NewRequest()
		if err := ss.RecvMsg(req); err != nil {
			// A client can send an invalid request at will, which is not an error of the server.
			logger := m.logger.WithError(err)
			if status.Code(err) == codes.InvalidArgument {
				logger.InfoContext(ss.Context(), "lookup target repository for housekeeping")
			} else {
				logger.ErrorContext(ss.Context(), "lookup target repository for housekeeping")
			}
			return handler(srv, middleware.NewPeekedStream(ss.Context(), nil, err, ss))
		}

		targetRepo, err := methodInfo.TargetRepo(req)
		if err != nil {
			return handler(srv, middleware.NewPeekedStream(ss.Context(), req, nil, ss))
		}

		key := m.getRepoKey(targetRepo)

		switch methodInfo.Operation {
		case protoregistry.OpMaintenance:
			m.mu.Lock()
			if m.isActive(key) {
				m.mu.Unlock()
				return structerr.NewAlreadyExists("housekeeping already executing for repository")
			}

			m.markHousekeepingActive(key)
			m.mu.Unlock()

			// Ensure that the first message we consumed earlier is relayed to the client.
			err = handler(srv, middleware.NewPeekedStream(ss.Context(), req, nil, ss))

			m.markHousekeepingInactive(key)

			return err
		case protoregistry.OpMutator:
			// Execute the handler first so that housekeeping incorporates the latest writes. We also ensure that
			// the scheduling logic doesn't run for invalid requests.
			if err := handler(srv, middleware.NewPeekedStream(ss.Context(), req, nil, ss)); err != nil {
				return err
			}

			_, forceHousekeeping := forceHousekeepingRPCs[methodInfo.FullMethodName()]
			m.scheduleHousekeeping(ss.Context(), targetRepo, forceHousekeeping)
			return nil
		}

		// Ensure that the first message we consumed earlier is relayed to the client.
		return handler(srv, middleware.NewPeekedStream(ss.Context(), req, nil, ss))
	}
}

func (m *Middleware) markHousekeepingActive(key repoKey) {
	a, ok := m.repoActivity[key]
	if !ok {
		a = newActivity()
		m.repoActivity[key] = a
	}

	a.active = true
}

func (m *Middleware) markHousekeepingInactive(key repoKey) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.repoActivity[key]
	if !ok {
		a = newActivity()
		m.repoActivity[key] = a
	}

	a.active = false
}

func (m *Middleware) isActive(key repoKey) bool {
	a, ok := m.repoActivity[key]
	if !ok {
		return false
	}

	return a.active
}

// allOperations contains all known operation types.
var allOperations = []config.OperationType{
	config.OpRepackRefs, config.OpRepackObjects, config.OpPruneObjects, config.OpWriteCommitGraph,
	config.OpCollectMVCCGarbage,
}

// pendingOperations returns operations that have exceeded their RPC interval thresholds.
func (m *Middleware) pendingOperations(a *activity, force bool) []config.OperationType {
	var pending []config.OperationType

	for _, op := range allOperations {
		thresholds := m.getThresholds(op)
		lastRun := a.writeCountAtLastRun[op]
		writesSinceLastRun := a.writeCount - lastRun

		if force || writesSinceLastRun > thresholds.RPCInterval {
			pending = append(pending, op)
		}
	}

	return pending
}

func (m *Middleware) scheduleHousekeeping(ctx context.Context, repo *gitalypb.Repository, force bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := m.getRepoKey(repo)

	a, ok := m.repoActivity[key]
	if !ok {
		a = newActivity()
		m.repoActivity[key] = a
	}
	a.writeCount++

	if a.active {
		return
	}

	pendingOps := m.pendingOperations(a, force)
	if len(pendingOps) == 0 {
		return
	}

	m.logger.WithFields(log.Fields{
		"forced":     force,
		"operations": pendingOps,
	}).InfoContext(ctx, "beginning scheduled housekeeping")

	// Mark that these operations are running at the current write count
	for _, op := range pendingOps {
		a.writeCountAtLastRun[op] = a.writeCount
	}

	m.markHousekeepingActive(key)

	m.wg.Add(1)
	go func() {
		// We need to call OptimizeRepository with a child context that's disowned from the parent's
		// cancellation signals we're executing it asynchronously. Providing the existing `ctx` would
		// cause it to fail, since `ctx` would be cancelled when this request completes. We still want
		// to be able to abort the worker when the middleware shuts down though, so we propagate
		// cancellation from the middleware's shutdown context.
		// The MVCC cache is stripped as well: if the caller ever ends up with one on
		// its context, this worker must not reuse it. It outlives the RPC that owns
		// that cache, and it should optimize whichever manifest is current when it
		// runs rather than the one the RPC pinned.
		housekeepingCtx, housekeepingCancel := context.WithCancel(mvcc.ContextWithoutCache(context.WithoutCancel(ctx)))
		stopShutdownPropagation := context.AfterFunc(m.shutdownCtx, housekeepingCancel)

		defer func() {
			stopShutdownPropagation()
			m.markHousekeepingInactive(key)
			m.logger.InfoContext(housekeepingCtx, "ended scheduled housekeeping")
			housekeepingCancel()
			m.wg.Done()
		}()

		localRepo := m.localRepoFactory.Build(repo)
		if err := m.manager.OptimizeRepository(housekeepingCtx, localRepo,
			manager.WithOptimizationStrategyConstructor(
				func(info stats.RepositoryInfo) housekeeping.OptimizationStrategy {
					return housekeeping.NewSelectiveOptimizationStrategy(info, pendingOps)
				},
			),
		); err != nil {
			m.logger.WithError(err).ErrorContext(housekeepingCtx, "failed scheduled housekeeping")
		}

		// Garbage collection runs outside OptimizeRepository, which holds the MVCC
		// repository lock while it optimizes and would lock collection out. It runs
		// once the optimization is done, so that a manifest the optimization published
		// counts towards reachability. Nothing else about it depends on the
		// optimization: what gets removed is decided by the retention policy and by
		// reachability against what is on disk.
		if slices.Contains(pendingOps, config.OpCollectMVCCGarbage) {
			if err := m.manager.RunMVCCGarbageCollection(housekeepingCtx, localRepo, housekeeping.DefaultMVCCConfig()); err != nil {
				m.logger.WithError(err).ErrorContext(housekeepingCtx, "failed collecting MVCC garbage")
			}
		}
	}()
}

func (m *Middleware) getRepoKey(repo *gitalypb.Repository) repoKey {
	return repoKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath()}
}
