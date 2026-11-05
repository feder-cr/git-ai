package testserver

import (
	"context"
	"net"
	"os"
	"testing"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/backup"
	"github.com/feder-cr/git-ai/internal/bundleuri"
	"github.com/feder-cr/git-ai/internal/cache"
	"github.com/feder-cr/git-ai/internal/cgroups"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	housekeepingmgr "github.com/feder-cr/git-ai/internal/git/housekeeping/manager"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	gitmvcc "github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/server"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/relational"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/storagemgr/partition/migration"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gate"
	corestoresyncer "github.com/feder-cr/git-ai/internal/graft/corestore/syncer"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
	"github.com/feder-cr/git-ai/internal/graft/landlane"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/limithandler"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/mvcc"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/statushandler"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/limiter"
	"github.com/feder-cr/git-ai/internal/log"
	praefectconfig "github.com/feder-cr/git-ai/internal/praefect/config"
	"github.com/feder-cr/git-ai/internal/streamcache"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// RunGitalyServer starts gitaly server based on the provided cfg and returns a connection address.
// It accepts addition Registrar to register all required service instead of
// calling service.RegisterAll explicitly because it creates a circular dependency
// when the function is used in on of internal/gitaly/service/... packages.
func RunGitalyServer(tb testing.TB, cfg config.Cfg, registrar func(srv *grpc.Server, deps *service.Dependencies), opts ...GitalyServerOpt) string {
	return StartGitalyServer(tb, cfg, registrar, opts...).Address()
}

// StartGitalyServer creates and runs gitaly (and praefect as a proxy) server.
func StartGitalyServer(tb testing.TB, cfg config.Cfg, registrar func(srv *grpc.Server, deps *service.Dependencies), opts ...GitalyServerOpt) GitalyServer {
	gitalySrv, gitalyAddr, disablePraefect := runGitaly(tb, cfg, registrar, opts...)

	if !testhelper.IsPraefectEnabled() || disablePraefect {
		return GitalyServer{
			Server:   gitalySrv,
			shutdown: gitalySrv.Stop,
			address:  gitalyAddr,
		}
	}

	praefectServer := runPraefectProxy(tb, cfg, gitalyAddr)
	return GitalyServer{
		Server: gitalySrv,
		shutdown: func() {
			praefectServer.Shutdown()
			gitalySrv.Stop()
		},
		address: praefectServer.Address(),
	}
}

func runPraefectProxy(tb testing.TB, gitalyCfg config.Cfg, gitalyAddr string) PraefectServer {
	var virtualStorages []*praefectconfig.VirtualStorage
	for _, storage := range gitalyCfg.Storages {
		virtualStorages = append(virtualStorages, &praefectconfig.VirtualStorage{
			Name: storage.Name,
			Nodes: []*praefectconfig.Node{
				{
					Storage: storage.Name,
					Address: gitalyAddr,
					Token:   gitalyCfg.Auth.GetToken(),
				},
			},
		})
	}

	return StartPraefect(tb, praefectconfig.Config{
		SocketPath: testhelper.GetTemporaryGitalySocketFileName(tb),
		Auth: auth.Config{
			Token: gitalyCfg.Auth.GetToken(),
		},
		DB: testdb.GetConfig(tb, testdb.New(tb).Name),
		Failover: praefectconfig.Failover{
			Enabled:          true,
			ElectionStrategy: praefectconfig.ElectionStrategyLocal,
		},
		BackgroundVerification: praefectconfig.BackgroundVerification{
			// Some tests cases purposefully create bad metadata by deleting a repository off
			// the disk. If the background verifier is running, it could find these repositories
			// and remove the invalid metadata related to them. This can cause the test assertions
			// to fail. As the background verifier runs asynchronously and possibly changes state
			// during a test and issues unexpected RPCs, it's disabled generally for all tests.
			VerificationInterval: -1,
		},
		Replication: praefectconfig.DefaultReplicationConfig(),
		Logging: log.Config{
			Format: "json",
			Level:  "info",
		},
		VirtualStorages: virtualStorages,
	})
}

// GitalyServer is a helper that carries additional info and
// functionality about gitaly (+praefect) server.
type GitalyServer struct {
	Server   *grpc.Server
	shutdown func()
	address  string
}

// Shutdown terminates running gitaly (+praefect) server.
func (gs GitalyServer) Shutdown() {
	gs.shutdown()
}

// Address returns address of the running gitaly (or praefect) service.
func (gs GitalyServer) Address() string {
	return gs.address
}

// waitHealthy waits until the server hosted at address becomes healthy.
func waitHealthy(tb testing.TB, ctx context.Context, addr string, authToken string) {
	grpcOpts := []grpc.DialOption{
		client.UnaryInterceptor(),
		client.StreamInterceptor(),
	}
	if authToken != "" {
		grpcOpts = append(grpcOpts, grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(authToken)))
	}

	conn, err := client.New(ctx, addr, client.WithGrpcOptions(grpcOpts))
	require.NoError(tb, err)
	defer testhelper.MustClose(tb, conn)

	healthClient := healthpb.NewHealthClient(conn)

	resp, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true))
	require.NoError(tb, err)
	require.Equal(tb, healthpb.HealthCheckResponse_SERVING, resp.GetStatus(), "server not yet ready to serve")
}

