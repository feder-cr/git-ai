package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// runDuringCommit runs fn once, while the next write that reaches the database
// has passed every local check and uploaded its pack, and has not committed. That
// is the window in which another node's write is invisible to the first one. The
// hook is a package variable shared by every server of the process, so this test
// does not run in parallel with the other tests of the package.
func runDuringCommit(t *testing.T, fn func()) {
	t.Helper()

	fired := false
	voter.FaultHook = func(point string) error {
		if point != "after_upload" || fired {
			return nil
		}
		fired = true
		fn()
		return nil
	}
	t.Cleanup(func() { voter.FaultHook = nil })
}

// TestStorageConflictingNamesAcrossNodes races real writes of two nodes that
// create the two sides of a file/directory conflict. Each node's git accepts its
// own, as the other's is not in its copy yet. The database lets one through; the
// loser's client gets a refusal, and the repository stays served by both nodes.
func TestStorageConflictingNamesAcrossNodes(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			graft, tokens := workerGraft(2)
			cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, unbatched(graft))
			a := cluster.addNode(t)
			b := cluster.addNode(t)

			rel := a.createRepository(t).GetRelativePath()
			onA := a.agent(t, rel, tokens[workerName(0)])
			onB := b.agent(t, rel, tokens[workerName(1)])

			base, err := commitFile(a.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
			require.NoError(t, err)
			_, err = onB.ResolveRevision(b.ctx, "main")
			require.NoError(t, err)

			for _, tc := range []struct{ desc, winner, loser string }{
				{"the child loses to the parent", "race-a", "race-a/child"},
				{"the parent loses to the child", "race-b/child", "race-b"},
			} {
				t.Run(tc.desc, func(t *testing.T) {
					// A write through UserCommitFiles, whose reference transaction the server
					// drives itself: the client gets the code of git's own refusal.
					var winnerErr error
					runDuringCommit(t, func() {
						_, winnerErr = commitFile(a.ctx, onA, workerName(0), tc.winner, base, "a.txt", "a\n")
					})
					_, err := commitFile(b.ctx, onB, workerName(1), tc.loser, base, "b.txt", "b\n")
					voter.FaultHook = nil

					require.NoError(t, winnerErr)
					require.Equal(t, codes.FailedPrecondition, sdk.Code(err), "error: %v", err)
					require.Contains(t, a.dbRefs(t, rel), "refs/heads/"+tc.winner)
					require.NotContains(t, a.dbRefs(t, rel), "refs/heads/"+tc.loser)

					// Both nodes go on serving the repository, the winner's write included.
					for _, repo := range []*sdk.Repo{onA, onB} {
						_, err := repo.ResolveRevision(a.ctx, tc.winner)
						require.NoError(t, err)
					}
					a.requireConsistent(t, rel)
					b.requireConsistent(t, rel)
				})
			}

			// A write through a git process, which runs the hook itself and reports its
			// failure on stderr only: the reference is refused all the same, with the code
			// that this RPC gives git's own file/directory conflict.
			t.Run("through a git process", func(t *testing.T) {
				var winnerErr error
				runDuringCommit(t, func() {
					_, winnerErr = commitFile(a.ctx, onA, workerName(0), "race-c", base, "a.txt", "a\n")
				})

				stream, err := gitalypb.NewRefServiceClient(b.conn(t)).UpdateReferences(b.ctx)
				require.NoError(t, err)
				require.NoError(t, stream.Send(&gitalypb.UpdateReferencesRequest{
					Repository: b.repository(rel),
					Updates: []*gitalypb.UpdateReferencesRequest_Update{{
						Reference:   []byte("refs/heads/race-c/child"),
						OldObjectId: []byte(gittest.DefaultObjectHash.ZeroOID.String()),
						NewObjectId: []byte(base),
					}},
				}))
				_, err = stream.CloseAndRecv()
				voter.FaultHook = nil

				require.NoError(t, winnerErr)
				require.Equal(t, codes.Internal, status.Code(err), "error: %v", err)
				require.NotContains(t, a.dbRefs(t, rel), "refs/heads/race-c/child")

				_, err = onB.ResolveRevision(b.ctx, "race-c")
				require.NoError(t, err)
				a.requireConsistent(t, rel)
				b.requireConsistent(t, rel)
			})
		})
	}
}

// TestStorageConflictingNameFailsTheWholePush pushes, without --atomic, a branch
// that conflicts only with a branch another node committed meanwhile, together
// with a branch that conflicts with nothing. git on the pushing node accepts
// both. The database refuses the changeset, and git's transaction holds both
// updates, so the whole push fails: neither branch lands, unlike on a plain git
// server where only the conflicting one would be refused. Pushed again, the
// branch without a conflict lands.
func TestStorageConflictingNameFailsTheWholePush(t *testing.T) {
	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			graft, tokens := workerGraft(2)
			graft.AgentRPCs = []string{"/gitaly.SSHService/SSHReceivePack"}
			cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, unbatched(graft))
			a := cluster.addNode(t)
			b := cluster.addNode(t)

			rel := a.createRepository(t).GetRelativePath()
			onA := a.agent(t, rel, tokens[workerName(0)])
			base, err := commitFile(a.ctx, onA, workerName(0), "main", "", "README.md", "base\n")
			require.NoError(t, err)
			_, err = b.agent(t, rel, tokens[workerName(1)]).ResolveRevision(b.ctx, "main")
			require.NoError(t, err)

			client := newPushClient(t, b, rel, "client", workerName(1), tokens[workerName(1)])
			pushed := client.commit("pushed.txt")

			var winnerErr error
			runDuringCommit(t, func() {
				_, winnerErr = commitFile(a.ctx, onA, workerName(0), "race", base, "a.txt", "a\n")
			})
			out, ok := client.push("HEAD:refs/heads/race/child", "HEAD:refs/heads/unrelated")
			voter.FaultHook = nil

			require.NoError(t, winnerErr)
			require.False(t, ok, "the push fails: %s", out)
			// receive-pack itself dies, as for any refusal of the database: the client gets
			// no status line per reference. git gives the reference-transaction hook no side
			// band, so the reason reaches an SSH client as a plain line.
			require.Contains(t, out, "fatal: in 'prepared' phase, update aborted by the reference-transaction hook")
			require.Regexp(t, `(?m)^refusing refs/heads/race/child: git cannot hold it next to refs/heads/race; push the other references without it$`, out)
			require.NotContains(t, out, "[new branch]")
			refs := a.dbRefs(t, rel)
			require.Contains(t, refs, "refs/heads/race")
			require.NotContains(t, refs, "refs/heads/race/child")
			require.NotContains(t, refs, "refs/heads/unrelated", "the other update of the push does not land either")
			a.requireConsistent(t, rel)
			b.requireConsistent(t, rel)

			out, ok = client.push("HEAD:refs/heads/unrelated")
			require.True(t, ok, out)
			require.Equal(t, corestore.Target(pushed), a.dbRefs(t, rel)["refs/heads/unrelated"])
			b.requireConsistent(t, rel)
		})
	}
}
