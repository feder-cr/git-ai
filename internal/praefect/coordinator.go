package praefect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/grpc/metadata"
	"github.com/feder-cr/git-ai/internal/grpc/protoregistry"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/praefect/config"
	"github.com/feder-cr/git-ai/internal/praefect/datastore"
	"github.com/feder-cr/git-ai/internal/praefect/metrics"
	"github.com/feder-cr/git-ai/internal/praefect/nodes"
	"github.com/feder-cr/git-ai/internal/praefect/transactions"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/transaction/txinfo"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/proxy"
	"github.com/prometheus/client_golang/prometheus"
	"gitlab.com/gitlab-org/labkit/correlation"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	grpc_metadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	// VirtualStorageKey is used to set the virtual storage name
	// as an entry in a request's metadata
	VirtualStorageKey = "virtual_storage"
)

// ErrRepositoryReadOnly is returned when the repository is in read-only mode. This happens
// if the primary does not have the latest changes.
var ErrRepositoryReadOnly = structerr.NewFailedPrecondition("repository is in read-only mode")

type transactionsCondition func(context.Context) bool

func transactionsEnabled(context.Context) bool  { return true }
func transactionsDisabled(context.Context) bool { return false }

// transactionRPCs contains the list of repository-scoped mutating calls which may take part in
// transactions. An optional feature flag can be added to conditionally enable transactional
// behaviour. If none is given, it's always enabled.
var transactionRPCs = map[string]transactionsCondition{
	"/gitaly.CleanupService/ApplyBfgObjectMapStream":         transactionsEnabled,
	"/gitaly.CleanupService/RewriteHistory":                  transactionsEnabled,
	"/gitaly.ConflictsService/ResolveConflicts":              transactionsEnabled,
	"/gitaly.ObjectPoolService/DisconnectGitAlternates":      transactionsEnabled,
	"/gitaly.ObjectPoolService/FetchIntoObjectPool":          transactionsEnabled,
	"/gitaly.ObjectPoolService/LinkRepositoryToObjectPool":   transactionsEnabled,
	"/gitaly.OperationService/UserApplyPatch":                transactionsEnabled,
	"/gitaly.OperationService/UserCherryPick":                transactionsEnabled,
	"/gitaly.OperationService/UserCommitFiles":               transactionsEnabled,
	"/gitaly.OperationService/UserCreateBranch":              transactionsEnabled,
	"/gitaly.OperationService/UserCreateTag":                 transactionsEnabled,
	"/gitaly.OperationService/UserDeleteBranch":              transactionsEnabled,
	"/gitaly.OperationService/UserDeleteTag":                 transactionsEnabled,
	"/gitaly.OperationService/UserFFBranch":                  transactionsEnabled,
	"/gitaly.OperationService/UserLand":                      transactionsEnabled,
	"/gitaly.OperationService/UserMergeBranch":               transactionsEnabled,
	"/gitaly.OperationService/UserMergeToRef":                transactionsEnabled,
	"/gitaly.OperationService/UserRebaseToRef":               transactionsEnabled,
	"/gitaly.OperationService/UserRebaseConfirmable":         transactionsEnabled,
	"/gitaly.OperationService/UserRevert":                    transactionsEnabled,
	"/gitaly.OperationService/UserSquash":                    transactionsEnabled,
	"/gitaly.OperationService/UserUpdateBranch":              transactionsEnabled,
	"/gitaly.OperationService/UserUpdateSubmodule":           transactionsEnabled,
	"/gitaly.RefService/DeleteRefs":                          transactionsEnabled,
	"/gitaly.RefService/UpdateReferences":                    transactionsEnabled,
	"/gitaly.RepositoryService/CreateFork":                   transactionsEnabled,
	"/gitaly.RepositoryService/CreateRepository":             transactionsEnabled,
	"/gitaly.RepositoryService/CreateRepositoryFromBundle":   transactionsEnabled,
	"/gitaly.RepositoryService/CreateRepositoryFromSnapshot": transactionsEnabled,
	"/gitaly.RepositoryService/CreateRepositoryFromURL":      transactionsEnabled,
	"/gitaly.RepositoryService/FetchBundle":                  transactionsEnabled,
	"/gitaly.RepositoryService/FetchRemote":                  transactionsEnabled,
	"/gitaly.RepositoryService/FetchSourceBranch":            transactionsEnabled,
	"/gitaly.RepositoryService/RemoveRepository":             transactionsEnabled,
	"/gitaly.RepositoryService/ReplicateRepository":          transactionsEnabled,
	"/gitaly.RepositoryService/RestoreCustomHooks":           transactionsEnabled,
	"/gitaly.RepositoryService/RestoreRepository":            transactionsEnabled,
	"/gitaly.RepositoryService/SetCustomHooks":               transactionsEnabled,
	"/gitaly.RepositoryService/WriteRef":                     transactionsEnabled,
	"/gitaly.SSHService/SSHReceivePack":                      transactionsEnabled,
	"/gitaly.SmartHTTPService/PostReceivePack":               transactionsEnabled,
	"/gitaly.HookService/ReceiveReportHook":                  transactionsEnabled,
	"/gitaly.CommitService/CommitLanguages":                  transactionsEnabled,

	// The following RPCs currently aren't transactional, but we may consider making them
	// transactional in the future if the need arises.
	"/gitaly.ObjectPoolService/CreateObjectPool": transactionsDisabled,
	"/gitaly.ObjectPoolService/DeleteObjectPool": transactionsDisabled,

	// Graft reviews are stored next to the repository, not in Git references, so they do not
	// vote. Behind Praefect they are kept on the primary only (see doc/graft/REVIEW.md).
	"/gitaly.ReviewService/CreateReview":        transactionsDisabled,
	"/gitaly.ReviewService/UpdateReview":        transactionsDisabled,
	"/gitaly.ReviewService/AddReviewComment":    transactionsDisabled,
	"/gitaly.ReviewService/SubmitReviewVerdict": transactionsDisabled,
	"/gitaly.ReviewService/CloseReview":         transactionsDisabled,

	// The Graft storage core keeps its state in its own database, not in Git references. Its
	// authoritative mode is not supported behind Praefect (see doc/graft/STORAGE.md).
	"/gitaly.StorageCoreService/ImportRepository": transactionsDisabled,
	// The storage core's operator RPCs write its database and its bucket, never the references
	// or objects of a Gitaly replica, so there is nothing for the replicas to vote on:
	// CreateCheckpoint records a pack and a row (authoritative mode only, which is refused
	// behind Praefect); CollectGarbage deletes bucket objects and rows (also in shadow mode).
	"/gitaly.StorageCoreService/CreateCheckpoint": transactionsDisabled,
	"/gitaly.StorageCoreService/CollectGarbage":   transactionsDisabled,
	// DeleteConflictingReference writes a changeset to the database (authoritative mode only,
	// which is refused behind Praefect).
	"/gitaly.StorageCoreService/DeleteConflictingReference": transactionsDisabled,
}

