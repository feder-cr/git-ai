package operations

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

//nolint:revive // This is unintentionally missing documentation.
type Server struct {
	gitalypb.UnimplementedOperationServiceServer
	logger       log.Logger
	hookManager  hook.Manager
	txManager    transaction.Manager
	locator      storage.Locator
	conns        *client.Pool
	catfileCache catfile.Cache
	updater      *updateref.UpdaterWithHooks
	gitConfig    config.Git
	graft        config.Graft
	// graftPolicy holds the standalone access policy in force (doc/graft/PLAN12.md); graft.Policy
	// is the one of the startup and is not read.
	graftPolicy      *policystore.Store
	localRepoFactory localrepo.Factory
	// resolveCfg configures conflict resolution of UserLand.
	resolveCfg config.GraftResolver
	// proposalSweeper sweeps the proposals of a branch, one land at a time; the servers of the
	// process share proposals.ProcessSweeper.
	proposalSweeper *proposals.Sweeper
	// checkBatchBytes, when set, replaces the bound of the file data of one check request
	// (landCheckBatchBytes); only tests set it.
	checkBatchBytes int64
	// landCheckBatchHook, when set, runs in every check batch while it holds its slot, with the
	// context of the batch; only tests set it.
	landCheckBatchHook func(ctx context.Context)
	// verifyAfterWriteHook, when set, runs between the writing of a checkout and its read-back; only
	// tests set it, to let a deadline pass there.
	verifyAfterWriteHook func()
	// verifyTmpDir is where the checkouts of the verification are made; empty means the system's
	// temporary directory (TMPDIR). Tests set it to see what is left behind.
	verifyTmpDir string
	// landLaneHook, when set, runs in the lane when it applies the verdicts of a land, holding the
	// lane for as long as it runs; only tests set it, to keep a lane busy.
	landLaneHook func()
	// landInlineCallContext, when set, replaces context.WithTimeout for every inline call of the lane (land_inline.go); only tests set it.
	landInlineCallContext func(ctx context.Context, allowed time.Duration) (context.Context, context.CancelFunc)
	// landDeadlineContext, when set, replaces context.WithTimeout for the deadline of a land with resolve_conflicts (landResolving); only tests set it.
	landDeadlineContext func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc)
	// landResolveHook, when set, runs in every resolver call while it holds its resolve slot; only
	// tests set it.
	landResolveHook func()
	// landInlineOff is set when the resolver did not honour a request for its rules only (an older
	// graft-resolve): the lane then never resolves inline for the life of the process (land_inline.go).
	landInlineOff atomic.Bool
	// landFinalPassHook, when set, runs in every pass of the final pass of a land between its
	// pre-resolution and its lane, with the number of the pass; only tests set it.
	landFinalPassHook func(pass int)
	// verifyPool bounds the verifications of the process (PLAN10 15.9): the operator's command runs
	// on the tree of every land that needs it, out of the lane, whatever made the resolution. It is
	// set whenever verify.enabled is, with or without an AI resolver.
	verifyPool *resolver.VerifyPool
	// conflictResolver answers the resolve requests of UserLand. Nil when resolution is disabled,
	// or when the configured resolver cannot be built: then resolverErr says why, and every land
	// that needs the resolver fails closed with it (landResolveAt, landWithResolution).
	conflictResolver *resolver.Client
	resolverErr      error
	// landLanes batches the UserLand requests of a branch. Nil disables batching, and every
	// request then lands on its own. The lanes are shared by all servers of the process.
	landLanes *landBatchLanes
	// shutdownCtx is cancelled once the graceful shutdown period has elapsed. It ends the land
	// batches, which do not end with the request of any submitter.
	shutdownCtx context.Context
	// storageSync catches up the local repository from the storage core before a land is retried,
	// see doc/graft/STORAGE.md. It does nothing unless the storage core is authoritative.
	storageSync corestore.Freshener
	// graftStorage is the storage core. In its authoritative mode the land identities are looked
	// up in its database (doc/graft/PLAN11.md). Nil without a storage core.
	graftStorage *corestore.Core
	// testHook, when set by a test, is called at named points of a request.
	testHook func(point string)
}

