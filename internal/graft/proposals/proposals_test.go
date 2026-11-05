package proposals

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/agentrefs"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// setup returns a repository and a writer of commits dated as given.
func setup(t *testing.T) (*localrepo.Repo, string, func(time.Time) git.ObjectID, func(string) []string) {
	t.Helper()
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	n := 0
	commit := func(when time.Time) git.ObjectID {
		n++
		return gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage(fmt.Sprintf("proposals %d", n)), gittest.WithCommitterDate(when))
	}
	list := func(prefix string) []string {
		return strings.Fields(text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref", "--format=%(refname)", prefix)))
	}
	return repo, repoPath, commit, list
}

// TestUpdate_onlyProposalReferences: the one writer of the proposal references refuses any other
// reference before git runs, whatever the caller passes; nothing of the batch is written.
func TestUpdate_onlyProposalReferences(t *testing.T) {
	t.Parallel()

	repo, _, commit, list := setup(t)
	ctx := testhelper.Context(t)
	c := commit(time.Now())
	valid := Ref("main", c)
	require.True(t, RefNameValid(valid))
	for _, name := range []git.ReferenceName{
		"refs/heads/main",
		"refs/graft/green/main",
		git.ReferenceName("refs/graft/proposals/x/" + c.String()),
		git.ReferenceName(strings.TrimSuffix(BranchDir("main"), "/")),
		git.ReferenceName(BranchDir("main") + "not-an-oid"),
		git.ReferenceName(BranchDir("main") + c.String() + "/x"),
	} {
		err := Update(ctx, repo, []Op{{Name: valid, NewOID: c, OldOID: gittest.DefaultObjectHash.ZeroOID}, {Name: name, NewOID: c, OldOID: gittest.DefaultObjectHash.ZeroOID}})
		require.ErrorContains(t, err, "not a proposal reference", "%s", name)
	}
	require.Empty(t, list(RefPrefix), "nothing was written")
}

// TestPrune: per branch, proposals past the TTL go, and at most MaxPerBranch of the newest stay;
// the whole prefix prunes every branch, idle or deleted ones included.
func TestPrune(t *testing.T) {
	t.Parallel()

	repo, _, commit, list := setup(t)
	ctx := testhelper.Context(t)
	now := time.Now()
	zero := gittest.DefaultObjectHash.ZeroOID
	name := func(branch string, i int) git.ReferenceName {
		return git.ReferenceName(fmt.Sprintf("%s%040x", BranchDir(branch), i+1))
	}
	var ops []Op
	// main: 22 recent proposals and an old one; idle: one old proposal, on a branch nothing lands on.
	for i := 0; i < 22; i++ {
		ops = append(ops, Op{Name: name("main", i), NewOID: commit(now.Add(-time.Duration(i+1) * time.Minute)), OldOID: zero})
	}
	ops = append(ops, Op{Name: name("main", 100), NewOID: commit(now.Add(-8 * 24 * time.Hour)), OldOID: zero})
	ops = append(ops, Op{Name: name("idle", 0), NewOID: commit(now.Add(-8 * 24 * time.Hour)), OldOID: zero})
	ops = append(ops, Op{Name: name("fresh", 0), NewOID: commit(now.Add(-time.Hour)), OldOID: zero})
	require.NoError(t, Update(ctx, repo, ops))

	// The branch's prune touches its own directory only.
	require.NoError(t, NewSweeper().Drop(ctx, repo, name("main", 0)))
	n, err := Prune(ctx, repo, BranchDir("main"), 7*24*time.Hour, now)
	require.NoError(t, err)
	// 22 recent: the dropped one went, and of the 21 left the oldest is beyond 20; plus the old one.
	require.Equal(t, 2, n, "the old one, and the oldest beyond 20")
	refs := list(BranchDir("main"))
	require.Len(t, refs, 20)
	require.NotContains(t, refs, name("main", 0).String(), "dropped")
	require.NotContains(t, refs, name("main", 100).String(), "past the TTL")
	require.NotContains(t, refs, name("main", 21).String(), "beyond 20")
	require.Contains(t, refs, name("main", 20).String())
	require.Contains(t, list(BranchDir("idle")), name("idle", 0).String(), "another branch is not touched")

	// The housekeeping prune covers every branch: the idle branch's old proposal expires.
	n, err = Prune(ctx, repo, RefPrefix, 7*24*time.Hour, now)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Empty(t, list(BranchDir("idle")))
	require.Len(t, list(BranchDir("fresh")), 1)

	_, err = Prune(ctx, repo, "refs/heads/", time.Hour, now)
	require.ErrorContains(t, err, "is not under")
}

// TestPrune_skipsAReferenceToNoCommit: a proposal reference that points to a blob, which Update
// never writes, is left and counted, and the pruning of the others goes on.
func TestPrune_skipsAReferenceToNoCommit(t *testing.T) {
	t.Parallel()

	repo, _, commit, list := setup(t)
	ctx := testhelper.Context(t)
	now := time.Now()
	zero := gittest.DefaultObjectHash.ZeroOID
	blob, err := repo.WriteBlob(ctx, strings.NewReader("not a commit"), localrepo.WriteBlobConfig{})
	require.NoError(t, err)
	stray := git.ReferenceName(fmt.Sprintf("%s%040x", BranchDir("main"), 1))
	old := git.ReferenceName(fmt.Sprintf("%s%040x", BranchDir("main"), 2))
	require.NoError(t, Update(ctx, repo, []Op{
		{Name: stray, NewOID: blob, OldOID: zero},
		{Name: old, NewOID: commit(now.Add(-8 * 24 * time.Hour)), OldOID: zero},
	}))

	before := testutil.ToFloat64(pruneSkipped)
	n, err := Prune(ctx, repo, RefPrefix, 7*24*time.Hour, now)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the old proposal is pruned")
	require.Equal(t, []string{stray.String()}, list(BranchDir("main")))
	require.GreaterOrEqual(t, testutil.ToFloat64(pruneSkipped)-before, 1.0)
}

// TestUpdate_compareAndSwap: a deletion that expects a target the reference no longer has fails,
// and the reference stays.
func TestUpdate_compareAndSwap(t *testing.T) {
	t.Parallel()

	repo, _, commit, list := setup(t)
	ctx := testhelper.Context(t)
	zero := gittest.DefaultObjectHash.ZeroOID
	first, second := commit(time.Now()), commit(time.Now())
	ref := Ref("main", first)
	require.NoError(t, Update(ctx, repo, []Op{{Name: ref, NewOID: second, OldOID: zero}}))
	require.Error(t, Update(ctx, repo, []Op{{Name: ref, NewOID: zero, OldOID: first}}), "the reference moved to second")
	require.Equal(t, []string{ref.String()}, list(RefPrefix))
	require.NoError(t, Update(ctx, repo, []Op{{Name: ref, NewOID: zero, OldOID: second}}))
	require.Empty(t, list(RefPrefix))
}

// TestRefPrefix_isAServerNamespace: the proposals' namespace is one agentrefs lists, so that an
// agent cannot reach it by its short form either.
func TestRefPrefix_isAServerNamespace(t *testing.T) {
	t.Parallel()

	var listed []string
	for _, ns := range agentrefs.ServerNamespaces {
		listed = append(listed, agentrefs.HiddenPrefix+ns+"/")
	}
	require.Contains(t, listed, RefPrefix)
}