func runGitaly(tb testing.TB, cfg config.Cfg, registrar func(srv *grpc.Server, deps *service.Dependencies), opts ...GitalyServerOpt) (*grpc.Server, string, bool) {
	tb.Helper()

	ctx := testhelper.Context(tb)

	var gsd gitalyServerDeps
	for _, opt := range opts {
		gsd = opt(gsd)
	}

	// We set up the structerr interceptors so that any error metadata that gets set via
	// `structerr.WithMetadata()` is not only logged, but also present in the error details.
	serverOpts := []server.Option{
		server.WithUnaryInterceptor(StructErrUnaryInterceptor),
		server.WithUnaryInterceptor(statushandler.AbortedErrorUnaryInterceptor),
		server.WithStreamInterceptor(StructErrStreamInterceptor),
		server.WithStreamInterceptor(statushandler.AbortedErrorStreamInterceptor),
	}

	deps := gsd.createDependencies(tb, ctx, cfg)
	tb.Cleanup(func() { testhelper.MustClose(tb, gsd.conns) })

	if gsd.interceptorsFn != nil {
		unary, stream := gsd.interceptorsFn(deps.GetLogger(), deps.GetRepositoryFactory())
		for _, v := range unary {
			serverOpts = append(serverOpts, server.WithUnaryInterceptor(v))
		}
		for _, v := range stream {
			serverOpts = append(serverOpts, server.WithStreamInterceptor(v))
		}
	}

	if gsd.graftStorageGate != nil {
		serverOpts = append(serverOpts,
			server.WithUnaryInterceptor(gsd.graftStorageGate.UnaryInterceptor()),
			server.WithStreamInterceptor(gsd.graftStorageGate.StreamInterceptor()),
		)
	}

	mvccInterceptor := mvcc.NewMvccInterceptor(gitmvcc.NewCacheFactory(cfg.RuntimeDir, deps.GetLocator()), deps.GetLogger(), protoregistry.GitalyProtoPreregistered, deps.GetLocator())
	serverFactory := server.NewGitalyServerFactory(
		cfg,
		gsd.logger.WithField("test", tb.Name()),
		deps.GetBackchannelRegistry(),
		deps.GetDiskCache(),
		[]*limithandler.LimiterMiddleware{deps.GetLimitHandler()},
		nil,
		server.TransactionMiddleware{},
		mvccInterceptor,
	)

	lc := net.ListenConfig{}
	if cfg.RuntimeDir != "" {
		internalServer, err := serverFactory.CreateInternal(serverOpts...)
		require.NoError(tb, err)

		registrar(internalServer, deps)
		registerHealthServerIfNotRegistered(internalServer)

		require.NoError(tb, os.MkdirAll(cfg.InternalSocketDir(), mode.Directory))
		tb.Cleanup(func() { require.NoError(tb, os.RemoveAll(cfg.InternalSocketDir())) })

		internalListener, err := lc.Listen(ctx, "unix", cfg.InternalSocketPath())
		require.NoError(tb, err)
		go func() {
			assert.NoError(tb, internalServer.Serve(internalListener), "failure to serve internal gRPC")
		}()

		waitHealthy(tb, ctx, "unix://"+internalListener.Addr().String(), cfg.Auth.GetToken())
	}

	secure := cfg.TLS.CertPath != "" && cfg.TLS.KeyPath != ""

	externalServer, err := serverFactory.CreateExternal(secure, serverOpts...)
	require.NoError(tb, err)

	tb.Cleanup(serverFactory.GracefulStop)

	registrar(externalServer, deps)
	registerHealthServerIfNotRegistered(externalServer)

	var listener net.Listener
	var addr string
	switch {
	case cfg.TLSListenAddr != "":
		listener, err = lc.Listen(ctx, "tcp", cfg.TLSListenAddr)
		require.NoError(tb, err)
		_, port, err := net.SplitHostPort(listener.Addr().String())
		require.NoError(tb, err)
		addr = "tls://localhost:" + port
	case cfg.ListenAddr != "":
		listener, err = lc.Listen(ctx, "tcp", cfg.ListenAddr)
		require.NoError(tb, err)
		addr = "tcp://" + listener.Addr().String()
	default:
		serverSocketPath := testhelper.GetTemporaryGitalySocketFileName(tb)
		listener, err = lc.Listen(ctx, "unix", serverSocketPath)
		require.NoError(tb, err)
		addr = "unix://" + serverSocketPath
	}

	go func() {
		assert.NoError(tb, externalServer.Serve(listener), "failure to serve external gRPC")
	}()

	waitHealthy(tb, ctx, addr, cfg.Auth.GetToken())

	return externalServer, addr, gsd.disablePraefect
}

