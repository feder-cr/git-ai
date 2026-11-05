package storagecore

import (
	"context"
	"errors"
	"strings"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// VerifyRepository implements gitalypb.StorageCoreServiceServer. In
// authoritative mode the local repository is caught up first (or hydrated when
// this node has no copy), so that a divergence reports a real inconsistency of
// the local repository rather than a delay of the catch-up. A catch-up that
// fails because the database holds references git cannot hold together is
// reported as such, with the way out (refNameConflictError).
//
// With check_connectivity it also runs `git fsck --connectivity-only` on the
// local repository and checks that the packs of the database's changesets hold
// every object reachable from the database references (checkStoredObjects, a
// dry hydration that downloads every pack of the repository); a failure of
// either is reported in connectivity_error.
//
// The comparison is not atomic with concurrent writes: a reference written
// between the database read and the local read can show up as a divergence.
// Verification is meant for a quiet repository, or to be repeated.
func (s *server) VerifyRepository(ctx context.Context, req *gitalypb.VerifyRepositoryRequest) (*gitalypb.VerifyRepositoryResponse, error) {
	core, err := s.enabledCore()
	if err != nil {
		return nil, err
	}

	repo := req.GetRepository()
	if err := s.validateRepository(ctx, repo, storage.WithSkipRepositoryExistenceCheck()); err != nil {
		return nil, err
	}

	if core.Authoritative() {
		if err := s.deps.GetGraftStorageSyncer().EnsureFresh(ctx, repo); err != nil {
			if conflictErr := refNameConflictError(ctx, core, repo); conflictErr != nil {
				return nil, conflictErr
			}
			return nil, err
		}
	}

	record, err := core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return nil, structerr.NewFailedPrecondition("repository is not imported into the storage core")
	}
	if err != nil {
		return nil, structerr.NewUnavailable("storage core: %w", err)
	}
	if record.HeadSeq == 0 {
		// An incomplete import (IMPORT RULE): the database has no reference yet.
		// Authoritative mode stopped at EnsureFresh already.
		return nil, structerr.NewFailedPrecondition("repository import is not complete; run ImportRepository again")
	}

	if err := s.deps.GetLocator().ValidateRepository(ctx, repo); err != nil {
		return nil, err
	}

	dbRefs, headSeq, err := core.Meta.Refs(ctx, record.ID)
	if err != nil {
		return nil, structerr.NewUnavailable("storage core: %w", err)
	}

	local := s.localRepo(repo)
	localRefs, err := gitops.ReadRefs(ctx, local)
	if err != nil {
		return nil, structerr.NewInternal("read references: %w", err)
	}

	resp := &gitalypb.VerifyRepositoryResponse{HeadSequence: headSeq}
	// An absent reference reads as the empty Target on both sides, so a
	// reference missing from one side is a divergence like any other. The
	// divergences are sorted by name.
	for _, name := range sortedNames(localRefs, dbRefs) {
		localTarget, dbTarget := localRefs[name], dbRefs[name]
		if localTarget == dbTarget {
			continue
		}
		resp.Divergences = append(resp.Divergences, &gitalypb.VerifyRepositoryResponse_ReferenceDivergence{
			Reference:      []byte(name),
			LocalTarget:    string(localTarget),
			DatabaseTarget: string(dbTarget),
		})
	}

	if req.GetCheckConnectivity() {
		ok, msg, err := checkConnectivity(ctx, local)
		if err != nil {
			return nil, err
		}

		// Equal references are not enough: the object storage must also hold
		// every object they reach, or a hydration on another node fails.
		storedOK, storedMsg, err := s.checkStoredObjects(ctx, core, repo, record)
		if err != nil {
			return nil, err
		}
		if !storedOK {
			ok = false
			msg = strings.TrimSpace(msg + "\nstorage core objects: " + storedMsg)
		}

		resp.ConnectivityOk, resp.ConnectivityError = ok, msg
	}

	return resp, nil
}
