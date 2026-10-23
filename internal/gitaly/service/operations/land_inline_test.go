package operations

import (
	"fmt"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests of the inline resolution of the rules in the lane (land_inline.go), with the resolver of
// land_queue_test.go: a union of the sides, a rule.

// landAll lands the commits at the same time and returns the error of each.
func (q queueFixture) landAll(commits []git.ObjectID) []error {
	var wg sync.WaitGroup
	errs := make([]error, len(commits))
	for i, c := range commits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = q.land(c)
		}()
	}
	wg.Wait()
	return errs
}

func inlineOn(c *config.GraftResolver) { c.InlineTimeout = "" } // the default: on

// TestUserLand_theRulesAreResolvedInTheLane: with resolve_rounds of 1, a land that loses its first round has
// no second one, and still lands: the lane resolves it, as the rules do, within the batch. The control, with
// the inline resolution off, does lose lands to the cap.
func TestUserLand_theRulesAreResolvedInTheLane(t *testing.T) {
	t.Parallel()

	const lands = 8
	tune := func(inline bool) func(*config.GraftResolver) {
		return func(c *config.GraftResolver) {
			c.ResolveRounds = 1
			if inline {
				inlineOn(c)
			} else {
				c.InlineTimeout = "0s"
			}
		}
	}

	on := newQueueFixture(t, lands, tune(true))
	for i, err := range on.landAll(on.onA) {
		require.NoError(t, err, "land %d of %d with resolve_rounds 1", i+1, lands)
	}
	require.Len(t, strings.Split(on.branchBlob(t, "a"), "\n"), lands+1, "every land is in the file")
	require.Positive(t, inlineCalls(t, on.dir), "the lane asked for the rules only")

	// The control does not depend on who is faster: the resolver answers the two lands only once both have
	// asked, so both resolved against the same tip, and the one that lands second has a stale verdict and,
	// with one round, no other.
	off := newQueueFixtureMode(t, 2, "barrier", tune(false))
	failed := 0
	for _, err := range off.landAll(off.onA) {
		if err != nil {
			failed++
		}
	}
	require.Equal(t, 1, failed, "the control: without the inline resolution the cap of 1 round ends the land that lost it")
	require.Zero(t, inlineCalls(t, off.dir))
}

// TestUserLand_aRuleThatDoesNotEndInTimeIsHandedBack: the rule takes 3 s and the lane gives it 300 ms: the
// members go out of the lane, and are resolved there (the resolver answers the ordinary request at once).
// How long the lane stalls is measured by TestLandResolveInline_aBatchLosesAtMostItsBudget.
func TestUserLand_aRuleThatDoesNotEndInTimeIsHandedBack(t *testing.T) {
	t.Parallel()

	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	q := newQueueFixtureMode(t, 3, "slow", func(c *config.GraftResolver) { c.InlineTimeout = "300ms" })
	require.NotNil(t, s)
	calls := countInlineCalls(s)
	for i, err := range q.landAll(q.onA) {
		require.NoError(t, err, "land %d", i+1)
	}
	require.Positive(t, calls.Load(), "the lane tried")
	require.Len(t, strings.Split(q.branchBlob(t, "a"), "\n"), 4)
	require.Empty(t, landTickets.keysOf(q.scope), "no ticket is kept")
}

// TestUserLand_anOlderResolverIsNeverAskedAgain: a resolver that does not say that it honoured the request
// for the rules only may have called a model: its answer is not taken, the lane never asks again for the life
// of the process (lands that were already in the lane when the first answer came may have asked too), and
// the lands go on out of the lane.
func TestUserLand_anOlderResolverIsNeverAskedAgain(t *testing.T) {
	t.Parallel()

	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	q := newQueueFixtureMode(t, 10, "noecho", func(c *config.GraftResolver) { c.InlineTimeout = "" })
	require.NotNil(t, s, "the service's own Server")

	for i, err := range q.landAll(q.onA[:5]) {
		require.NoError(t, err, "land %d", i+1)
	}
	require.True(t, s.landInlineOff.Load(), "the lane stopped asking")
	asked := inlineCalls(t, q.dir)
	require.Positive(t, asked)

	for i, err := range q.landAll(q.onA[5:]) {
		require.NoError(t, err, "land %d of the second wave", i+1)
	}
	require.Equal(t, asked, inlineCalls(t, q.dir), "not asked again")
	require.Len(t, strings.Split(q.branchBlob(t, "a"), "\n"), 11)
}

// TestUserLand_withAVerificationTheLaneNeverResolves: every tree is verified out of the lane, so the lane
// is not asked for anything.
func TestUserLand_withAVerificationTheLaneNeverResolves(t *testing.T) {
	t.Parallel()

	_, vargv := countingVerify(t, "exit 0")
	q := newQueueFixture(t, 4, func(c *config.GraftResolver) {
		c.InlineTimeout = ""
		*c = withVerify(*c, vargv, "")
	})
	for i, err := range q.landAll(q.onA) {
		require.NoError(t, err, "land %d", i+1)
	}
	require.Zero(t, inlineCalls(t, q.dir))
}

