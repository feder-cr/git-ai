package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/landlane"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
)

// resolverScript writes a resolver command into a new temporary directory and returns the
// directory and the argv of the command. Every run counts itself in dir/calls and stores the
// request it received as dir/request-N.json, N being the 1-based number of the run. body is shell
// code run before the answer, which is dir/answer.json (see writeAnswer). body may exit to refuse
// or fail the resolution.
func resolverScript(t *testing.T, body string) (string, []string) {
	t.Helper()

	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
DIR='%s'
N=$(cat "$DIR/calls" 2>/dev/null || echo 0)
N=$((N+1))
echo "$N" > "$DIR/calls"
cat > "$DIR/request-$N.json"
%s
cat "$DIR/answer.json"
`, dir, body)

	path := filepath.Join(dir, "resolver.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return dir, []string{path}
}

// writeAnswer sets the answer the resolver script of dir gives.
func writeAnswer(t *testing.T, dir string, resp resolver.Response) {
	t.Helper()

	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "answer.json"), raw, 0o600))
}

// resolverCalls returns the number of times the resolver script of dir ran.
func resolverCalls(t *testing.T, dir string) int {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "calls"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)

	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	return n
}

// resolverRequest returns the n-th request (1-based) the resolver script of dir received.
func resolverRequest(t *testing.T, dir string, n int) resolver.Request {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("request-%d.json", n)))
	require.NoError(t, err)

	var req resolver.Request
	require.NoError(t, json.Unmarshal(raw, &req))
	return req
}

// validAnswer resolves the conflicting files a and b.
func validAnswer() resolver.Response {
	return resolver.Response{
		Version:  resolver.ProtocolVersion,
		Resolver: "graft-resolve/fake",
		Files: []resolver.ResponseFile{
			{Origin: resolver.OriginRule, Path: "a", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged-a\n")},
			{Origin: resolver.OriginRule, Path: "b", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged-b\n")},
		},
	}
}

// enabledResolver is a resolver configuration with resolution on, the given command and every
// default.
//
// The inline resolution of the rules in the lane (land_inline.go) is off here: most of these tests are about
// what happens out of the lane, with rounds, and the tests of the inline resolution turn it on.
func enabledResolver(argv []string) config.GraftResolver {
	return config.GraftResolver{Enabled: true, Command: argv, InlineTimeout: "0s"}
}

// gitBinary returns the absolute path of the git executable Gitaly runs, for scripts that move
// references outside of Gitaly.
func gitBinary(t *testing.T, cfg config.Cfg) string {
	t.Helper()

	return gittest.NewCommandFactory(t, cfg).GetExecutionEnvironment(testhelper.Context(t)).BinaryPath
}

// resolveFixture is a repository where branch main (tip) and commit conflict on a and b, while
// commit also adds d and leaves c alone. base is the merge base of tip and commit. The operation
// service runs with the resolver configuration the fixture was created with.
type resolveFixture struct {
	ctx       context.Context
	cfg       config.Cfg
	repoProto *gitalypb.Repository
	repoPath  string
	repo      *localrepo.Repo
	client    gitalypb.OperationServiceClient
	base      git.ObjectID
	tip       git.ObjectID
	commit    git.ObjectID
	// lanes are the land lanes of the service when the fixture runs a standalone server (the lands
	// then batch through them); nil otherwise.
	lanes *landlane.Shared
}

// laneQueueDepth returns the number of land requests queued in the lanes of the fixture.
func (fx resolveFixture) laneQueueDepth(t *testing.T) float64 {
	t.Helper()

	require.NotNil(t, fx.lanes, "the fixture has no lanes of its own")
	return lanesQueueDepth(t, fx.lanes)
}

// lanesQueueDepth returns the number of land requests queued in lanes.
func lanesQueueDepth(t *testing.T, lanes *landlane.Shared) float64 {
	t.Helper()

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(lanes))
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == "graft_land_lane_queue_depth" {
			return family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	require.FailNow(t, "the lanes export no graft_land_lane_queue_depth")
	return 0
}

// newResolveFixture starts an operation service that resolves with rcfg and creates the
// repository. Tests that resolve through the service pass the resolver script in rcfg.
func newResolveFixture(t *testing.T, rcfg config.GraftResolver) resolveFixture {
	t.Helper()

	return newResolveFixtureWith(t, rcfg,
		[]gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: "base\n"},
			{Mode: "100644", Path: "b", Content: "base\n"},
			{Mode: "100644", Path: "c", Content: "c\n"},
		},
		[]gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: "ours\n"},
			{Mode: "100644", Path: "b", Content: "ours\n"},
			{Mode: "100644", Path: "c", Content: "c\n"},
		},
		[]gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: "theirs\n"},
			{Mode: "100644", Path: "b", Content: "theirs\n"},
			{Mode: "100644", Path: "c", Content: "c\n"},
			{Mode: "100644", Path: "d", Content: "d\n"},
		},
	)
}

// newResolveFixtureWith is newResolveFixture with the trees of the base, the tip and the landed
// commit given by the caller.
func newResolveFixtureWith(t *testing.T, rcfg config.GraftResolver, baseEntries, tipEntries, commitEntries []gittest.TreeEntry) resolveFixture {
	t.Helper()

	return newResolveFixtureIn(t, config.Graft{Land: config.GraftLand{Resolver: rcfg}}, baseEntries, tipEntries, commitEntries)
}

// newResolveFixtureIn is newResolveFixtureWith with the whole Graft configuration of the service,
// for the tests that land through the lanes of a standalone server.
func newResolveFixtureIn(t *testing.T, graft config.Graft, baseEntries, tipEntries, commitEntries []gittest.TreeEntry) resolveFixture {
	t.Helper()

	cfg := testcfg.Build(t)
	cfg.Graft = graft
	var lanes *landlane.Shared
	if graft.Standalone {
		laneCfg, err := landlane.ConfigFromGraft(graft.Land.Batch)
		require.NoError(t, err)
		lanes = landlane.NewShared(laneCfg)
		t.Cleanup(lanes.Close)
	}
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)
	cfg.SocketPath = runOperationServiceServerWithLanes(t, cfg, lanes)
	client, conn := newOperationClient(t, cfg.SocketPath)
	t.Cleanup(func() { conn.Close() })
	ctx := testhelper.MergeOutgoingMetadata(testhelper.Context(t), testcfg.GitalyServersMetadataFromCfg(t, cfg))
	// The resolution writes commits directly, outside an RPC, so the feature flags that
	// commit writing checks are set explicitly.
	ctx = featureflag.ContextWithFeatureFlag(ctx, featureflag.GPGSigning, true)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntries = nestTreeEntries(t, cfg, repoPath, baseEntries)
	tipEntries = nestTreeEntries(t, cfg, repoPath, tipEntries)
	commitEntries = nestTreeEntries(t, cfg, repoPath, commitEntries)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(baseEntries...),
	)
	tip := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(tipEntries...),
	)
	commit := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(commitEntries...),
	)

	return resolveFixture{
		ctx:       ctx,
		cfg:       cfg,
		repoProto: repoProto,
		repoPath:  repoPath,
		repo:      localrepo.NewTestRepo(t, cfg, repoProto),
		client:    client,
		base:      base,
		tip:       tip,
		commit:    commit,
		lanes:     lanes,
	}
}

// server returns a server, without a running gRPC service, that resolves with rcfg.
func (fx resolveFixture) server(t *testing.T, rcfg config.GraftResolver) *Server {
	t.Helper()

	client, err := resolver.New(rcfg)
	require.NoError(t, err)
	cache := catfile.NewCache(fx.cfg)
	t.Cleanup(cache.Stop)
	var pool *resolver.VerifyPool
	if rcfg.Verify.Enabled {
		pool = resolver.NewVerifyPool(rcfg)
	}
	return &Server{
		logger: testhelper.NewLogger(t), gitConfig: fx.cfg.Git, resolveCfg: rcfg, conflictResolver: client, verifyPool: pool,
		catfileCache:     cache,
		localRepoFactory: localrepo.NewFactory(testhelper.NewLogger(t), config.NewLocator(fx.cfg), gittest.NewCommandFactory(t, fx.cfg), cache),
	}
}

func (fx resolveFixture) request(message string) *gitalypb.UserLandRequest {
	return &gitalypb.UserLandRequest{
		Repository:       fx.repoProto,
		User:             gittest.TestUser,
		Branch:           []byte(git.DefaultBranch),
		CommitId:         fx.commit.String(),
		ResolveConflicts: true,
		MergeMessage:     []byte(message),
	}
}

func (fx resolveFixture) committer() git.Signature {
	return git.Signature{Name: "Committer", Email: "committer@example.com", When: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

// blobAt returns the content of path in the tree of rev.
func (fx resolveFixture) blobAt(t *testing.T, rev git.ObjectID, path string) string {
	t.Helper()

	oid, err := fx.repo.ResolveRevision(fx.ctx, git.Revision(rev.String()+":"+path))
	require.NoError(t, err)
	content, err := fx.repo.ReadObject(fx.ctx, oid)
	require.NoError(t, err)
	return string(content)
}

func TestResolveLandConflicts_mergesResolvedFiles(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	fx := newResolveFixture(t, config.GraftResolver{})
	s := fx.server(t, enabledResolver(argv))

	res, err := s.resolveLandConflicts(fx.ctx, fx.request("Land agent work"), fx.repo, fx.tip, fx.commit, fx.committer())
	require.NoError(t, err)

	// The request carries the three versions of every conflicting file.
	require.Equal(t, 1, resolverCalls(t, dir))
	req := resolverRequest(t, dir, 1)
	require.Equal(t, resolver.ProtocolVersion, req.Version)
	require.Equal(t, git.DefaultBranch, req.Branch)
	require.Equal(t, fx.commit.String(), req.Commit)
	// A request carries only what a file's verdict may depend on (PLAN10 15.8): not the tip, its
	// merge base or its message, which a verdict reused at another tip must not depend on.
	require.Empty(t, req.Tip)
	require.Empty(t, req.MergeBase)
	require.Empty(t, req.Messages.Tip)
	// There is no authenticated identity, so the request carries no actor, whatever the user name.
	require.Empty(t, req.Actor)
	require.Equal(t, int64(1048576), req.Limits.MaxFileBytes)
	require.Len(t, req.Files, 2)
	for _, f := range req.Files {
		require.Equal(t, resolver.KindContent, f.Kind, f.Path)
		require.Equal(t, "base\n", string(f.Base.Content), f.Path)
		require.Equal(t, "ours\n", string(f.Ours.Content), f.Path)
		require.Equal(t, "theirs\n", string(f.Theirs.Content), f.Path)
	}

	require.Equal(t, "graft-resolve/fake", res.resolver)
	require.Equal(t, [][]byte{[]byte("a"), []byte("b")}, res.resolvedPaths)

	// The resolution is a merge commit of the tip and the change, with the trailers.
	landed, err := fx.repo.ReadCommit(fx.ctx, res.commit.Revision())
	require.NoError(t, err)
	require.Equal(t, []string{fx.tip.String(), fx.commit.String()}, landed.GetParentIds())
	require.Equal(t, "Committer", string(landed.GetAuthor().GetName()))
	require.Contains(t, string(landed.GetBody()), "Land agent work\n\n")
	require.Contains(t, string(landed.GetBody()), "Graft-Resolved-By: graft-resolve/fake\n")
	require.Contains(t, string(landed.GetBody()), "Graft-Resolved-Path: a\n")
	require.Contains(t, string(landed.GetBody()), "Graft-Resolved-Path: b\n")

	// The resolved files hold the answer, the clean parts of the merge hold both sides.
	require.Equal(t, "merged-a\n", fx.blobAt(t, res.commit, "a"))
	require.Equal(t, "merged-b\n", fx.blobAt(t, res.commit, "b"))
	require.Equal(t, "c\n", fx.blobAt(t, res.commit, "c"))
	require.Equal(t, "d\n", fx.blobAt(t, res.commit, "d"))
}

func TestResolveLandConflicts_logsResolution(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	answer := validAnswer()
	answer.Explanation = "kept both lines " + strings.Repeat("é", maxLoggedExplanationBytes)
	writeAnswer(t, dir, answer)
	fx := newResolveFixture(t, config.GraftResolver{})
	s := fx.server(t, enabledResolver(argv))
	logger := testhelper.NewLogger(t)
	hook := testhelper.AddLoggerHook(logger)
	s.logger = logger

	ctx := identity.NewContext(fx.ctx, identity.Identity{Name: "agent-7", Source: identity.SourceAgent})
	res, err := s.resolveLandConflicts(ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
	require.NoError(t, err)

	// The server keeps a record of the resolution and of what the resolver said about it.
	var found bool
	for _, entry := range hook.AllEntries() {
		if entry.Message != "graft: conflicts of land resolved" {
			continue
		}
		found = true
		require.Equal(t, "info", entry.Level.String())
		require.Equal(t, git.DefaultBranch, entry.Data["branch"])
		require.Equal(t, fx.tip.String(), entry.Data["tip"])
		require.Equal(t, fx.commit.String(), entry.Data["commit"])
		require.Equal(t, res.commit.String(), entry.Data["merge_commit"])
		require.Equal(t, "agent-7", entry.Data["actor"])
		require.Equal(t, "graft-resolve/fake", entry.Data["resolver"])
		require.Equal(t, []string{"a", "b"}, entry.Data["paths"])

		// The explanation is kept, cut to its bound on a rune boundary.
		explanation, ok := entry.Data["explanation"].(string)
		require.True(t, ok)
		require.True(t, strings.HasPrefix(explanation, "kept both lines é"), explanation)
		require.True(t, strings.HasSuffix(explanation, " [truncated]"))
		require.LessOrEqual(t, len(explanation), maxLoggedExplanationBytes+len(" [truncated]"))
		require.True(t, utf8.ValidString(explanation))
	}
	require.True(t, found, "the resolution is logged")
}

func TestLandTruncate(t *testing.T) {
	t.Parallel()

	require.Equal(t, "short", landTruncate("short", 8))
	require.Equal(t, "exactly8", landTruncate("exactly8", 8))
	require.Equal(t, "abcd [truncated]", landTruncate("abcdefgh", 4))
	// A rune cut in half is dropped, not left as an invalid byte.
	require.Equal(t, "a [truncated]", landTruncate("aéé", 2))
	require.Equal(t, "ab", landTruncate("a\xffb", 8))
}

func TestResolveLandConflicts_pathEncoding(t *testing.T) {
	t.Parallel()

	entries := func(p, content string) []gittest.TreeEntry {
		return []gittest.TreeEntry{{Mode: "100644", Path: p, Content: content}}
	}

	t.Run("path that is not valid UTF-8", func(t *testing.T) {
		t.Parallel()

		// A Latin-1 file name would reach the resolver as "caf�.txt", so the answer could
		// never match. The conflict is not sent.
		const p = "caf\xe9.txt"
		dir, argv := resolverScript(t, "")
		answer := resolver.Response{
			Version:  resolver.ProtocolVersion,
			Resolver: "graft-resolve/fake",
			Files: []resolver.ResponseFile{
				{Origin: resolver.OriginRule, Path: strings.ToValidUTF8(p, "�"), Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged\n")},
			},
		}
		writeAnswer(t, dir, answer)
		fx := newResolveFixtureWith(t, config.GraftResolver{}, entries(p, "base\n"), entries(p, "ours\n"), entries(p, "theirs\n"))
		s := fx.server(t, enabledResolver(argv))

		_, err := s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
		require.ErrorContains(t, err, "is not valid UTF-8")
		require.Equal(t, 0, resolverCalls(t, dir))
	})

	t.Run("path in UTF-8 beyond ASCII", func(t *testing.T) {
		t.Parallel()

		const p = "café.txt"
		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, resolver.Response{
			Version:  resolver.ProtocolVersion,
			Resolver: "graft-resolve/fake",
			Files: []resolver.ResponseFile{
				{Origin: resolver.OriginRule, Path: p, Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged\n")},
			},
		})
		fx := newResolveFixtureWith(t, config.GraftResolver{}, entries(p, "base\n"), entries(p, "ours\n"), entries(p, "theirs\n"))
		s := fx.server(t, enabledResolver(argv))

		res, err := s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
		require.NoError(t, err)
		require.Equal(t, p, resolverRequest(t, dir, 1).Files[0].Path)
		require.Equal(t, [][]byte{[]byte(p)}, res.resolvedPaths)
		require.Equal(t, "merged\n", fx.blobAt(t, res.commit, p))
	})
}

func TestResolveLandConflicts_defaultMessage(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	fx := newResolveFixture(t, config.GraftResolver{})
	s := fx.server(t, enabledResolver(argv))

	res, err := s.resolveLandConflicts(fx.ctx, fx.request(""), fx.repo, fx.tip, fx.commit, fx.committer())
	require.NoError(t, err)

	landed, err := fx.repo.ReadCommit(fx.ctx, res.commit.Revision())
	require.NoError(t, err)
	wantPrefix := fmt.Sprintf("Merge %s into %s\n\n", fx.commit.String()[:7], git.DefaultBranch)
	require.True(t, strings.HasPrefix(string(landed.GetBody()), wantPrefix), string(landed.GetBody()))
	require.Contains(t, string(landed.GetBody()), "Graft-Resolved-Path: b\n")
}

func TestResolveLandConflicts_rejectedAnswers(t *testing.T) {
	t.Parallel()

	fx := newResolveFixture(t, config.GraftResolver{})

	for _, tc := range []struct {
		desc     string
		maxBytes int64
		mutate   func(*resolver.Response)
	}{
		{desc: "unsupported version", mutate: func(r *resolver.Response) { r.Version = 2 }},
		{desc: "path that did not conflict", mutate: func(r *resolver.Response) {
			r.Files = append(r.Files, resolver.ResponseFile{Origin: resolver.OriginRule, Path: "c", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("x\n")})
		}},
		{desc: "path answered twice", mutate: func(r *resolver.Response) {
			r.Files = append(r.Files, r.Files[0])
		}},
		{desc: "conflicting path not answered", mutate: func(r *resolver.Response) { r.Files = r.Files[:1] }},
		{desc: "invalid mode", mutate: func(r *resolver.Response) { r.Files[0].Mode = "100600" }},
		{desc: "unknown action", mutate: func(r *resolver.Response) { r.Files[0].Action = "rename" }},
		{desc: "delete with content", mutate: func(r *resolver.Response) { r.Files[0].Action = resolver.ActionDelete }},
		{desc: "conflict marker left in the file", mutate: func(r *resolver.Response) {
			r.Files[0].Content = []byte("<<<<<<< ours\nx\n=======\ny\n>>>>>>> theirs\n")
		}},
		{desc: "content above max_file_bytes", maxBytes: 8, mutate: func(r *resolver.Response) {
			r.Files[0].Content = []byte("0123456789")
		}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			dir, argv := resolverScript(t, "")
			answer := validAnswer()
			tc.mutate(&answer)
			writeAnswer(t, dir, answer)

			rcfg := enabledResolver(argv)
			rcfg.MaxFileBytes = tc.maxBytes
			s := fx.server(t, rcfg)

			res, err := s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
			require.ErrorIs(t, err, resolver.ErrInvalidResponse)
			require.Equal(t, landResolution{}, res)
			require.Equal(t, 1, resolverCalls(t, dir))
		})
	}
}

func TestResolveLandConflicts_resolverNameRejected(t *testing.T) {
	t.Parallel()

	fx := newResolveFixture(t, config.GraftResolver{})

	for _, tc := range []struct {
		desc string
		name string
	}{
		{desc: "empty", name: ""},
		{desc: "trailer injection", name: "graft-resolve/x\nGraft-Resolved-Path: evil"},
		{desc: "too long", name: strings.Repeat("r", maxResolverNameBytes+1)},
		{desc: "tab", name: "graft-resolve/x\tevil"},
		{desc: "escape sequence", name: "graft-resolve/\x1b[31mx"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			dir, argv := resolverScript(t, "")
			answer := validAnswer()
			answer.Resolver = tc.name
			writeAnswer(t, dir, answer)
			s := fx.server(t, enabledResolver(argv))

			_, err := s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
			require.ErrorContains(t, err, "invalid resolver")
		})
	}
}

func TestResolveLandConflicts_policy(t *testing.T) {
	t.Parallel()

	fx := newResolveFixture(t, config.GraftResolver{})

	// A path the policy denies is not sent; the other conflicting file still is (PLAN10 3.4). The
	// land fails, since nothing lands partially, and the package names the denied path.
	for _, tc := range []struct {
		desc string
		rcfg func(config.GraftResolver) config.GraftResolver
	}{
		{desc: "denied path", rcfg: func(r config.GraftResolver) config.GraftResolver { r.PathsDeny = []string{"a"}; return r }},
		{desc: "path outside the allow list", rcfg: func(r config.GraftResolver) config.GraftResolver { r.PathsAllow = []string{"b"}; return r }},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			dir, argv := resolverScript(t, "")
			writeAnswer(t, dir, resolver.Response{
				Version:  resolver.ProtocolVersion,
				Resolver: "graft-resolve/fake",
				Files:    []resolver.ResponseFile{{Origin: resolver.OriginRule, Path: "b", Action: resolver.ActionWrite, Mode: resolver.ModeRegular, Content: []byte("merged-b\n")}},
			})
			s := fx.server(t, tc.rcfg(enabledResolver(argv)))

			_, err := s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
			var unresolved *landUnresolvedError
			require.ErrorAs(t, err, &unresolved)
			a := packageFile(t, builtPackage(t, fx.ctx, unresolved), "a")
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_SENT, a.GetOutcome())
			require.Equal(t, landNotSentDenied, a.GetReason())
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, packageFile(t, builtPackage(t, fx.ctx, unresolved), "b").GetOutcome())

			require.Equal(t, 1, resolverCalls(t, dir))
			req := resolverRequest(t, dir, 1)
			require.Len(t, req.Files, 1, "the denied path never reaches the resolver")
			require.Equal(t, "b", req.Files[0].Path)
		})
	}

	// A refusal of the whole request is decided before the resolver is called, and every file is
	// not attempted.
	for _, tc := range []struct {
		desc   string
		rcfg   func(config.GraftResolver) config.GraftResolver
		reason string
	}{
		{
			desc:   "actor not allowed",
			rcfg:   func(r config.GraftResolver) config.GraftResolver { r.Actors = []string{"someone-else"}; return r },
			reason: "may not resolve conflicts",
		},
		{
			desc:   "more conflicting files than max_files",
			rcfg:   func(r config.GraftResolver) config.GraftResolver { r.MaxFiles = 1; return r },
			reason: "exceed max_files",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			dir, argv := resolverScript(t, "")
			writeAnswer(t, dir, validAnswer())
			s := fx.server(t, tc.rcfg(enabledResolver(argv)))

			_, err := s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
			var unresolved *landUnresolvedError
			require.ErrorAs(t, err, &unresolved)
			pkg := builtPackage(t, fx.ctx, unresolved)
			require.Contains(t, pkg.GetReason(), tc.reason)
			require.Len(t, pkg.GetFiles(), 2, "the package lists every conflicting file, whatever max_files")
			for _, f := range pkg.GetFiles() {
				require.Equal(t, gitalypb.LandConflictFile_OUTCOME_NOT_ATTEMPTED, f.GetOutcome())
			}
			require.Equal(t, 0, resolverCalls(t, dir))
		})
	}
}

func TestResolveLandConflicts_policyAllowsListedPaths(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())
	fx := newResolveFixture(t, config.GraftResolver{})

	rcfg := enabledResolver(argv)
	rcfg.PathsAllow = []string{"a", "b"}
	rcfg.Actors = []string{"agent-*"}
	rcfg.MaxFiles = 2
	s := fx.server(t, rcfg)

	ctx := identity.NewContext(fx.ctx, identity.Identity{Name: "agent-7", Source: identity.SourceAgent})
	_, err := s.resolveLandConflicts(ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
	require.NoError(t, err)
	require.Equal(t, 1, resolverCalls(t, dir))
}

func TestResolveLandConflicts_refusedFailedAndDisabled(t *testing.T) {
	t.Parallel()

	fx := newResolveFixture(t, config.GraftResolver{})

	t.Run("refusal", func(t *testing.T) {
		dir, argv := resolverScript(t, `echo "no safe merge" >&2; exit 1`)
		writeAnswer(t, dir, validAnswer())
		s := fx.server(t, enabledResolver(argv))

		_, err := s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
		require.ErrorIs(t, err, resolver.ErrUnresolvable)
		require.ErrorContains(t, err, "no safe merge")
	})

	t.Run("timeout", func(t *testing.T) {
		dir, argv := resolverScript(t, "exec sleep 30")
		writeAnswer(t, dir, validAnswer())
		rcfg := enabledResolver(argv)
		rcfg.Timeout = "200ms"
		s := fx.server(t, rcfg)

		start := time.Now()
		_, err := s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
		require.ErrorContains(t, err, "timed out")
		require.Less(t, time.Since(start), 20*time.Second)
	})

	t.Run("disabled", func(t *testing.T) {
		s := &Server{gitConfig: fx.cfg.Git, resolveCfg: config.GraftResolver{}, conflictResolver: nil}

		_, err := s.resolveLandConflicts(fx.ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
		require.ErrorContains(t, err, "not enabled")
	})
}

func TestNewServer_warnsWhenResolverActorsMatchNobody(t *testing.T) {
	t.Parallel()

	const warning = "no [[graft.agent]] is configured"

	for _, tc := range []struct {
		desc     string
		actors   []string
		agents   []config.GraftAgent
		wantWarn bool
	}{
		// Only agent authentication sets an identity: without agents a GitLab user name in
		// actors admits nobody.
		{desc: "actors without agents", actors: []string{"alice"}, wantWarn: true},
		{desc: "actors with agents", actors: []string{"builder"}, agents: []config.GraftAgent{{Name: "builder", Token: "secret"}}},
		{desc: "no actors without agents"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			cfg := config.Cfg{}
			cfg.Graft.Agents = tc.agents
			cfg.Graft.Land.Resolver = config.GraftResolver{Enabled: true, Command: []string{"/bin/true"}, Actors: tc.actors}
			require.Equal(t, tc.wantWarn, resolverActorsUnmatchable(cfg.Graft))

			logger := testhelper.NewLogger(t)
			hook := testhelper.AddLoggerHook(logger)
			s := NewServer(&service.Dependencies{Logger: logger, Cfg: cfg})
			require.NotNil(t, s.conflictResolver)

			var warned bool
			for _, entry := range hook.AllEntries() {
				if strings.Contains(entry.Message, warning) {
					warned = true
					require.Equal(t, "warning", entry.Level.String())
				}
			}
			require.Equal(t, tc.wantWarn, warned)
		})
	}
}

func TestLandTrailerValueValid(t *testing.T) {
	t.Parallel()

	require.True(t, landTrailerValueValid("a/b c.go"))
	require.True(t, landTrailerValueValid("résumé.md"))
	for _, v := range []string{
		"",
		"a\nGraft-Resolved-By: evil",
		"a\rb",
		"a\x00b",
		"a\tb",
		"a b",
		"a\u0085b",
		"a\xffb",
	} {
		require.False(t, landTrailerValueValid(v), "%q", v)
	}
}

func TestLandResolutionTrailers(t *testing.T) {
	t.Parallel()

	got, err := landResolutionTrailers("agent/a", "graft-resolve", []string{"a", "dir/b c"})
	require.NoError(t, err)
	require.Equal(t, "Graft-Resolved-By: agent/a\nGraft-Checked-By: graft-resolve\nGraft-Resolved-Path: a\nGraft-Resolved-Path: dir/b c\n", got)

	for name, tc := range map[string]struct {
		resolver, checkedBy string
		paths               []string
		trailer             string
	}{
		"resolver":   {resolver: "agent/x\nGraft-Resolved-Path: evil", paths: []string{"a"}, trailer: landTrailerResolvedBy},
		"no name":    {resolver: "", paths: []string{"a"}, trailer: landTrailerResolvedBy},
		"checked by": {resolver: "agent", checkedBy: "c\rd", paths: []string{"a"}, trailer: landTrailerCheckedBy},
		"path":       {resolver: "agent", paths: []string{"a", lineBreakPath}, trailer: landTrailerResolvedPath},
	} {
		_, err := landResolutionTrailers(tc.resolver, tc.checkedBy, tc.paths)
		var unsafe *landTrailerUnsafeError
		require.ErrorAs(t, err, &unsafe, name)
		require.Equal(t, tc.trailer, unsafe.trailer, name)
	}
}

func TestLandResolverNameValid(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"graft-resolve/trivial",
		"graft-resolve/llm (model 7)",
		"résolveur",
		strings.Repeat("r", maxResolverNameBytes),
	} {
		require.True(t, landResolverNameValid(name), "%q", name)
	}

	for _, name := range []string{
		"",
		strings.Repeat("r", maxResolverNameBytes+1),
		"a\nGraft-Resolved-Path: evil",
		"a\rb",
		"a\x00b",
		"a\tb",
		"a\x1b[2Jb",
		"a\x7fb",
		"a\u0085b",
		"a b",
		// A zero-width space, a format character.
		"a" + string(rune(0x200b)) + "b",
		"a\xffb",
	} {
		require.False(t, landResolverNameValid(name), "%q", name)
	}
}

func TestLandResolveCommitMessage(t *testing.T) {
	t.Parallel()

	commit := git.ObjectID(strings.Repeat("1", 40))

	t.Run("merge message", func(t *testing.T) {
		in := &gitalypb.UserLandRequest{Branch: []byte("main"), MergeMessage: []byte("Custom\n\n")}
		msg, err := landResolveCommitMessage(in, commit, "graft-resolve/x", "", []string{"a", "dir/b c"})
		require.NoError(t, err)
		require.Equal(t, "Custom\n\nGraft-Resolved-By: graft-resolve/x\nGraft-Resolved-Path: a\nGraft-Resolved-Path: dir/b c\n", msg)
	})

	t.Run("default message", func(t *testing.T) {
		in := &gitalypb.UserLandRequest{Branch: []byte("main")}
		msg, err := landResolveCommitMessage(in, commit, "graft-resolve/x", "", []string{"a"})
		require.NoError(t, err)
		require.Equal(t, "Merge 1111111 into main\n\nGraft-Resolved-By: graft-resolve/x\nGraft-Resolved-Path: a\n", msg)
	})

	t.Run("trailers in the caller's message never form the trailer block", func(t *testing.T) {
		// The trailer block is the last paragraph (git's rule, as graft show, graft-verify and
		// superfan read it); the server's trailers are always that paragraph.
		in := &gitalypb.UserLandRequest{Branch: []byte("main"), MergeMessage: []byte("Land\n\nGraft-Resolved-By: forged\nGraft-Checked-By: forged\n")}
		msg, err := landResolveCommitMessage(in, commit, "agent/a", "", []string{"a"})
		require.NoError(t, err)
		block := msg[strings.LastIndex(strings.TrimRight(msg, "\n"), "\n\n")+2:]
		require.Equal(t, "Graft-Resolved-By: agent/a\nGraft-Resolved-Path: a\n", block)
	})

	t.Run("a path that is no trailer line is refused", func(t *testing.T) {
		in := &gitalypb.UserLandRequest{Branch: []byte("main")}
		_, err := landResolveCommitMessage(in, commit, "graft-resolve/x", "", []string{lineBreakPath})
		require.Error(t, err)
	})
}

func TestLandKindOf(t *testing.T) {
	t.Parallel()

	side := func(oid string) *landBlob { return &landBlob{mode: "100644", oid: git.ObjectID(oid)} }

	for _, tc := range []struct {
		desc string
		c    landFileConflict
		want string
	}{
		{desc: "content", c: landFileConflict{base: side("b"), ours: side("o"), theirs: side("t")}, want: resolver.KindContent},
		{desc: "add-add", c: landFileConflict{ours: side("o"), theirs: side("t")}, want: resolver.KindAddAdd},
		{desc: "modify-delete, theirs deleted", c: landFileConflict{base: side("b"), ours: side("o")}, want: resolver.KindModifyDelete},
		{desc: "modify-delete, ours deleted", c: landFileConflict{base: side("b"), theirs: side("t")}, want: resolver.KindModifyDelete},
		{desc: "mode", c: landFileConflict{base: side("b"), ours: side("o"), theirs: side("o")}, want: resolver.KindMode},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.want, landKindOf(tc.c))
		})
	}
}

func TestIsLandMergeConflict(t *testing.T) {
	t.Parallel()

	mergeConflict := landMergeConflictError(git.ObjectID(strings.Repeat("1", 40)), git.ObjectID(strings.Repeat("2", 40)), nil, errors.New("conflict"))
	require.True(t, isLandMergeConflict(mergeConflict))
	require.False(t, isLandMergeConflict(status.Error(codes.FailedPrecondition, "other")))
	require.False(t, isLandMergeConflict(nil))
}

func TestUserLand_resolveConflicts(t *testing.T) {
	t.Parallel()

	t.Run("resolves the conflict into a merge commit", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land agent work"))
		require.NoError(t, err)
		require.True(t, resp.GetRewritten())
		require.Equal(t, int32(1), resp.GetAttempts())
		require.Equal(t, "graft-resolve/fake", resp.GetResolver())
		require.Equal(t, [][]byte{[]byte("a"), []byte("b")}, resp.GetResolvedPaths())
		require.Equal(t, fx.tip.String(), resp.GetOldTip())

		landed := git.ObjectID(resp.GetLandedCommitId())
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, landed)
		require.Equal(t, 1, resolverCalls(t, dir))

		commit, err := fx.repo.ReadCommit(fx.ctx, landed.Revision())
		require.NoError(t, err)
		require.Equal(t, []string{fx.tip.String(), fx.commit.String()}, commit.GetParentIds())
		require.Equal(t, string(gittest.TestUser.Name), string(commit.GetAuthor().GetName()))
		require.Contains(t, string(commit.GetBody()), "Graft-Resolved-By: graft-resolve/fake\n")

		require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "a"))
		require.Equal(t, "merged-b\n", fx.blobAt(t, landed, "b"))
		require.Equal(t, "d\n", fx.blobAt(t, landed, "d"))
	})

	t.Run("without resolve_conflicts the resolver is not called", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))

		req := fx.request("Land")
		req.ResolveConflicts = false
		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		require.Equal(t, 0, resolverCalls(t, dir))
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("disabled resolver keeps the merge conflict", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, config.GraftResolver{})

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("invalid answer keeps the merge conflict", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		answer := validAnswer()
		answer.Files[0].Content = []byte("<<<<<<< ours\n")
		writeAnswer(t, dir, answer)
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("actor not allowed keeps the merge conflict", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		rcfg := enabledResolver(argv)
		rcfg.Actors = []string{"someone-else"}
		fx := newResolveFixture(t, rcfg)

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		require.Equal(t, 0, resolverCalls(t, dir))
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("fast-forward only is not resolved", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))

		req := fx.request("Land")
		req.Strategy = gitalypb.UserLandRequest_STRATEGY_FF_ONLY
		_, err := fx.client.UserLand(fx.ctx, req)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		require.NotNil(t, landErrorDetail(t, err).GetNotAncestor())
		require.Equal(t, 0, resolverCalls(t, dir))
	})

	t.Run("resolver timeout keeps the merge conflict", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "exec sleep 30")
		writeAnswer(t, dir, validAnswer())
		rcfg := enabledResolver(argv)
		rcfg.Timeout = "200ms"
		fx := newResolveFixture(t, rcfg)

		start := time.Now()
		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		require.Less(t, time.Since(start), 20*time.Second)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
	})

	t.Run("a writer that moves the tip on another file costs no second resolution", func(t *testing.T) {
		t.Parallel()

		// The script moves the branch to the writer commit while the land is being resolved, out
		// of the lane. The writer adds e and leaves a and b as they were: their verdicts hold at
		// the writer (PLAN10 15.8), so the lane lands them on the writer without calling the
		// resolver again. The paths of the repository and the commits are read from files the test
		// writes, because the repository exists only once the service runs.
		dir, argv := resolverScript(t, fmt.Sprintf(`if [ "$N" = 1 ]; then
  "$(cat "$DIR/git")" -C "$(cat "$DIR/repo")" update-ref refs/heads/%s "$(cat "$DIR/writer")" "$(cat "$DIR/tip")"
fi`, git.DefaultBranch))
		writeAnswer(t, dir, validAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))

		// The writer adds e on top of the tip.
		writer := gittest.WriteCommit(t, fx.cfg, fx.repoPath,
			gittest.WithParents(fx.tip),
			gittest.WithTreeEntries(
				gittest.TreeEntry{Mode: "100644", Path: "a", Content: "ours\n"},
				gittest.TreeEntry{Mode: "100644", Path: "b", Content: "ours\n"},
				gittest.TreeEntry{Mode: "100644", Path: "c", Content: "c\n"},
				gittest.TreeEntry{Mode: "100644", Path: "e", Content: "e\n"},
			),
		)
		for name, value := range map[string]string{
			"git":    gitBinary(t, fx.cfg),
			"repo":   fx.repoPath,
			"writer": writer.String(),
			"tip":    fx.tip.String(),
		} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600))
		}

		resp, err := fx.client.UserLand(fx.ctx, fx.request("Land agent work"))
		require.NoError(t, err)
		require.Equal(t, int32(1), resp.GetAttempts())
		require.Equal(t, 1, resolverCalls(t, dir), "the verdicts on a and b are reused at the writer")
		// The old tip reported is the tip of the attempt that landed, the writer commit.
		require.Equal(t, writer.String(), resp.GetOldTip())

		landed := git.ObjectID(resp.GetLandedCommitId())
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, landed)

		commit, err := fx.repo.ReadCommit(fx.ctx, landed.Revision())
		require.NoError(t, err)
		require.Equal(t, []string{writer.String(), fx.commit.String()}, commit.GetParentIds())
		require.Equal(t, "e\n", fx.blobAt(t, landed, "e"))
		require.Equal(t, "merged-a\n", fx.blobAt(t, landed, "a"))
	})
}

// requireMergeConflict asserts that err is the merge conflict a landing reports without resolution.
func requireMergeConflict(t *testing.T, err error) {
	t.Helper()

	require.Equal(t, codes.FailedPrecondition, status.Code(err), "error: %v", err)
	require.NotNil(t, landErrorDetail(t, err).GetMergeConflict())
}