// NewServer creates a new instance of a grpc OperationServiceServer
func NewServer(deps *service.Dependencies) *Server {
	resolveCfg := deps.GetCfg().Graft.Land.Resolver

	// The configuration is validated at startup (config.GraftResolver.validate), so the resolver
	// fails to build only when something changed since (a token file removed, say). A configured
	// resolver is then never treated as no resolver: resolverErr makes every land that needs it
	// fail closed. The resolver of the process is shared by every server (service.Dependencies);
	// a server without one builds its own.
	shared := deps.GetGraftResolver()
	if shared == nil {
		shared = resolver.NewShared(resolveCfg)
		if shared.Err != nil {
			deps.GetLogger().WithError(shared.Err).Error("graft: the conflict resolver of land cannot be built: lands that need it fail")
		}
	}
	conflictResolver, resolverErr, verifyPool := shared.Client, shared.Err, shared.Verify
	if conflictResolver != nil && resolverActorsUnmatchable(deps.GetCfg().Graft) {
		deps.GetLogger().Warn("graft: [graft.land.resolver] actors is set but no [[graft.agent]] is " +
			"configured, so callers carry no identity and no landing may resolve conflicts")
	}

	s := &Server{
		logger:           deps.GetLogger(),
		hookManager:      deps.GetHookManager(),
		txManager:        deps.GetTxManager(),
		locator:          deps.GetLocator(),
		conns:            deps.GetConnsPool(),
		catfileCache:     deps.GetCatfileCache(),
		updater:          deps.GetUpdaterWithHooks(),
		gitConfig:        deps.GetCfg().Git,
		graft:            deps.GetCfg().Graft,
		graftPolicy:      deps.GetGraftPolicy(),
		localRepoFactory: deps.GetRepositoryFactory(),
		resolveCfg:       resolveCfg,
		proposalSweeper:  proposals.ProcessSweeper,
		conflictResolver: conflictResolver,
		resolverErr:      resolverErr,
		verifyPool:       verifyPool,
		shutdownCtx:      deps.GetShutdownContext(),
		storageSync:      deps.GetGraftStorageSyncer(),
		graftStorage:     deps.GetGraftStorage(),
	}

	// The lanes may run the batches with the server, so they are taken after it is created.
	s.landLanes = newLandBatchLanes(s, deps.GetCfg().Graft, deps.GetGraftLandLanes(), deps.GetLogger())

	return s
}

// resolverActorsUnmatchable reports whether the actors of conflict resolution can match no caller.
// The actor is the authenticated identity, which only agent authentication sets: an agent name,
// or "shared" for callers of the shared token. Without [[graft.agent]] no caller has an identity,
// so a non-empty actor list, for example of GitLab user names, admits nobody.
func resolverActorsUnmatchable(cfg config.Graft) bool {
	return len(cfg.Land.Resolver.Actors) > 0 && len(cfg.Agents) == 0
}

func (s *Server) quarantinedRepo(ctx context.Context, repo *gitalypb.Repository) (*quarantine.Dir, *localrepo.Repo, func(), error) {
	quarantineDir, cleanup, err := quarantine.New(ctx, repo, s.logger, s.locator)
	if err != nil {
		return nil, nil, nil, structerr.NewInternal("creating object quarantine: %w", err)
	}

	quarantineRepo := s.localRepoFactory.Build(quarantineDir.QuarantinedRepo())
	return quarantineDir, quarantineRepo, cleanup, nil
}

// ensureStorageFresh catches up the local repository from the storage core. It does nothing unless
// the storage core is authoritative.
func (s *Server) ensureStorageFresh(ctx context.Context, repo *gitalypb.Repository) error {
	if s.storageSync == nil {
		return nil
	}
	return s.storageSync.EnsureFresh(ctx, repo)
}

// graftInForce returns the [graft] configuration with the standalone access policy that decides the
// policy checks of ctx: the snapshot ctx carries, which a land batch attempt gives its members
// (policystore.WithSnapshot), or else the policy the server holds now.
func (s *Server) graftInForce(ctx context.Context) config.Graft {
	graft := s.graft
	if snapshot, ok := policystore.SnapshotFrom(ctx); ok {
		graft.Policy = snapshot.Rules
	} else {
		graft.Policy = s.graftPolicy.Current().Rules
	}
	return graft
}
