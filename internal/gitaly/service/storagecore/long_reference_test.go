package storagecore

import (
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRefuseLongReferenceNames(t *testing.T) {
	t.Parallel()

	name := func(i, length int) string {
		prefix := fmt.Sprintf("refs/heads/%d", i)
		return prefix + strings.Repeat("x", length-len(prefix))
	}
	oid := corestore.Target(strings.Repeat("1", 40))

	require.NoError(t, refuseLongReferenceNames(map[string]corestore.Target{
		name(0, 1024): oid,
		"HEAD":        corestore.Target(corestore.SymbolicPrefix + name(0, 1024)),
	}))

	err := refuseLongReferenceNames(map[string]corestore.Target{
		"HEAD":        corestore.Target(corestore.SymbolicPrefix + name(9, 1025)),
		name(0, 1024): oid,
		name(1, 1025): oid,
		name(2, 2000): oid,
		name(3, 1025): oid,
		name(4, 1025): oid,
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "5 references have a name or symbolic target over the limit of 1024 bytes")
	require.ErrorContains(t, err, "refs/heads/1xxx")
	require.NotContains(t, err.Error(), "refs/heads/4xxx", "only the first few are quoted")
	require.NotContains(t, err.Error(), "resume", "retrying does not help")
	require.Less(t, len(err.Error()), 512, "the names are truncated")
}

// TestImportRepository_longReferenceName seeds the long name directly in packed-refs.
func TestImportRepository_longReferenceName(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect)
		h.seed()

		long := gittest.ReferenceNameOfLength(t, "refs/heads/", 1025)
		gittest.SeedPackedReference(t, h.repoPath, long, gittest.ResolveRevision(t, h.cfg, h.repoPath, "HEAD"))

		_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
		require.ErrorContains(t, err, "1 references have a name or symbolic target over the limit of 1024 bytes")

		// Nothing was registered, so no pack was uploaded and the writes that clean up still work.
		_, err = h.core.Meta.GetRepository(h.ctx, corestore.KeyOf(h.repo))
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)

		gittest.Exec(t, h.cfg, "-C", h.repoPath, "update-ref", "-d", long.String())

		res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.NotZero(t, res.GetSequence())
	})
}

// TestImportRepository_longReferenceAfterACompleteImport: a repeated import only brings history, so a
// repository imported before the limit is not refused.
func TestImportRepository_longReferenceAfterACompleteImport(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect)
		h.seed()
		h.importRepo()

		gittest.SeedPackedReference(t, h.repoPath, gittest.ReferenceNameOfLength(t, "refs/heads/", 1025), gittest.ResolveRevision(t, h.cfg, h.repoPath, "HEAD"))

		_, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
	})
}

// TestImportRepository_longReferenceWithAnIncompleteRegistration is the way out documented in
// doc/graft/LIMITATIONS.md: an earlier failed import left a registration, the refusal still holds, and
// deleting the name with git on the node lets the import resume.
func TestImportRepository_longReferenceWithAnIncompleteRegistration(t *testing.T) {
	forEachCore(t, func(t *testing.T, mode corestore.Mode, dialect sqlstore.Dialect) {
		h := setup(t, mode, dialect)
		h.seed()

		key := corestore.KeyOf(h.repo)
		_, _, err := h.core.Meta.RegisterRepository(h.ctx, key, gittest.DefaultObjectHash.Format, corestore.InitialState{})
		require.NoError(t, err)

		long := gittest.ReferenceNameOfLength(t, "refs/heads/", 1025)
		gittest.SeedPackedReference(t, h.repoPath, long, gittest.ResolveRevision(t, h.cfg, h.repoPath, "HEAD"))

		_, err = h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
		require.ErrorContains(t, err, "over the limit of 1024 bytes")

		gittest.Exec(t, h.cfg, "-C", h.repoPath, "update-ref", "--no-deref", "-d", long.String())

		res, err := h.client.ImportRepository(h.ctx, &gitalypb.ImportRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.NotZero(t, res.GetSequence())
	})
}
