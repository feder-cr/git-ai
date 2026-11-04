package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const staleCreateURL = "git@localhost:graft/push.git"

// staleCreateClient is a git client that pushes to one node over gitaly-ssh.
type staleCreateClient struct {
	t    *testing.T
	node storageNode
	path string
	env  []string
}

// newPushClient clones the repository rel of node into a new client named name,
// which pushes to node over gitaly-ssh as the agent who with token.
func newPushClient(t *testing.T, node storageNode, rel, name, who, token string) staleCreateClient {
	t.Helper()

	payload, err := protojson.Marshal(&gitalypb.SSHReceivePackRequest{
		Repository: &gitalypb.Repository{
			StorageName:  node.cfg.Storages[0].Name,
			RelativePath: rel,
			GlRepository: "graft:" + node.cfg.Storages[0].Name + "/" + rel,
		},
		GlId:         who,
		GlUsername:   who,
		GlRepository: "graft:" + node.cfg.Storages[0].Name + "/" + rel,
	})
	require.NoError(t, err)

	var flags []string
	for flag, value := range featureflag.FromContext(node.ctx) {
		flags = append(flags, flag.FormatWithValue(value))
	}

	path := filepath.Join(t.TempDir(), name)
	gittest.Exec(t, node.cfg, "clone", "--quiet", node.repoPath(rel), path)
	return staleCreateClient{t: t, node: node, path: path, env: []string{
		"GITALY_PAYLOAD=" + string(payload),
		"GITALY_ADDRESS=" + node.cfg.SocketPath,
		"GITALY_TOKEN=" + token,
		"GITALY_FEATUREFLAGS=" + strings.Join(flags, ","),
		fmt.Sprintf("GIT_SSH_COMMAND=%s receive-pack", node.cfg.BinaryPath("gitaly-ssh")),
	}}
}

// push runs git push with the refspecs and returns its output and whether it
// succeeded.
func (c staleCreateClient) push(refspecs ...string) (string, bool) {
	c.t.Helper()

	cmd := gittest.NewCommand(c.t, c.node.cfg, append([]string{"-C", c.path, "push", staleCreateURL}, refspecs...)...)
	cmd.Env = append(cmd.Env, c.env...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// commit writes a new commit in the client and returns its object ID.
func (c staleCreateClient) commit(file string) string {
	c.t.Helper()

	require.NoError(c.t, os.WriteFile(filepath.Join(c.path, file), []byte(file+"\n"), 0o644))
	gittest.Exec(c.t, c.node.cfg, "-C", c.path, "add", file)
	gittest.Exec(c.t, c.node.cfg, "-C", c.path, "-c", "user.name=b", "-c", "user.email=b@example.com", "commit", "--quiet", "-m", file)
	return strings.TrimSpace(string(gittest.Exec(c.t, c.node.cfg, "-C", c.path, "rev-parse", "HEAD")))
}

// losesTheRaceTo makes every push of c run a push of the other client after c
// read the references of the server and before it sends its commands: c's
// commands are then based on references that changed, as when two clients push
// at the same time.
func (c staleCreateClient) losesTheRaceTo(other staleCreateClient, refspec string) {
	c.t.Helper()

	hooks := filepath.Join(c.path, ".git", "hooks")
	require.NoError(c.t, os.MkdirAll(hooks, 0o755))
	script := fmt.Sprintf("#!/bin/sh\nunset GIT_DIR GIT_WORK_TREE GIT_PREFIX\nexec git -C %s push %s %s\n", other.path, staleCreateURL, refspec)
	require.NoError(c.t, os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(script), 0o755))
}

// TestStorageStaleCreateDoesNotOverwriteTheBranch races two clients to create
// one branch. A non-atomic receive-pack lets the loser's update fail on its own,
// and git still feeds that update to the reference-transaction hook. Its zero old
// value there means "must not exist", as receive-pack always sends the old value,
// so it must never be recorded as a forced update: the database would hold the
// loser's commit, and every node would lose the winner's on its next catch-up.
func TestStorageStaleCreateDoesNotOverwriteTheBranch(t *testing.T) {
	t.Parallel()

	for _, mode := range []corestore.Mode{corestore.ModeAuthoritative, corestore.ModeShadow} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				who := workerName(0)
				graft, tokens := workerGraft(1)
				graft.AgentRPCs = []string{"/gitaly.SSHService/SSHReceivePack"}
				cluster := newStorageCluster(t, dialect, mode, graft)
				a := cluster.addNode(t)
				rel := a.createRepository(t).GetRelativePath()

				client := func(name string) staleCreateClient {
					return newPushClient(t, a, rel, name, who, tokens[who])
				}

				winner := client("winner")
				base := winner.commit("base.txt")
				out, ok := winner.push("HEAD:refs/heads/main")
				require.True(t, ok, out)
				if mode == corestore.ModeShadow {
					_, err := a.operator(t, rel).StorageImport(a.ctx)
					require.NoError(t, err)
				}

				loser := client("loser")
				loserCommit := loser.commit("loser.txt")
				winnerCommit := winner.commit("winner.txt")
				loser.losesTheRaceTo(winner, "HEAD:refs/heads/race")

				out, ok = loser.push("HEAD:refs/heads/race")
				require.False(t, ok, "the loser's push is refused: %s", out)

				target := func() corestore.Target { return a.dbRefs(t, rel)["refs/heads/race"] }
				require.Equal(t, corestore.Target(winnerCommit), target(),
					"the database holds the winner %s, not the loser %s", winnerCommit, loserCommit)
				require.Equal(t, corestore.Target(winnerCommit), a.localRefs(t, rel)["refs/heads/race"])

				// The updates that do overwrite a reference keep working, whichever way they
				// say so. A forced push names the old value it read.
				out, ok = winner.push("+" + base + ":refs/heads/race")
				require.True(t, ok, out)
				require.Equal(t, corestore.Target(base), target())

				// An update-ref without an old value, as UpdateReferences runs it, reaches the
				// hook with a zero old value too, and means "whatever it is now".
				stream, err := gitalypb.NewRefServiceClient(a.conn(t)).UpdateReferences(a.ctx)
				require.NoError(t, err)
				require.NoError(t, stream.Send(&gitalypb.UpdateReferencesRequest{
					Repository: a.repository(rel),
					Updates: []*gitalypb.UpdateReferencesRequest_Update{{
						Reference:   []byte("refs/heads/race"),
						NewObjectId: []byte(winnerCommit),
					}},
				}))
				_, err = stream.CloseAndRecv()
				require.NoError(t, err)
				require.Equal(t, corestore.Target(winnerCommit), target())
				require.Equal(t, a.dbRefs(t, rel), a.localRefs(t, rel))

				if mode == corestore.ModeAuthoritative {
					a.requireConsistent(t, rel)

					// Another node hydrates the winner's commit.
					b := cluster.addNode(t)
					tip, err := b.agent(t, rel, tokens[who]).ResolveRevision(b.ctx, "race")
					require.NoError(t, err)
					require.Equal(t, winnerCommit, tip)
				}
			})
		})
	}
}
