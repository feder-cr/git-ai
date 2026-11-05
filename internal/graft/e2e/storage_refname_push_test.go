package e2e

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
)

// pushRPCs are the RPCs a stock git client needs to push over SSH and over smart HTTP.
var pushRPCs = []string{
	"/gitaly.SSHService/SSHReceivePack",
	"/gitaly.SmartHTTPService/InfoRefsReceivePack",
	"/gitaly.SmartHTTPService/PostReceivePack",
}

// newHTTPPushRemote serves the smart HTTP protocol of git receive-pack for the repository rel of
// node, as a web front end would, by passing each request to the node's SmartHTTPService as the
// agent who with token. It returns the URL a stock git client pushes to.
func newHTTPPushRemote(t *testing.T, node storageNode, rel, who, token string) string {
	t.Helper()

	conn, err := client.New(node.ctx, node.cfg.SocketPath, client.WithGrpcOptions([]grpc.DialOption{
		grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token)),
		client.UnaryInterceptor(), client.StreamInterceptor(),
	}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	smartHTTP := gitalypb.NewSmartHTTPServiceClient(conn)

	glRepository := "graft:" + node.cfg.Storages[0].Name + "/" + rel
	repo := &gitalypb.Repository{StorageName: node.cfg.Storages[0].Name, RelativePath: rel, GlRepository: glRepository}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(err error) { http.Error(w, err.Error(), http.StatusBadGateway) }
		// The advertisement of receive-pack asks for this flag, which the node's context leaves out.
		ctx := featureflag.OutgoingCtxWithFeatureFlag(node.ctx, featureflag.BundleURI, false)

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-receive-pack":
			stream, err := smartHTTP.InfoRefsReceivePack(ctx, &gitalypb.InfoRefsRequest{
				Repository: repo, GitProtocol: r.Header.Get("Git-Protocol"),
			})
			if err != nil {
				fail(err)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			if _, err := io.Copy(w, streamio.NewReader(func() ([]byte, error) {
				resp, err := stream.Recv()
				return resp.GetData(), err
			})); err != nil {
				t.Logf("info/refs: %v", err)
			}

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
			body := io.Reader(r.Body)
			if r.Header.Get("Content-Encoding") == "gzip" {
				gz, err := gzip.NewReader(r.Body)
				if err != nil {
					fail(err)
					return
				}
				body = gz
			}

			stream, err := smartHTTP.PostReceivePack(ctx)
			if err != nil {
				fail(err)
				return
			}
			first := &gitalypb.PostReceivePackRequest{
				Repository: repo, GlId: who, GlUsername: who, GlRepository: glRepository,
				GitProtocol: r.Header.Get("Git-Protocol"),
			}
			sendErr := make(chan error, 1)
			go func() {
				sendErr <- func() error {
					if err := stream.Send(first); err != nil {
						return err
					}
					if _, err := io.Copy(streamio.NewWriter(func(p []byte) error {
						return stream.Send(&gitalypb.PostReceivePackRequest{Data: p})
					}), body); err != nil {
						return err
					}
					return stream.CloseSend()
				}()
			}()

			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			if _, err := io.Copy(w, streamio.NewReader(func() ([]byte, error) {
				resp, err := stream.Recv()
				return resp.GetData(), err
			})); err != nil {
				t.Logf("git-receive-pack: %v", err)
			}
			if err := <-sendErr; err != nil && !errors.Is(err, io.EOF) {
				t.Logf("git-receive-pack request: %v", err)
			}

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	return server.URL + "/" + rel
}

// pushOver pushes refspecs from the client over the transport, "ssh" or "http", and returns
// the client's output and whether the push succeeded.
func pushOver(t *testing.T, c staleCreateClient, transport, httpURL string, refspecs ...string) (string, bool) {
	t.Helper()

	if transport == "ssh" {
		return c.push(refspecs...)
	}
	cmd := gittest.NewCommand(t, c.node.cfg, append([]string{"-C", c.path, "push", httpURL}, refspecs...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// TestStoragePushRefusesConflictingNameAlone pushes, without --atomic, a branch that git cannot
// hold next to a branch of the database, with a branch that conflicts with nothing. In
// authoritative mode the update hook refuses the conflicting branch alone, with the reason on the
// client's terminal, over SSH and over smart HTTP; the other branch lands, and the database records
// it. In shadow mode the push goes through as on a plain git server.
func TestStoragePushRefusesConflictingNameAlone(t *testing.T) {
	t.Parallel()

	for _, mode := range []corestore.Mode{corestore.ModeAuthoritative, corestore.ModeShadow} {
		for _, transport := range []string{"ssh", "http"} {
			t.Run(string(mode)+"/"+transport, func(t *testing.T) {
				t.Parallel()

				forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
					who := workerName(0)
					graft, tokens := workerGraft(1)
					graft.AgentRPCs = pushRPCs
					cluster := newStorageCluster(t, dialect, mode, graft)
					a := cluster.addNode(t)
					rel := a.createRepository(t).GetRelativePath()

					client := newPushClient(t, a, rel, "client", who, tokens[who])
					httpURL := newHTTPPushRemote(t, a, rel, who, tokens[who])
					base := client.commit("base.txt")
					out, ok := pushOver(t, client, transport, httpURL, "HEAD:refs/heads/main", "HEAD:refs/heads/existing")
					require.True(t, ok, out)
					if mode == corestore.ModeShadow {
						_, err := a.operator(t, rel).StorageImport(a.ctx)
						require.NoError(t, err)
					}

					pushed := client.commit("pushed.txt")

					if mode == corestore.ModeShadow {
						// Shadow mode never declines a git write because of the database, which
						// may be behind git: a name only the database holds does not stop the
						// push. The database records none of it, and verification says so.
						_, err := a.core.Meta.CommitChangeset(a.ctx, corestore.ChangesetInput{
							RepoID:    a.record(t, rel).ID,
							Updates:   []corestore.RefUpdate{{Name: "refs/heads/dbonly", Expect: corestore.ExpectValue, New: corestore.Target(base)}},
							Operation: "test", Actor: "tester", NodeID: "node-x",
						})
						require.NoError(t, err)

						out, ok = pushOver(t, client, transport, httpURL, "HEAD:refs/heads/dbonly/below", "HEAD:refs/heads/unrelated")
						require.True(t, ok, out)
						require.NotContains(t, out, "refusing")
						local := a.localRefs(t, rel)
						require.Equal(t, corestore.Target(pushed), local["refs/heads/dbonly/below"])
						require.Equal(t, corestore.Target(pushed), local["refs/heads/unrelated"])
						db := a.dbRefs(t, rel)
						require.NotContains(t, db, "refs/heads/dbonly/below")
						require.NotContains(t, db, "refs/heads/unrelated")

						verified, err := a.operator(t, rel).StorageVerify(a.ctx, false)
						require.NoError(t, err)
						var diverged []string
						for _, d := range verified.Divergences {
							diverged = append(diverged, d.Reference)
						}
						require.ElementsMatch(t, []string{"refs/heads/dbonly", "refs/heads/dbonly/below", "refs/heads/unrelated"}, diverged)
						return
					}

					out, ok = pushOver(t, client, transport, httpURL, "HEAD:refs/heads/existing/below", "HEAD:refs/heads/unrelated")

					require.False(t, ok, "one reference is refused: %s", out)
					require.Contains(t, out, "remote: refusing refs/heads/existing/below: git cannot hold it next to refs/heads/existing; push the other references without it")
					require.Contains(t, out, "existing/below (hook declined)")
					require.NotContains(t, out, "error executing git hook")

					db := a.dbRefs(t, rel)
					require.Equal(t, corestore.Target(base), db["refs/heads/existing"])
					require.NotContains(t, db, "refs/heads/existing/below")
					require.Equal(t, corestore.Target(pushed), db["refs/heads/unrelated"], "the other reference lands and is recorded")
					require.Equal(t, db, a.localRefs(t, rel))
					a.requireConsistent(t, rel)
				})
			})
		}
	}
}

// TestStoragePushConflictWithHiddenName has an agent push the one name that conflicts with a
// reference of the database under refs/graft/, which agents cannot see. receive-pack refuses
// it before any hook (receive.hideRefs covers the name of the directory too), and nothing names
// the hidden reference. The update hook's masking of such a name (TestUpdateHook_storageRefNameConflict)
// is a second line for writers that receive.hideRefs does not cover.
func TestStoragePushConflictWithHiddenName(t *testing.T) {
	t.Parallel()

	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			t.Parallel()

			who := workerName(0)
			graft, tokens := workerGraft(1)
			graft.AgentRPCs = pushRPCs
			cluster := newStorageCluster(t, dialect, corestore.ModeAuthoritative, graft)
			a := cluster.addNode(t)
			rel := a.createRepository(t).GetRelativePath()

			client := newPushClient(t, a, rel, "client", who, tokens[who])
			base := client.commit("base.txt")
			out, ok := client.push("HEAD:refs/heads/main")
			require.True(t, ok, out)

			// A reference of the server under refs/graft/, which agents never see.
			const hidden = "refs/graft/proposals/1"
			_, err := a.core.Meta.CommitChangeset(a.ctx, corestore.ChangesetInput{
				RepoID:    a.record(t, rel).ID,
				Updates:   []corestore.RefUpdate{{Name: hidden, Expect: corestore.ExpectValue, New: corestore.Target(base)}},
				Operation: "test",
				Actor:     "server",
				NodeID:    "node-a",
			})
			require.NoError(t, err)

			out, ok = client.push("HEAD:refs/graft")
			require.False(t, ok, out)
			require.Contains(t, out, "refs/graft (deny updating a hidden ref)")
			require.NotContains(t, out, hidden)
			require.NotContains(t, a.dbRefs(t, rel), "refs/graft")
		})
	}
}