// forcePrimaryRoutingRPCs tracks RPCs which need to always get routed to the primary. This should
// really be a last-resort measure for a given RPC, so each RPC added must have a strong reason why
// it's being added.
var forcePrimaryRPCs = map[string]bool{
	// GetObjectDirectorySize depends on a repository's on-disk state. It depends on when a
	// repository was last packed and on git-pack-objects(1) producing deterministic results.
	// Given that we can neither guarantee that replicas are always packed at the same time,
	// nor that git-pack-objects(1) produces the same packs. We always report sizes for the
	// primary node.
	"/gitaly.RepositoryService/GetObjectDirectorySize": true,
	// Same reasoning as for GetObjectDirectorySize.
	"/gitaly.RepositoryService/RepositorySize": true,
	// The changeset log is per-replica on-disk state: every Gitaly node appends to its own
	// <repo>/graft/changesets.jsonl from its reference-transaction hook, and the log is not
	// replicated. Sequence numbers, timestamps and metadata therefore differ between replicas,
	// and a replica that caught up through replication has gaps or different entries. Reading
	// from the primary keeps the cursor of a paging or resumed client on one dense log.
	"/gitaly.ChangesetService/ListChangesets":  true,
	"/gitaly.ChangesetService/WatchChangesets": true,
}

// postReceiveHookRPCs contains the list of RPCs that trigger the PostReceiveHook.
// For these RPCs, Praefect needs to signal the waiting hook after finalizers complete.
var postReceiveHookRPCs = map[string]bool{
	"/gitaly.SSHService/SSHReceivePack":              true,
	"/gitaly.SmartHTTPService/PostReceivePack":       true,
	"/gitaly.OperationService/UserApplyPatch":        true,
	"/gitaly.OperationService/UserCherryPick":        true,
	"/gitaly.OperationService/UserCommitFiles":       true,
	"/gitaly.OperationService/UserCreateBranch":      true,
	"/gitaly.OperationService/UserCreateTag":         true,
	"/gitaly.OperationService/UserDeleteBranch":      true,
	"/gitaly.OperationService/UserDeleteTag":         true,
	"/gitaly.OperationService/UserFFBranch":          true,
	"/gitaly.OperationService/UserLand":              true,
	"/gitaly.OperationService/UserMergeBranch":       true,
	"/gitaly.OperationService/UserRebaseConfirmable": true,
	"/gitaly.OperationService/UserRevert":            true,
	"/gitaly.OperationService/UserUpdateBranch":      true,
	"/gitaly.OperationService/UserUpdateSubmodule":   true,
	"/gitaly.ConflictsService/ResolveConflicts":      true,
}

// methodTriggersPostReceiveHook returns true if the given method triggers the PostReceiveHook.
func methodTriggersPostReceiveHook(method string) bool {
	return postReceiveHookRPCs[method]
}

// postReceiveSignal holds information needed to signal the PostReceiveHook
// that Praefect coordination is complete.
type postReceiveSignal struct {
	transactionID  uint64
	primaryConn    *grpc.ClientConn
	primaryStorage string
}

func init() {
	// Safety checks to verify that all registered RPCs are in `transactionRPCs`
	for _, method := range protoregistry.GitalyProtoPreregistered.Methods() {
		if method.Operation != protoregistry.OpMutator || method.Scope != protoregistry.ScopeRepository {
			continue
		}

		if _, ok := transactionRPCs[method.FullMethodName()]; !ok {
			panic(fmt.Sprintf("transactional RPCs miss repository-scoped mutator %q", method.FullMethodName()))
		}
	}

	// Safety checks to verify that `transactionRPCs` has no unknown RPCs
	for transactionalRPC := range transactionRPCs {
		method, err := protoregistry.GitalyProtoPreregistered.LookupMethod(transactionalRPC)
		if err != nil {
			panic(fmt.Sprintf("transactional RPC not a registered method: %q", err))
		}
		if method.Operation != protoregistry.OpMutator {
			panic(fmt.Sprintf("transactional RPC is not a mutator: %q", method.FullMethodName()))
		}
		if method.Scope != protoregistry.ScopeRepository {
			panic(fmt.Sprintf("transactional RPC is not repository-scoped: %q", method.FullMethodName()))
		}
	}
}

func shouldUseTransaction(ctx context.Context, method string) bool {
	condition, ok := transactionRPCs[method]
	if !ok {
		return false
	}

	return condition(ctx)
}

// getReplicationDetails determines the type of job and additional details based on the method name and incoming message
func getReplicationDetails(methodName string, m proto.Message) (datastore.ChangeType, datastore.Params, error) {
	switch methodName {
	case "/gitaly.RepositoryService/RemoveRepository":
		return datastore.DeleteRepo, nil, nil
	case "/gitaly.ObjectPoolService/CreateObjectPool",
		"/gitaly.RepositoryService/CreateFork",
		"/gitaly.RepositoryService/CreateRepository",
		"/gitaly.RepositoryService/CreateRepositoryFromBundle",
		"/gitaly.RepositoryService/CreateRepositoryFromSnapshot",
		"/gitaly.RepositoryService/CreateRepositoryFromURL",
		"/gitaly.RepositoryService/ReplicateRepository",
		"/gitaly.RepositoryService/RestoreRepository":
		return datastore.CreateRepo, nil, nil
	default:
		return datastore.UpdateRepo, nil, nil
	}
}

// grpcCall is a wrapper to assemble a set of parameters that represents an gRPC call method.
type grpcCall struct {
	fullMethodName string
	methodInfo     protoregistry.MethodInfo
	msg            proto.Message
	targetRepo     *gitalypb.Repository
}

// Coordinator takes care of directing client requests to the appropriate
// downstream server. The coordinator is thread safe; concurrent calls to
// register nodes are safe.
type Coordinator struct {
	logger                   log.Logger
	router                   Router
	txMgr                    *transactions.Manager
	queue                    datastore.ReplicationEventQueue
	rs                       datastore.RepositoryStore
	pts                      datastore.PendingTransactionStore
	registry                 *protoregistry.Registry
	conf                     config.Config
	votersMetric             *prometheus.HistogramVec
	txReplicationCountMetric *prometheus.CounterVec
	// committedWaitTimeout bounds how long monitorCommittedAndSignal waits for the
	// committed phase to conclude. The zero value means defaultCommittedWaitTimeout.
	committedWaitTimeout time.Duration
	// postReceiveSignalTimeout bounds the SignalPostReceiveReady call to the primary.
	// The zero value means defaultPostReceiveSignalTimeout.
	postReceiveSignalTimeout time.Duration
}

// Default timeouts of the post-receive signal path. Both are overridable via the
// respective Coordinator fields so that tests need not race the wall clock.
const (
	defaultCommittedWaitTimeout     = 60 * time.Second
	defaultPostReceiveSignalTimeout = 5 * time.Second
)

// NewCoordinator returns a new Coordinator that utilizes the provided logger
func NewCoordinator(
	logger log.Logger,
	queue datastore.ReplicationEventQueue,
	rs datastore.RepositoryStore,
	pts datastore.PendingTransactionStore,
	router Router,
	txMgr *transactions.Manager,
	conf config.Config,
	r *protoregistry.Registry,
) *Coordinator {
	maxVoters := 1
	for _, storage := range conf.VirtualStorages {
		if len(storage.Nodes) > maxVoters {
			maxVoters = len(storage.Nodes)
		}
	}

	coordinator := &Coordinator{
		logger:   logger,
		queue:    queue,
		rs:       rs,
		pts:      pts,
		registry: r,
		router:   router,
		txMgr:    txMgr,
		conf:     conf,
		votersMetric: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "gitaly_praefect_voters_per_transaction_total",
				Help:    "The number of voters a given transaction was created with",
				Buckets: prometheus.LinearBuckets(1, 1, maxVoters),
			},
			[]string{"virtual_storage"},
		),
		txReplicationCountMetric: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gitaly_praefect_tx_replications_total",
				Help: "The number of replication jobs scheduled for transactional RPCs",
			},
			[]string{"reason"},
		),
	}

	return coordinator
}

