// Package conformance holds the conformance harness of gitai: stock git clients run the same
// operations against `git http-backend`, the reference, and against gitai in each of its modes, and
// every difference must be listed with its reason (allowlist_test.go) or the test fails. The RPCs
// git has no client for get a matrix of their own (namespace_test.go, decoy_test.go).
//
// `make test-conformance` runs it. `_support/conformance-replay.sh` runs it against the pre-fix
// commits of past bugs, in a copy of their tree, to prove that it catches them.
package conformance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/gitalyclient"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/grpc/sidechannel"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

func TestMain(m *testing.M) {
	if os.Getenv(fetchTransportEnv) != "" {
		os.Exit(runFetchTransport())
	}
	testhelper.Run(m)
}

// fetchTransportEnv makes the test binary act as the transport of a client's fetch, see
// runFetchTransport.
const fetchTransportEnv = "CONFORMANCE_FETCH_TRANSPORT"

// runFetchTransport is the command git runs as its SSH client for a fetch from gitai: it connects the
// client to SSHUploadPackWithSidechannel with the request of GITALY_PAYLOAD and the agent's token, as
// the SSH front end of an operator who opened that RPC to agents would.
//
// gitaly-ssh does this for pushes, but cannot for fetches: it adds transfer.hideRefs=!refs to every
// fetch, an internal tool's way to see every reference, and the server refuses git_config_options
// from an agent.
func runFetchTransport() int {
	var request gitalypb.SSHUploadPackWithSidechannelRequest
	if err := protojson.Unmarshal([]byte(os.Getenv("GITALY_PAYLOAD")), &request); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	ctx := context.Background()
	for _, pair := range strings.Split(os.Getenv("GITALY_FEATUREFLAGS"), ",") {
		if name, value, ok := strings.Cut(pair, ":"); ok {
			ctx = featureflag.OutgoingCtxWithFeatureFlag(ctx, featureflag.FeatureFlag{Name: name}, value == "true")
		}
	}

	logger, err := log.Configure(os.Stderr, "text", "error")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	registry := sidechannel.NewRegistry()
	conn, err := sidechannel.Dial(ctx, registry, logger, os.Getenv("GITALY_ADDRESS"),
		client.WithGrpcOptions([]grpc.DialOption{grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(os.Getenv("GITALY_TOKEN")))}))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer conn.Close()

	result, err := gitalyclient.UploadPackWithSidechannel(ctx, conn, registry, os.Stdin, os.Stdout, os.Stderr, &request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return int(result.ExitCode)
}

// The direction of a client operation: which gitaly-ssh sub-command serves it.
const (
	uploadPack  = "upload-pack"
	receivePack = "receive-pack"
)

// A remote is a repository on a backend, as a stock client sees it.
type remote struct {
	// url is what the client clones from and pushes to.
	url string
	// path is the repository's directory on the server.
	path string
	// repo is the repository on a gitai backend, for the tables of RPCs.
	repo *gitalypb.Repository
	// env returns the environment a client operation needs to reach the repository.
	env func(direction string, protocol int) []string
	// invariants returns what is wrong with the backend's own view of the repository after the
	// row ran, which has no counterpart in stock git.
	invariants func(t *testing.T) []string
}

// A backend hosts the repositories of one mode.
type backend interface {
	newRemote(t *testing.T) remote
}

// stockGit returns the git client of the machine. The tests of this repository put a shim named git
// first in PATH, which refuses to run outside of the command factory.
func stockGit(t *testing.T) string {
	t.Helper()

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if strings.Contains(dir, filepath.Join("testhelper", "testdata")) {
			continue
		}
		candidate := filepath.Join(dir, "git")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	require.FailNow(t, "the harness needs a stock git with http-backend in PATH")
	return ""
}

// stockEnv is the environment of every stock git process: the one the machine's git would run with,
// free of the variables testhelper injects to catch stray git calls and of the user's configuration.
func stockEnv(home string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	if format := os.Getenv("GITALY_TEST_REF_FORMAT"); format != "" {
		env = append(env, "GIT_DEFAULT_REF_FORMAT="+format)
	}
	return env
}

// runStock runs the stock git in dir and returns its exit code, standard output and standard error.
func runStock(ctx context.Context, t *testing.T, dir string, env []string, stdin []byte, args ...string) (int, string, string) {
	t.Helper()

	cmd := exec.CommandContext(ctx, stockGit(t), args...)
	cmd.Dir = dir
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			require.NoError(t, err, "running git %v", args)
		}
	}
	return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()
}