// TestStoragePushCreatingBothNamesOfAPair pushes, without --atomic, two new branches that git
// cannot hold together. The update hook cannot see that (each name conflicts only with the
// other), git refuses one of them but still hands both to the reference-transaction hook, and the
// database refuses the changeset that holds both. In authoritative mode the whole push fails:
// over SSH the client sees the reason as a plain line, over smart HTTP it sees no reason at all.
// In shadow mode the push behaves as on a plain git server, the database records none of it, and
// verification reports the branch git wrote until its next write records it.
func TestStoragePushCreatingBothNamesOfAPair(t *testing.T) {
	t.Parallel()

	for _, mode := range []corestore.Mode{corestore.ModeAuthoritative, corestore.ModeShadow} {
		for _, transport := range []string{"ssh", "http"} {
			t.Run(string(mode)+"/"+transport, func(t *testing.T) {
				t.Parallel()

				forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
					who := workerName(0)
					graft, tokens := workerGraft(1)
					graft.AgentRPCs = pushRPCs
					cluster := newStorageCluster(t, dialect, mode, graft)
					a := cluster.addNode(t)
					rel := a.createRepository(t).GetRelativePath()

					client := newPushClient(t, a, rel, "client", who, tokens[who])
					httpURL := newHTTPPushRemote(t, a, rel, who, tokens[who])
					client.commit("base.txt")
					out, ok := pushOver(t, client, transport, httpURL, "HEAD:refs/heads/main")
					require.True(t, ok, out)
					if mode == corestore.ModeShadow {
						_, err := a.operator(t, rel).StorageImport(a.ctx)
						require.NoError(t, err)
					}

					pushed := client.commit("pushed.txt")
					out, ok = pushOver(t, client, transport, httpURL, "HEAD:refs/heads/pair", "HEAD:refs/heads/pair/below")
					require.False(t, ok, "git refuses one of the two names in every mode: %s", out)
					db := a.dbRefs(t, rel)
					require.NotContains(t, db, "refs/heads/pair")
					require.NotContains(t, db, "refs/heads/pair/below")

					if mode == corestore.ModeAuthoritative {
						require.Contains(t, out, "fatal: the remote end hung up unexpectedly")
						if transport == "ssh" {
							require.Contains(t, out, "fatal: in 'prepared' phase, update aborted by the reference-transaction hook")
							require.Regexp(t, `(?m)^refusing refs/heads/pair(/below)?: git cannot hold it next to refs/heads/pair(/below)?; push the other references without it$`, out)
						} else {
							require.NotContains(t, out, "refusing", "git sends this hook's output to no HTTP client")
						}
						require.NotContains(t, a.localRefs(t, rel), "refs/heads/pair")
						require.NotContains(t, a.localRefs(t, rel), "refs/heads/pair/below")
						a.requireConsistent(t, rel)
						return
					}

					// Shadow mode: git wrote one of the two, which the database misses.
					local := a.localRefs(t, rel)
					written := "refs/heads/pair/below"
					if _, ok := local[written]; !ok {
						written = "refs/heads/pair"
					}
					require.Equal(t, corestore.Target(pushed), local[written])
					verified, err := a.operator(t, rel).StorageVerify(a.ctx, false)
					require.NoError(t, err)
					require.Len(t, verified.Divergences, 1)
					require.Equal(t, written, verified.Divergences[0].Reference)

					// The next write of that branch records it.
					next := client.commit("next.txt")
					out, ok = pushOver(t, client, transport, httpURL, "HEAD:"+written)
					require.True(t, ok, out)
					require.Equal(t, corestore.Target(next), a.dbRefs(t, rel)[written])
					verified, err = a.operator(t, rel).StorageVerify(a.ctx, false)
					require.NoError(t, err)
					require.Empty(t, verified.Divergences)
				})
			})
		}
	}
}