func registerHealthServerIfNotRegistered(srv *grpc.Server) {
	if _, found := srv.GetServiceInfo()["grpc.health.v1.Health"]; !found {
		// we should register health service as it is used for the health checks
		// praefect service executes periodically (and on the bootstrap step)
		healthpb.RegisterHealthServer(srv, health.NewServer())
	}
}

type gitalyServerDeps struct {
	disablePraefect        bool
	logger                 log.Logger
	conns                  *client.Pool
	locator                storage.Locator
	txMgr                  transaction.Manager
	hookMgr                hook.Manager
	gitlabClient           gitlab.Client
	gitCmdFactory          gitcmd.CommandFactory
	cgroupsManager         cgroups.Manager
	backchannelReg         *backchannel.Registry
	catfileCache           catfile.Cache
	diskCache              cache.Cache
	packObjectsCache       streamcache.Cache
	packObjectsLimiter     limiter.Limiter
	limitHandler           *limithandler.LimiterMiddleware
	repositoryCounter      *counter.RepositoryCounter
	updaterWithHooks       *updateref.UpdaterWithHooks
	housekeepingManager    housekeepingmgr.Manager
	backupSink             *backup.Sink
	backupLocator          backup.Locator
	signingKey             string
	postReceiveRegistry    *hook.PostReceiveRegistry
	bundleURIManager       *bundleuri.GenerationManager
	bundleURISink          *bundleuri.Sink
	bundleURIStrategy      bundleuri.GenerationStrategy
	localRepoFactory       localrepo.Factory
	migrations             *[]migration.Migration
	archiveCache           streamcache.Cache
	MigrationStateManager  migration.StateManager
	interceptorsFn         InterceptorsFunc
	poolMetadataStore      relational.PoolStore
	objectPoolStateManager relational.ObjectPoolStateManager
	graftLandLanes         *landlane.Shared
	graftStorage           *corestore.Core
	graftStorageSyncer     *corestoresyncer.Syncer
	graftStorageGate       *gate.Gate
	graftPolicy            *policystore.Store
	hookMgrWrapper         func(hook.Manager) hook.Manager
	storageVoterObserver   func(*voter.Voter)
}

