package operations

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/graft/ancestry"
	"github.com/feder-cr/git-ai/internal/graft/rebase"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// landPhaseObserver observes the time a phase of a landing attempt took, from start until now.
// Batch attempts observe graft_land_batch_phase_seconds (observeLandBatchPhase); an unbatched land
// observes nothing.
type landPhaseObserver func(phase string, start time.Time)

// ignoreLandPhase is the landPhaseObserver of an unbatched land.
func ignoreLandPhase(string, time.Time) {}

// landPublishMode is how a landing attempt makes the objects it writes durable. Both modes keep the
// durability contract of a quarantine: every object file in it is durable before the quarantine
// migration moves it into the repository, which therefore only has to make the links durable.
type landPublishMode int

const (
	// landPublishPack is the mode of a batch attempt. Its rebaser writes the objects of all members
	// without fsync, and publish packs every loose object of the quarantine into one pack, which git
	// pack-objects fsyncs, before the update. A pack is the cheap way to make many objects durable.
	landPublishPack landPublishMode = iota + 1
	// landPublishLoose is the mode of an unbatched attempt. Its rebaser writes the few objects of
	// one change with the server's fsync configuration, and the migration moves them as they are.
	// A pack would cost a process and as many fsyncs for these few objects, and the attempts that
	// lose the compare-and-swap would each leave a pack in the repository.
	landPublishLoose
)

// landingAttempt is one attempt to land changes onto a branch, in a quarantine of its own. The
// unbatched land and every attempt of a batch use it, so both decide how a change lands and
// rebase it the same way (doc/graft/PLAN8.md): one listing of the commits a change adds to the tip
// decides, and one rebase engine replays. How the objects become durable is the mode of the attempt.
type landingAttempt struct {
	server        *Server
	repository    *gitalypb.Repository
	quarantineDir *quarantine.Dir
	repo          *localrepo.Repo
	objectHash    git.ObjectHash
	// rebaser replays the rebased changes of the attempt. It serves the whole attempt.
	rebaser batchRebaser
	// ancestry lists the commits that a change adds to the tip, with the graph index when it is
	// enabled.
	ancestry ancestry.Checker
	// observe observes the phases of the attempt.
	observe landPhaseObserver
	// mode is how the attempt makes its objects durable.
	mode landPublishMode
	// inline is the time the members of the attempt have spent in inline calls (land_inline.go).
	inline landInlineBudget
}

// batchRebaser replays the commits of the changes of an attempt. It is rebase.Engine.
type batchRebaser interface {
	Replay(ctx context.Context, upstream git.ObjectID, todo []git.ObjectID, committer *git.Signature) (git.ObjectID, error)
	Close() error
}

// newBatchRebaser starts the rebaser of an attempt in mode on the quarantine of the attempt. Only
// the pack mode defers the durability of the objects it writes to the publish of the attempt (see
// landPublishPack); the loose mode writes them with the server's fsync configuration. It is a
// variable so that tests can observe the replays.
var newBatchRebaser = func(repo *localrepo.Repo, mode landPublishMode) batchRebaser {
	if mode == landPublishPack {
		return rebase.New(repo, rebase.WithDeferredDurability())
	}
	return rebase.New(repo)
}

// newLandingAttempt starts an attempt on repository in mode: it opens the quarantine of the attempt
// and the processes that serve it. The returned function ends the attempt.
func (s *Server) newLandingAttempt(ctx context.Context, repository *gitalypb.Repository, mode landPublishMode, observe landPhaseObserver) (*landingAttempt, func(), error) {
	if mode != landPublishPack && mode != landPublishLoose {
		return nil, nil, structerr.NewInternal("unknown publish mode %d", mode)
	}

	defer observe(landBatchPhaseQuarantine, time.Now())

	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, repository)
	if err != nil {
		return nil, nil, err
	}

	objectHash, err := quarantineRepo.ObjectHash(ctx)
	if err != nil {
		cleanup()
		return nil, nil, structerr.NewInternal("detecting object hash: %w", err)
	}

	checker, releaseChecker, err := s.landAncestry(ctx, quarantineRepo)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	// One rebase engine serves the whole attempt, and the attempt ends with it.
	rebaser := newBatchRebaser(quarantineRepo, mode)

	return &landingAttempt{
			server:        s,
			repository:    repository,
			quarantineDir: quarantineDir,
			repo:          quarantineRepo,
			objectHash:    objectHash,
			rebaser:       rebaser,
			ancestry:      checker,
			observe:       observe,
			mode:          mode,
		}, func() {
			releaseChecker()
			if err := rebaser.Close(); err != nil {
				s.logger.WithError(err).Error("graft: closing the rebaser of a landing attempt")
			}
			cleanup()
		}, nil
}