// mustStock is runStock for a command that has to succeed, and returns its trimmed output.
func mustStock(ctx context.Context, t *testing.T, dir string, env []string, stdin []byte, args ...string) string {
	t.Helper()

	code, stdout, stderr := runStock(ctx, t, dir, env, stdin, args...)
	require.Equal(t, 0, code, "git %v: %s", args, stderr)
	return strings.TrimSpace(stdout)
}

// referenceBackend is `git http-backend` behind an HTTP server: what stock git does when it is left
// alone. Filters are turned on, as a server that serves partial clones has to.
type referenceBackend struct {
	root string
	url  string
	env  []string
	n    atomic.Int64
}

func newReferenceBackend(t *testing.T) *referenceBackend {
	t.Helper()

	root := testhelper.TempDir(t)
	env := stockEnv(root)
	handler := &cgi.Handler{
		Path:   stockGit(t),
		Dir:    root,
		Args:   []string{"http-backend"},
		Env:    append([]string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}, env...),
		Stderr: io.Discard,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http/cgi refuses chunked bodies, which git sends for a pack above http.postBuffer: a web
		// server in front of http-backend buffers them the same way.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.TransferEncoding = nil
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	return &referenceBackend{root: root, url: server.URL, env: env}
}

func (b *referenceBackend) newRemote(t *testing.T) remote {
	t.Helper()

	ctx := testhelper.Context(t)
	name := fmt.Sprintf("repo-%d.git", b.n.Add(1))
	path := filepath.Join(b.root, name)
	mustStock(ctx, t, b.root, b.env, nil, "init", "--quiet", "--bare", "--initial-branch=main", path)
	mustStock(ctx, t, path, b.env, nil, "config", "http.receivepack", "true")
	mustStock(ctx, t, path, b.env, nil, "config", "uploadpack.allowFilter", "true")

	return remote{
		url:  b.url + "/" + name,
		path: path,
		env: func(string, int) []string {
			return nil
		},
		invariants: func(*testing.T) []string { return nil },
	}
}

// The agent every gitai client acts as, with what it takes to use git through gitai: the operator's
// opt-in for both directions (AGENT-SECURITY.md) and a policy that allows everything, so that a
// difference comes from the server and not from a rule of this harness.
const (
	agentName       = "conformance"
	agentToken      = "conformance-secret-for-tests"
	sharedToken     = "shared-test-token"
	receivePackRPC  = "/gitaly.SSHService/SSHReceivePack"
	uploadPackRPC   = "/gitaly.SSHService/SSHUploadPackWithSidechannel"
	bareFakeRepoURL = "git@localhost:conformance/repo.git"
)

func agentGraft(extraRPCs ...string) config.Graft {
	return config.Graft{
		Agents:    []config.GraftAgent{{Name: agentName, Token: agentToken}},
		AgentRPCs: append([]string{receivePackRPC, uploadPackRPC}, extraRPCs...),
		Policy:    []config.GraftPolicyRule{{Refs: []string{"HEAD", "refs/**"}, Allow: true}},
	}
}

// gitaiMode is a mode of gitai the harness runs: standalone or not without a storage core, and the
// two modes of the core on each database.
type gitaiMode struct {
	name       string
	standalone bool
	coreMode   corestore.Mode
	dialect    sqlstore.Dialect
}

// gitaiModes are the modes of the tables. Postgres takes part when coretest finds a server.
func gitaiModes() []gitaiMode {
	modes := []gitaiMode{
		{name: "off-standalone", standalone: true},
		{name: "off-gitlab", standalone: false},
	}
	for _, dialect := range coretest.Dialects() {
		modes = append(modes,
			gitaiMode{name: "shadow-" + string(dialect), standalone: true, coreMode: corestore.ModeShadow, dialect: dialect},
			gitaiMode{name: "authoritative-" + string(dialect), standalone: true, coreMode: corestore.ModeAuthoritative, dialect: dialect},
		)
	}
	return modes
}

// gitaiBackend is a real gitai server of one mode.
type gitaiBackend struct {
	mode     gitaiMode
	ctx      context.Context
	setupCtx context.Context
	cfg      config.Cfg
	core     *corestore.Core
	flags    string
}

