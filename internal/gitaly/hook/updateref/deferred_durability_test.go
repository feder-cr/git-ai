package updateref

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// durabilityVoter is a storage voter that only answers whether it defers the durability of the
// references, and counts the ends of the processes it deferred.
type durabilityVoter struct {
	deferred bool
	ended    *int
}

func (durabilityVoter) TransactionStarts(context.Context, gitcmd.HooksPayload) {}

func (durabilityVoter) Preparing(context.Context, gitcmd.HooksPayload, []byte) {}

func (durabilityVoter) Prepared(context.Context, gitcmd.HooksPayload, []byte, changelog.Meta) error {
	return nil
}

func (durabilityVoter) Committed(context.Context, gitcmd.HooksPayload, []byte) {}

func (durabilityVoter) Aborted(context.Context, gitcmd.HooksPayload, []byte) {}

func (durabilityVoter) Authoritative() bool { return true }

func (v durabilityVoter) CreatedRefConflict(context.Context, *gitalypb.Repository, string) (string, error) {
	return "", nil
}

func (v durabilityVoter) DeferReferenceWrites(context.Context, *gitalypb.Repository) (func(), bool) {
	if !v.deferred {
		return nil, false
	}
	return func() { *v.ended++ }, true
}

// liveUpdateRefs returns the update-ref processes that are children of the
// test process and still running.
func liveUpdateRefs(t *testing.T) []int {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	require.NoError(t, err)
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...; comm may contain spaces, so the fields are
		// read after its closing parenthesis.
		fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
		if len(fields) < 2 || fields[0] == "Z" || fields[1] != strconv.Itoa(os.Getpid()) {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if strings.Contains(string(cmdline), "update-ref") {
			pids = append(pids, pid)
		}
	}
	return pids
}

// endingVoter is a voter that defers every reference write and checks, when
// the end of a deferred git process is reported, that the process has exited.
type endingVoter struct {
	durabilityVoter
	t      *testing.T
	ends   *int
	failed *string
}

func (v endingVoter) CreatedRefConflict(context.Context, *gitalypb.Repository, string) (string, error) {
	return "", nil
}

func (v endingVoter) DeferReferenceWrites(context.Context, *gitalypb.Repository) (func(), bool) {
	return func() {
		*v.ends++
		if live := liveUpdateRefs(v.t); len(live) > 0 {
			*v.failed = fmt.Sprintf("the end ran while update-ref %v was still running", live)
		}
	}, true
}

// TestUpdateReferenceEndsTheDeferredProcessAfterItExited verifies that
// UpdateWithHooks reports the end of a deferred update-ref only once the
// process has exited (doc/graft/PLAN7.md, LOCAL REFERENCE WRITES RULE): until
// then it may still write a table or compact the stack. It does not run in
// parallel, so that no other test's update-ref is a child at the same time.
func TestUpdateReferenceEndsTheDeferredProcessAfterItExited(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	testcfg.BuildGitalyHooks(t, cfg)
	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	next := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

	var ends int
	var failed string
	manager := hook.NewManager(cfg, locator, logger, gitCmdFactory, nil, nil,
		gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
		hook.NewPostReceiveRegistry(),
		hook.WithStorageVoter(endingVoter{t: t, ends: &ends, failed: &failed}))
	updater := NewUpdaterWithHooks(cfg, logger, locator, manager, gitCmdFactory, nil)

	require.NoError(t, updater.UpdateReference(ctx, repo, gittest.TestUser, nil, "refs/heads/main", next, base))
	require.Equal(t, 1, ends)
	require.Empty(t, failed)
	require.Equal(t, next, gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/main"))
}

// TestDeferReferenceWrites verifies that the updater writes references without fsync only when
// the storage voter of its hook manager defers their durability: never without the storage
// core, nor when the voter does not (shadow mode, authoritative mode where the kernel exposes no
// boot identity, or a process that does not own the storage; the voter's own answer is tested in
// the voter package).
func TestDeferReferenceWrites(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	newManager := func(opts ...hook.ManagerOption) hook.Manager {
		return hook.NewManager(cfg, config.NewLocator(cfg), testhelper.NewLogger(t), gittest.NewCommandFactory(t, cfg), nil, nil,
			gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
			hook.NewPostReceiveRegistry(), opts...)
	}
	var ended int

	for _, tc := range []struct {
		desc     string
		manager  hook.Manager
		expected bool
	}{
		{desc: "storage core off", manager: newManager()},
		{desc: "voter keeps the fsync", manager: newManager(hook.WithStorageVoter(durabilityVoter{deferred: false, ended: &ended}))},
		{desc: "voter defers the durability", manager: newManager(hook.WithStorageVoter(durabilityVoter{deferred: true, ended: &ended})), expected: true},
		{desc: "manager without a storage voter", manager: hook.NewMockManager(t, hook.NopPreReceive, hook.NopPostReceive, hook.NopUpdate, hook.NopReferenceTransaction)},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			end, ok := deferReferenceWrites(ctx, tc.manager, &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"})
			require.Equal(t, tc.expected, ok)
			if ok {
				end()
				require.Equal(t, 1, ended)
			}
		})
	}
}
