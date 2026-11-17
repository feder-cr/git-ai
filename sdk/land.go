package sdk

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// LandStrategy selects how a change that is not a fast-forward is landed.
type LandStrategy = gitalypb.UserLandRequest_Strategy

// Landing strategies.
const (
	// LandRebase rebases the change onto the branch tip, producing linear
	// history. This is the default.
	LandRebase = gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED
	// LandMerge creates a merge commit with the branch tip and the change as
	// parents.
	LandMerge = gitalypb.UserLandRequest_STRATEGY_MERGE
	// LandFastForwardOnly only fast-forwards the branch. Otherwise Land fails
	// with a not-ancestor error (see IsNotAncestor).
	LandFastForwardOnly = gitalypb.UserLandRequest_STRATEGY_FF_ONLY
)

// LandOptions describes a change to land on a branch.
type LandOptions struct {
	// Branch is the branch to land on, without the refs/heads/ prefix.
	Branch string
	// Commit is the full object ID of the commit to land.
	Commit string
	// User is the user the change is landed as. It is the committer of
	// rewritten commits.
	User User
	// Strategy selects rebase (default), merge or fast-forward only.
	Strategy LandStrategy
	// MaxAttempts bounds the compare-and-swap retries when the branch moves
	// concurrently. Zero means the server default of 20.
	MaxAttempts int32
	// CreateBranch allows creating Branch at Commit if it does not exist.
	CreateBranch bool
	// MergeMessage is the message of the merge commit for LandMerge. If
	// empty, a default message is used.
	MergeMessage string
	// Timestamp is the committer date of rewritten commits. If zero, the
	// server's current time is used.
	Timestamp time.Time
	// ResolveConflicts asks the server to resolve merge conflicts with its
	// configured conflict resolver (see doc/graft/RESOLVE.md). A resolved
	// landing is a merge commit of the branch tip and the change. If the
	// resolver is disabled, refuses a file or fails, the landing fails with a
	// merge conflict as without this option, and the error carries the
	// conflict package (see LandConflict).
	ResolveConflicts bool
	// Resolution lands Commit with the caller's resolution of the conflicts of
	// a conflict package (see LandConflict and ResolutionFor), as a merge
	// commit of the branch tip and the change. The server accepts it only when
	// it was made against the current tip and resolves exactly the conflicting
	// paths; otherwise Land fails with a fresh package. The request must fit in
	// MaxLandRequestBytes.
	Resolution *Resolution
	// RetryUnavailable is how Land retries a call that the server answers
	// with Unavailable (see UnavailableRetry). It is on by default.
	RetryUnavailable UnavailableRetry
}

// UnavailableRetry is the client side of the server's contract for
// Unavailable: a node that cannot serve a land right now (its storage core
// sync ran out of sync_timeout, its land lane was closed) answers Unavailable,
// and the client retries later (doc/graft/LAND-BATCHING.md, Limits). Land
// retries only codes.Unavailable, never another code, with an exponential
// backoff with full jitter: before retry n it waits a random time between zero
// and min(Cap, Base x 2^(n-1)). A context that ends stops the waiting.
//
// The response of a call can be lost after the server landed the change. The
// retry then answers AlreadyLanded and lands nothing (doc/graft/PLAN11.md): with
// LandMerge and LandFastForwardOnly, Commit is reachable from the tip; with
// LandRebase, ResolveConflicts included, the branch holds a rebased copy of
// Commit, and the server answers from its record of the first land, with
// LandedCommitID the copy and LandedSequence its changelog sequence. This holds
// also when a later commit reverted the change in between, for example
// graft-verify reverting the culprit: the change is not landed again, and
// RevertedByCommitID names the revert. A retry whose first call never reached
// the update lands the change; with ResolveConflicts it may run the conflict
// resolver again, a model included, for the same conflict, and LandResult
// reports the retries (UnavailableRetries), so that this cost is visible.
//
// The record of the first land needs a server released after v0.2.0 with its
// changeset log on ([graft.changelog] not disabled). Against an older server,
// or one whose changeset log is disabled, the retry of a lost LandRebase replays
// the change: every commit of it comes out empty and the branch is left
// unchanged, without AlreadyLanded, unless a later commit reverted the change in
// between, which the retry then lands again. Set SkipRebaseLands for such
// servers.
type UnavailableRetry struct {
	// Disabled turns the retries off: Land returns Unavailable at once.
	Disabled bool
	// SkipRebaseLands turns the retries off for LandRebase lands only.
	SkipRebaseLands bool
	// MaxRetries bounds the retries after the first call. Zero means 5.
	MaxRetries int
	// Base is the backoff of the first retry. Zero means 100ms.
	Base time.Duration
	// Cap bounds the backoff of every retry. Zero means 2s.
	Cap time.Duration
}