func (gsd *gitalyServerDeps) createDependencies(tb testing.TB, ctx context.Context, cfg config.Cfg) *service.Dependencies {
	if gsd.logger == nil {
		gsd.logger = testhelper.NewLogger(tb, testhelper.WithLoggerName("gitaly"))
	}

	if gsd.conns == nil {
		gsd.conns = client.NewPool(client.WithDialOptions(client.UnaryInterceptor(), client.StreamInterceptor()))
	}

	if gsd.locator == nil {
		gsd.locator = config.NewLocator(cfg)
	}

	if gsd.gitlabClient == nil {
		gsd.gitlabClient = gitlab.NewMockClient(
			tb, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive,
		)
	}

	if gsd.backchannelReg == nil {
		gsd.backchannelReg = backchannel.NewRegistry()
	}

	if gsd.txMgr == nil {
		gsd.txMgr = transaction.NewManager(cfg, gsd.logger, gsd.backchannelReg)
	}

	if gsd.cgroupsManager == nil {
		gsd.cgroupsManager = cgroups.NewManager(cfg.Cgroups, gsd.logger, os.Getpid())
	}

	if gsd.gitCmdFactory == nil {
		gsd.gitCmdFactory = gittest.NewCommandFactory(tb, cfg, gitcmd.WithCgroupsManager(gsd.cgroupsManager))
	}

	if gsd.postReceiveRegistry == nil && !gsd.disablePraefect {
		gsd.postReceiveRegistry = hook.NewPostReceiveRegistry()
	}

	if gsd.MigrationStateManager == nil {
		gsd.MigrationStateManager = migration.NewStateManager(gsd.migrations)
	}

	// This is to allow building a bundle generation from a Sink
	// without having to create one beforehand.
	// If bundleURIManager is defined though, we use that one.
	if gsd.bundleURIManager == nil && gsd.bundleURISink != nil {
		var strategy bundleuri.GenerationStrategy
		if gsd.bundleURIStrategy != nil {
			strategy = gsd.bundleURIStrategy
		} else {
			strategy = bundleuri.NewSimpleStrategy(true)
		}
		manager, err := bundleuri.NewGenerationManager(ctx, gsd.bundleURISink, gsd.logger, nil, strategy)
		require.NoError(tb, err)

		gsd.bundleURIManager = manager
	}

	// The Graft storage core is wired as in serve.go: the voter in the hook manager, the syncer in
	// the dependencies and, in authoritative mode, the gate interceptors (see runGitaly).
	// One store of the standalone access policy serves the hook manager and the services, as in
	// serve.go, so that a test that reloads it reaches every reader.
	if gsd.graftPolicy == nil {
		gsd.graftPolicy = policystore.New(cfg.Graft.Policy)
	}
	hookManagerOpts := []hook.ManagerOption{hook.WithPolicyStore(gsd.graftPolicy)}
	if gsd.graftStorage.Enabled() {
		if gsd.catfileCache == nil {
			cache := catfile.NewCache(cfg)
			gsd.catfileCache = cache
			tb.Cleanup(cache.Stop)
		}
		repoFactory := localrepo.NewFactory(gsd.logger, gsd.locator, gsd.gitCmdFactory, gsd.catfileCache)
		gsd.graftStorageSyncer = corestoresyncer.New(gsd.graftStorage, gsd.logger, gsd.locator, repoFactory, gsd.gitCmdFactory)
		storageVoter := voter.New(gsd.graftStorage, gsd.logger, repoFactory, gsd.locator, gsd.graftStorageSyncer)
		if gsd.storageVoterObserver != nil {
			gsd.storageVoterObserver(storageVoter)
		}
		hookManagerOpts = append(hookManagerOpts, hook.WithStorageVoter(storageVoter))
		if gsd.graftStorage.Authoritative() {
			gsd.graftStorageGate = gate.New(gsd.graftStorage, gsd.graftStorageSyncer, gsd.logger, gsd.locator, repoFactory, protoregistry.GitalyProtoPreregistered)
		}
	}

	if gsd.hookMgr == nil {
		gsd.hookMgr = hook.NewManager(
			cfg, gsd.locator,
			gsd.logger,
			gsd.gitCmdFactory,
			gsd.cgroupsManager,
			gsd.txMgr,
			gsd.gitlabClient,
			gsd.postReceiveRegistry,
			hookManagerOpts...,
		)
	}
	if gsd.hookMgrWrapper != nil {
		gsd.hookMgr = gsd.hookMgrWrapper(gsd.hookMgr)
	}

	if gsd.catfileCache == nil {
		cache := catfile.NewCache(cfg)
		gsd.catfileCache = cache
		tb.Cleanup(cache.Stop)
	}

	if gsd.diskCache == nil {
		gsd.diskCache = cache.New(cfg, gsd.locator, gsd.logger)
	}

	if gsd.packObjectsCache == nil {
		gsd.packObjectsCache = streamcache.New(cfg.PackObjectsCache, gsd.logger)
		tb.Cleanup(gsd.packObjectsCache.Stop)
	}

	if gsd.packObjectsLimiter == nil {
		gsd.packObjectsLimiter = limiter.NewConcurrencyLimiter(
			limiter.NewAdaptiveLimit("staticLimit", limiter.AdaptiveSetting{Initial: 0}),
			0,
			0,
			limiter.NewNoopConcurrencyMonitor(),
		)
	}

	if gsd.archiveCache == nil {
		gsd.archiveCache = streamcache.New(cfg.ArchiveCache, gsd.logger)
		tb.Cleanup(gsd.archiveCache.Stop)
	}

	if gsd.limitHandler == nil {
		_, setupPerRPCConcurrencyLimiters := limithandler.WithConcurrencyLimiters(cfg)
		gsd.limitHandler = limithandler.New(cfg, limithandler.LimitConcurrencyByRepo, setupPerRPCConcurrencyLimiters)
	}

	if gsd.repositoryCounter == nil {
		gsd.repositoryCounter = counter.NewRepositoryCounter(cfg.Storages)
	}

	if gsd.updaterWithHooks == nil {
		gsd.updaterWithHooks = updateref.NewUpdaterWithHooks(cfg, gsd.logger, gsd.locator, gsd.hookMgr, gsd.gitCmdFactory, gsd.catfileCache)
	}

	if gsd.housekeepingManager == nil {
		gsd.housekeepingManager = housekeepingmgr.New(cfg.Prometheus, gsd.logger, gsd.txMgr, nil)
	}

	if gsd.signingKey != "" {
		cfg.Git.SigningKey = gsd.signingKey
	}

	gsd.localRepoFactory = localrepo.NewFactory(gsd.logger, gsd.locator, gsd.gitCmdFactory, gsd.catfileCache)

	if gsd.graftLandLanes == nil {
		// As in serve.go, the servers of the internal and the external listener share the lanes. An
		// invalid configuration leaves them unset, and the operations service then disables batching
		// as it does for such a configuration.
		if laneCfg, err := landlane.ConfigFromGraft(cfg.Graft.Land.Batch); err == nil {
			gsd.graftLandLanes = landlane.NewShared(laneCfg)
			tb.Cleanup(gsd.graftLandLanes.Close)
		}
	}

	var storageSyncer corestore.Freshener
	if gsd.graftStorageSyncer != nil {
		storageSyncer = gsd.graftStorageSyncer
	}

	return &service.Dependencies{
		GraftStorage:           gsd.graftStorage,
		GraftStorageSyncer:     storageSyncer,
		Logger:                 gsd.logger,
		Cfg:                    cfg,
		ClientPool:             gsd.conns,
		StorageLocator:         gsd.locator,
		TransactionManager:     gsd.txMgr,
		GitalyHookManager:      gsd.hookMgr,
		GitCmdFactory:          gsd.gitCmdFactory,
		CgroupsManager:         gsd.cgroupsManager,
		BackchannelRegistry:    gsd.backchannelReg,
		GitlabClient:           gsd.gitlabClient,
		CatfileCache:           gsd.catfileCache,
		DiskCache:              gsd.diskCache,
		PackObjectsCache:       gsd.packObjectsCache,
		PackObjectsLimiter:     gsd.packObjectsLimiter,
		LimitHandler:           gsd.limitHandler,
		RepositoryCounter:      gsd.repositoryCounter,
		UpdaterWithHooks:       gsd.updaterWithHooks,
		HousekeepingManager:    gsd.housekeepingManager,
		BackupSink:             gsd.backupSink,
		BackupLocator:          gsd.backupLocator,
		PostReceiveRegistry:    gsd.postReceiveRegistry,
		BundleURIManager:       gsd.bundleURIManager,
		LocalRepositoryFactory: gsd.localRepoFactory,
		MigrationStateManager:  gsd.MigrationStateManager,
		ArchiveCache:           gsd.archiveCache,
		PoolMetadataStore:      gsd.poolMetadataStore,
		ObjectPoolStateManager: gsd.objectPoolStateManager,
		GraftLandLanes:         gsd.graftLandLanes,
		GraftPolicy:            gsd.graftPolicy,
	}
}

