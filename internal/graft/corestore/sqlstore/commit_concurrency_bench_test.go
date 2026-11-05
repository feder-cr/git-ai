package sqlstore_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// BenchmarkCommitChangesetConcurrent measures the changesets per second that the database commits
// for concurrent writers, without git: each writer commits changesets of one reference update with
// a changelog, as a land does, either each in a repository of its own ("repos") or all in one
// repository, each on a branch of its own ("branches"). The difference between the two is what the
// database itself serializes per repository. It reports tx/s; b.N is the total number of commits.
func BenchmarkCommitChangesetConcurrent(b *testing.B) {
	for _, dialect := range coretest.Dialects() {
		for _, layout := range []string{"repos", "branches"} {
			for _, writers := range []int{1, 4, 16, 64} {
				b.Run(fmt.Sprintf("%s/%s/%d", dialect, layout, writers), func(b *testing.B) {
					ctx := testhelper.Context(b)
					s := openStore(b, ctx, coretest.DatabaseURL(b, dialect))

					repoIDs := make([]int64, writers)
					for w := range repoIDs {
						if layout == "branches" && w > 0 {
							repoIDs[w] = repoIDs[0]
							continue
						}
						repo, _, err := s.RegisterRepository(ctx, testKey(fmt.Sprint("bench-", w)), "sha1", corestore.InitialState{})
						if err != nil {
							b.Fatal(err)
						}
						repoIDs[w] = repo.ID
					}

					var next atomic.Int64
					var mu sync.Mutex
					var failed error
					var wg sync.WaitGroup
					b.ResetTimer()
					start := time.Now()
					for w := 0; w < writers; w++ {
						wg.Add(1)
						go func(w int) {
							defer wg.Done()
							branch := fmt.Sprintf("refs/heads/agent-%d", w)
							old := corestore.Target("")
							for i := 0; next.Add(1) <= int64(b.N); i++ {
								tip := oid(10000000*(w+1) + i)
								update := corestore.RefUpdate{Name: branch, Expect: corestore.ExpectValue, Old: old, New: tip}
								if _, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
									RepoID:    repoIDs[w],
									Updates:   []corestore.RefUpdate{update},
									Operation: "land_batch",
									Actor:     "graft-batch",
									NodeID:    "node-a",
									Changelog: &changelog.Meta{Operation: "land_batch", Actor: "graft-batch"},
								}); err != nil {
									mu.Lock()
									failed = err
									mu.Unlock()
									return
								}
								old = tip
							}
						}(w)
					}
					wg.Wait()
					elapsed := time.Since(start)
					b.StopTimer()
					if failed != nil {
						b.Fatal(failed)
					}
					b.ReportMetric(float64(b.N)/elapsed.Seconds(), "tx/s")
				})
			}
		}
	}
}
