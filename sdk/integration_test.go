package sdk_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/sdk"
)

// zeroOID is the all-zero SHA-1 object ID, used by Git for "no object".
const zeroOID = "0000000000000000000000000000000000000000"

// missingOID is a well-formed object ID that does not exist in test repositories.
const missingOID = "0123456789abcdef0123456789abcdef01234567"

// integrationEnv is a real Gitaly server with every service registered, one
// repository on it, and an SDK client connected to it.
type integrationEnv struct {
	ctx      context.Context
	cfg      config.Cfg
	addr     string
	relPath  string
	client   *sdk.Client
	repo     *sdk.Repo
	repoPath string
}

// startIntegration starts Gitaly with all services and creates an empty
// repository on it. modify may adjust the configuration before the server
// starts. The client connects with the configured storage and opts.
func startIntegration(t *testing.T, modify func(cfg *config.Cfg), opts ...sdk.DialOption) integrationEnv {
	t.Helper()

	// The test context marks feature flags as explicit, so every flag a server
	// checks must be set. GPGSigning is checked by UserCommitFiles.
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	if modify != nil {
		modify(&cfg)
	}

	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)

	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll)
	cfg.SocketPath = addr

	// Only the setup call carries gitaly-servers metadata. The client calls
	// may authenticate as agents, which may not send it.
	setupCtx := testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))
	repoProto, repoPath := gittest.CreateRepository(t, setupCtx, cfg)

	opts = append([]sdk.DialOption{sdk.WithStorage(cfg.Storages[0].Name)}, opts...)
	client, err := sdk.Dial(ctx, addr, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return integrationEnv{
		ctx:      ctx,
		cfg:      cfg,
		addr:     addr,
		relPath:  repoProto.GetRelativePath(),
		client:   client,
		repo:     client.Repo(repoProto.GetRelativePath()),
		repoPath: repoPath,
	}
}

// dial connects a new client to the same server with opts and returns a handle
// to the test repository through it.
func (e integrationEnv) dial(t *testing.T, opts ...sdk.DialOption) *sdk.Repo {
	t.Helper()

	client, err := sdk.Dial(e.ctx, e.addr, append([]sdk.DialOption{sdk.WithStorage(e.cfg.Storages[0].Name)}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return client.Repo(e.relPath)
}

// writeCommit writes a commit whose tree holds exactly files, with the given
// parents. If branch is not empty, the branch is pointed at the commit.
func (e integrationEnv) writeCommit(t *testing.T, branch string, files map[string]string, parents ...git.ObjectID) git.ObjectID {
	t.Helper()

	opts := []gittest.WriteCommitOption{
		gittest.WithTree(e.writeTree(t, files)),
		gittest.WithParents(parents...),
	}
	if branch != "" {
		opts = append(opts, gittest.WithBranch(branch))
	}

	return gittest.WriteCommit(t, e.cfg, e.repoPath, opts...)
}

// writeTree writes the nested tree that holds exactly files, keyed by their
// slash-separated paths.
func (e integrationEnv) writeTree(t *testing.T, files map[string]string) git.ObjectID {
	t.Helper()

	blobs := map[string]string{}
	dirs := map[string]map[string]string{}
	for path, content := range files {
		name, rest, nested := strings.Cut(path, "/")
		if !nested {
			blobs[name] = content
			continue
		}
		if dirs[name] == nil {
			dirs[name] = map[string]string{}
		}
		dirs[name][rest] = content
	}

	entries := make([]gittest.TreeEntry, 0, len(blobs)+len(dirs))
	for name, content := range blobs {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: name, Content: content})
	}
	for name, sub := range dirs {
		entries = append(entries, gittest.TreeEntry{Mode: "040000", Path: name, OID: e.writeTree(t, sub)})
	}

	return gittest.WriteTree(t, e.cfg, e.repoPath, entries)
}

// git runs a Git command in the test repository and returns its trimmed output.
func (e integrationEnv) git(t *testing.T, args ...string) string {
	t.Helper()

	return strings.TrimSpace(string(gittest.Exec(t, e.cfg, append([]string{"-C", e.repoPath}, args...)...)))
}

// mustResolve resolves rev with the SDK and fails the test on error.
func (e integrationEnv) mustResolve(t *testing.T, rev string) string {
	t.Helper()

	id, err := e.repo.ResolveRevision(e.ctx, rev)
	require.NoError(t, err)
	return id
}

// readString reads the file at path in rev and returns its contents.
func (e integrationEnv) readString(t *testing.T, rev, path string) string {
	t.Helper()

	data, err := e.repo.ReadFile(e.ctx, rev, path)
	require.NoError(t, err)
	return string(data)
}

func TestIntegration_ResolveRevision(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	base := env.writeCommit(t, "main", map[string]string{"a.txt": "a"})

	for _, tc := range []struct {
		desc string
		rev  string
		want string
	}{
		{desc: "branch", rev: "main", want: base.String()},
		{desc: "fully qualified branch", rev: "refs/heads/main", want: base.String()},
		{desc: "full object ID", rev: base.String(), want: base.String()},
		{desc: "parent expression", rev: "main^{commit}", want: base.String()},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := env.repo.ResolveRevision(env.ctx, tc.rev)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	t.Run("unknown revision", func(t *testing.T) {
		_, err := env.repo.ResolveRevision(env.ctx, "no-such-branch")
		require.Error(t, err)
		require.True(t, sdk.IsNotFound(err), "got %v", err)
	})

	t.Run("missing object", func(t *testing.T) {
		_, err := env.repo.ResolveRevision(env.ctx, missingOID)
		require.Error(t, err)
	})

	t.Run("empty revision", func(t *testing.T) {
		_, err := env.repo.ResolveRevision(env.ctx, "")
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})
}

func TestIntegration_ClientAndRepoHandles(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)

	require.Equal(t, env.cfg.Storages[0].Name, env.client.Storage())
	require.Equal(t, env.relPath, env.repo.RelativePath())
	require.Equal(t, env.relPath, env.client.Repo(env.relPath).RelativePath())
}