// GitalyServerOpt is a helper type to shorten declarations.
type GitalyServerOpt func(gitalyServerDeps) gitalyServerDeps

// WithLogger sets a log.Logger instance that will be used for gitaly services initialisation.
func WithLogger(logger log.Logger) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.logger = logger
		return deps
	}
}

// WithLocator sets a storage.Locator instance that will be used for gitaly services initialisation.
func WithLocator(locator storage.Locator) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.locator = locator
		return deps
	}
}

// WithGitCommandFactory sets a gitcmd.CommandFactory instance that will be used for gitaly services
// initialisation.
func WithGitCommandFactory(gitCmdFactory gitcmd.CommandFactory) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.gitCmdFactory = gitCmdFactory
		return deps
	}
}

// WithCgroupsManager sets the cgroups.Manager used for gitaly services initialisation.
func WithCgroupsManager(cgroupsManager cgroups.Manager) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.cgroupsManager = cgroupsManager
		return deps
	}
}

// WithGitLabClient sets gitlab.Client instance that will be used for gitaly services initialisation.
func WithGitLabClient(gitlabClient gitlab.Client) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.gitlabClient = gitlabClient
		return deps
	}
}

// WithGraftPolicy sets the store of the standalone access policy that the server's hook manager and
// the services read, so that a test can reload the policy while the server runs. Without it, the
// server has a store of the configuration's policy. A hook manager that WithHookManager sets keeps
// its own store, which this one does not reach.
func WithGraftPolicy(store *policystore.Store) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.graftPolicy = store
		return deps
	}
}

