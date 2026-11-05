package sqlstore_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// BenchmarkRefNameCheck measures what the refusal of names git cannot hold together costs: a
// commit that creates one reference and a commit that updates one, which the commit's check
// does not read for, and the read of the update hook for one created name. The repository holds
// benchRefs references, under the directory of the created ones too.
func BenchmarkRefNameCheck(b *testing.B) {
	const benchRefs = 10000

	for _, dialect := range coretest.Dialects() {
		b.Run(string(dialect), func(b *testing.B) {
			ctx := testhelper.Context(b)
			s := openStore(b, ctx, coretest.DatabaseURL(b, dialect))
			repo, _, err := s.RegisterRepository(ctx, testKey("bench"), "sha1", corestore.InitialState{})
			require.NoError(b, err)

			var fill []corestore.RefUpdate
			for i := 0; i < benchRefs; i++ {
				fill = append(fill, create(fmt.Sprintf("refs/heads/team/existing-%05d", i), oid(1)))
				if len(fill) == 500 {
					_, err := commit(ctx, s, repo.ID, fill...)
					require.NoError(b, err)
					fill = nil
				}
			}
			_, err = commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
			require.NoError(b, err)

			// The testing package runs each benchmark function more than once, on the same database.
			created, current := 0, oid(1)
			b.Run("commit-created", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					created++
					_, err := commit(ctx, s, repo.ID, create(fmt.Sprintf("refs/heads/team/new-%09d", created), oid(2)))
					if err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("commit-updated", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					next := oid(3)
					if current == next {
						next = oid(4)
					}
					if _, err := commit(ctx, s, repo.ID, update("refs/heads/main", current, next)); err != nil {
						b.Fatal(err)
					}
					current = next
				}
			})
			b.Run("update-hook-read", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if _, err := s.GetRepository(ctx, testKey("bench")); err != nil {
						b.Fatal(err)
					}
					if _, _, err := s.ConflictingRef(ctx, repo.ID, fmt.Sprintf("refs/heads/team/probe-%09d", i)); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