// Tip returns the tip of the reference, read by its exact name (landBranchTip). A missing reference
// yields an empty ID.
func (a *landingAttempt) Tip(ctx context.Context, reference git.ReferenceName) (git.ObjectID, error) {
	defer a.observe(landBatchPhaseTip, time.Now())

	tip, err := a.server.landBranchTip(ctx, a.repo, reference)
	if err != nil {
		return "", structerr.NewInternal("resolving branch: %w", err)
	}

	return tip, nil
}

// decide lands the change of req onto tip in the quarantine of the attempt, without publishing it:
// it peels the commit ID, decides from one listing of the commits the change adds to tip whether it
// is already landed, fast-forwards tip or diverges, and rebases or merges a diverged change, resolving
// its conflicts when the request asks for it. The returned step tells what the change did to tip.
//
// The git work runs in ctx with the identity of the request, which decides conflict resolution.
func (a *landingAttempt) decide(ctx context.Context, tip git.ObjectID, req *landBatchRequest) (landStep, error) {
	ctx = requestIdentityContext(ctx, req)
	in := req.in

	commitID, err := a.objectHash.FromHex(in.GetCommitId())
	if err != nil {
		return landStep{tipIndependent: true}, structerr.NewInvalidArgument("cannot parse commit ID: %w", err).
			WithMetadata("commit_id", in.GetCommitId())
	}

	// The ID may refer to a tag object or another non-commit object that peels to a commit. A
	// branch can only point to a commit, so the peeled commit is the one that lands.
	commitID, err = a.peel(ctx, commitID)
	if err != nil {
		if errors.As(err, new(catfile.NotFoundError)) {
			return landStep{tipIndependent: true}, structerr.NewInvalidArgument("cannot resolve commit ID: %w", err).
				WithMetadata("commit_id", in.GetCommitId())
		}

		return landStep{}, structerr.NewInternal("peeling commit ID: %w", err)
	}

	if tip == "" {
		if !in.GetCreateBranch() {
			return landStep{}, structerr.NewNotFound("branch %q does not exist", string(in.GetBranch()))
		}

		// The branch is created at the commit. The compare-and-swap expects the zero OID, so a
		// concurrent creation is detected.
		return landStep{commit: commitID, tip: commitID, created: true}, nil
	}

	plan, err := a.plan(ctx, tip, commitID)
	if err != nil {
		return landStep{}, err
	}

	switch plan.Kind() {
	case rebase.LandAlreadyLanded:
		// The commit is already reachable from the tip, so there is nothing to do. This also covers
		// commit_id being equal to the tip.
		return landStep{commit: commitID, tip: tip, alreadyLanded: true}, nil

	case rebase.LandFastForward:
		return landStep{commit: commitID, tip: commitID}, nil
	}

	// commit_id is not on the branch, but an earlier land may have put it there as a rewritten
	// commit, whose response was lost. Its record answers, so the change is not landed twice.
	earlier, err := a.landedEarlier(ctx, req.reference(), tip, commitID)
	if err != nil {
		return landStep{}, err
	}
	if earlier != nil {
		return landStep{commit: commitID, tip: tip, alreadyLanded: true, earlier: earlier}, nil
	}

	// A resolution from the caller lands as a merge commit of the tip and the change, when it was
	// made against this tip; it replaces the server's attempt to merge or rebase.
	if in.GetResolution() != nil {
		res, err := a.server.landWithResolution(ctx, in, a.repo, tip, commitID, req.committer, req.precheck)
		if err != nil {
			var unresolved *landUnresolvedError
			if errors.As(err, &unresolved) {
				// The paths of UserLandError are bounded like the files of the package.
				files := unresolved.listing.files[:min(len(unresolved.listing.files), landMaxPackageFiles)]
				paths := make([][]byte, 0, len(files))
				for _, f := range files {
					paths = append(paths, []byte(f.conflict.path))
				}
				return landStep{}, landWithPackage(ctx, landMergeConflictError(tip, commitID, paths, err), err)
			}
			return landStep{}, err
		}
		return landStep{commit: commitID, tip: res.commit, rewritten: true, resolution: &res}, nil
	}

	newTip, err := a.computeTip(ctx, in, plan, tip, commitID, req.committer)
	var resolution *landResolution
	if err != nil {
		if !in.GetResolveConflicts() || !isLandMergeConflict(err) {
			return landStep{}, err
		}

		// The conflict is resolved into a merge commit from the verdicts made before the lane
		// (PLAN10 15.8); the resolver is never called here. Files without a verdict go back to
		// UserLand to be resolved out of the lane. When the land fails otherwise, the request gets
		// the merge conflict it would have got without resolution, with the conflict package as a
		// second detail when the server listed the conflicts.
		res, resolveErr := a.server.landApplyVerdicts(withLandInlineBudget(ctx, &a.inline), in, a.repo, tip, commitID, req.committer, req.preResolution)
		var need *landNeedsResolution
		if errors.As(resolveErr, &need) {
			return landStep{}, need
		}
		if resolveErr != nil {
			a.server.logger.WithError(resolveErr).WithField("branch", string(in.GetBranch())).
				Info("graft: conflict resolution of land failed")
			return landStep{}, landWithPackage(ctx, err, resolveErr)
		}

		resolution = &res
		newTip = res.commit
	}

	// Rebasing drops the commits that become empty on top of the tip, so every commit of the change
	// was dropped when the tip already holds all of its changes, for example because they were
	// applied upstream as cherry-picks. commit_id itself is not reachable from the tip, so the change
	// is not reported as already landed: the step reports a landing that left the branch unchanged.
	if resolution == nil && newTip == tip {
		return landStep{commit: commitID, tip: tip}, nil
	}

	return landStep{commit: commitID, tip: newTip, rewritten: true, resolution: resolution}, nil
}