//nolint:revive // This is unintentionally missing documentation.
func (c *Coordinator) Describe(descs chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, descs)
}

//nolint:revive // This is unintentionally missing documentation.
func (c *Coordinator) Collect(metrics chan<- prometheus.Metric) {
	c.votersMetric.Collect(metrics)
	c.txReplicationCountMetric.Collect(metrics)
}

func (c *Coordinator) directRepositoryScopedMessage(ctx context.Context, call grpcCall) (*proxy.StreamParameters, error) {
	if fields := log.CustomFieldsFromContext(ctx); fields != nil {
		fields.RecordMetadata("virtual_storage", call.targetRepo.GetStorageName())
		fields.RecordMetadata("relative_path", call.targetRepo.GetRelativePath())
	}

	var err error
	var ps *proxy.StreamParameters

	switch call.methodInfo.Operation {
	case protoregistry.OpAccessor:
		ps, err = c.accessorStreamParameters(ctx, call)
	case protoregistry.OpMutator:
		ps, err = c.mutatorStreamParameters(ctx, call)
	case protoregistry.OpMaintenance:
		ps, err = c.maintenanceStreamParameters(ctx, call)
	default:
		err = fmt.Errorf("unknown operation type: %v", call.methodInfo.Operation)
	}

	if err != nil {
		return nil, err
	}

	return ps, nil
}

func shouldRouteRepositoryAccessorToPrimary(ctx context.Context, call grpcCall) bool {
	forcePrimary := forcePrimaryRPCs[call.fullMethodName]

	// In case the call's metadata tells us to force-route to the primary, then we must abide
	// and ignore what `forcePrimaryRPCs` says.
	if md, ok := grpc_metadata.FromIncomingContext(ctx); ok {
		header := md.Get(routeRepositoryAccessorPolicy)
		if len(header) == 0 {
			return forcePrimary
		}

		if header[0] == routeRepositoryAccessorPolicyPrimaryOnly {
			return true
		}
	}

	return forcePrimary
}

func (c *Coordinator) accessorStreamParameters(ctx context.Context, call grpcCall) (*proxy.StreamParameters, error) {
	repoPath := call.targetRepo.GetRelativePath()
	virtualStorage := call.targetRepo.GetStorageName()

	route, err := c.router.RouteRepositoryAccessor(
		ctx, virtualStorage, repoPath, shouldRouteRepositoryAccessorToPrimary(ctx, call),
	)
	if err != nil {
		return nil, fmt.Errorf("accessor call: route repository accessor: %w", err)
	}

	route.addLogFields(ctx)

	b, err := rewrittenRepositoryMessage(call.methodInfo, call.msg, route.Node.Storage, route.ReplicaPath, "")
	if err != nil {
		return nil, fmt.Errorf("accessor call: rewrite storage: %w", err)
	}

	metrics.ReadDistribution.WithLabelValues(virtualStorage, route.Node.Storage).Inc()

	return proxy.NewStreamParameters(proxy.Destination{
		Ctx:  streamParametersContext(ctx),
		Conn: route.Node.Connection,
		Msg:  b,
	}, nil, nil, nil), nil
}

func (c *Coordinator) registerTransaction(ctx context.Context, primary RouterNode, secondaries []RouterNode) (transactions.Transaction, transactions.CancelFunc, error) {
	var voters []transactions.Voter
	var threshold uint

	// This voting-strategy is a majority-wins one: the primary always needs to agree
	// with at least half of the secondaries.

	secondaryLen := uint(len(secondaries))

	// In order to ensure that no quorum can be reached without the primary, its number
	// of votes needs to exceed the number of secondaries.
	voters = append(voters, transactions.Voter{
		Name:  primary.Storage,
		Votes: secondaryLen + 1,
	})
	threshold = secondaryLen + 1

	for _, secondary := range secondaries {
		voters = append(voters, transactions.Voter{
			Name:  secondary.Storage,
			Votes: 1,
		})
	}

	// If we only got a single secondary (or none), we don't increase the threshold so
	// that it's allowed to disagree with the primary without blocking the transaction.
	// Otherwise, we add `Math.ceil(len(secondaries) / 2.0)`, which means that at least
	// half of the secondaries need to agree with the primary.
	if len(secondaries) > 1 {
		threshold += (secondaryLen + 1) / 2
	}

	return c.txMgr.RegisterTransaction(ctx, voters, threshold)
}

type nodeErrors struct {
	sync.Mutex
	errByNode map[string]error
}