func TestIntegration_ReadFile(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	large := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB spans several messages

	env.writeCommit(t, "main", map[string]string{
		"README.md":  "# graft\n",
		"dir/a.txt":  "nested",
		"empty.txt":  "",
		"big.bin":    string(large),
		"dir/sub/ok": "deep",
	})

	require.Equal(t, "# graft\n", env.readString(t, "main", "README.md"))
	require.Equal(t, "nested", env.readString(t, "main", "dir/a.txt"))
	require.Equal(t, "deep", env.readString(t, "main", "dir/sub/ok"))
	require.Equal(t, "", env.readString(t, "main", "empty.txt"))
	require.Equal(t, large, mustReadFile(t, env, "main", "big.bin"))

	t.Run("full object ID as revision", func(t *testing.T) {
		commitID := env.mustResolve(t, "main")
		require.Equal(t, "nested", env.readString(t, commitID, "dir/a.txt"))
	})

	t.Run("missing path", func(t *testing.T) {
		_, err := env.repo.ReadFile(env.ctx, "main", "does/not/exist")
		require.True(t, sdk.IsNotFound(err), "got %v", err)
	})

	t.Run("directory is not a file", func(t *testing.T) {
		_, err := env.repo.ReadFile(env.ctx, "main", "dir")
		require.Equal(t, codes.FailedPrecondition, sdk.Code(err), "got %v", err)
	})

	t.Run("unknown revision", func(t *testing.T) {
		_, err := env.repo.ReadFile(env.ctx, "no-such-branch", "README.md")
		require.Error(t, err)
	})

	t.Run("validation", func(t *testing.T) {
		_, err := env.repo.ReadFile(env.ctx, "", "README.md")
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))

		_, err = env.repo.ReadFile(env.ctx, "main", "")
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})
}

func mustReadFile(t *testing.T, env integrationEnv, rev, path string) []byte {
	t.Helper()

	data, err := env.repo.ReadFile(env.ctx, rev, path)
	require.NoError(t, err)
	return data
}

func TestIntegration_ListTree(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	env.writeCommit(t, "main", map[string]string{
		"README":      "readme",
		"dir/a.txt":   "aaa",
		"dir/sub/b.t": "bb",
	})

	type entry struct {
		path string
		typ  sdk.ObjectType
		size int64
	}
	flatten := func(entries []sdk.TreeEntry) []entry {
		out := make([]entry, 0, len(entries))
		for _, e := range entries {
			out = append(out, entry{path: e.Path, typ: e.Type, size: e.Size})
		}
		return out
	}

	t.Run("root, not recursive", func(t *testing.T) {
		entries, err := env.repo.ListTree(env.ctx, "main", "", false)
		require.NoError(t, err)
		require.Equal(t, []entry{
			{path: "README", typ: sdk.ObjectBlob, size: 6},
			{path: "dir", typ: sdk.ObjectTree, size: 0},
		}, flatten(entries))
		require.False(t, entries[0].IsDir())
		require.True(t, entries[1].IsDir())
		require.NotEmpty(t, entries[0].OID)
		require.Equal(t, int32(0100644), entries[0].Mode)
	})

	t.Run("root, recursive", func(t *testing.T) {
		entries, err := env.repo.ListTree(env.ctx, "main", "", true)
		require.NoError(t, err)
		require.Equal(t, []entry{
			{path: "README", typ: sdk.ObjectBlob, size: 6},
			{path: "dir", typ: sdk.ObjectTree},
			{path: "dir/a.txt", typ: sdk.ObjectBlob, size: 3},
			{path: "dir/sub", typ: sdk.ObjectTree},
			{path: "dir/sub/b.t", typ: sdk.ObjectBlob, size: 2},
		}, flatten(entries))
	})

	t.Run("subdirectory paths are relative to it", func(t *testing.T) {
		entries, err := env.repo.ListTree(env.ctx, "main", "dir", false)
		require.NoError(t, err)
		require.Equal(t, []entry{
			{path: "a.txt", typ: sdk.ObjectBlob, size: 3},
			{path: "sub", typ: sdk.ObjectTree},
		}, flatten(entries))

		entries, err = env.repo.ListTree(env.ctx, "main", "dir", true)
		require.NoError(t, err)
		require.Equal(t, []string{"a.txt", "sub", "sub/b.t"}, paths(entries))
	})

	t.Run("missing path", func(t *testing.T) {
		_, err := env.repo.ListTree(env.ctx, "main", "nope", false)
		require.True(t, sdk.IsNotFound(err), "got %v", err)
	})

	t.Run("validation", func(t *testing.T) {
		_, err := env.repo.ListTree(env.ctx, "", "", false)
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})
}

func TestIntegration_GetObjects(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)

	content := []byte("hello, graft\n")
	blob := gittest.WriteBlob(t, env.cfg, env.repoPath, content)
	commit := env.writeCommit(t, "main", map[string]string{"greeting.txt": string(content)})
	tree := env.git(t, "rev-parse", "main^{tree}")

	objects, err := env.repo.GetObjects(env.ctx, []string{
		blob.String(), tree, commit.String(), missingOID, blob.String(),
	})
	require.NoError(t, err)
	require.Len(t, objects, 4, "duplicates are returned once")

	require.Equal(t, blob.String(), objects[0].OID)
	require.Equal(t, sdk.ObjectBlob, objects[0].Type)
	require.Equal(t, int64(len(content)), objects[0].Size)
	require.Equal(t, content, objects[0].Data)
	require.False(t, objects[0].Missing)

	require.Equal(t, tree, objects[1].OID)
	require.Equal(t, sdk.ObjectTree, objects[1].Type)
	require.Equal(t, int64(len(objects[1].Data)), objects[1].Size)
	require.Contains(t, string(objects[1].Data), "greeting.txt")

	require.Equal(t, commit.String(), objects[2].OID)
	require.Equal(t, sdk.ObjectCommit, objects[2].Type)
	require.True(t, bytes.HasPrefix(objects[2].Data, []byte("tree "+tree+"\n")))

	require.Equal(t, missingOID, objects[3].OID)
	require.True(t, objects[3].Missing)
	require.Empty(t, objects[3].Data)
	require.Zero(t, objects[3].Type)
}