// UnavailableRetriesError is the error of a land that failed after the SDK
// retried Unavailable answers: the error of the last call, or of the context
// that ended while Land waited to retry. It carries the status of that error,
// so Code, LandError and the Is* helpers read it as they read the error itself.
type UnavailableRetriesError struct {
	// Retries is the number of Unavailable answers that were retried.
	Retries int
	// LastUnavailable is the message of the last of them.
	LastUnavailable string
	// Err is the error the land failed with.
	Err error
}

func (e *UnavailableRetriesError) Error() string {
	return fmt.Sprintf("%v (after %d retries of Unavailable, the last: %s)", e.Err, e.Retries, e.LastUnavailable)
}

// Unwrap returns the error the land failed with.
func (e *UnavailableRetriesError) Unwrap() error { return e.Err }

// GRPCStatus returns the status of the error the land failed with.
func (e *UnavailableRetriesError) GRPCStatus() *status.Status { return status.Convert(e.Err) }

// Defaults of UnavailableRetry.
const (
	defaultUnavailableRetries = 5
	defaultUnavailableBase    = 100 * time.Millisecond
	defaultUnavailableCap     = 2 * time.Second
)

// backoff returns the random wait before retry n (n >= 1).
func (r UnavailableRetry) backoff(n int) time.Duration {
	base, maxBackoff := r.Base, r.Cap
	if base <= 0 {
		base = defaultUnavailableBase
	}
	if maxBackoff <= 0 {
		maxBackoff = defaultUnavailableCap
	}
	limit := base
	for i := 1; i < n && limit < maxBackoff; i++ {
		limit *= 2
	}
	limit = min(limit, maxBackoff)
	return time.Duration(rand.Int64N(int64(limit) + 1))
}

// maxRetries returns the retries of a land with the given strategy.
//
// A land with a Resolution is always a merge of the tip and the change, whatever its strategy: after a
// lost response Commit is reachable from the tip, so the retry is reported as AlreadyLanded and nothing is
// replayed. SkipRebaseLands, which protects against the replay of a rebase, does not apply to it.
func (r UnavailableRetry) maxRetries(strategy LandStrategy, resolution bool) int {
	switch {
	case r.Disabled, strategy == LandRebase && !resolution && r.SkipRebaseLands:
		return 0
	case r.MaxRetries > 0:
		return r.MaxRetries
	default:
		return defaultUnavailableRetries
	}
}

// LandResult describes the outcome of a successful Land call.
type LandResult struct {
	// LandedCommitID is the commit at the branch tip after landing: Commit
	// itself after a fast-forward, otherwise the rebased or merge commit.
	LandedCommitID string
	// OldTip is the branch tip that was replaced. Empty if the branch was
	// created.
	OldTip string
	// Rewritten is true if the server rebased or merged the change.
	Rewritten bool
	// AlreadyLanded is true if the change was already on the branch and the
	// branch was left unchanged: Commit is reachable from the tip, or an
	// earlier land of Commit on the branch put LandedCommitID on it, which is
	// still reachable from the tip (LandedSequence is then set). A change that
	// a later commit reverted is not landed again: landing it again needs a
	// new commit.
	AlreadyLanded bool
	// LandedSequence is the changelog sequence of the earlier land when the
	// server answered AlreadyLanded from its record (see AlreadyLanded); zero
	// otherwise. LandedCommitID is then the commit of that land.
	LandedSequence uint64
	// RevertedByCommitID is the commit after LandedCommitID on the branch
	// whose message says that it reverts it ("This reverts commit <id>"), when
	// LandedSequence is set and such a commit exists.
	RevertedByCommitID string
	// AlreadyLandedDetail explains an AlreadyLanded answer that LandedSequence
	// is set for, for example "landed as <oid> at changelog 42; it was
	// reverted later by <oid>".
	AlreadyLandedDetail string
	// BranchCreated is true if the branch was created by this call.
	BranchCreated bool
	// Attempts is the number of compare-and-swap attempts the server used.
	Attempts int32
	// ResolvedPaths are the conflicting files the resolver resolved, empty if
	// the landing did not need resolution.
	ResolvedPaths []string
	// Resolver is the name of the resolver that resolved the conflicts, such as
	// graft-resolve/trivial. Empty if no conflict was resolved.
	Resolver string
	// Batch is the land batch the change was part of. A change that landed
	// alone has Size 1 and Position 1, see BatchInfo.
	Batch BatchInfo
	// UnavailableRetries is the number of calls the server answered with
	// Unavailable before this one succeeded (see UnavailableRetry).
	UnavailableRetries int
	// LastUnavailable is the message of the last of those answers, empty
	// without retries.
	LastUnavailable string
}

