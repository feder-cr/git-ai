package operations

import (
	"context"
	"errors"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/landlane"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// landBatchLanes batches the UserLand requests of a branch. Requests of one branch of one repository
// share a lane, and each batch of a lane is executed by Server.executeLandBatch.
type landBatchLanes = landlane.Registry[*landBatchRequest, *gitalypb.UserLandResponse]

// newLandBatchLanes returns the lanes of UserLand. It returns nil, which disables batching, when the
// batching is disabled or its configuration is invalid.
//
// The lanes are those of the process (see service.Dependencies.GraftLandLanes), which all servers of
// the process share, so that the requests for one branch run in one lane whichever listener they
// arrive on. The first server builds them with its executor. The servers are built from the same
// dependencies, so the executor of any of them lands the batches of all of them. Dependencies
// without lanes, which tests build, get lanes of this server only, from [graft.land.batch].
func newLandBatchLanes(s *Server, cfg config.Graft, shared *landlane.Shared, logger log.Logger) *landBatchLanes {
	if cfg.Land.Batch.Disabled {
		return nil
	}

	if shared == nil {
		laneCfg, err := landlane.ConfigFromGraft(cfg.Land.Batch)
		if err != nil {
			logger.WithError(err).Error("graft: land batching is disabled")
			return nil
		}

		shared = landlane.NewShared(laneCfg)
	}

	lanes, err := landlane.SharedRegistry(shared, s.executeLandBatch)
	if err != nil {
		logger.WithError(err).Error("graft: land batching is disabled")
		return nil
	}

	return lanes
}

// landBatchable reports whether the request lands through its lane. Batching applies to standalone
// Graft only, and not to MVCC snapshots, which are pinned to the tip they were taken at. A request
// for a missing branch lands alone, as it creates the branch. A request with its own object
// directories lands alone too: the quarantine of a batch is built from its first request, so the
// objects that only the request's directories hold would not be visible to the batch.
func (s *Server) landBatchable(ctx context.Context, in *gitalypb.UserLandRequest) bool {
	if s.landLanes == nil || !s.graft.Standalone || mvcc.CacheFromContext(ctx) != nil {
		return false
	}

	repo := in.GetRepository()
	if repo.GetGitObjectDirectory() != "" || len(repo.GetGitAlternateObjectDirectories()) > 0 {
		return false
	}

	reference := git.NewReferenceNameFromBranchName(string(in.GetBranch()))
	tip, err := s.landBranchTip(ctx, s.localRepoFactory.Build(in.GetRepository()), reference)
	if err != nil || tip == "" {
		return false
	}

	return true
}

// userLandBatched lands the request through its lane. The response is built from the outcome of
// the batch that executed the request.
func (s *Server) userLandBatched(ctx context.Context, in *gitalypb.UserLandRequest, committer git.Signature, precheck *landPrecheck, pre *landPreResolution) (*gitalypb.UserLandResponse, error) {
	repo := in.GetRepository()
	key := landlane.Key{
		Storage:      repo.GetStorageName(),
		RelativePath: repo.GetRelativePath(),
		Branch:       string(in.GetBranch()),
	}

	req := &landBatchRequest{
		ctx:           ctx,
		in:            in,
		committer:     committer,
		actor:         landBatchActor(ctx, in),
		precheck:      precheck,
		preResolution: pre,
	}

	resp, err := s.landLanes.Submit(ctx, key, req)
	if err != nil {
		return nil, batchSubmitError(err)
	}

	if resp.Value != nil && resp.Value.GetBatchSize() == 0 {
		// The request was delivered before its batch ended (see executeLandBatch), so the batch did not
		// report its size and position in the value. The lane reports them.
		resp.Value.BatchSize = int32(resp.BatchSize)
		resp.Value.BatchPosition = int32(resp.BatchPosition + 1)
	}

	return resp.Value, nil
}

// batchSubmitError returns the gRPC error of a request that did not get the result of its batch.
// A request whose context ended is reported with the code of the reason it ended.
func batchSubmitError(err error) error {
	switch {
	case errors.Is(err, landlane.ErrClosed):
		return structerr.NewUnavailable("land batching is shut down: %w", err)
	case errors.Is(err, context.DeadlineExceeded):
		return structerr.NewDeadlineExceeded("waiting for landing batch: %w", err)
	case errors.Is(err, context.Canceled):
		return structerr.NewCanceled("waiting for landing batch: %w", err)
	}

	return err
}

// executeLandBatch lands the requests of one lane as a batch. It is the executor of the lane registry,
// and the lane runs one batch at a time. The context is the lane context, which is not cancelled when
// a submitter cancels its request.
func (s *Server) executeLandBatch(ctx context.Context, _ landlane.Key, reqs []*landBatchRequest) ([]landlane.Result[*gitalypb.UserLandResponse], error) {
	defer observeLandBatchPhase(landBatchPhaseTotal, time.Now())

	repository := reqs[0].in.GetRepository()

	executor := landBatchExecutor{
		maxRebuilds: landBatchMaxRebuilds,
		newStepper: func(ctx context.Context) (landBatchStepper, func(), error) {
			stepper, cleanup, err := s.newLandBatchStepper(ctx, repository)
			if err != nil {
				return nil, nil, err
			}
			return stepper, cleanup, nil
		},
		fallback: func(req *landBatchRequest) (*gitalypb.UserLandResponse, error) {
			return s.landWithRetries(req.ctx, req.in, req.committer, req.precheck, req.preResolution)
		},
		giveUp: landBatchGiveUp,
		backoff: func(ctx context.Context, attempt int) error {
			if err := waitLandBackoff(ctx, attempt); err != nil {
				return err
			}
			// In authoritative storage mode the branch may have moved on another node: the
			// rebuilt batch must start from the tip that the database has.
			return s.ensureStorageFresh(ctx, repository)
		},
		early: func(ctx context.Context, index int, outcome landBatchOutcome) {
			landlane.CompleteEarly(ctx, index, landBatchResult(outcome))
		},
	}

	// The lane context carries the values of the first request but no cancellation. Git commands
	// need a context that can be done, so the batch gets one of its own that ends with the batch,
	// or when the graceful shutdown period of the server has elapsed: the lanes wait for their
	// running batches when they are closed at shutdown.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.shutdownCtx, cancel)()

	outcomes := executor.Execute(ctx, reqs)
	countLandBatchMembers(outcomes)

	results := make([]landlane.Result[*gitalypb.UserLandResponse], len(outcomes))
	for i, outcome := range outcomes {
		results[i] = landBatchResult(outcome)
	}

	return results, nil
}

// newLandBatchStepper starts a batch attempt on repository. Its phases are observed in
// graft_land_batch_phase_seconds. The returned function ends the attempt.
func (s *Server) newLandBatchStepper(ctx context.Context, repository *gitalypb.Repository) (*serverLandBatchStepper, func(), error) {
	attempt, cleanup, err := s.newLandingAttempt(ctx, repository, landPublishPack, observeLandBatchPhase)
	if err != nil {
		return nil, nil, err
	}

	return &serverLandBatchStepper{landingAttempt: attempt}, cleanup, nil
}

// landBatchResult builds the result of a request of a batch.
func landBatchResult(outcome landBatchOutcome) landlane.Result[*gitalypb.UserLandResponse] {
	if outcome.Err != nil {
		return landlane.Result[*gitalypb.UserLandResponse]{Err: outcome.Err}
	}

	if outcome.Response != nil {
		// A request landed alone after its batch was rejected. It did not share a batch.
		outcome.Response.BatchSize = 1
		outcome.Response.BatchPosition = 1
		return landlane.Result[*gitalypb.UserLandResponse]{Value: outcome.Response}
	}

	resp := landResponse(outcome.OldTip, outcome.LandedCommit, outcome.Rewritten, outcome.AlreadyLanded, outcome.Created)
	resp.Attempts = int32(outcome.Attempts)
	resp.BatchSize = int32(outcome.BatchSize)
	resp.BatchPosition = int32(outcome.BatchPosition + 1)
	if outcome.Earlier != nil {
		outcome.Earlier.setOn(resp)
	}
	if outcome.Resolution != nil {
		resp.ResolvedPaths = outcome.Resolution.resolvedPaths
		resp.Resolver = outcome.Resolution.resolver
		outcome.Resolution.landed()
	}

	return landlane.Result[*gitalypb.UserLandResponse]{Value: resp}
}

// landBatchGiveUp returns the error of a request whose batch lost the compare-and-swap on every
// attempt.
func landBatchGiveUp(req *landBatchRequest, cause error, attempts int) error {
	var conflict landConflict
	if !errors.As(cause, &conflict) {
		return structerr.NewInternal("updating ref with hooks: %w", cause)
	}

	return landKeptMoving(req.in, req.preResolution, conflict, attempts)
}

// serverLandBatchStepper implements landBatchStepper on one batch attempt: the landing attempt that
// every land uses (see landingAttempt), plus the in-process admission of each request and the
// combined update of the batch.
type serverLandBatchStepper struct {
	*landingAttempt
}

// Land lands the request onto tip with the decision that every land makes (landingAttempt.decide).
// The standalone policy is then evaluated for the actor on the new tip the request would create,
// before the request changes the running tip: the combined update runs as graft-batch, so the hooks
// do not judge the request.
//
// The git work runs in the context of the batch, which a submitter does not cancel: the request is
// part of the batch from the moment it lands. The identity is the one of the request, not the one of
// the first request of the batch, because conflict resolution is decided by it.
func (st *serverLandBatchStepper) Land(ctx context.Context, tip git.ObjectID, req *landBatchRequest) (landStep, error) {
	defer observeLandBatchPhase(landBatchPhaseLandMember, time.Now())

	step, err := st.decide(ctx, tip, req)
	if err != nil {
		return step, err
	}

	// A request that changes the running tip is admitted for the update it makes. A created branch
	// is updated from the zero OID.
	if step.alreadyLanded || (!step.created && step.tip == tip) {
		return step, nil
	}

	oldTip := tip
	if step.created {
		oldTip = st.objectHash.ZeroOID
	}
	if err := st.admit(req, req.reference(), oldTip, step.tip); err != nil {
		return landStep{}, err
	}

	return step, nil
}

// admit evaluates the standalone access policy for the actor of req on the update of reference from
// oldOID to newOID. The actor is the one the pre-receive hook would evaluate for the request alone: the
// agent of the request, or else its GitLab user ID. The combined update does not evaluate it, as it
// runs as the graft-batch user, so every member is checked here, before it enters the running tip.
// A denied request fails alone with the reason of the policy.
func (st *serverLandBatchStepper) admit(req *landBatchRequest, reference git.ReferenceName, oldOID, newOID git.ObjectID) error {
	allowed, message, err := hook.EvaluateGraftPolicy(st.server.graft, req.actor, reference.String(), oldOID.String(), newOID.String())
	if err != nil {
		return structerr.NewInternal("evaluating graft policy: %w", err)
	}

	if allowed {
		return nil
	}

	return structerr.NewPermissionDenied("%s", message).WithDetail(&gitalypb.UserLandError{
		Error: &gitalypb.UserLandError_AccessCheck{
			AccessCheck: &gitalypb.AccessCheckError{
				ErrorMessage: message,
				UserId:       req.actor,
			},
		},
	})
}

// Update publishes the batch with one compare-and-swap. The hooks run once for the combined update,
// as the synthetic graft-batch user, and the changelog records the members of the batch.
func (st *serverLandBatchStepper) Update(ctx context.Context, reference git.ReferenceName, members []changelog.Member, rewritten bool, old, newTip git.ObjectID) (landUpdateKind, error) {
	if err := st.publish(ctx); err != nil {
		return landUpdateFailed, err
	}
	defer observeLandBatchPhase(landBatchPhaseUpdate, time.Now())

	oldOID := old
	if old == "" {
		oldOID = st.objectHash.ZeroOID
	}

	ctx = batchUpdateContext(ctx)
	ctx = changelog.WithBatch(changelog.WithOperation(ctx, changelog.OperationLandBatch), members)

	// The compare-and-swap reads only the repository and the user of the request.
	in := &gitalypb.UserLandRequest{
		Repository: st.repository,
		User:       landBatchIdentity,
	}

	return st.updateKind(st.server.updateLandedReference(ctx, in, st.quarantineDir, st.repo, reference, oldOID, newTip, rewritten, landsOf(members)))
}

// UpdateSolo publishes a batch of one member as a land: the update runs with the identity of the member
// and as the land operation. The changelog records the member as the changeset of the land, so the
// original commit of the member maps to the commit it landed as. The compare-and-swap and its outcomes
// are those of Update.
func (st *serverLandBatchStepper) UpdateSolo(ctx context.Context, req *landBatchRequest, member changelog.Member, reference git.ReferenceName, rewritten bool, old, newTip git.ObjectID) (landUpdateKind, error) {
	if err := st.publish(ctx); err != nil {
		return landUpdateFailed, err
	}
	defer observeLandBatchPhase(landBatchPhaseUpdate, time.Now())

	oldOID := old
	if old == "" {
		oldOID = st.objectHash.ZeroOID
	}

	ctx = requestIdentityContext(ctx, req)
	ctx = changelog.WithBatch(changelog.WithOperation(ctx, changelogOperationLand), []changelog.Member{member})

	return st.updateKind(st.server.updateLandedReference(ctx, req.in, st.quarantineDir, st.repo, reference, oldOID, newTip, rewritten, landsOf([]changelog.Member{member})))
}

// landsOf returns the identities of the changes that members land (doc/graft/PLAN11.md): each
// member's original commit and the commit of the chain that holds it.
func landsOf(members []changelog.Member) []changelog.Land {
	lands := make([]changelog.Land, len(members))
	for i, member := range members {
		lands[i] = changelog.Land{Original: member.OriginalCommit, Landed: member.Commit}
	}
	return lands
}

// changelogOperationLand is the operation of an unbatched land in the changelog. The lanes run outside
// of the gRPC method of the requests, so the operation is set for the update of a batch of one.
const changelogOperationLand = "land"

// updateKind classifies the error of the compare-and-swap of a batch or of a batch of one.
func (st *serverLandBatchStepper) updateKind(err error) (landUpdateKind, error) {
	if err == nil {
		return landUpdated, nil
	}

	var conflict landConflict
	if errors.As(err, &conflict) {
		return landUpdateLost, err
	}

	var customHookErr updateref.CustomHookError
	var notAllowedErr hook.NotAllowedError
	if errors.As(err, &customHookErr) || errors.As(err, &notAllowedErr) {
		return landUpdateRejected, err
	}

	return landUpdateFailed, err
}