func TestIntegration_GetObjects_Empty(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)

	objects, err := env.repo.GetObjects(env.ctx, nil)
	require.NoError(t, err)
	require.Empty(t, objects)
}

func TestIntegration_GetObjects_Validation(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, func(cfg *config.Cfg) {
		cfg.Graft.ObjectService.MaxObjects = 2
	})
	blob := gittest.WriteBlob(t, env.cfg, env.repoPath, []byte("x"))

	t.Run("empty object ID", func(t *testing.T) {
		_, err := env.repo.GetObjects(env.ctx, []string{blob.String(), ""})
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})

	t.Run("malformed object ID", func(t *testing.T) {
		_, err := env.repo.GetObjects(env.ctx, []string{"not-an-object-id"})
		require.Equal(t, codes.InvalidArgument, sdk.Code(err), "got %v", err)
	})

	t.Run("at the limit", func(t *testing.T) {
		objects, err := env.repo.GetObjects(env.ctx, []string{blob.String(), missingOID})
		require.NoError(t, err)
		require.Len(t, objects, 2)
	})

	t.Run("above the server limit is split into requests within it", func(t *testing.T) {
		objects, err := env.repo.GetObjects(env.ctx, []string{blob.String(), missingOID, zeroOID})
		require.NoError(t, err)
		require.Len(t, objects, 3)
		require.Equal(t, []byte("x"), objects[0].Data)
		require.True(t, objects[1].Missing)
		require.True(t, objects[2].Missing)
	})

	t.Run("malformed object ID above the server limit", func(t *testing.T) {
		_, err := env.repo.GetObjects(env.ctx, []string{blob.String(), missingOID, "not-an-object-id"})
		require.Equal(t, codes.InvalidArgument, sdk.Code(err), "got %v", err)
		require.ErrorContains(t, err, "invalid object ID")
	})
}

func TestIntegration_GetObjects_ChunksLargeObjects(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, func(cfg *config.Cfg) {
		cfg.Graft.ObjectService.ChunkSize = 16
	})

	large := bytes.Repeat([]byte("abcdefghij"), 500)
	small := []byte("tiny")
	large1 := gittest.WriteBlob(t, env.cfg, env.repoPath, large)
	small1 := gittest.WriteBlob(t, env.cfg, env.repoPath, small)

	objects, err := env.repo.GetObjects(env.ctx, []string{large1.String(), small1.String()})
	require.NoError(t, err)
	require.Len(t, objects, 2)
	require.Equal(t, large, objects[0].Data)
	require.Equal(t, int64(len(large)), objects[0].Size)
	require.Equal(t, small, objects[1].Data)
}

func TestIntegration_GetObjects_Batches(t *testing.T) {
	t.Parallel()

	// More than one batch of the SDK, and more objects than fit in one
	// request by default, so the requests are split.
	env := startIntegration(t, nil)

	const count = 520
	oids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		oids = append(oids, gittest.WriteBlob(t, env.cfg, env.repoPath, []byte(fmt.Sprintf("object %d\n", i))).String())
	}

	objects, err := env.repo.GetObjects(env.ctx, oids)
	require.NoError(t, err)
	require.Len(t, objects, count)
	for i, obj := range objects {
		require.Equal(t, oids[i], obj.OID, "objects keep request order")
		require.Equal(t, sdk.ObjectBlob, obj.Type)
		require.Equal(t, fmt.Sprintf("object %d\n", i), string(obj.Data))
	}
}

// flakyObjects drops the first failures GetObjects streams after
// afterMessages messages, the way a broken connection would.
type flakyObjects struct {
	mu            sync.Mutex
	failures      int
	afterMessages int
	streams       int
}

