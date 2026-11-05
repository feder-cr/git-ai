package storagecore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	// deleteConflictingOperation is the operation of the changeset that deletes a
	// reference of a conflicting pair.
	deleteConflictingOperation = "delete-conflicting-reference"
	// maxReportedConflicts bounds the pairs named by the error of a repository
	// that holds conflicting references.
	maxReportedConflicts = 10
)

// refNameConflictError returns the error of a catch-up that failed because the
// database holds references git cannot hold together, naming the pairs and the
// way out (doc/graft/STORAGE.md, Failures), and nil when the database holds no
// such pair or cannot tell: the caller then returns the error of the catch-up.
func refNameConflictError(ctx context.Context, core *corestore.Core, repo *gitalypb.Repository) error {
	record, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	if err != nil {
		return nil
	}
	refs, _, err := core.Meta.Refs(ctx, record.ID)
	if err != nil {
		return nil
	}
	conflicts := corestore.RefNameConflicts(refs)
	if len(conflicts) == 0 {
		return nil
	}

	var pairs []string
	for _, c := range conflicts[:min(len(conflicts), maxReportedConflicts)] {
		pairs = append(pairs, fmt.Sprintf("%q at %q and %q at %q", c.Parent, refs[c.Parent], c.Child, refs[c.Child]))
	}
	return structerr.NewFailedPrecondition(
		"the database holds %d pair(s) of references that git cannot hold together, so no node can catch up or hydrate this repository: %s; "+
			"keep what you need of the side you choose, then delete it with: graft storage delete-conflicting-ref REFERENCE EXPECTED_TARGET",
		len(conflicts), strings.Join(pairs, "; "))
}

// DeleteConflictingReference implements gitalypb.StorageCoreServiceServer. It is
// the operator's way out of a repository that holds a pair of references git
// cannot hold together, which only an older version can have written.
//
// The check that the reference is part of a pair and the compare-and-swap of its
// value run in the transaction of the commit (ChangesetInput.OnlyConflictingNames),
// so two operators who delete the two sides of a pair at once delete one: the
// second finds no pair left and is refused.
func (s *server) DeleteConflictingReference(ctx context.Context, req *gitalypb.DeleteConflictingReferenceRequest) (*gitalypb.DeleteConflictingReferenceResponse, error) {
	repo := req.GetRepository()
	if err := s.validateRepository(ctx, repo, storage.WithSkipRepositoryExistenceCheck()); err != nil {
		return nil, err
	}
	reference := string(req.GetReference())
	if err := gitops.ValidateName(reference); err != nil {
		return nil, structerr.NewInvalidArgument("reference: %w", err)
	}
	expected := corestore.Target(req.GetExpectedTarget())
	if expected.Absent() {
		return nil, structerr.NewInvalidArgument("empty expected target")
	}

	core, err := s.enabledCore()
	if err != nil {
		return nil, err
	}
	if !core.Authoritative() {
		// In shadow mode no node catches up from the database, so nothing is stuck;
		// the local references are what the database must learn.
		return nil, structerr.NewFailedPrecondition("a conflicting reference is only deleted in authoritative mode")
	}

	record, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return nil, structerr.NewFailedPrecondition("repository is not imported into the storage core")
	}
	if err != nil {
		return nil, structerr.NewUnavailable("storage core: %w", err)
	}

	actor := identity.Actor(ctx, deleteConflictingOperation)
	res, err := core.Meta.CommitChangeset(ctx, corestore.ChangesetInput{
		RepoID:               record.ID,
		Updates:              []corestore.RefUpdate{{Name: reference, Expect: corestore.ExpectValue, Old: expected}},
		OnlyConflictingNames: true,
		Operation:            deleteConflictingOperation,
		Actor:                actor,
		NodeID:               core.NodeID,
		Changelog:            &changelog.Meta{Operation: deleteConflictingOperation, Actor: actor},
	})
	if err != nil {
		var conflict *corestore.ErrRefConflict
		switch {
		case errors.Is(err, corestore.ErrRefNotConflicting):
			return nil, structerr.NewFailedPrecondition("%w", err)
		case errors.As(err, &conflict), errors.Is(err, corestore.ErrReviewRootInUse):
			return nil, structerr.NewFailedPrecondition("%w", err)
		case errors.Is(err, corestore.ErrRepositoryNotFound):
			return nil, structerr.NewFailedPrecondition("repository is not imported into the storage core")
		default:
			return nil, structerr.NewUnavailable("storage core: %w", err)
		}
	}
	if len(res.Changelog) > 0 {
		corestore.NotifyChangelog(record.ID)
	}

	s.deps.GetLogger().WithFields(log.Fields{
		"repo_id":   record.ID,
		"reference": reference,
		"target":    string(expected),
		"actor":     actor,
		"sequence":  res.Seq,
	}).Warn("storage core: deleted a reference of a conflicting pair")

	return &gitalypb.DeleteConflictingReferenceResponse{Sequence: res.Seq}, nil
}