// peel returns the commit that commitID is or peels to, through an object info process of the
// quarantine. commitID is a full object ID, so nothing is abbreviated or ambiguous. An object that
// does not exist or does not peel to a commit is a catfile.NotFoundError.
//
// Each peel takes the process from the catfile cache and gives it back, which checks out the same
// process while it works. A process that fails is not handed to later peels for long: the cache
// drops a closed process, and one that died leaves its next request outstanding, so the cache drops
// it as dirty after that request fails. A dead process thus fails the request that met it and at
// most the next one, not every later request of the attempt.
func (a *landingAttempt) peel(ctx context.Context, commitID git.ObjectID) (git.ObjectID, error) {
	reader, release, err := a.server.catfileCache.ObjectInfoReader(ctx, a.repo)
	if err != nil {
		return "", fmt.Errorf("reading object info: %w", err)
	}
	defer release()

	info, err := reader.Info(ctx, git.Revision(commitID.String()+"^{commit}"))
	if err != nil {
		return "", err
	}

	return info.Oid, nil
}

// plan decides how commitID lands onto tip (see rebase.Plan): from one listing of the commits it
// adds to tip, made exact when the change diverges.
func (a *landingAttempt) plan(ctx context.Context, tip, commitID git.ObjectID) (rebase.LandPlan, error) {
	defer a.observe(landBatchPhasePlan, time.Now())

	plan, err := rebase.Plan(ctx, a.ancestry, tip, commitID)
	if err != nil {
		return rebase.LandPlan{}, structerr.NewInternal("planning the landing: %w", err)
	}

	return plan, nil
}