func (f *flakyObjects) intercept(
	ctx context.Context,
	desc *grpc.StreamDesc,
	cc *grpc.ClientConn,
	method string,
	streamer grpc.Streamer,
	opts ...grpc.CallOption,
) (grpc.ClientStream, error) {
	stream, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil || method != "/gitaly.ObjectService/GetObjects" {
		return stream, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.streams++
	if f.failures == 0 {
		return stream, nil
	}
	f.failures--

	return &droppingStream{ClientStream: stream, remaining: f.afterMessages}, nil
}

type droppingStream struct {
	grpc.ClientStream

	remaining int
}

func (s *droppingStream) RecvMsg(m any) error {
	if s.remaining == 0 {
		return status.Error(codes.Unavailable, "connection dropped")
	}
	s.remaining--

	return s.ClientStream.RecvMsg(m)
}

func TestIntegration_GetObjects_ResumesAfterDroppedStreams(t *testing.T) {
	t.Parallel()

	flaky := &flakyObjects{failures: 2, afterMessages: 3}
	env := startIntegration(t, func(cfg *config.Cfg) {
		cfg.Graft.ObjectService.ChunkSize = 16
	}, sdk.WithGRPCOptions(grpc.WithStreamInterceptor(flaky.intercept)))

	first := bytes.Repeat([]byte("0123456789"), 20) // 200 bytes, 13 chunks
	second := bytes.Repeat([]byte("xyz"), 30)
	blob1 := gittest.WriteBlob(t, env.cfg, env.repoPath, first)
	blob2 := gittest.WriteBlob(t, env.cfg, env.repoPath, second)

	objects, err := env.repo.GetObjects(env.ctx, []string{blob1.String(), blob2.String()})
	require.NoError(t, err)
	require.Len(t, objects, 2)
	require.Equal(t, first, objects[0].Data)
	require.Equal(t, second, objects[1].Data)

	flaky.mu.Lock()
	defer flaky.mu.Unlock()
	require.Equal(t, 0, flaky.failures)
	require.Equal(t, 3, flaky.streams, "two dropped streams were resumed")
}

func TestIntegration_GetObjects_GivesUpWithoutProgress(t *testing.T) {
	t.Parallel()

	flaky := &flakyObjects{failures: 1000, afterMessages: 0}
	env := startIntegration(t, nil, sdk.WithGRPCOptions(grpc.WithStreamInterceptor(flaky.intercept)))

	blob := gittest.WriteBlob(t, env.cfg, env.repoPath, []byte("never arrives"))

	_, err := env.repo.GetObjects(env.ctx, []string{blob.String()})
	require.Equal(t, codes.Unavailable, sdk.Code(err), "got %v", err)
}

func TestIntegration_CommitFiles(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	base := env.writeCommit(t, "main", map[string]string{"keep.txt": "keep"})

	user := sdk.User{ID: "user-1", Name: "Ada", Email: "ada@example.com", Username: "ada"}
	large := strings.Repeat("0123456789abcdef", 8192) // 128 KiB spans several content messages

	t.Run("creates a branch with new files", func(t *testing.T) {
		result, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
			Branch:    "feature",
			StartRev:  "main",
			Message:   "add files",
			Author:    sdk.Signature{Name: "Grace", Email: "grace@example.com"},
			User:      user,
			Timestamp: time.Unix(1700000000, 0),
			Actions: []sdk.Action{
				sdk.Put{Path: "dir/new.txt", Content: []byte("new")},
				sdk.Put{Path: "big.bin", Content: []byte(large)},
				sdk.Put{Path: "tool.sh", Content: []byte("#!/bin/sh\n"), Executable: true},
				sdk.Put{Path: "empty.txt"},
			},
		})
		require.NoError(t, err)
		require.True(t, result.BranchCreated)
		require.Equal(t, env.mustResolve(t, "feature"), result.CommitID)

		require.Equal(t, base.String(), env.git(t, "rev-parse", "feature^"))
		require.Equal(t, "new", env.readString(t, "feature", "dir/new.txt"))
		require.Equal(t, large, string(mustReadFile(t, env, "feature", "big.bin")))
		require.Equal(t, "keep", env.readString(t, "feature", "keep.txt"))
		require.Equal(t, "", env.readString(t, "feature", "empty.txt"))
		require.Contains(t, env.git(t, "ls-tree", "feature", "tool.sh"), "100755")
		require.Equal(t, "Grace <grace@example.com> 1700000000", env.git(t, "log", "-1", "--format=%an <%ae> %at", "feature"))
	})

	t.Run("updates an existing branch", func(t *testing.T) {
		before := env.mustResolve(t, "feature")

		result, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
			Branch:         "refs/heads/feature",
			StartRev:       "feature",
			ExpectedOldOID: before,
			Message:        "reshape",
			Author:         sdk.Signature{Name: "Grace", Email: "grace@example.com"},
			User:           user,
			Actions: []sdk.Action{
				sdk.Delete{Path: "keep.txt"},
				sdk.Move{From: "dir/new.txt", To: "dir/moved.txt"},
				sdk.Chmod{Path: "big.bin", Executable: true},
				sdk.Put{Path: "big.bin", Content: []byte("small"), Overwrite: true},
			},
		})
		require.NoError(t, err)
		require.False(t, result.BranchCreated)
		require.Equal(t, before, env.git(t, "rev-parse", "feature^"))
		require.Equal(t, env.mustResolve(t, "feature"), result.CommitID)

		_, err = env.repo.ReadFile(env.ctx, "feature", "keep.txt")
		require.True(t, sdk.IsNotFound(err), "deleted file is gone, got %v", err)
		require.Equal(t, "new", env.readString(t, "feature", "dir/moved.txt"))
		require.Equal(t, "small", env.readString(t, "feature", "big.bin"))
		require.Contains(t, env.git(t, "ls-tree", "feature", "big.bin"), "100755")
	})

	t.Run("creating an existing file fails", func(t *testing.T) {
		_, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
			Branch:   "feature",
			StartRev: "feature",
			Message:  "conflict",
			User:     user,
			Actions:  []sdk.Action{sdk.Put{Path: "dir/moved.txt", Content: []byte("again")}},
		})
		require.Equal(t, codes.AlreadyExists, sdk.Code(err), "got %v", err)
	})

	t.Run("unknown start revision", func(t *testing.T) {
		_, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
			Branch:   "other",
			StartRev: "no-such-branch",
			Message:  "msg",
			User:     user,
			Actions:  []sdk.Action{sdk.Put{Path: "x", Content: []byte("x")}},
		})
		require.True(t, sdk.IsNotFound(err), "got %v", err)
	})

	t.Run("stale expected old object ID is rejected", func(t *testing.T) {
		featureBefore := env.mustResolve(t, "feature")

		_, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
			Branch:         "feature",
			StartRev:       "feature",
			ExpectedOldOID: base.String(),
			Message:        "stale",
			User:           user,
			Actions:        []sdk.Action{sdk.Put{Path: "stale.txt", Content: []byte("x")}},
		})
		require.Error(t, err)
		require.Equal(t, featureBefore, env.mustResolve(t, "feature"), "branch is unchanged")
	})

	t.Run("expected old object ID zero requires a new branch", func(t *testing.T) {
		_, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
			Branch:         "feature",
			StartRev:       "main",
			ExpectedOldOID: zeroOID,
			Message:        "create",
			User:           user,
			Actions:        []sdk.Action{sdk.Put{Path: "z.txt", Content: []byte("z")}},
		})
		require.Error(t, err, "feature exists, so the zero OID must not match")
	})
}