func (c *Coordinator) mutatorStreamParameters(ctx context.Context, call grpcCall) (*proxy.StreamParameters, error) {
	targetRepo := call.targetRepo
	virtualStorage := call.targetRepo.GetStorageName()

	change, params, err := getReplicationDetails(call.fullMethodName, call.msg)
	if err != nil {
		return nil, fmt.Errorf("mutator call: replication details: %w", err)
	}

	partitioningHintRewritten := false
	var additionalRepoRelativePath string
	if additionalRepo, err := call.methodInfo.AdditionalRepo(call.msg); errors.Is(err, protoregistry.ErrRepositoryFieldNotFound) {
		// We can land here in two cases: either the message doesn't have an additional
		// repository, or the repository wasn't set. The former case is obviously fine, but
		// the latter case is fine, too, given that the additional repository may be an
		// optional field for some RPC calls. The Gitaly-side RPC handlers should know to
		// handle this case anyway, so we just leave the field unset in that case.

		// If a partitioning hint is provided in the request metadata, extract and rewrite it to the appropriate
		// replicate path. The hint is the @hashed/... relative path to the repo and the rewritten hint is the
		// equivalent @cluster/... replica path tto the repo.
		partitioningHintRewritten = true
		additionalRepoRelativePath, err = storage.ExtractPartitioningHint(ctx)
		if err != nil {
			return nil, fmt.Errorf("mutator call: extract partitioning hint: %w", err)
		}

		if additionalRepoRelativePath == "" && call.methodInfo.FullMethodName() == gitalypb.RepositoryService_CreateFork_FullMethodName {
			// The source repository is not marked as an additional repository so extract it manually.
			// Gitaly needs the rewritten relative path as a partitioning hint.
			additionalRepoRelativePath = call.msg.(*gitalypb.CreateForkRequest).GetSourceRepository().GetRelativePath()
		}
	} else if err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	} else {
		// We do not support resolving multiple different repositories that reside on
		// different virtual storages. This kind of makes sense from a technical point of
		// view as Praefect cannot guarantee to resolve both virtual storages. So for the
		// time being we accept this restriction and handle it explicitly.
		//
		// Note that this is the same condition as in `rewrittenRepositoryMessage()`. This
		// is done so that we detect such erroneous requests before we try to connect to the
		// target node, which allows us to return a proper error to the user that indicates
		// the underlying issue instead of an unrelated symptom.
		//
		// This limitation may be lifted in the future.
		if virtualStorage != additionalRepo.GetStorageName() {
			return nil, structerr.NewInvalidArgument("resolving additional repository on different storage than target repository is not supported")
		}

		additionalRepoRelativePath = additionalRepo.GetRelativePath()
	}

	var route RepositoryMutatorRoute
	switch change {
	case datastore.CreateRepo:
		route, err = c.router.RouteRepositoryCreation(ctx, virtualStorage, targetRepo.GetRelativePath(), additionalRepoRelativePath)

		// These RPCs are repository upserts. They should work if the
		// repository ID already exists in Praefect.
		if (call.fullMethodName == "/gitaly.RepositoryService/ReplicateRepository" ||
			call.fullMethodName == "/gitaly.RepositoryService/RestoreRepository") &&
			errors.Is(err, datastore.ErrRepositoryAlreadyExists) {
			change = datastore.UpdateRepo
			route, err = c.router.RouteRepositoryMutator(ctx, virtualStorage, targetRepo.GetRelativePath(), additionalRepoRelativePath)
		}
		if err != nil {
			return nil, fmt.Errorf("route repository creation: %w", err)
		}
	default:
		route, err = c.router.RouteRepositoryMutator(ctx, virtualStorage, targetRepo.GetRelativePath(), additionalRepoRelativePath)
		if err != nil {
			if errors.Is(err, ErrRepositoryReadOnly) {
				return nil, err
			}

			return nil, fmt.Errorf("mutator call: route repository mutator: %w", err)
		}
	}

	route.addLogFields(ctx)

	primaryMessage, err := rewrittenRepositoryMessage(call.methodInfo, call.msg, route.Primary.Storage, route.ReplicaPath, route.AdditionalReplicaPath)
	if err != nil {
		return nil, fmt.Errorf("mutator call: rewrite storage: %w", err)
	}

	if partitioningHintRewritten && additionalRepoRelativePath != "" {
		// Send the rewritten path as partitioning hint to Gitaly.
		ctx = storage.ContextWithPartitioningHint(ctx, route.AdditionalReplicaPath)
	}

	var finalizers []func() error

	primaryDest := proxy.Destination{
		Ctx:  streamParametersContext(ctx),
		Conn: route.Primary.Connection,
		Msg:  primaryMessage,
	}

	var secondaryDests []proxy.Destination

	if shouldUseTransaction(ctx, call.fullMethodName) {
		c.votersMetric.WithLabelValues(virtualStorage).Observe(float64(1 + len(route.Secondaries)))

		transaction, transactionCleanup, err := c.registerTransaction(ctx, route.Primary, route.Secondaries)
		if err != nil {
			return nil, fmt.Errorf("%w: %v %v", err, route.Primary, route.Secondaries)
		}
		finalizers = append(finalizers, transactionCleanup)

		// Write a crash-recovery record before any Gitaly node can commit changes. If Praefect
		// crashes between quorum and the finalizer, this record lets startup recovery
		// detect and reconcile the inconsistency.
		var pendingTxID int64
		if c.pts != nil && change == datastore.UpdateRepo {
			allVoters := make([]string, 0, 1+len(route.Secondaries))
			allVoters = append(allVoters, route.Primary.Storage)
			for _, s := range route.Secondaries {
				allVoters = append(allVoters, s.Storage)
			}

			id, err := c.pts.RecordPendingTransaction(ctx, datastore.PendingTransaction{
				RepositoryID:   route.RepositoryID,
				VirtualStorage: virtualStorage,
				RelativePath:   targetRepo.GetRelativePath(),
				ReplicaPath:    route.ReplicaPath,
				PrimaryStorage: route.Primary.Storage,
				ExpectedVoters: allVoters,
			})
			if err != nil {
				return nil, fmt.Errorf("record pending transaction for crash recovery: %w", err)
			}
			pendingTxID = id
		}

		nodeErrors := &nodeErrors{
			errByNode: make(map[string]error),
		}

		var (
			dbWriteOnce sync.Once
			dbWriteErr  error
		)

		// dbWrite records the generation update + schedules replication, exactly once,
		// whether driven by the success/failure monitor (post-receive RPCs) or the
		// teardown finalizer (everything else). IncrementGeneration is NOT idempotent,
		// so the Once is mandatory. performDBWriteAndReplication already detaches from
		// the request ctx (context.WithoutCancel), so a canceled ctx here is safe.
		// Per-transaction scope: declared locally, never on the Coordinator struct.
		dbWrite := func(ctx context.Context) error {
			dbWriteOnce.Do(func() {
				primaryDirtied, updated, outdated := getUpdatedAndOutdatedSecondaries(
					ctx, c.logger, route, transaction, nodeErrors, c.txReplicationCountMetric)
				if !primaryDirtied {

					// If the primary replica was not modified then we don't need to consider the secondaries
					// outdated. Praefect requires the primary to be always part of the quorum, so no changes
					// to secondaries would be made without primary being in agreement.
					if c.pts != nil && pendingTxID != 0 {
						if err := c.pts.DeletePendingTransaction(ctx, pendingTxID); err != nil {
							c.logger.WithError(err).WithField("transaction_id", pendingTxID).WarnContext(ctx, "failed to delete pending transaction record")
						}
					}
					return
				}
				dbWriteErr = c.performDBWriteAndReplication(
					ctx, route.RepositoryID, virtualStorage, targetRepo, route.ReplicaPath,
					route.Primary.Storage, updated, outdated, change, params,
					call.fullMethodName, route.AdditionalReplicaPath)

				// Finalizer succeeded: the generation has been incremented. Safe to remove the crash-recovery record.
				if c.pts != nil && pendingTxID != 0 {
					if err := c.pts.DeletePendingTransaction(ctx, pendingTxID); err != nil {
						c.logger.WithError(err).WithField("transaction_id", pendingTxID).WarnContext(ctx, "failed to delete pending transaction record")
					}
				}
			})
			return dbWriteErr // sync.Once provides happens-before; safe cross-goroutine
		}

		// Success-path coordinator: only needed for post-receive RPCs, and only when the
		// feature flag is on. FF off → no monitor, no signal → identical to master
		// (finalizer-only). waitForPraefectSignal on Gitaly is also FF-gated, so there's
		// no deadlock with the FF off.
		if methodTriggersPostReceiveHook(call.fullMethodName) && featureflag.PraefectPostReceiveSignal.IsEnabled(ctx) {
			go func() {
				if err := c.monitorCommittedAndSignal(ctx, transaction, dbWrite, &postReceiveSignal{
					transactionID:  transaction.ID(),
					primaryConn:    route.Primary.Connection,
					primaryStorage: route.Primary.Storage,
				}); err != nil {
					c.logger.WithError(err).
						WithField("transaction.id", transaction.ID()).
						WarnContext(ctx, "failed to signal post-receive ready to primary")
				}
			}()
		}

		injectedCtx, err := txinfo.InjectTransaction(ctx, transaction.ID(), route.Primary.Storage, true)
		if err != nil {
			return nil, err
		}
		primaryDest.Ctx = streamParametersContext(injectedCtx)
		primaryDest.ErrHandler = func(err error) error {
			nodeErrors.Lock()
			defer nodeErrors.Unlock()
			nodeErrors.errByNode[route.Primary.Storage] = err
			return err
		}

		for _, secondary := range route.Secondaries {
			secondaryMsg, err := rewrittenRepositoryMessage(call.methodInfo, call.msg, secondary.Storage, route.ReplicaPath, route.AdditionalReplicaPath)
			if err != nil {
				return nil, err
			}

			injectedCtx, err := txinfo.InjectTransaction(ctx, transaction.ID(), secondary.Storage, false)
			if err != nil {
				return nil, err
			}

			secondaryDests = append(secondaryDests, proxy.Destination{
				Ctx:  streamParametersContext(injectedCtx),
				Conn: secondary.Connection,
				Msg:  secondaryMsg,
				ErrHandler: func(err error) error {
					nodeErrors.Lock()
					defer nodeErrors.Unlock()
					nodeErrors.errByNode[secondary.Storage] = err

					c.logger.WithError(err).
						ErrorContext(ctx, "proxying to secondary failed")

					// Cancels failed node's voter in its current subtransaction.
					// Also updates internal state of subtransaction to fail and
					// release blocked voters if quorum becomes impossible.
					if err := c.txMgr.CancelTransactionNodeVoter(transaction.ID(), secondary.Storage); err != nil {
						c.logger.WithError(err).
							ErrorContext(ctx, "canceling secondary voter failed")
					}

					// The error is ignored, so we do not abort transactions
					// which are ongoing and may succeed even with a subset
					// of secondaries bailing out.
					return nil
				},
			})
		}

		finalizers = append(finalizers, func() error {
			if err := dbWrite(ctx); err != nil {
				return err
			}
			return ctx.Err()
		})
	} else {
		finalizers = append(finalizers,
			c.newRequestFinalizer(
				ctx,
				route.RepositoryID,
				virtualStorage,
				targetRepo,
				route.ReplicaPath,
				route.Primary.Storage,
				nil,
				append(routerNodesToStorages(route.Secondaries), route.ReplicationTargets...),
				change,
				params,
				call.fullMethodName,
				route.AdditionalReplicaPath,
			))
	}

	reqFinalizer := func() error {
		var firstErr error
		for _, finalizer := range finalizers {
			err := finalizer()
			if err == nil {
				continue
			}

			if firstErr == nil {
				firstErr = err
				continue
			}

			c.logger.
				WithError(err).
				ErrorContext(ctx, "coordinator proxy stream finalizer failure")
		}

		// Note: Signal sending is now handled by monitorTransactionAndSignal goroutine
		// that runs during RPC execution, not here in the finalizer. The finalizer
		// only handles transaction cleanup now.

		return firstErr
	}
	return proxy.NewStreamParameters(primaryDest, secondaryDests, reqFinalizer, nil), nil
}

