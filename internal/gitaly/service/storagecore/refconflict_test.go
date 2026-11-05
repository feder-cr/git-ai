package storagecore

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// conflictingPair imports the repository and makes the database hold
// refs/heads/a and refs/heads/a/b, as an older version could. It returns the
// repository ID and the value of both references.
func (h *harness) conflictingPair() (int64, git.ObjectID) {
	h.t.Helper()

	main := h.seed()
	id := h.importRepo()
	_, err := h.core.Meta.CommitChangeset(h.ctx, corestore.ChangesetInput{
		RepoID:    id,
		Updates:   []corestore.RefUpdate{{Name: "refs/heads/a", Expect: corestore.ExpectValue, New: corestore.Target(main)}},
		Operation: "test",
		Actor:     "other-node",
		NodeID:    "node-b",
	})
	require.NoError(h.t, err)
	coretest.CommitRefWithoutNameCheck(h.t, h.ctx, h.core.Meta, id, "refs/heads/a/b", corestore.Target(main))
	return id, main
}

// TestVerifyRepositoryNamesConflictingReferences checks that the operator is
// told what is wrong and what to do, instead of a failed catch-up.
func TestVerifyRepositoryNamesConflictingReferences(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeAuthoritative, dialect)
		_, main := h.conflictingPair()

		_, err := h.client.VerifyRepository(h.ctx, &gitalypb.VerifyRepositoryRequest{Repository: h.repo})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "error: %v", err)
		message := status.Convert(err).Message()
		require.Contains(t, message, `"refs/heads/a" at "`+main.String()+`"`)
		require.Contains(t, message, `"refs/heads/a/b" at "`+main.String()+`"`)
		require.Contains(t, message, "graft storage delete-conflicting-ref")
	})
}

// TestDeleteConflictingReference deletes each side of a pair: the repository
// recovers, the choice stays the operator's and the deletion is in the log.
func TestDeleteConflictingReference(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, tc := range []struct{ deleted, kept string }{
			{"refs/heads/a", "refs/heads/a/b"},
			{"refs/heads/a/b", "refs/heads/a"},
		} {
			t.Run(tc.deleted, func(t *testing.T) {
				h := setup(t, corestore.ModeAuthoritative, dialect)
				id, main := h.conflictingPair()
				before, err := h.core.Meta.GetRepository(h.ctx, corestore.KeyOf(h.repo))
				require.NoError(t, err)

				resp, err := h.client.DeleteConflictingReference(h.ctx, &gitalypb.DeleteConflictingReferenceRequest{
					Repository:     h.repo,
					Reference:      []byte(tc.deleted),
					ExpectedTarget: main.String(),
				})
				require.NoError(t, err)
				require.Equal(t, before.HeadSeq+1, resp.GetSequence())

				refs, _, err := h.core.Meta.Refs(h.ctx, id)
				require.NoError(t, err)
				require.NotContains(t, refs, tc.deleted)
				require.Equal(t, corestore.Target(main), refs[tc.kept])

				// The node catches up, and holds what the database holds.
				require.Empty(t, h.verify(false).GetDivergences())
				local := h.localRefs()
				require.NotContains(t, local, tc.deleted)
				require.Equal(t, corestore.Target(main), local[tc.kept])

				changesets, err := h.core.Meta.ListChangesets(h.ctx, id, before.HeadSeq, resp.GetSequence())
				require.NoError(t, err)
				require.Len(t, changesets, 1)
				require.Equal(t, deleteConflictingOperation, changesets[0].Operation)
				require.Equal(t, []corestore.AppliedUpdate{{Name: tc.deleted, Old: corestore.Target(main)}}, changesets[0].Updates)

				entries, err := h.core.Meta.ScanChangelog(h.ctx, id, 0, 0)
				require.NoError(t, err)
				require.Equal(t, tc.deleted, entries[len(entries)-1].Reference, "the deletion is in the changelog")
				require.Equal(t, deleteConflictingOperation, entries[len(entries)-1].Operation)
			})
		}
	})
}