func newGitaiBackend(t *testing.T, mode gitaiMode, graft config.Graft) *gitaiBackend {
	t.Helper()

	cfg := testcfg.Build(t)
	cfg.Auth.Token = sharedToken
	cfg.Graft = graft
	cfg.Graft.Standalone = mode.standalone
	// What gitaly serve reads from its configuration. The hooks payload asks for the Preparing phase
	// of a transaction, and with it the admission of authoritative mode, only when this says so.
	cfg.Graft.Storage.Mode = string(mode.coreMode)
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	opts := []testserver.GitalyServerOpt{
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))),
	}
	var core *corestore.Core
	if mode.coreMode != "" {
		core = coretest.Open(t, coretest.Options{
			Mode: mode.coreMode, Dialect: mode.dialect,
			// Shadow mode gives up on a write that overruns its budget and counts a divergence, by
			// design (TestStorageShadowModeBudgetOverrun). The default of 5 s is short for a loaded
			// machine, and an overrun here would say nothing about the row.
			ShadowTimeout: 5 * time.Minute,
		})
		if mode.coreMode == corestore.ModeAuthoritative {
			// As gitaly serve does: the admission and the deferred durability of the local writes.
			coretest.EnableLocalRefs(t, core, cfg)
		}
		opts = append(opts, testserver.WithGraftStorage(core))
	}
	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, setup.RegisterAll, opts...)

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	var flags []string
	for flag, value := range featureflag.FromContext(ctx) {
		flags = append(flags, flag.FormatWithValue(value))
	}

	return &gitaiBackend{
		mode:     mode,
		ctx:      ctx,
		setupCtx: testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg)),
		cfg:      cfg,
		core:     core,
		flags:    strings.Join(flags, ","),
	}
}

// operator returns a handle to the repository with the shared token.
func (b *gitaiBackend) operator(t *testing.T, relPath string) *sdk.Repo {
	t.Helper()

	conn, err := sdk.Dial(b.ctx, b.cfg.SocketPath, sdk.WithStorage(b.cfg.Storages[0].Name), sdk.WithToken(sharedToken))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn.Repo(relPath)
}

// operatorConnection connects to the backend with the shared token.
func (b *gitaiBackend) operatorConnection(t *testing.T) *grpc.ClientConn {
	t.Helper()
	return gittest.DialService(t, b.ctx, b.cfg)
}

func (b *gitaiBackend) newRemote(t *testing.T) remote {
	t.Helper()

	repo, path := gittest.CreateRepository(t, b.setupCtx, b.cfg)
	relPath := repo.GetRelativePath()
	if b.mode.coreMode == corestore.ModeShadow {
		_, err := b.operator(t, relPath).StorageImport(b.ctx)
		require.NoError(t, err)
	}

	glRepository := "graft:" + repo.GetStorageName() + "/" + relPath
	wire := &gitalypb.Repository{StorageName: repo.GetStorageName(), RelativePath: relPath, GlRepository: glRepository}

	return remote{
		url:  bareFakeRepoURL,
		path: path,
		repo: wire,
		env: func(direction string, protocol int) []string {
			gitProtocol := ""
			if protocol != 0 {
				gitProtocol = fmt.Sprintf("version=%d", protocol)
			}

			var request proto.Message
			if direction == receivePack {
				request = &gitalypb.SSHReceivePackRequest{
					Repository: wire, GlId: agentName, GlUsername: agentName, GlRepository: glRepository, GitProtocol: gitProtocol,
				}
			} else {
				request = &gitalypb.SSHUploadPackWithSidechannelRequest{Repository: wire, GitProtocol: gitProtocol}
			}
			payload, err := protojson.Marshal(request)
			require.NoError(t, err)

			env := []string{
				"GITALY_PAYLOAD=" + string(payload),
				"GITALY_ADDRESS=" + b.cfg.SocketPath,
				"GITALY_TOKEN=" + agentToken,
				"GITALY_FEATUREFLAGS=" + b.flags,
				// simple: git would otherwise probe the command with -G to learn what it is.
				"GIT_SSH_VARIANT=simple",
			}
			if direction == receivePack {
				return append(env, "GIT_SSH_COMMAND="+b.cfg.BinaryPath("gitaly-ssh")+" "+receivePack)
			}
			self, err := os.Executable()
			require.NoError(t, err)
			return append(env, fetchTransportEnv+"=1", "GIT_SSH_COMMAND="+self)
		},
		invariants: func(t *testing.T) []string {
			if b.core == nil {
				return nil
			}
			verified, err := b.operator(t, relPath).StorageVerify(b.ctx, true)
			require.NoError(t, err)

			var problems []string
			for _, divergence := range verified.Divergences {
				problems = append(problems, fmt.Sprintf("the database diverges at %s: local %q, database %q",
					divergence.Reference, divergence.LocalTarget, divergence.DatabaseTarget))
			}
			if !verified.ConnectivityOK {
				problems = append(problems, "connectivity: "+verified.ConnectivityError)
			}
			return problems
		},
	}
}