// TestUserLand_theLaneIsEnteredFirstWhenItResolvesTheRules: with the inline resolution on, a land enters the
// lane at once: a land without conflicts is never pre-resolved (the resolver is not called), and one with
// conflicts that a rule resolves is resolved in the lane by one request for the rules only, with none out
// of the lane.
func TestUserLand_theLaneIsEnteredFirstWhenItResolvesTheRules(t *testing.T) {
	t.Parallel()

	q := newQueueFixture(t, 1, func(c *config.GraftResolver) { c.InlineTimeout = "" })

	// The land on b only has its conflicts resolved in the lane.
	_, err := q.land(q.onlyB)
	require.NoError(t, err)
	require.Equal(t, 1, unionCalls(t, q.dir))
	require.Equal(t, 1, inlineCalls(t, q.dir), "the one request was the lane's, for the rules only")

	// And the other one too.
	_, err = q.land(q.onA[0])
	require.NoError(t, err)
	require.Equal(t, 2, unionCalls(t, q.dir))
	require.Equal(t, 2, inlineCalls(t, q.dir), "no request out of the lane")
	require.Equal(t, []string{"ours", "theirs-0"}, strings.Split(q.branchBlob(t, "a"), "\n")[:2])
}

// TestLandResolveInline_neverForAnAgentThePolicyDenies: the policy comes first, as out of the lane: an agent
// that may not update the branch gets nothing resolved in the lane either, and the land goes out of it, where
// the policy refuses it.
func TestLandResolveInline_neverForAnAgentThePolicyDenies(t *testing.T) {
	t.Parallel()

	dir, argv := unionResolverMode(t, "")
	rcfg := enabledResolver(argv)
	rcfg.InlineTimeout = ""
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	s.graft = config.Graft{Standalone: true, Policy: []config.GraftPolicyRule{{Actor: "agent-7", Refs: []string{"refs/heads/other"}, Allow: true}}}
	s.graftPolicy = policystore.New(s.graft.Policy)
	ctx := identity.NewContext(fx.ctx, identity.Identity{Name: "agent-7", Source: identity.SourceAgent})

	l, err := landListConflicts(ctx, fx.repo, fx.tip, fx.commit, 1<<20)
	require.NoError(t, err)
	require.NoError(t, s.landClassify(ctx, fx.repo, l.files, l.base, fx.tip, fx.commit, newLandClassifyCache(l.files, fx.tip)))
	pre := newLandPreResolution()
	require.Equal(t, landInlineNotTried, s.landResolveInline(ctx, fx.request("Land"), fx.tip, fx.commit, l, pre), "denied by the policy")
	require.Zero(t, unionCalls(t, dir), "nothing was resolved")

	// The same agent where the policy lets it update the branch.
	s.graft.Policy = []config.GraftPolicyRule{{Actor: "agent-7", Refs: []string{"refs/heads/**"}, Allow: true}}
	s.graftPolicy = policystore.New(s.graft.Policy)
	require.Equal(t, landInlineResolved, s.landResolveInline(ctx, fx.request("Land"), fx.tip, fx.commit, l, pre))
	require.Equal(t, 1, unionCalls(t, dir))
}

// TestUserLand_aRuleResolvedInTheLaneRidesTheBatchOfTheCleanLands: through the lanes of a standalone server,
// a land with a conflict that a rule resolves enters the lane with the clean lands, is resolved there by
// one request for the rules only, and lands in their batch, not in one of its own.
func TestUserLand_aRuleResolvedInTheLaneRidesTheBatchOfTheCleanLands(t *testing.T) {
	t.Parallel()

	dir, argv := unionResolver(t)
	rcfg := config.GraftResolver{Enabled: true, Command: argv, ResolveRounds: 1}
	cfg := testcfg.Build(t)
	cfg.Graft = config.Graft{
		Standalone: true,
		// The lane runs the batch once its five requests are queued, however slowly they reach it (as in
		// agentBatchingGraft): the window never ends it, and the adaptation is off. With a real 100 ms a loaded
		// machine let the requests arrive in different batches.
		Land: config.GraftLand{Batch: config.GraftLandBatch{Window: "1m", MaxSize: 5, TargetLatency: "0s"}, Resolver: rcfg},
	}
	ctx, cfg, client := setupOperationsServiceWithCfg(t, testhelper.Context(t), cfg)
	ctx = featureflag.ContextWithFeatureFlag(ctx, featureflag.GPGSigning, true)
	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	file := func(path, content string) gittest.TreeEntry {
		return gittest.TreeEntry{Mode: "100644", Path: path, Content: content}
	}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(file("a", "base\n")))
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(file("a", "ours\n")))

	conflicting := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(file("a", "theirs\n")))
	var clean []git.ObjectID
	for i := 0; i < 4; i++ {
		clean = append(clean, gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
			gittest.WithTreeEntries(file("a", "base\n"), file(fmt.Sprintf("p%d", i), "p\n"))))
	}

	var wg sync.WaitGroup
	sizes := make([]int32, 0, 5)
	var mu sync.Mutex
	land := func(commit git.ObjectID, resolve bool) {
		defer wg.Done()
		resp, err := client.UserLand(ctx, &gitalypb.UserLandRequest{
			Repository: repo, User: gittest.TestUser, Branch: []byte(git.DefaultBranch),
			CommitId: commit.String(), ResolveConflicts: resolve, MergeMessage: []byte("Land " + commit.String()),
		})
		require.NoError(t, err)
		if resolve {
			mu.Lock()
			sizes = append(sizes, resp.GetBatchSize())
			mu.Unlock()
		}
	}
	wg.Add(1)
	go land(conflicting, true)
	for _, c := range clean {
		wg.Add(1)
		go land(c, true) // they ask for the resolution too: the rest of the clean lands never need it
	}
	wg.Wait()

	require.Equal(t, 1, unionCalls(t, dir), "the conflict was resolved by one request")
	require.Equal(t, 1, inlineCalls(t, dir), "which was the lane's, for the rules only")
	for _, size := range sizes {
		require.Greater(t, size, int32(1), "every land rode a batch of several")
	}
}