func TestIntegration_Land(t *testing.T) {
	t.Parallel()

	user := sdk.User{ID: "user-1", Name: "Ada", Email: "ada@example.com"}

	t.Run("fast-forward", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)
		base := env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
		change := env.writeCommit(t, "", map[string]string{"base.txt": "base", "new.txt": "new"}, base)

		result, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: change.String(), User: user})
		require.NoError(t, err)
		require.Equal(t, change.String(), result.LandedCommitID)
		require.Equal(t, base.String(), result.OldTip)
		require.False(t, result.Rewritten)
		require.False(t, result.AlreadyLanded)
		require.False(t, result.BranchCreated)
		require.Equal(t, int32(1), result.Attempts)
		require.Equal(t, change.String(), env.mustResolve(t, "main"))
		require.Equal(t, "new", env.readString(t, "main", "new.txt"))
	})

	t.Run("already landed leaves the branch alone", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)
		base := env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
		tip := env.writeCommit(t, "main", map[string]string{"base.txt": "base", "x": "x"}, base)

		result, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: base.String(), User: user})
		require.NoError(t, err)
		require.True(t, result.AlreadyLanded)
		require.False(t, result.Rewritten)
		require.Equal(t, tip.String(), result.LandedCommitID)
		require.Equal(t, tip.String(), env.mustResolve(t, "main"))
	})

	t.Run("rebases onto a diverged tip", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)
		base := env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
		tip := env.writeCommit(t, "main", map[string]string{"base.txt": "base", "tip.txt": "tip"}, base)
		change := env.writeCommit(t, "", map[string]string{"base.txt": "base", "change.txt": "change"}, base)

		result, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: change.String(), User: user})
		require.NoError(t, err)
		require.True(t, result.Rewritten)
		require.False(t, result.AlreadyLanded)
		require.Equal(t, tip.String(), result.OldTip)
		require.NotEqual(t, change.String(), result.LandedCommitID)
		require.Equal(t, result.LandedCommitID, env.mustResolve(t, "main"))

		require.Equal(t, tip.String(), env.git(t, "rev-parse", "main^"), "history is linear on top of the tip")
		require.Empty(t, env.git(t, "rev-list", "--merges", "main"))
		require.Equal(t, "tip", env.readString(t, "main", "tip.txt"))
		require.Equal(t, "change", env.readString(t, "main", "change.txt"))
	})

	t.Run("merge strategy creates a merge commit", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)
		base := env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
		tip := env.writeCommit(t, "main", map[string]string{"base.txt": "base", "tip.txt": "tip"}, base)
		change := env.writeCommit(t, "", map[string]string{"base.txt": "base", "change.txt": "change"}, base)

		result, err := env.repo.Land(env.ctx, sdk.LandOptions{
			Branch:       "main",
			Commit:       change.String(),
			User:         user,
			Strategy:     sdk.LandMerge,
			MergeMessage: "merge the change",
		})
		require.NoError(t, err)
		require.True(t, result.Rewritten)
		require.Equal(t, result.LandedCommitID, env.mustResolve(t, "main"))
		require.Equal(t, tip.String(), env.git(t, "rev-parse", "main^1"))
		require.Equal(t, change.String(), env.git(t, "rev-parse", "main^2"))
		require.Equal(t, "merge the change", env.git(t, "log", "-1", "--format=%s", "main"))
	})

	t.Run("fast-forward only fails when not a fast-forward", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)
		base := env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
		tip := env.writeCommit(t, "main", map[string]string{"base.txt": "base", "tip.txt": "tip"}, base)
		change := env.writeCommit(t, "", map[string]string{"base.txt": "base", "change.txt": "change"}, base)

		_, err := env.repo.Land(env.ctx, sdk.LandOptions{
			Branch:   "main",
			Commit:   change.String(),
			User:     user,
			Strategy: sdk.LandFastForwardOnly,
		})
		require.True(t, sdk.IsNotAncestor(err), "got %v", err)
		require.Equal(t, codes.FailedPrecondition, sdk.Code(err))
		require.Equal(t, tip.String(), env.mustResolve(t, "main"), "branch is unchanged")
	})

	t.Run("conflicting change is reported", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)
		base := env.writeCommit(t, "main", map[string]string{"f.txt": "base\n"})
		tip := env.writeCommit(t, "main", map[string]string{"f.txt": "tip\n"}, base)
		change := env.writeCommit(t, "", map[string]string{"f.txt": "change\n"}, base)

		_, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: change.String(), User: user})
		require.True(t, sdk.IsMergeConflict(err), "got %v", err)
		require.Equal(t, codes.FailedPrecondition, sdk.Code(err))
		require.Equal(t, tip.String(), env.mustResolve(t, "main"), "branch is unchanged")
		pkg, ok := sdk.LandConflict(err)
		require.True(t, ok, "ordinary lands also return a conflict package")
		require.Equal(t, tip.String(), pkg.GetTip())
		require.Equal(t, change.String(), pkg.GetCommit())
		sides, err := env.repo.ConflictSides(env.ctx, pkg)
		require.NoError(t, err)
		require.Equal(t, "base\n", string(sides["f.txt"].Base))
		require.Equal(t, "tip\n", string(sides["f.txt"].Ours))
		require.Equal(t, "change\n", string(sides["f.txt"].Theirs))
	})

	t.Run("the conflict package and the re-land", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil) // no resolver: the agent resolves
		base := env.writeCommit(t, "main", map[string]string{"f.txt": "1\nbase\n3\n", "g.txt": "g\n"})
		tip := env.writeCommit(t, "main", map[string]string{"f.txt": "1\ntip\n3\n", "g.txt": "g\n"}, base)
		change := env.writeCommit(t, "", map[string]string{"f.txt": "1\nchange\n3\n", "g.txt": "g\n", "h.txt": "h\n"}, base)

		_, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: change.String(), User: user, ResolveConflicts: true})
		require.True(t, sdk.IsMergeConflict(err), "got %v", err)
		pkg, ok := sdk.LandConflict(err)
		require.True(t, ok, "the merge conflict carries the package")
		require.Equal(t, tip.String(), pkg.GetTip())
		require.Equal(t, change.String(), pkg.GetCommit())
		require.Equal(t, base.String(), pkg.GetMergeBase())
		require.Len(t, pkg.GetFiles(), 1)
		f := pkg.GetFiles()[0]
		require.Equal(t, "f.txt", string(f.GetPath()))
		require.Len(t, f.GetHunks(), 1)
		require.Equal(t, uint32(2), f.GetHunks()[0].GetOurs().GetStart())

		sides, err := env.repo.ConflictSides(env.ctx, pkg)
		require.NoError(t, err)
		require.Equal(t, sdk.ConflictSides{Base: []byte("1\nbase\n3\n"), Ours: []byte("1\ntip\n3\n"), Theirs: []byte("1\nchange\n3\n")}, sides["f.txt"])

		resolution := sdk.ResolutionFor(pkg)
		resolution.Files = []sdk.ResolvedFile{{Path: "f.txt", Mode: "100644", Content: []byte("1\ntip and change\n3\n")}}
		result, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: change.String(), User: user, Resolution: resolution})
		require.NoError(t, err)
		require.True(t, result.Rewritten)
		require.Equal(t, "agent", result.Resolver)
		require.Equal(t, []string{"f.txt"}, result.ResolvedPaths)
		require.Equal(t, "1\ntip and change\n3\n", env.readString(t, "main", "f.txt"))
		require.Equal(t, "h\n", env.readString(t, "main", "h.txt"))
		body := env.git(t, "log", "-1", "--format=%B", "main")
		require.Contains(t, body, "Graft-Resolved-By: agent\n")
		require.NotContains(t, body, "Graft-Checked-By:")
	})

	t.Run("a side the repository does not keep", func(t *testing.T) {
		t.Parallel()

		// The tip renames a.txt to c.txt and changes its last line; the change changes its first
		// line and adds a c.txt of its own. The tip's side of the add/add conflict on c.txt is the
		// content merge of the rename, which no tree holds.
		env := startIntegration(t, nil)
		base := env.writeCommit(t, "main", map[string]string{"a.txt": "1\n2\n3\n4\n5\n6\n7\n8\n"})
		env.writeCommit(t, "main", map[string]string{"c.txt": "1\n2\n3\n4\n5\n6\n7\neight\n"}, base)
		change := env.writeCommit(t, "", map[string]string{"a.txt": "one\n2\n3\n4\n5\n6\n7\n8\n", "c.txt": "theirs\n"}, base)

		_, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: change.String(), User: user, ResolveConflicts: true})
		pkg, ok := sdk.LandConflict(err)
		require.True(t, ok, "got %v", err)
		sides, err := env.repo.ConflictSides(env.ctx, pkg)
		require.NoError(t, err, "an unavailable side fails no other side")
		c := sides["c.txt"]
		require.Nil(t, c.Ours)
		require.Contains(t, c.Unreadable["ours"], "the merge wrote this blob")
		require.Equal(t, []byte("theirs\n"), c.Theirs)
	})

	t.Run("create branch", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)
		base := env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
		change := env.writeCommit(t, "", map[string]string{"base.txt": "base", "n.txt": "n"}, base)

		_, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "topic", Commit: change.String(), User: user})
		require.True(t, sdk.IsNotFound(err), "missing branch without create_branch, got %v", err)

		result, err := env.repo.Land(env.ctx, sdk.LandOptions{
			Branch:       "topic",
			Commit:       change.String(),
			User:         user,
			CreateBranch: true,
		})
		require.NoError(t, err)
		require.True(t, result.BranchCreated)
		require.Empty(t, result.OldTip)
		require.Equal(t, change.String(), result.LandedCommitID)
		require.Equal(t, change.String(), env.mustResolve(t, "topic"))
	})

	t.Run("merge commits cannot be rebased", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)
		base := env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
		env.writeCommit(t, "main", map[string]string{"base.txt": "base", "tip.txt": "tip"}, base)
		left := env.writeCommit(t, "", map[string]string{"base.txt": "base", "left.txt": "l"}, base)
		right := env.writeCommit(t, "", map[string]string{"base.txt": "base", "right.txt": "r"}, base)
		merge := env.writeCommit(t, "", map[string]string{"base.txt": "base", "left.txt": "l", "right.txt": "r"}, left, right)

		_, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: merge.String(), User: user})
		require.Equal(t, codes.InvalidArgument, sdk.Code(err), "got %v", err)

		_, err = env.repo.Land(env.ctx, sdk.LandOptions{
			Branch:   "main",
			Commit:   merge.String(),
			User:     user,
			Strategy: sdk.LandMerge,
		})
		require.NoError(t, err, "merge strategy accepts a merge commit")
	})

	t.Run("unknown commit", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)
		env.writeCommit(t, "main", map[string]string{"base.txt": "base"})

		_, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: missingOID, User: user})
		require.Equal(t, codes.InvalidArgument, sdk.Code(err), "got %v", err)
	})

	t.Run("validation", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)

		_, err := env.repo.Land(env.ctx, sdk.LandOptions{Commit: missingOID, User: user})
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))

		_, err = env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", User: user})
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))

		_, err = env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: missingOID, MaxAttempts: -1, User: user})
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})

	t.Run("concurrent disjoint changes all land linearly", func(t *testing.T) {
		t.Parallel()

		env := startIntegration(t, nil)
		base := env.writeCommit(t, "main", map[string]string{"base.txt": "base"})

		const writers = 12
		changes := make([]git.ObjectID, 0, writers)
		for i := 0; i < writers; i++ {
			changes = append(changes, env.writeCommit(t, "",
				map[string]string{"base.txt": "base", fmt.Sprintf("writer-%02d.txt", i): "data"}, base))
		}

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			results []*sdk.LandResult
			errs    []error
		)
		for i := range changes {
			wg.Add(1)
			go func(commit git.ObjectID) {
				defer wg.Done()

				result, err := env.repo.Land(env.ctx, sdk.LandOptions{
					Branch: "main",
					Commit: commit.String(),
					User: sdk.User{
						ID:    fmt.Sprintf("user-%s", commit.String()[:8]),
						Name:  "Writer",
						Email: "writer@example.com",
					},
				})

				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				results = append(results, result)
			}(changes[i])
		}
		wg.Wait()

		require.Empty(t, errs)
		require.Len(t, results, writers)
		history := strings.Fields(env.git(t, "rev-list", "main"))
		for _, result := range results {
			require.NotEmpty(t, result.LandedCommitID)
			require.GreaterOrEqual(t, result.Attempts, int32(1))
			require.Contains(t, history, result.LandedCommitID, "every landed commit is on the branch")
		}

		tree := env.git(t, "ls-tree", "--name-only", "main")
		for i := 0; i < writers; i++ {
			require.Contains(t, tree, fmt.Sprintf("writer-%02d.txt", i))
		}
		require.Equal(t, writers+1, len(strings.Fields(env.git(t, "rev-list", "main"))))
		require.Empty(t, env.git(t, "rev-list", "--merges", "main"))
	})
}