// TestDeleteConflictingReferenceRefusals covers what the RPC refuses: it deletes
// no reference that is not part of a pair, and none that has another value.
func TestDeleteConflictingReferenceRefusals(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h := setup(t, corestore.ModeAuthoritative, dialect)
		id, main := h.conflictingPair()
		before, _, err := h.core.Meta.Refs(h.ctx, id)
		require.NoError(t, err)

		for _, tc := range []struct {
			desc string
			req  *gitalypb.DeleteConflictingReferenceRequest
			code codes.Code
		}{
			{
				desc: "no repository",
				req:  &gitalypb.DeleteConflictingReferenceRequest{Reference: []byte("refs/heads/a"), ExpectedTarget: main.String()},
				code: codes.InvalidArgument,
			},
			{
				desc: "invalid reference name",
				req:  &gitalypb.DeleteConflictingReferenceRequest{Repository: h.repo, Reference: []byte("refs/heads/a..b"), ExpectedTarget: main.String()},
				code: codes.InvalidArgument,
			},
			{
				desc: "no expected target",
				req:  &gitalypb.DeleteConflictingReferenceRequest{Repository: h.repo, Reference: []byte("refs/heads/a")},
				code: codes.InvalidArgument,
			},
			{
				desc: "a reference outside any pair",
				req:  &gitalypb.DeleteConflictingReferenceRequest{Repository: h.repo, Reference: []byte("refs/heads/main"), ExpectedTarget: main.String()},
				code: codes.FailedPrecondition,
			},
			{
				desc: "an absent reference",
				req:  &gitalypb.DeleteConflictingReferenceRequest{Repository: h.repo, Reference: []byte("refs/heads/a/b/c"), ExpectedTarget: main.String()},
				code: codes.FailedPrecondition,
			},
			{
				desc: "another value than expected",
				req:  &gitalypb.DeleteConflictingReferenceRequest{Repository: h.repo, Reference: []byte("refs/heads/a"), ExpectedTarget: zeroOID()},
				code: codes.FailedPrecondition,
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				_, err := h.client.DeleteConflictingReference(h.ctx, tc.req)
				require.Equal(t, tc.code, status.Code(err), "error: %v", err)

				after, _, err := h.core.Meta.Refs(h.ctx, id)
				require.NoError(t, err)
				require.Equal(t, before, after, "nothing is deleted")
			})
		}
	})
}

// TestDeleteConflictingReferenceOnlyInAuthoritativeMode covers the other modes.
// In shadow mode the database does hold a pair and the request is otherwise
// valid, so only the mode refuses it.
func TestDeleteConflictingReferenceOnlyInAuthoritativeMode(t *testing.T) {
	t.Run(string(corestore.ModeOff), func(t *testing.T) {
		h := setup(t, corestore.ModeOff, sqlstore.SQLite)
		_, err := h.client.DeleteConflictingReference(h.ctx, &gitalypb.DeleteConflictingReferenceRequest{
			Repository:     h.repo,
			Reference:      []byte("refs/heads/a"),
			ExpectedTarget: zeroOID(),
		})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "error: %v", err)
	})

	t.Run(string(corestore.ModeShadow), func(t *testing.T) {
		h := setup(t, corestore.ModeShadow, sqlstore.SQLite)
		id, main := h.conflictingPair()

		_, err := h.client.DeleteConflictingReference(h.ctx, &gitalypb.DeleteConflictingReferenceRequest{
			Repository:     h.repo,
			Reference:      []byte("refs/heads/a"),
			ExpectedTarget: main.String(),
		})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "error: %v", err)
		require.Contains(t, status.Convert(err).Message(), "authoritative mode")

		refs, _, err := h.core.Meta.Refs(h.ctx, id)
		require.NoError(t, err)
		require.Contains(t, refs, "refs/heads/a", "nothing is deleted")
	})
}

// TestDeleteConflictingReferenceOnTheNodeThatKeepsTheOtherSide covers the usual repair: the node
// caught up one side of the pair before an older version wrote the other, so it is stuck, and the
// operator deletes the other side. The node recovers with the side it holds.
func TestDeleteConflictingReferenceOnTheNodeThatKeepsTheOtherSide(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, tc := range []struct{ kept, deleted string }{
			{"refs/heads/a", "refs/heads/a/b"},
			{"refs/heads/a/b", "refs/heads/a"},
		} {
			t.Run(tc.kept, func(t *testing.T) {
				h := setup(t, corestore.ModeAuthoritative, dialect)
				main := h.seed()
				id := h.importRepo()
				_, err := h.core.Meta.CommitChangeset(h.ctx, corestore.ChangesetInput{
					RepoID:    id,
					Updates:   []corestore.RefUpdate{{Name: tc.kept, Expect: corestore.ExpectValue, New: corestore.Target(main)}},
					Operation: "test", Actor: "other-node", NodeID: "node-b",
				})
				require.NoError(t, err)
				require.Empty(t, h.verify(false).GetDivergences())
				require.Contains(t, h.localRefs(), tc.kept)

				coretest.CommitRefWithoutNameCheck(t, h.ctx, h.core.Meta, id, tc.deleted, corestore.Target(main))
				_, err = h.client.VerifyRepository(h.ctx, &gitalypb.VerifyRepositoryRequest{Repository: h.repo})
				require.Equal(t, codes.FailedPrecondition, status.Code(err), "the node is stuck: %v", err)

				_, err = h.client.DeleteConflictingReference(h.ctx, &gitalypb.DeleteConflictingReferenceRequest{
					Repository: h.repo, Reference: []byte(tc.deleted), ExpectedTarget: main.String(),
				})
				require.NoError(t, err)

				resp, err := h.client.VerifyRepository(h.ctx, &gitalypb.VerifyRepositoryRequest{Repository: h.repo})
				require.NoError(t, err, "the node recovers after the repair")
				require.Empty(t, resp.GetDivergences())
				local := h.localRefs()
				require.Contains(t, local, tc.kept)
				require.NotContains(t, local, tc.deleted)
			})
		}
	})
}