// WithHookManagerWrapper wraps the hook manager of the server, the one WithHookManager sets or the
// server's own, with wrap: the services and the updater with hooks call the wrapper, which a test
// uses to act at a point of the hooks.
func WithHookManagerWrapper(wrap func(hook.Manager) hook.Manager) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.hookMgrWrapper = wrap
		return deps
	}
}

// WithStorageVoterObserver gives fn the storage voter of the server (WithGraftStorage), so that a
// test can read what the voter keeps between the phases of a transaction.
func WithStorageVoterObserver(fn func(*voter.Voter)) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.storageVoterObserver = fn
		return deps
	}
}

// WithHookManager sets hook.Manager instance that will be used for gitaly services initialisation.
func WithHookManager(hookMgr hook.Manager) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.hookMgr = hookMgr
		return deps
	}
}

// WithTransactionManager sets transaction.Manager instance that will be used for gitaly services initialisation.
func WithTransactionManager(txMgr transaction.Manager) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.txMgr = txMgr
		return deps
	}
}

// WithDisablePraefect disables setup and usage of the praefect as a proxy before the gitaly service.
func WithDisablePraefect() GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.disablePraefect = true
		return deps
	}
}

// WithBackchannelRegistry sets backchannel.Registry instance that will be used for gitaly services initialisation.
func WithBackchannelRegistry(backchannelReg *backchannel.Registry) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.backchannelReg = backchannelReg
		return deps
	}
}

// WithDiskCache sets the cache.Cache instance that will be used for gitaly services initialisation.
func WithDiskCache(diskCache cache.Cache) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.diskCache = diskCache
		return deps
	}
}