func TestIntegration_Changesets(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)

	changesets, err := env.repo.Changesets(env.ctx, 0, 0)
	require.NoError(t, err)
	require.Empty(t, changesets)

	env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
	user := sdk.User{ID: "user-1", Name: "Ada", Email: "ada@example.com"}

	feature, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
		Branch:   "feature",
		StartRev: "main",
		Message:  "feature",
		User:     user,
		Actions:  []sdk.Action{sdk.Put{Path: "f.txt", Content: []byte("f")}},
	})
	require.NoError(t, err)

	landed, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: feature.CommitID, User: user})
	require.NoError(t, err)

	changesets, err = env.repo.Changesets(env.ctx, 0, 0)
	require.NoError(t, err)
	require.Len(t, changesets, 2)
	require.Equal(t, uint64(1), changesets[0].GetSequence())
	require.Equal(t, uint64(2), changesets[1].GetSequence())
	require.Equal(t, "refs/heads/feature", string(changesets[0].GetReference()))
	require.Equal(t, zeroOID, changesets[0].GetOldOid(), "the branch was created")
	require.Equal(t, feature.CommitID, changesets[0].GetNewOid())
	require.Equal(t, "land", changesets[1].GetOperation())
	require.Equal(t, "refs/heads/main", string(changesets[1].GetReference()))
	require.Equal(t, landed.LandedCommitID, changesets[1].GetNewOid())
	require.Equal(t, landed.Rewritten, changesets[1].GetRewritten())

	after, err := env.repo.Changesets(env.ctx, 1, 0)
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Equal(t, uint64(2), after[0].GetSequence())

	limited, err := env.repo.Changesets(env.ctx, 0, 1)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	require.Equal(t, uint64(1), limited[0].GetSequence())
}