// oldTip maps the zero object ID that the server reports for a created branch
// to the empty string.
func oldTip(oid string) string {
	if strings.Trim(oid, "0") == "" {
		return ""
	}
	return oid
}

// Land lands a commit on a branch on the server. The server rebases or merges
// the change onto the current tip when the branch has moved, and retries
// concurrent updates, so many writers can land changes on the same branch
// without a merge queue.
//
// An Unavailable answer is retried as opts.RetryUnavailable says. A land that
// fails after retries returns an *UnavailableRetriesError, with the retries and
// the status of the last error.
//
// Failures carry structured details: use IsMergeConflict, IsNotAncestor,
// IsReferenceUpdateConflict, IsRejectedByHook or LandError, and LandConflict
// for the conflict package, also when no automatic resolution was requested.
func (r *Repo) Land(ctx context.Context, opts LandOptions) (*LandResult, error) {
	switch {
	case trimBranchPrefix(opts.Branch) == "":
		return nil, status.Error(codes.InvalidArgument, "branch is required")
	case opts.Commit == "":
		return nil, status.Error(codes.InvalidArgument, "commit is required")
	case opts.MaxAttempts < 0:
		return nil, status.Error(codes.InvalidArgument, "max attempts must not be negative")
	case opts.RetryUnavailable.MaxRetries < 0, opts.RetryUnavailable.Base < 0, opts.RetryUnavailable.Cap < 0:
		return nil, status.Error(codes.InvalidArgument, "the retries of Unavailable must not be negative")
	}

	req := &gitalypb.UserLandRequest{
		Repository:       r.repository(),
		User:             opts.User.toProto(),
		Branch:           []byte(trimBranchPrefix(opts.Branch)),
		CommitId:         opts.Commit,
		Strategy:         opts.Strategy,
		MaxAttempts:      opts.MaxAttempts,
		CreateBranch:     opts.CreateBranch,
		MergeMessage:     []byte(opts.MergeMessage),
		ResolveConflicts: opts.ResolveConflicts,
	}
	if !opts.Timestamp.IsZero() {
		req.Timestamp = timestamppb.New(opts.Timestamp)
	}
	if opts.Resolution != nil {
		req.Resolution = opts.Resolution.toProto()
		if err := checkLandRequestSize(req); err != nil {
			return nil, err
		}
	}

	var (
		resp            *gitalypb.UserLandResponse
		err             error
		retries         int
		lastUnavailable string
	)
	for {
		resp, err = r.client.operations.UserLand(ctx, req)
		if status.Code(err) != codes.Unavailable || retries >= opts.RetryUnavailable.maxRetries(opts.Strategy, opts.Resolution != nil) {
			break
		}
		retries++
		lastUnavailable = status.Convert(err).Message()

		timer := time.NewTimer(opts.RetryUnavailable.backoff(retries))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, &UnavailableRetriesError{Retries: retries, LastUnavailable: lastUnavailable, Err: status.FromContextError(ctx.Err()).Err()}
		case <-timer.C:
		}
	}
	if err != nil {
		if retries > 0 {
			return nil, &UnavailableRetriesError{Retries: retries, LastUnavailable: lastUnavailable, Err: err}
		}
		return nil, err
	}

	var resolvedPaths []string
	for _, p := range resp.GetResolvedPaths() {
		resolvedPaths = append(resolvedPaths, string(p))
	}

	return &LandResult{
		LandedCommitID: resp.GetLandedCommitId(),
		OldTip:         oldTip(resp.GetOldTip()),
		Rewritten:      resp.GetRewritten(),
		AlreadyLanded:  resp.GetAlreadyLanded(),
		BranchCreated:  resp.GetBranchUpdate().GetBranchCreated(),
		Attempts:       resp.GetAttempts(),
		ResolvedPaths:  resolvedPaths,
		Resolver:       resp.GetResolver(),
		Batch:          batchOf(resp),

		LandedSequence:      resp.GetLandedSequence(),
		RevertedByCommitID:  resp.GetRevertedByCommitId(),
		AlreadyLandedDetail: resp.GetAlreadyLandedDetail(),

		UnavailableRetries: retries,
		LastUnavailable:    lastUnavailable,
	}, nil
}