// computeTip computes the new tip for a commit whose plan is diverged: it is neither reachable from
// the tip nor a fast-forward of it. The strategy of the request decides: FF_ONLY fails with a
// NotAncestor error, MERGE merges the change into the tip, and REBASE replays the commits of the plan
// with the rebaser of the attempt. A rebase refuses a range with merge commits or without a common
// ancestor (InvalidArgument), and a conflict of the merge or the rebase is a merge conflict.
func (a *landingAttempt) computeTip(ctx context.Context, in *gitalypb.UserLandRequest, plan rebase.LandPlan, tip, commitID git.ObjectID, committer git.Signature) (git.ObjectID, error) {
	switch in.GetStrategy() {
	case gitalypb.UserLandRequest_STRATEGY_FF_ONLY:
		return "", structerr.NewFailedPrecondition("branch %q cannot be fast-forwarded to commit", string(in.GetBranch())).
			WithDetail(&gitalypb.UserLandError{
				Error: &gitalypb.UserLandError_NotAncestor{
					NotAncestor: &gitalypb.NotAncestorError{
						ParentRevision: []byte(tip.String()),
						ChildRevision:  []byte(commitID.String()),
					},
				},
			})

	case gitalypb.UserLandRequest_STRATEGY_MERGE:
		mergeMessage := string(in.GetMergeMessage())
		if mergeMessage == "" {
			mergeMessage = fmt.Sprintf("Merge commit '%s' into %s", commitID, string(in.GetBranch()))
		}

		mergeCommitID, err := a.server.merge(ctx, a.repo,
			committer,
			committer,
			mergeMessage,
			tip.String(),
			commitID.String(),
			false,
			false,
		)
		if err != nil {
			var conflictErr *localrepo.MergeTreeConflictError
			if errors.As(err, &conflictErr) {
				return "", landMergeConflictError(tip, commitID, conflictingFileNames(conflictErr), conflictErr)
			}

			return "", structerr.NewInternal("merging commit: %w", err)
		}

		return git.ObjectID(mergeCommitID), nil

	case gitalypb.UserLandRequest_STRATEGY_REBASE_UNSPECIFIED:
		todo, err := plan.Replay()
		switch {
		case errors.Is(err, rebase.ErrMergesInRange):
			// Rebasing drops merge commits, so such a change can only be landed with STRATEGY_MERGE.
			return "", structerr.NewInvalidArgument("commit range contains merge commits, which cannot be rebased: use STRATEGY_MERGE")
		case errors.Is(err, rebase.ErrUnrelated):
			// Rebasing needs a merge base. Unrelated histories can only be landed with STRATEGY_MERGE.
			return "", structerr.NewInvalidArgument("commit shares no history with branch %q: use STRATEGY_MERGE to land unrelated history", string(in.GetBranch()))
		case err != nil:
			return "", structerr.NewInternal("planning the rebase: %w", err)
		}

		// Commits that are patch-equivalent to a commit of the tip's history are reapplied instead of
		// being dropped. The tip may have reverted such a commit since, and dropping it would lose its
		// changes while the landing succeeds. A reapplied commit whose changes the tip still holds
		// becomes empty and is dropped by the replay. One whose changes the tip modified further
		// conflicts, which is reported rather than guessed at.
		defer a.observe(landBatchPhaseReplay, time.Now())
		rebasedCommitID, err := a.rebaser.Replay(ctx, tip, todo, &committer)
		if err != nil {
			// The conflict error is passed as the cause itself, not wrapped, because structerr takes
			// the code of the innermost error.
			var conflictErr *localrepo.RebaseConflictError
			if errors.As(err, &conflictErr) {
				return "", landMergeConflictError(tip, commitID, conflictingFileNames(conflictErr.ConflictError), conflictErr)
			}

			return "", structerr.NewInternal("rebasing commit: %w", err)
		}

		return rebasedCommitID, nil

	default:
		return "", structerr.NewInvalidArgument("unknown strategy: %d", in.GetStrategy())
	}
}

// publish makes the objects that the attempt wrote durable before its compare-and-swap, so that the
// quarantine migration of the update only moves durable files. In the pack mode it packs every
// loose object of the quarantine into one fsynced pack (see packAttemptObjects); a failure fails the
// update of the attempt, and nothing is published. In the loose mode the objects were fsynced when
// they were written, and there is nothing to do.
func (a *landingAttempt) publish(ctx context.Context) error {
	if a.mode == landPublishLoose {
		return nil
	}

	defer a.observe(landBatchPhasePack, time.Now())

	repoPath, err := a.repo.Path(ctx)
	if err != nil {
		return structerr.NewInternal("getting quarantine path: %w", err)
	}

	objectDir := a.quarantineDir.QuarantinedRepo().GetGitObjectDirectory()
	if !filepath.IsAbs(objectDir) {
		objectDir = filepath.Join(repoPath, objectDir)
	}

	packed, err := packBatchObjects(ctx, a.repo, objectDir)
	if err != nil {
		return structerr.NewInternal("packing the objects of the attempt: %w", err)
	}
	landBatchPackedObjectsTotal.Add(float64(packed))

	return nil
}