// nextChangeset receives the next changeset from a watch or fails the test.
func nextChangeset(t *testing.T, changes <-chan *sdk.Changeset, errs <-chan error) *sdk.Changeset {
	t.Helper()

	select {
	case changeset, ok := <-changes:
		require.True(t, ok, "watch ended unexpectedly")
		return changeset
	case err := <-errs:
		t.Fatalf("watch failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a changeset")
	}
	return nil
}

func TestIntegration_WatchChangesets(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
	user := sdk.User{ID: "user-1", Name: "Ada", Email: "ada@example.com"}

	land := func(name string) *sdk.LandResult {
		commit, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
			Branch:   name,
			StartRev: "main",
			Message:  name,
			User:     user,
			Actions:  []sdk.Action{sdk.Put{Path: name + ".txt", Content: []byte(name)}},
		})
		require.NoError(t, err)

		result, err := env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: commit.CommitID, User: user})
		require.NoError(t, err)
		return result
	}

	land("one")
	land("two")

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()

	changes, errs := env.repo.WatchChangesets(ctx, 0)

	// The backlog comes first, in order: two branch creations and two lands.
	for want := uint64(1); want <= 4; want++ {
		require.Equal(t, want, nextChangeset(t, changes, errs).GetSequence())
	}

	// Then changes recorded while the watch is open.
	land("three")
	require.Equal(t, uint64(5), nextChangeset(t, changes, errs).GetSequence())
	last := nextChangeset(t, changes, errs)
	require.Equal(t, uint64(6), last.GetSequence())
	require.Equal(t, "land", last.GetOperation())

	cancel()
	requireClosed(t, changes, errs)
}

func TestIntegration_WatchChangesets_StartsAfterCursor(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
	user := sdk.User{ID: "user-1", Name: "Ada", Email: "ada@example.com"}

	for _, name := range []string{"a", "b"} {
		commit, err := env.repo.CommitFiles(env.ctx, sdk.CommitOptions{
			Branch:   name,
			StartRev: "main",
			Message:  name,
			User:     user,
			Actions:  []sdk.Action{sdk.Put{Path: name, Content: []byte(name)}},
		})
		require.NoError(t, err)
		_, err = env.repo.Land(env.ctx, sdk.LandOptions{Branch: "main", Commit: commit.CommitID, User: user})
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()

	changes, errs := env.repo.WatchChangesets(ctx, 1)
	require.Equal(t, uint64(2), nextChangeset(t, changes, errs).GetSequence())
	cancel()
	requireClosed(t, changes, errs)
}

func TestIntegration_WatchChangesets_UnknownRepository(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)

	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()

	missing := env.client.Repo("@hashed/does/not/exist.git")
	changes, errs := missing.WatchChangesets(ctx, 0)

	select {
	case err := <-errs:
		require.NotNil(t, err)
		require.NotEqual(t, codes.OK, sdk.Code(err))
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the watch to fail")
	}

	requireClosed(t, changes, errs)
}

// requireClosed waits until both channels of a watch are closed.
func requireClosed(t *testing.T, changes <-chan *sdk.Changeset, errs <-chan error) {
	t.Helper()

	timeout := time.After(10 * time.Second)
	for changes != nil || errs != nil {
		select {
		case _, ok := <-changes:
			if !ok {
				changes = nil
			}
		case _, ok := <-errs:
			if !ok {
				errs = nil
			}
		case <-timeout:
			t.Fatal("watch did not close after cancellation")
		}
	}
}