// WithRepositoryCounter sets the counter.RepositoryCounter instance that will be used for gitaly services initialisation.
func WithRepositoryCounter(repositoryCounter *counter.RepositoryCounter) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.repositoryCounter = repositoryCounter
		return deps
	}
}

// WithPackObjectsLimiter sets the PackObjectsLimiter that will be
// used for gitaly services initialization.
func WithPackObjectsLimiter(limiter *limiter.ConcurrencyLimiter) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.packObjectsLimiter = limiter
		return deps
	}
}

// WithPackObjectsCache sets the PackObjectsCache that will be
// used for gitaly services initialization.
func WithPackObjectsCache(cache streamcache.Cache) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.packObjectsCache = cache
		return deps
	}
}

// WithHousekeepingManager sets the housekeeping.Manager that will be used for Gitaly services
// initialization.
func WithHousekeepingManager(manager housekeepingmgr.Manager) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.housekeepingManager = manager
		return deps
	}
}

// WithBackupSink sets the backup.Sink that will be used for Gitaly services
func WithBackupSink(backupSink *backup.Sink) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.backupSink = backupSink
		return deps
	}
}

// WithBackupLocator sets the backup.Locator that will be used for Gitaly services
func WithBackupLocator(backupLocator backup.Locator) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.backupLocator = backupLocator
		return deps
	}
}

// WithBundleURIStrategy sets the bundleuri.GenerationStrategy that will be used by the bundleuri.GenerationManager
// The default value is bundleuri.NewSimpleStrategy(true)
func WithBundleURIStrategy(strategy bundleuri.GenerationStrategy) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.bundleURIStrategy = strategy
		return deps
	}
}

// WithBundleURISink sets the *bundleuri.Sink that will be used by the bundleuri.GenerationManager
func WithBundleURISink(sink *bundleuri.Sink) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.bundleURISink = sink
		return deps
	}
}

// WithBundleURIManager sets the *bundleuri.GenerationManager that will be used by the bundleuri.GenerationManager
func WithBundleURIManager(manager *bundleuri.GenerationManager) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.bundleURIManager = manager
		return deps
	}
}

// WithSigningKey sets the signing key path that will be used for Gitaly
// services.
func WithSigningKey(signingKey string) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.signingKey = signingKey
		return deps
	}
}

// WithPostReceiveRegistry sets the post receive registry that will be used for Gitaly services.
func WithPostReceiveRegistry(registry *hook.PostReceiveRegistry) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.postReceiveRegistry = registry
		return deps
	}
}

// WithRepositoryFactory sets the localrepo.Factory that will be used for gitaly services initialisation.
func WithRepositoryFactory(repoFactory localrepo.Factory) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.localRepoFactory = repoFactory
		return deps
	}
}

// InterceptorsFunc builds additional gRPC interceptors for the test server.
type InterceptorsFunc func(log.Logger, localrepo.Factory) ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor)

// WithInterceptors allows for setting additional gRPC interceptors to the server via
// a callback function. These are added as server options.
func WithInterceptors(fn InterceptorsFunc) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.interceptorsFn = fn
		return deps
	}
}

// WithMigrations registers migration tasks to be executed before any transaction begins.
func WithMigrations(migrations *[]migration.Migration) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.migrations = migrations
		return deps
	}
}

// WithPoolMetadataStore sets the relational.PoolStore that will be used for gitaly services initialisation.
func WithPoolMetadataStore(store relational.PoolStore) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.poolMetadataStore = store
		return deps
	}
}

// WithObjectPoolStateManager sets the relational.ObjectPoolStateManager that will be used for gitaly services initialisation.
func WithObjectPoolStateManager(mgr relational.ObjectPoolStateManager) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.objectPoolStateManager = mgr
		return deps
	}
}

// WithGraftStorage runs the server with the Graft storage core, wired as in serve.go. The core is
// usually opened with corestore/coretest. A nil core leaves the storage core off.
func WithGraftStorage(core *corestore.Core) GitalyServerOpt {
	return func(deps gitalyServerDeps) gitalyServerDeps {
		deps.graftStorage = core
		return deps
	}
}
