package sqlstore_test

import (
	"context"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/stretchr/testify/require"
)

func nameOfLength(length int) string {
	return "refs/heads/" + strings.Repeat("x", length-len("refs/heads/"))
}

func TestCommitChangeset_referenceNameLength(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("length"))

		_, err := commit(ctx, s, repo.ID, create(nameOfLength(1025), oid(1)))
		require.ErrorContains(t, err, "over the limit of 1024 bytes")

		_, err = commit(ctx, s, repo.ID, create("refs/heads/alias", corestore.Target(corestore.SymbolicPrefix+nameOfLength(1025))))
		require.ErrorContains(t, err, "over the limit of 1024 bytes")

		refs, _, err := s.Refs(ctx, repo.ID)
		require.NoError(t, err)
		require.Empty(t, refs)

		_, err = commit(ctx, s, repo.ID,
			create(nameOfLength(1024), oid(1)),
			create("refs/heads/alias", corestore.Target(corestore.SymbolicPrefix+nameOfLength(1024))),
		)
		require.NoError(t, err)

		refs, _, err = s.Refs(ctx, repo.ID)
		require.NoError(t, err)
		require.Len(t, refs, 2)
	})
}

// TestCommitChangeset_deleteExistingLongReference deletes a row that a direct insert made, as a reference
// recorded before the limit is: no git process and no changeset writes the name.
func TestCommitChangeset_deleteExistingLongReference(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		repo := register(t, ctx, s, testKey("delete-long"))
		name := nameOfLength(1025)
		require.NoError(t, s.SeedReference(ctx, repo.ID, name, oid(1)))

		refs, _, err := s.Refs(ctx, repo.ID)
		require.NoError(t, err)
		require.Contains(t, refs, name, "an existing long reference stays readable")

		_, err = commit(ctx, s, repo.ID, update(name, oid(1), oid(2)))
		require.ErrorContains(t, err, "over the limit of 1024 bytes", "an update is a write")

		_, err = commit(ctx, s, repo.ID, update(name, oid(1), ""))
		require.NoError(t, err)

		refs, _, err = s.Refs(ctx, repo.ID)
		require.NoError(t, err)
		require.NotContains(t, refs, name)
	})
}