func TestIntegration_Blame(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	first := env.writeCommit(t, "main", map[string]string{"f.txt": "one\ntwo\n", "dir/x": "x"})
	second := env.writeCommit(t, "main", map[string]string{"f.txt": "one\nTWO\nthree\n", "dir/x": "x"}, first)

	t.Run("attributes lines to the commits that changed them", func(t *testing.T) {
		blame, err := env.repo.Blame(env.ctx, "main", "f.txt")
		require.NoError(t, err)
		require.Equal(t, second.String(), blame.CommitID)
		require.Len(t, blame.Lines, 3)

		require.Equal(t, sdk.BlameLine{
			LineNumber: 1, CommitID: first.String(), OriginalLineNumber: 1, Content: []byte("one"),
		}, blame.Lines[0])
		require.Equal(t, sdk.BlameLine{
			LineNumber: 2, CommitID: second.String(), OriginalLineNumber: 2, Content: []byte("TWO"),
		}, blame.Lines[1])
		require.Equal(t, sdk.BlameLine{
			LineNumber: 3, CommitID: second.String(), OriginalLineNumber: 3, Content: []byte("three"),
		}, blame.Lines[2])
	})

	t.Run("older revision", func(t *testing.T) {
		blame, err := env.repo.Blame(env.ctx, first.String(), "f.txt")
		require.NoError(t, err)
		require.Equal(t, first.String(), blame.CommitID)
		require.Len(t, blame.Lines, 2)
		for _, line := range blame.Lines {
			require.Equal(t, first.String(), line.CommitID)
		}
	})

	t.Run("missing path", func(t *testing.T) {
		_, err := env.repo.Blame(env.ctx, "main", "nope.txt")
		require.True(t, sdk.IsNotFound(err), "got %v", err)
	})

	t.Run("directory is not a file", func(t *testing.T) {
		_, err := env.repo.Blame(env.ctx, "main", "dir")
		require.Equal(t, codes.InvalidArgument, sdk.Code(err), "got %v", err)
	})

	t.Run("unknown revision", func(t *testing.T) {
		_, err := env.repo.Blame(env.ctx, "no-such-branch", "f.txt")
		require.True(t, sdk.IsNotFound(err), "got %v", err)
	})

	t.Run("validation", func(t *testing.T) {
		_, err := env.repo.Blame(env.ctx, "", "f.txt")
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))

		_, err = env.repo.Blame(env.ctx, "main", "")
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})
}

func TestIntegration_PathHistory(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, nil)
	c1 := env.writeCommit(t, "main", map[string]string{"a": "1", "b": "0"})
	c2 := env.writeCommit(t, "main", map[string]string{"a": "2", "b": "0"}, c1)
	c3 := env.writeCommit(t, "main", map[string]string{"a": "2", "b": "9"}, c2)
	c4 := env.writeCommit(t, "main", map[string]string{"a": "3", "b": "9"}, c3)

	commitIDs := func(commits []*sdk.Commit) []string {
		out := make([]string, 0, len(commits))
		for _, c := range commits {
			out = append(out, c.GetId())
		}
		return out
	}

	t.Run("only commits that changed the path, newest first", func(t *testing.T) {
		commits, err := env.repo.PathHistory(env.ctx, "main", "a", 0)
		require.NoError(t, err)
		require.Equal(t, []string{c4.String(), c2.String(), c1.String()}, commitIDs(commits))
	})

	t.Run("limit", func(t *testing.T) {
		commits, err := env.repo.PathHistory(env.ctx, "main", "a", 2)
		require.NoError(t, err)
		require.Equal(t, []string{c4.String(), c2.String()}, commitIDs(commits))
	})

	t.Run("starts at the given revision", func(t *testing.T) {
		commits, err := env.repo.PathHistory(env.ctx, c3.String(), "a", 0)
		require.NoError(t, err)
		require.Equal(t, []string{c2.String(), c1.String()}, commitIDs(commits))
	})

	t.Run("unknown revision", func(t *testing.T) {
		_, err := env.repo.PathHistory(env.ctx, "no-such-branch", "a", 0)
		require.Error(t, err)
	})

	t.Run("validation", func(t *testing.T) {
		_, err := env.repo.PathHistory(env.ctx, "", "a", 0)
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))

		_, err = env.repo.PathHistory(env.ctx, "main", "", 0)
		require.Equal(t, codes.InvalidArgument, sdk.Code(err))
	})
}

func TestIntegration_Authentication(t *testing.T) {
	t.Parallel()

	env := startIntegration(t, func(cfg *config.Cfg) {
		cfg.Auth.Token = "shared-secret"
		cfg.Graft.Agents = []config.GraftAgent{{Name: "agent-1", Token: "agent-secret"}}
	})
	base := env.writeCommit(t, "main", map[string]string{"base.txt": "base"})
	want := base.String()

	t.Run("shared token as v2 HMAC", func(t *testing.T) {
		repo := env.dial(t, sdk.WithToken("shared-secret"))
		got, err := repo.ResolveRevision(env.ctx, "main")
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("agent token as bearer", func(t *testing.T) {
		repo := env.dial(t, sdk.WithAgentToken("agent-secret"))
		got, err := repo.ResolveRevision(env.ctx, "main")
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("agent token as v2 HMAC", func(t *testing.T) {
		repo := env.dial(t, sdk.WithToken("agent-secret"))
		got, err := repo.ResolveRevision(env.ctx, "main")
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("shared token is not accepted as a plain bearer", func(t *testing.T) {
		repo := env.dial(t, sdk.WithAgentToken("shared-secret"))
		_, err := repo.ResolveRevision(env.ctx, "main")
		require.Equal(t, codes.Unauthenticated, sdk.Code(err), "got %v", err)
	})

	t.Run("wrong token", func(t *testing.T) {
		repo := env.dial(t, sdk.WithToken("wrong"))
		_, err := repo.ResolveRevision(env.ctx, "main")
		require.Equal(t, codes.Unauthenticated, sdk.Code(err), "got %v", err)
	})

	t.Run("wrong agent token", func(t *testing.T) {
		repo := env.dial(t, sdk.WithAgentToken("wrong"))
		_, err := repo.ResolveRevision(env.ctx, "main")
		require.Equal(t, codes.Unauthenticated, sdk.Code(err), "got %v", err)
	})

	t.Run("no token", func(t *testing.T) {
		repo := env.dial(t)
		_, err := repo.ResolveRevision(env.ctx, "main")
		require.Equal(t, codes.Unauthenticated, sdk.Code(err), "got %v", err)
	})
}