// maintenanceStreamParameters returns stream parameters for a maintenance-style RPC. The RPC call
// is proxied to all nodes. Because it shouldn't matter whether a node is the primary or not in this
// context, we just pick the first node returned by the router to be the primary. Returns an error
// in case any of the nodes has failed to perform the maintenance RPC.
func (c *Coordinator) maintenanceStreamParameters(ctx context.Context, call grpcCall) (*proxy.StreamParameters, error) {
	route, err := c.router.RouteRepositoryMaintenance(ctx, call.targetRepo.GetStorageName(), call.targetRepo.GetRelativePath())
	if err != nil {
		return nil, fmt.Errorf("routing repository maintenance: %w", err)
	}

	route.addLogFields(ctx)

	peerCtx := streamParametersContext(ctx)

	nodeDests := make([]proxy.Destination, 0, len(route.Nodes))
	nodeErrors := &nodeErrors{
		errByNode: make(map[string]error),
	}

	for _, node := range route.Nodes {
		nodeMsg, err := rewrittenRepositoryMessage(call.methodInfo, call.msg, node.Storage, route.ReplicaPath, "")
		if err != nil {
			return nil, err
		}

		nodeDests = append(nodeDests, proxy.Destination{
			Ctx:  peerCtx,
			Conn: node.Connection,
			Msg:  nodeMsg,
			ErrHandler: func(err error) error {
				nodeErrors.Lock()
				defer nodeErrors.Unlock()
				nodeErrors.errByNode[node.Storage] = err

				c.logger.WithField("gitaly_storage", node.Storage).WithError(err).ErrorContext(ctx, "proxying maintenance RPC to node failed")

				// We ignore any errors returned by nodes such that they all have a
				// chance to finish their maintenance RPC in a best-effort strategy.
				// In case any node fails though, the RPC call will return with an
				// error after all nodes have finished.
				return nil
			},
		})
	}

	return proxy.NewStreamParameters(nodeDests[0], nodeDests[1:], func() error {
		nodeErrors.Lock()
		defer nodeErrors.Unlock()

		// In case any of the nodes has recorded an error we will return it. It shouldn't
		// matter which error we return exactly, so we just return errors in the order we've
		// got from the router. This also has the nice property that any error returned by
		// the primary node would be prioritized over all the others.
		for _, node := range route.Nodes {
			if nodeErr, ok := nodeErrors.errByNode[node.Storage]; ok && nodeErr != nil {
				return nodeErr
			}
		}

		return nil
	}, nil), nil
}

// streamParametersContexts converts the contexts with incoming metadata into a context that is
// usable by peer Gitaly nodes.
func streamParametersContext(ctx context.Context) context.Context {
	// When upgrading Gitaly nodes where the upgrade contains feature flag default changes, then
	// there will be a window where a subset of Gitaly nodes has a different understanding of
	// the current default value. If the feature flag wasn't set explicitly on upgrade by
	// outside callers, then these Gitaly nodes will thus see different default values and run
	// different code. This is something we want to avoid: all Gitalies should have the same
	// world view of which features are enabled and which ones aren't and ideally do the same
	// thing.
	//
	// This problem isn't solvable on Gitaly side, but Praefect is in a perfect position to do
	// so. While it may have the same problem in a load-balanced multi-Praefect setup, this is
	// much less of a problem: the most important thing is that the view on feature flags is
	// consistent for a single RPC call, and that will always be the case regardless of which
	// Praefect is proxying the call. The worst case scenario is that we'll execute the RPC once
	// with a feature flag enabled, and once with it disabled, which shouldn't typically be an
	// issue.
	//
	// To fix this scenario, we thus inject all known feature flags with their current default
	// value as seen by Praefect into Gitaly's context if they haven't explicitly been set by
	// the caller. Like this, Gitalies will never even consider the default value in a cluster
	// setup, except when it is being introduced for the first time when Gitaly restarts before
	// Praefect. But given that feature flags should be introduced with a default value of
	// `false` to account for zero-dodwntime upgrades, the view would also be consistent in that
	// case.

	explicitlySetFlags := map[string]bool{}
	for flag := range featureflag.FromContext(ctx) {
		explicitlySetFlags[flag.Name] = true
	}

	outgoingCtx := metadata.IncomingToOutgoing(ctx)
	for _, flag := range featureflag.DefinedFlags() {
		if !explicitlySetFlags[flag.Name] {
			outgoingCtx = featureflag.OutgoingCtxWithFeatureFlag(
				outgoingCtx, flag, flag.OnByDefault,
			)
		}
	}

	return outgoingCtx
}

// StreamDirector determines which downstream servers receive requests
func (c *Coordinator) StreamDirector(ctx context.Context, fullMethodName string, peeker proxy.StreamPeeker) (*proxy.StreamParameters, error) {
	// For phase 1, we need to route messages based on the storage location
	// to the appropriate Gitaly node.
	c.logger.WithField("method", fullMethodName).DebugContext(ctx, "Stream director received method")

	mi, err := c.registry.LookupMethod(fullMethodName)
	if err != nil {
		return nil, err
	}

	payload, err := peeker.Peek()
	if err != nil {
		return nil, err
	}

	m, err := protoMessage(mi, payload)
	if err != nil {
		return nil, err
	}

	if mi.Scope == protoregistry.ScopeRepository {
		targetRepo, err := mi.TargetRepo(m)
		if err != nil {
			if errors.Is(err, protoregistry.ErrRepositoryFieldNotFound) {
				return nil, structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet)
			}

			return nil, structerr.New("repo scoped: %w", err)
		}

		ctx = injectVirtualStorageInMetadata(ctx, targetRepo.GetStorageName())

		if err := c.validateTargetRepo(targetRepo); err != nil {
			return nil, structerr.NewInvalidArgument("%w", err)
		}

		sp, err := c.directRepositoryScopedMessage(ctx, grpcCall{
			fullMethodName: fullMethodName,
			methodInfo:     mi,
			msg:            m,
			targetRepo:     targetRepo,
		})
		if err != nil {
			var additionalRepoNotFound additionalRepositoryNotFoundError
			if errors.As(err, &additionalRepoNotFound) {
				return nil, structerr.NewNotFound("%w", err).WithMetadataItems(
					structerr.MetadataItem{Key: "storage_name", Value: additionalRepoNotFound.storageName},
					structerr.MetadataItem{Key: "relative_path", Value: additionalRepoNotFound.relativePath},
				)
			}

			if errors.Is(err, datastore.ErrRepositoryNotFound) {
				return nil, storage.NewRepositoryNotFoundError(targetRepo.GetStorageName(), targetRepo.GetRelativePath())
			}

			if errors.Is(err, datastore.ErrRepositoryAlreadyExists) {
				return nil, structerr.NewAlreadyExists("%w", err)
			}

			return nil, err
		}
		return sp, nil
	}

	if mi.Scope == protoregistry.ScopeStorage {
		return c.directStorageScopedMessage(ctx, mi, m)
	}

	return nil, structerr.NewInternal("rpc with undefined scope %q", mi.Scope)
}

func (c *Coordinator) directStorageScopedMessage(ctx context.Context, mi protoregistry.MethodInfo, msg proto.Message) (*proxy.StreamParameters, error) {
	virtualStorage, err := mi.Storage(msg)
	if err != nil {
		return nil, structerr.NewInvalidArgument("storage scoped: %w", err)
	}

	if virtualStorage == "" {
		return nil, structerr.NewInvalidArgument("storage scoped: target storage is invalid")
	}

	var ps *proxy.StreamParameters
	switch mi.Operation {
	case protoregistry.OpAccessor:
		ps, err = c.accessorStorageStreamParameters(ctx, mi, msg, virtualStorage)
	case protoregistry.OpMutator:
		ps, err = c.mutatorStorageStreamParameters(ctx, mi, msg, virtualStorage)
	default:
		err = fmt.Errorf("storage scope: unknown operation type: %v", mi.Operation)
	}
	return ps, err
}

func (c *Coordinator) accessorStorageStreamParameters(ctx context.Context, mi protoregistry.MethodInfo, msg proto.Message, virtualStorage string) (*proxy.StreamParameters, error) {
	node, err := c.router.RouteStorageAccessor(ctx, virtualStorage)
	if err != nil {
		if errors.Is(err, nodes.ErrVirtualStorageNotExist) {
			return nil, structerr.NewInvalidArgument("%w", err)
		}
		return nil, structerr.NewInternal("accessor storage scoped: route storage accessor %q: %w", virtualStorage, err)
	}

	node.addLogFields(ctx)

	b, err := rewrittenStorageMessage(mi, msg, node.Storage)
	if err != nil {
		return nil, structerr.NewInvalidArgument("accessor storage scoped: %w", err)
	}

	// As this is a read operation it could be routed to another storage (not only primary) if it meets constraints
	// such as: it is healthy, it belongs to the same virtual storage bundle, etc.
	// https://gitlab.com/gitlab-org/gitaly/-/issues/2972
	primaryDest := proxy.Destination{
		Ctx:  ctx,
		Conn: node.Connection,
		Msg:  b,
	}

	return proxy.NewStreamParameters(primaryDest, nil, func() error { return nil }, nil), nil
}

func (c *Coordinator) mutatorStorageStreamParameters(ctx context.Context, mi protoregistry.MethodInfo, msg proto.Message, virtualStorage string) (*proxy.StreamParameters, error) {
	route, err := c.router.RouteStorageMutator(ctx, virtualStorage)
	if err != nil {
		if errors.Is(err, nodes.ErrVirtualStorageNotExist) {
			return nil, structerr.NewInvalidArgument("%w", err)
		}
		return nil, structerr.NewInternal("mutator storage scoped: get shard %q: %w", virtualStorage, err)
	}

	route.addLogFields(ctx)

	b, err := rewrittenStorageMessage(mi, msg, route.Primary.Storage)
	if err != nil {
		return nil, structerr.NewInvalidArgument("mutator storage scoped: %w", err)
	}

	primaryDest := proxy.Destination{
		Ctx:  ctx,
		Conn: route.Primary.Connection,
		Msg:  b,
	}

	secondaryDests := make([]proxy.Destination, len(route.Secondaries))
	for i, secondary := range route.Secondaries {
		b, err := rewrittenStorageMessage(mi, msg, secondary.Storage)
		if err != nil {
			return nil, structerr.NewInvalidArgument("mutator storage scoped: %w", err)
		}
		secondaryDests[i] = proxy.Destination{Ctx: ctx, Conn: secondary.Connection, Msg: b}
	}

	return proxy.NewStreamParameters(primaryDest, secondaryDests, func() error { return nil }, nil), nil
}

// rewrittenRepositoryMessage rewrites the repository storages and relative paths.
func rewrittenRepositoryMessage(mi protoregistry.MethodInfo, m proto.Message, storageName, relativePath, additionalRelativePath string) ([]byte, error) {
	// clone the message so the original is not changed
	m = proto.Clone(m)
	targetRepo, err := mi.TargetRepo(m)
	if err != nil {
		if errors.Is(err, protoregistry.ErrRepositoryFieldNotFound) {
			return nil, structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet)
		}

		return nil, structerr.New("%w", err)
	}

	if additionalRepo, err := mi.AdditionalRepo(m); errors.Is(err, protoregistry.ErrRepositoryFieldNotFound) {
		// Nothing to rewrite in case the additional repository either doesn't exist in the
		// message or wasn't set by the caller.
	} else if err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	} else {
		// We do not support resolving multiple different repositories that reside on
		// different virtual storages. This kind of makes sense from a technical point of
		// view as Praefect cannot guarantee to resolve both virtual storages. So for the
		// time being we accept this restriction and handle it explicitly.
		//
		// This limitation may be lifted in the future.
		if targetRepo.GetStorageName() != additionalRepo.GetStorageName() {
			return nil, structerr.NewInvalidArgument("resolving additional repository on different storage than target repository is not supported")
		}

		additionalRepo.StorageName = storageName
		additionalRepo.RelativePath = additionalRelativePath
	}

	// Rewrite the target repository. Note that we only do this after having written the
	// additional repository so that we can check whether the original storage name of both
	// repositories match.
	targetRepo.StorageName = storageName
	targetRepo.RelativePath = relativePath

	return proxy.NewCodec().Marshal(m)
}

func rewrittenStorageMessage(mi protoregistry.MethodInfo, m proto.Message, storage string) ([]byte, error) {
	if err := mi.SetStorage(m, storage); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	return proxy.NewCodec().Marshal(m)
}

func protoMessage(mi protoregistry.MethodInfo, frame []byte) (proto.Message, error) {
	m, err := mi.UnmarshalRequestProto(frame)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// getUpdatedAndOutdatedSecondaries returns all nodes which can be considered up-to-date or outdated
// after the given transaction. A node is considered outdated, if one of the following is true:
//
//   - No subtransactions were created and the RPC was successful on the primary. This really is only
//     a safeguard in case the RPC wasn't aware of transactions and thus failed to correctly assert
//     its state matches across nodes. This is rather pessimistic, as it could also indicate that an
//     RPC simply didn't change anything. If the RPC was a failure on the primary and there were no
//     subtransactions, we assume no changes were done and that the nodes failed prior to voting.
//
//   - The node failed to be part of the quorum. As a special case, if the primary fails the vote, all
//     nodes need to get replication jobs.
//
//   - The node has a different error state than the primary. If both primary and secondary have
//     returned the same error, then we assume they did the same thing and failed in the same
//     controlled way.
//
// Note that this function cannot and should not fail: if anything goes wrong, we need to create
// replication jobs to repair state.
func getUpdatedAndOutdatedSecondaries(
	ctx context.Context,
	logger log.Logger,
	route RepositoryMutatorRoute,
	transaction transactions.Transaction,
	nodeErrors *nodeErrors,
	replicationCountMetric *prometheus.CounterVec,
) (primaryDirtied bool, updated []string, outdated []string) {
	nodeErrors.Lock()
	defer nodeErrors.Unlock()

	primaryErr := nodeErrors.errByNode[route.Primary.Storage]

	// If there were no subtransactions and the primary failed the RPC, we assume no changes
	// have been made and the nodes simply failed before voting. We can thus return directly and
	// notify the caller that the primary is not considered to be dirty.
	if transaction.CountSubtransactions() == 0 && primaryErr != nil {
		return false, nil, nil
	}

	// If there was a single subtransactions but the primary didn't cast a vote, then it means
	// that the primary node has dropped out before secondaries were able to commit any changes
	// to disk. Given that they cannot ever succeed without the primary, no change to disk
	// should have happened.
	if transaction.CountSubtransactions() == 1 && !transaction.DidVote(route.Primary.Storage) {
		return false, nil, nil
	}

	primaryDirtied = true

	nodesByState := make(map[string][]string)
	defer func() {
		logger.
			WithField("transaction.primary", route.Primary.Storage).
			WithField("transaction.secondaries", nodesByState).
			InfoContext(ctx, "transactional node states")

		for reason, nodes := range nodesByState {
			replicationCountMetric.WithLabelValues(reason).Add(float64(len(nodes)))
		}
	}()

	markOutdated := func(reason string, nodes []string) {
		if len(nodes) != 0 {
			outdated = append(outdated, nodes...)
			nodesByState[reason] = append(nodesByState[reason], nodes...)
		}
	}

	// Replication targets were not added to the transaction, most likely because they are
	// either not healthy or out of date. We thus need to make sure to create replication jobs
	// for them.
	markOutdated("outdated", route.ReplicationTargets)

	// If no subtransaction happened, then the called RPC may not be aware of transactions or
	// the nodes failed before casting any votes. If the primary failed the RPC, we assume
	// no changes were done and the nodes hit an error prior to voting. If the primary processed
	// the RPC successfully, we assume the RPC is not correctly voting and replicate everywhere.
	if transaction.CountSubtransactions() == 0 {
		markOutdated("no-votes", routerNodesToStorages(route.Secondaries))
		return primaryDirtied, updated, outdated
	}

	// If we cannot get the transaction state, then something's gone awfully wrong. We go the
	// safe route and just replicate to all secondaries.
	nodeStates, err := transaction.State()
	if err != nil {
		markOutdated("missing-tx-state", routerNodesToStorages(route.Secondaries))
		return primaryDirtied, updated, outdated
	}

	// If the primary node did not commit the transaction but there were some subtransactions committed,
	// then we must assume that it dirtied on-disk state. This modified state may not be what we want,
	// but it's what we got. So in order to ensure a consistent state, we need to replicate.
	if state := nodeStates[route.Primary.Storage]; state != transactions.VoteCommitted {
		markOutdated("primary-not-committed", routerNodesToStorages(route.Secondaries))
		return primaryDirtied, updated, outdated
	}

	// Now we finally got the potentially happy case: when the secondary committed the
	// transaction and has the same error state as the primary, then it's considered up to date
	// and thus does not need replication.
	for _, secondary := range route.Secondaries {
		if !equalGrpcError(nodeErrors.errByNode[secondary.Storage], primaryErr) {
			markOutdated("node-error-status", []string{secondary.Storage})
			continue
		}

		if nodeStates[secondary.Storage] != transactions.VoteCommitted {
			markOutdated("node-not-committed", []string{secondary.Storage})
			continue
		}

		updated = append(updated, secondary.Storage)
		nodesByState["updated"] = append(nodesByState["updated"], secondary.Storage)
	}

	return primaryDirtied, updated, outdated
}

func equalGrpcError(err1, err2 error) bool {
	status1, ok := status.FromError(err1)
	if !ok {
		return false
	}

	status2, ok := status.FromError(err2)
	if !ok {
		return false
	}

	if status1.Code() != status2.Code() {
		return false
	}

	if status1.Message() != status2.Message() {
		return false
	}

	return true
}

func routerNodesToStorages(nodes []RouterNode) []string {
	storages := make([]string, len(nodes))
	for i, n := range nodes {
		storages[i] = n.Storage
	}
	return storages
}

// performDBWriteAndReplication performs the database update and queues replication jobs.
// This is extracted from newRequestFinalizer to allow it to be called from different contexts.
func (c *Coordinator) performDBWriteAndReplication(
	originalCtx context.Context,
	repositoryID int64,
	virtualStorage string,
	targetRepo *gitalypb.Repository,
	replicaPath string,
	primary string,
	updatedSecondaries []string,
	outdatedSecondaries []string,
	change datastore.ChangeType,
	params datastore.Params,
	cause string,
	additionalReplicaPath string,
) error {
	// Use a separate timeout for the database operations. If the request times out, the passed in context is
	// canceled. We need to perform the database updates regardless whether the request was canceled or not as
	// the primary replica could have been dirtied and secondaries become outdated. Otherwise we'd have no idea of
	// the possible changes performed on the disk.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(originalCtx), 30*time.Second)
	defer cancel()

	logEntry := c.logger.WithFields(log.Fields{
		"replication.cause":   cause,
		"replication.change":  change,
		"replication.primary": primary,
	})
	if len(updatedSecondaries) > 0 {
		logEntry = logEntry.WithField("replication.updated", updatedSecondaries)
	}
	if len(outdatedSecondaries) > 0 {
		logEntry = logEntry.WithField("replication.outdated", outdatedSecondaries)
	}
	logEntry.InfoContext(ctx, "queueing replication jobs")

	switch change {
	case datastore.UpdateRepo:
		// If this fails, the primary might have changes on it that are not recorded in the database. The secondaries will appear
		// consistent with the primary but might serve different stale data. Follow-up mutator calls will solve this state although
		// the primary will be a later generation in the mean while.
		if err := c.rs.IncrementGeneration(ctx, repositoryID, primary, updatedSecondaries); err != nil {
			return fmt.Errorf("increment generation: %w", err)
		}
	case datastore.CreateRepo:
		repositorySpecificPrimariesEnabled := c.conf.Failover.ElectionStrategy == config.ElectionStrategyPerRepository
		variableReplicationFactorEnabled := repositorySpecificPrimariesEnabled &&
			c.conf.DefaultReplicationFactors()[virtualStorage] > 0

		// The repository should be explicitly assigned to a subset of replicas if:
		// - A reduced replication factor is used; or
		// - The repository is a fork (thus additionalReplicaPath is the path of the upstream), and
		//   the upstream had explicit assignments.
		storeAssignments := variableReplicationFactorEnabled
		if !storeAssignments && additionalReplicaPath != "" {
			hasAssignments, err := c.rs.HasRepositoryAssignments(ctx, additionalReplicaPath)
			if err != nil {
				return fmt.Errorf("check repository assignments: %w", err)
			}
			storeAssignments = hasAssignments
		}

		if err := c.rs.CreateRepository(ctx,
			repositoryID,
			virtualStorage,
			targetRepo.GetRelativePath(),
			replicaPath,
			primary,
			updatedSecondaries,
			outdatedSecondaries,
			repositorySpecificPrimariesEnabled,
			storeAssignments,
		); err != nil {
			if errors.Is(err, datastore.ErrRepositoryAlreadyExists) {
				return structerr.NewAlreadyExists("%w", err)
			}

			return fmt.Errorf("create repository: %w", err)
		}
		change = datastore.UpdateRepo
	}

	correlationID := correlation.ExtractFromContextOrGenerate(ctx)

	g, ctx := errgroup.WithContext(ctx)
	for _, secondary := range outdatedSecondaries {
		event := datastore.ReplicationEvent{
			Job: datastore.ReplicationJob{
				RepositoryID:      repositoryID,
				Change:            change,
				RelativePath:      targetRepo.GetRelativePath(),
				VirtualStorage:    virtualStorage,
				SourceNodeStorage: primary,
				TargetNodeStorage: secondary,
				Params:            params,
			},
			Meta: datastore.Params{datastore.CorrelationIDKey: correlationID},
		}

		g.Go(func() error {
			if _, err := c.queue.Enqueue(ctx, event); err != nil {
				if errors.As(err, &datastore.ReplicationEventExistsError{}) {
					c.logger.WithError(err).InfoContext(ctx, "replication event queue already has similar entry")
					return nil
				}

				return fmt.Errorf("enqueue replication event: %w", err)
			}
			return nil
		})
	}

	return g.Wait()
}

func (c *Coordinator) newRequestFinalizer(
	originalCtx context.Context,
	repositoryID int64,
	virtualStorage string,
	targetRepo *gitalypb.Repository,
	replicaPath string,
	primary string,
	updatedSecondaries []string,
	outdatedSecondaries []string,
	change datastore.ChangeType,
	params datastore.Params,
	cause string,
	additionalReplicaPath string, // set for RPCs like CreateFork and CreateObjectPool
) func() error {
	return func() error {
		if err := c.performDBWriteAndReplication(
			originalCtx, repositoryID, virtualStorage, targetRepo, replicaPath,
			primary, updatedSecondaries, outdatedSecondaries, change, params, cause, additionalReplicaPath,
		); err != nil {
			return err
		}

		// The cancellation signal is suppressed earlier in the function, so we'd return no error if the
		// original context exceeded its deadline while running the request finalizer. If there were
		// no other errors, return the possible error from the context so we don't return OK code for
		// failed requests.
		return originalCtx.Err()
	}
}

func (c *Coordinator) validateTargetRepo(repo *gitalypb.Repository) error {
	if repo.GetStorageName() == "" && repo.GetRelativePath() == "" {
		return storage.ErrRepositoryNotSet
	}

	if repo.GetStorageName() == "" {
		return storage.ErrStorageNotSet
	}

	if repo.GetRelativePath() == "" {
		return storage.ErrRepositoryPathNotSet
	}

	if _, found := c.conf.StorageNames()[repo.GetStorageName()]; !found {
		return storage.NewStorageNotFoundError(repo.GetStorageName())
	}

	return nil
}

func injectVirtualStorageInMetadata(ctx context.Context, vs string) context.Context {
	md, ok := grpc_metadata.FromIncomingContext(ctx)
	if !ok {
		md = grpc_metadata.New(nil)
	}
	// Make a copy so we don't modify the original
	md = md.Copy()
	md.Append(VirtualStorageKey, vs)
	return grpc_metadata.NewIncomingContext(ctx, md)
}

// monitorCommittedAndSignal waits for the reference-transaction committed phase to
// conclude, records the generation update, and signals the waiting PostReceiveHook.
// It runs asynchronously (during RPC execution) to break the deadlock where
// PostReceiveHook blocks on a signal that the teardown finalizer can't send because
// the finalizer is itself downstream of PostReceiveHook completing.
func (c *Coordinator) monitorCommittedAndSignal(
	ctx context.Context,
	transaction transactions.Transaction,
	dbWrite func(context.Context) error,
	signalInfo *postReceiveSignal,
) error {
	committedWaitTimeout := timeoutOrDefault(c.committedWaitTimeout, defaultCommittedWaitTimeout)

	select {
	case <-transaction.CommittedConcluded():
		// Committed phase decided (quorum reached or failed). The on-disk outcome
		// is known; record it before signaling so routing reflects the write.
		dbErr := dbWrite(ctx)
		// Fail the push at the hook ONLY if the DB write failed (routing would be
		// stale). A failed committed *quorum* is not a signal failure: the primary
		// has the data and Praefect repairs via replication — matching master.
		if err := c.signalPostReceiveReady(signalInfo, dbErr != nil); err != nil {
			return fmt.Errorf("signaling post-receive ready: %w", err)
		}

		return nil
	case <-ctx.Done():
		// RPC ended before any committed vote (pre-receive reject / early failure /
		// client disconnect). No post-receive is waiting on us; the finalizer does
		// any needed repair write.
		c.logger.DebugContext(ctx, "transaction monitor exiting without signal: context done")
		return nil
	case <-time.After(committedWaitTimeout):
		return fmt.Errorf("timed out after %s waiting for committed phase to conclude", committedWaitTimeout)
	}
}

// signalPostReceiveReady signals the PostReceiveHook on the primary Gitaly node
// that Praefect has completed transaction coordination and database updates.
// This allows the hook to proceed with calling Rails for CI pipeline triggering.
func (c *Coordinator) signalPostReceiveReady(info *postReceiveSignal, failed bool) error {
	// Use a background context with timeout instead of the request context, as the
	// request may already be complete/canceled by the time finalizers run. We need
	// to ensure the signal gets delivered even if the original request is done.
	signalCtx, cancel := context.WithTimeout(context.Background(),
		timeoutOrDefault(c.postReceiveSignalTimeout, defaultPostReceiveSignalTimeout))
	defer cancel()

	client := gitalypb.NewHookServiceClient(info.primaryConn)
	if _, err := client.SignalPostReceiveReady(signalCtx, &gitalypb.SignalPostReceiveReadyRequest{
		StorageName:           info.primaryStorage,
		TransactionId:         info.transactionID,
		PostTransactionFailed: failed,
	}); err != nil {
		return err
	}

	return nil
}

// timeoutOrDefault returns the configured timeout, falling back to def if it is unset.
func timeoutOrDefault(timeout, def time.Duration) time.Duration {
	if timeout <= 0 {
		return def
	}

	return timeout
}
