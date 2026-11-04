package operations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The tests of the bounds of the inline resolution (land_inline.go): what the lane gives away to a call
// that fails, what it takes from a reply, and what it never waits for.

// inlineProbe is a listing of the fixture's conflicts, classified at its tip, with a server that resolves
// inline, for the tests that call landResolveInline directly.
type inlineProbe struct {
	fx  resolveFixture
	s   *Server
	dir string
	l   landListing
}

func newInlineProbe(t *testing.T, mode string, tune func(*config.GraftResolver)) inlineProbe {
	t.Helper()

	dir, argv := unionResolverMode(t, mode)
	rcfg := enabledResolver(argv)
	rcfg.InlineTimeout = ""
	if tune != nil {
		tune(&rcfg)
	}
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	l, err := landListConflicts(fx.ctx, fx.repo, fx.tip, fx.commit, 1<<20)
	require.NoError(t, err)
	require.NoError(t, s.landClassify(fx.ctx, fx.repo, l.files, l.base, fx.tip, fx.commit, newLandClassifyCache(l.files, fx.tip)))
	return inlineProbe{fx: fx, s: s, dir: dir, l: l}
}

func (p inlineProbe) resolve(ctx context.Context, t *testing.T, pre *landPreResolution) landInlineResult {
	t.Helper()
	return p.s.landResolveInline(ctx, p.fx.request("Land"), p.fx.tip, p.fx.commit, p.l, pre)
}

// countInlineCalls counts the inline calls of the lane (land_inline.go) where it makes them. A test that cuts a
// call with a real timeout must not count them in the log of the resolver: the log is written once the
// resolver's process has started, which a loaded machine does after the timeout has cut the call.
func countInlineCalls(s *Server) *atomic.Int32 {
	var calls atomic.Int32
	s.landInlineCallContext = func(ctx context.Context, allowed time.Duration) (context.Context, context.CancelFunc) {
		calls.Add(1)
		return context.WithTimeout(ctx, allowed)
	}
	return &calls
}

// TestLandResolveInline_aReplyThatIsNotTheRulesIsRejected: a resolver that echoes rules_only but answers with
// a model's file, a file that says nothing of its origin, or a file it holds, is not taken: the member goes out
// of the lane as one the rules cannot resolve, and nothing of the reply is kept. The guard is the lane's own;
// decideLand would refuse such a land too, but only after the lane had taken the verdict.
func TestLandResolveInline_aReplyThatIsNotTheRulesIsRejected(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"echo-model", "echo-noorigin", "echo-hold"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			p := newInlineProbe(t, mode, nil)
			pre := newLandPreResolution()
			require.Equal(t, landInlineRulesCannot, p.resolve(p.fx.ctx, t, pre))
			require.Empty(t, pre.verdicts, "nothing of the reply is kept")
			require.False(t, p.s.landInlineOff.Load(), "the resolver did honour the request")
			require.Equal(t, 1, inlineCalls(t, p.dir))
		})
	}
}

// TestLandResolveInline_announcesOnlyTheOrigin: the request asks for the rules only and announces no
// capability but the origin, so that a graft-resolve that gates the models on the capabilities refuses to
// call one even if it were to ignore rules_only.
func TestLandResolveInline_announcesOnlyTheOrigin(t *testing.T) {
	t.Parallel()

	p := newInlineProbe(t, "", nil)
	require.Equal(t, landInlineResolved, p.resolve(p.fx.ctx, t, newLandPreResolution()))

	reqs, err := filepath.Glob(filepath.Join(p.dir, "req-*.json"))
	require.NoError(t, err)
	require.Len(t, reqs, 1)
	raw, err := os.ReadFile(reqs[0])
	require.NoError(t, err)
	var req struct {
		RulesOnly    bool     `json:"rules_only"`
		Capabilities []string `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal(raw, &req))
	require.True(t, req.RulesOnly)
	require.Equal(t, []string{"origin"}, req.Capabilities)
}

// TestLandResolveInline_aResolverThatRefusesIsRulesCannot: the refusal of the rules is not a slow call: the
// member is one that only a model can resolve, and the lane charges the batch nothing but the call.
func TestLandResolveInline_aResolverThatRefusesIsRulesCannot(t *testing.T) {
	t.Parallel()

	p := newInlineProbe(t, "refuse", nil)
	pre := newLandPreResolution()
	require.Equal(t, landInlineRulesCannot, p.resolve(p.fx.ctx, t, pre))
	require.Empty(t, pre.verdicts)
	require.False(t, p.s.landInlineOff.Load())
}

// TestLandResolveInline_aBatchLosesAtMostItsBudget: the rule takes 3 s and the lane gives it 300 ms. Six
// members of one batch attempt (one budget) lose that once: the first call is cancelled, the other five are
// handed back without a call. Measured on the time the lane spends in the calls, which is what stalls the
// batch: six members at 300 ms each would be 1.8 s.
func TestLandResolveInline_aBatchLosesAtMostItsBudget(t *testing.T) {
	t.Parallel()

	const members = 6
	p := newInlineProbe(t, "slow", func(c *config.GraftResolver) { c.InlineTimeout = "300ms" })
	budget := &landInlineBudget{}
	ctx := withLandInlineBudget(p.fx.ctx, budget)

	calls := countInlineCalls(p.s)

	for i := 0; i < members; i++ {
		require.Equal(t, landInlineSlow, p.resolve(ctx, t, newLandPreResolution()), "member %d", i+1)
	}

	// One call, so the lane stalled once for it, not once per member (six at 300 ms would be 1.8 s).
	require.EqualValues(t, 1, calls.Load(), "only the first member called: the others found the budget spent")
	require.GreaterOrEqual(t, budget.lost, 250*time.Millisecond)

	// The next batch attempt has a budget of its own.
	require.Equal(t, landInlineSlow, p.resolve(withLandInlineBudget(p.fx.ctx, &landInlineBudget{}), t, newLandPreResolution()))
	require.EqualValues(t, 2, calls.Load())
}

// TestLandResolveInline_aBatchOfFastRulesIsNotCutShort: members that the rules resolve in milliseconds, against a
// budget that is large next to them, are all resolved in the lane; the time of their calls is charged all the same
// (the other side of TestLandResolveInline_aSlowRuleThatSucceedsIsChargedToTheBudget).
func TestLandResolveInline_aBatchOfFastRulesIsNotCutShort(t *testing.T) {
	t.Parallel()

	p := newInlineProbe(t, "", func(c *config.GraftResolver) { c.InlineTimeout = "30s" })
	budget := &landInlineBudget{}
	ctx := withLandInlineBudget(p.fx.ctx, budget)
	for i := 0; i < 4; i++ {
		require.Equal(t, landInlineResolved, p.resolve(ctx, t, newLandPreResolution()), "member %d", i+1)
	}
	require.Positive(t, budget.lost, "the time of the calls is charged")
	require.Less(t, budget.lost, 30*time.Second)
}

// TestLandResolveInline_neverWaitsForASlot: the lane takes a slot of its own pool without waiting. With the
// pool taken the member goes out of the lane at once, without a call; and a model that holds every resolve
// slot of the resolver does not stop the rules.
func TestLandResolveInline_neverWaitsForASlot(t *testing.T) {
	t.Parallel()

	p := newInlineProbe(t, "", nil)

	// Every resolve slot is held by models: the rules go on.
	var held []interface{ Release() }
	for i := 0; i < p.s.resolveCfg.ResolveConcurrencyOrDefault(); i++ {
		slot, err := p.s.conflictResolver.AcquireResolveSlot(p.fx.ctx)
		require.NoError(t, err)
		held = append(held, slot)
	}
	require.Equal(t, landInlineResolved, p.resolve(p.fx.ctx, t, newLandPreResolution()), "the resolve pool is not the lane's")
	for _, slot := range held {
		slot.Release()
	}
	calls := inlineCalls(t, p.dir)

	// The lane's own pool is taken (by the other members in flight): no wait, no call.
	var own []interface{ Release() }
	for {
		slot, ok := p.s.conflictResolver.TryAcquireInlineSlot()
		if !ok {
			break
		}
		own = append(own, slot)
	}
	require.NotEmpty(t, own)
	done := make(chan landInlineResult, 1)
	go func() { done <- p.resolve(p.fx.ctx, t, newLandPreResolution()) }()
	select {
	case res := <-done:
		require.Equal(t, landInlineSlow, res)
	case <-time.After(10 * time.Second):
		t.Fatal("the lane waited for a slot")
	}
	require.Equal(t, calls, inlineCalls(t, p.dir), "no call without a slot")
	for _, slot := range own {
		slot.Release()
	}
	require.Equal(t, landInlineResolved, p.resolve(p.fx.ctx, t, newLandPreResolution()), "the slots were given back")
}

// TestUserLand_aLandNeedingAModelDoesNotHoldBackTheUnionLands: one land on a hot file needs a model (the
// rules cannot resolve it, and the model takes as long as it takes: here, until the test says so), and
// six others on the same file are resolved by a rule. Without a verification what the model resolves never
// lands, so the land that needs it takes no ticket and the others are not made to wait for it: all six
// land while the model is still working, and no ticket is held.
func TestUserLand_aLandNeedingAModelDoesNotHoldBackTheUnionLands(t *testing.T) {
	t.Parallel()

	q := newQueueFixtureMode(t, 6, "model", func(c *config.GraftResolver) { c.InlineTimeout = "" })
	model := gittest.WriteCommit(t, q.cfg, q.repoPath, gittest.WithParents(q.base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "model-1\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"}))

	modelDone := make(chan error, 1)
	go func() {
		_, err := q.land(model)
		modelDone <- err
	}()
	// The model land is at the resolver, out of the lane: its request is the one that is not for the rules.
	require.Eventually(t, func() bool { return unionCalls(t, q.dir)-inlineCalls(t, q.dir) >= 1 }, 30*time.Second, 10*time.Millisecond)

	landed := make(chan []error, 1)
	go func() { landed <- q.landAll(q.onA) }()
	select {
	case errs := <-landed:
		for i, err := range errs {
			require.NoError(t, err, "union land %d", i+1)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the union lands waited for the land that needs a model")
	}
	require.Empty(t, landTickets.keysOf(q.scope), "the land that needs a model holds no ticket")
	select {
	case err := <-modelDone:
		t.Fatalf("the model land ended before the model answered: %v", err)
	default:
	}
	require.Len(t, strings.Split(q.branchBlob(t, "a"), "\n"), 7)

	// The model answers: its resolution is a proposal, as without a verification it always was.
	require.NoError(t, os.WriteFile(filepath.Join(q.dir, "release"), nil, 0o644))
	select {
	case err := <-modelDone:
		requireMergeConflict(t, err)
		require.NotNil(t, landConflictDetail(err), "the package with the proposal")
	case <-time.After(60 * time.Second):
		t.Fatal("the model land did not end")
	}
	require.Empty(t, landTickets.keysOf(q.scope))
}

// TestUserLand_aTicketOnTheFileDoesNotStopTheRulesInTheLane: another land holds the ticket of the file,
// and the lane resolves the rules of this one inline all the same: the ticket serves the out-of-lane
// resolution, and a rule does not wait for it. The block in the lane comes after the inline attempt; it
// hands back only what the lane could not resolve.
func TestUserLand_aTicketOnTheFileDoesNotStopTheRulesInTheLane(t *testing.T) {
	t.Parallel()

	q := newQueueFixture(t, 2, inlineOn)
	holder, err := landTickets.acquire(q.ctx, []string{q.keyOfA})
	require.NoError(t, err)
	defer holder.release()

	done := make(chan error, 1)
	go func() {
		_, err := q.land(q.onA[0])
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the land waited for the holder of the ticket")
	}
	require.Equal(t, 1, inlineCalls(t, q.dir), "resolved in the lane")
	require.Equal(t, 0, landTickets.waiting(q.keyOfA), "it never queued")
	require.True(t, landTickets.blocked([]string{q.keyOfA}, nil), "the holder is still the holder")
}

// TestUserLand_aSlowRuleWaitsForTheTicketHolder: when the lane could not resolve inline (the rule did not end in
// time), the land goes out of the lane and, with another land holding the ticket of the file, waits for its
// turn there as before.
func TestUserLand_aSlowRuleWaitsForTheTicketHolder(t *testing.T) {
	t.Parallel()

	q := newQueueFixtureMode(t, 1, "slow", func(c *config.GraftResolver) { c.InlineTimeout = "300ms" })
	holder, err := landTickets.acquire(q.ctx, []string{q.keyOfA})
	require.NoError(t, err)
	defer holder.release()

	done := make(chan error, 1)
	go func() {
		_, err := q.land(q.onA[0])
		done <- err
	}()
	require.Eventually(t, func() bool { return landTickets.waiting(q.keyOfA) == 1 }, 30*time.Second, 5*time.Millisecond, "the land never queued")
	select {
	case err := <-done:
		t.Fatalf("the land went past the holder of its file: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	holder.release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("the land did not land when the holder was done")
	}
}

// TestUserLand_oneFailedInlineCallDoesNotStartAConvoy: one rule call hiccups (3 s, cut at 300 ms) in the middle
// of a hot file's lands. The members the lane handed back are resolved out of the lane and come back, but no
// file stays ticketed: the next wave is resolved in the lane, at once, without one request out of it.
func TestUserLand_oneFailedInlineCallDoesNotStartAConvoy(t *testing.T) {
	t.Parallel()

	const lands = 8
	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	// No clock cuts a call: the timeout is far longer than any call. The hiccup is cut by the test, the moment
	// the resolver has started it (it has made its "slowed" marker): the calls in flight then are cancelled, and
	// the hiccup is one of them, whichever call it is. (With a real 300 ms a loaded machine cut the calls that
	// never hiccuped, which need that long to start a python process, and the lane handed those members back.)
	q := newQueueFixtureMode(t, 2*lands, "slowonce", func(c *config.GraftResolver) { c.InlineTimeout = "30s" })
	require.NotNil(t, s)
	var mu sync.Mutex
	var inflight []context.CancelFunc
	s.landInlineCallContext = func(ctx context.Context, allowed time.Duration) (context.Context, context.CancelFunc) {
		callCtx, cancel := context.WithTimeout(ctx, allowed)
		mu.Lock()
		defer mu.Unlock()
		inflight = append(inflight, cancel)
		return callCtx, cancel
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(q.dir, "slowed")); err == nil {
				mu.Lock()
				defer mu.Unlock()
				for _, cancel := range inflight {
					cancel()
				}
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	for i, err := range q.landAll(q.onA[:lands]) {
		require.NoError(t, err, "land %d of the first wave", i+1)
	}
	require.Empty(t, landTickets.keysOf(q.scope), "no ticket is kept after the hiccup")
	outOfLane := unionCalls(t, q.dir) - inlineCalls(t, q.dir)
	require.Positive(t, outOfLane, "the hiccup was handed back")
	inline := inlineCalls(t, q.dir)

	for i, err := range q.landAll(q.onA[lands:]) {
		require.NoError(t, err, "land %d of the second wave", i+1)
	}
	require.Equal(t, outOfLane, unionCalls(t, q.dir)-inlineCalls(t, q.dir), "the second wave never left the lane")
	require.Greater(t, inlineCalls(t, q.dir), inline)
	require.Len(t, strings.Split(q.branchBlob(t, "a"), "\n"), 2*lands+1)
	require.Empty(t, landTickets.keysOf(q.scope))
}

// TestUserLand_theLaneRoundDoesNotCountTowardTheRounds: with resolve_rounds of 1 a land that only a model can
// resolve still leaves the lane for the pre-resolution: it ends with the proposal, not with the file
// NOT_ATTEMPTED and nothing proposed.
func TestUserLand_theLaneRoundDoesNotCountTowardTheRounds(t *testing.T) {
	t.Parallel()

	q := newQueueFixtureMode(t, 1, "model", func(c *config.GraftResolver) {
		c.InlineTimeout = ""
		c.ResolveRounds = 1
	})
	require.NoError(t, os.WriteFile(filepath.Join(q.dir, "release"), nil, 0o644))
	model := gittest.WriteCommit(t, q.cfg, q.repoPath, gittest.WithParents(q.base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "model-1\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"}))

	_, err := q.land(model)
	requireMergeConflict(t, err)
	pkg := landConflictDetail(err)
	require.NotNil(t, pkg)
	require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, packageFile(t, pkg, "a").GetOutcome(), "the model was asked")
}

// TestUserLand_aLaneRoundThatHandsBackTakesNoTicket: the rule does not end in time, so the lane hands the land
// back; it is resolved out of the lane and comes back, and at no round does it hold a ticket (a ticket would
// make the lands on its file wait for it, for a hiccup of the lane).
func TestUserLand_aLaneRoundThatHandsBackTakesNoTicket(t *testing.T) {
	t.Parallel()

	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	q := newQueueFixtureMode(t, 1, "slow", func(c *config.GraftResolver) { c.InlineTimeout = "300ms" })
	require.NotNil(t, s)

	var mu sync.Mutex
	var held [][]string
	s.landLaneHook = func() {
		mu.Lock()
		defer mu.Unlock()
		held = append(held, landTickets.keysOf(q.scope))
	}
	calls := countInlineCalls(s)
	_, err := q.land(q.onA[0])
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(held), 2, "the land went through the lane, out of it, and back")
	for round, keys := range held {
		require.Empty(t, keys, "ticket held at the lane round %d", round+1)
	}
	require.Positive(t, calls.Load(), "the lane tried")
}

// TestUserLand_withNoInlineSlotAllTheLandsStillLand: every slot of the lane is taken (by calls in flight for other
// branches), so no member can be resolved in the lane; they all go out of it, with the queue, and all of
// them land with the default resolve_rounds: the lack of a slot costs time, not lands.
func TestUserLand_withNoInlineSlotAllTheLandsStillLand(t *testing.T) {
	t.Parallel()

	const lands = 8
	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	q := newQueueFixture(t, lands, inlineOn)
	require.NotNil(t, s)

	var own []interface{ Release() }
	for {
		slot, ok := s.conflictResolver.TryAcquireInlineSlot()
		if !ok {
			break
		}
		own = append(own, slot)
	}
	defer func() {
		for _, slot := range own {
			slot.Release()
		}
	}()

	for i, err := range q.landAll(q.onA) {
		require.NoError(t, err, "land %d of %d", i+1, lands)
	}
	require.Zero(t, inlineCalls(t, q.dir), "the lane never called")
	require.Len(t, strings.Split(q.branchBlob(t, "a"), "\n"), lands+1)
	require.Empty(t, landTickets.keysOf(q.scope))
}

// TestUserLand_aModelLandReleasesTheTicketItQueuedFor: a land that needs a model queues behind a holder (no inline
// slot is free, so the lane could not try the rules) and takes the ticket; the next lane round finds that the
// rules cannot resolve it, and it must give the ticket back before it calls the model out of the lane: a model's
// resolution never lands without a verification, and the lands on the file would wait for it.
func TestUserLand_aModelLandReleasesTheTicketItQueuedFor(t *testing.T) {
	t.Parallel()

	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	q := newQueueFixtureMode(t, 1, "model", func(c *config.GraftResolver) { c.InlineTimeout = "" })
	require.NotNil(t, s)

	var own []interface{ Release() }
	for {
		slot, ok := s.conflictResolver.TryAcquireInlineSlot()
		if !ok {
			break
		}
		own = append(own, slot)
	}
	holder, err := landTickets.acquire(q.ctx, []string{q.keyOfA})
	require.NoError(t, err)
	defer holder.release()

	model := gittest.WriteCommit(t, q.cfg, q.repoPath, gittest.WithParents(q.base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "model-1\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"}))
	done := make(chan error, 1)
	go func() {
		_, err := q.land(model)
		done <- err
	}()
	require.Eventually(t, func() bool { return landTickets.waiting(q.keyOfA) == 1 }, 30*time.Second, 5*time.Millisecond, "the model land queued")
	for _, slot := range own {
		slot.Release()
	}
	holder.release()

	// The model request is in flight: the one that is not for the rules only.
	require.Eventually(t, func() bool { return inlineCalls(t, q.dir) >= 1 && unionCalls(t, q.dir)-inlineCalls(t, q.dir) >= 1 }, 30*time.Second, 5*time.Millisecond)
	require.False(t, landTickets.blocked([]string{q.keyOfA}, nil), "the land that needs a model holds the ticket of the file during the model call")

	require.NoError(t, os.WriteFile(filepath.Join(q.dir, "release"), nil, 0o644))
	select {
	case err := <-done:
		requireMergeConflict(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("the model land did not end")
	}
}

// TestLandResolveInline_aSlowRuleThatSucceedsIsChargedToTheBudget: a rule that is slow but ends in time is taken, and
// all the time of its call is the batch's: what the next member is given is what is left of the budget, and once it
// is spent the rest of the members go out of the lane without a call, so six members do not stall the lane six
// times. The slowness is the budget's own bookkeeping (landInlineBudget.lost, which the test sets as a slow call
// would have), not a rule that sleeps against a real clock, which a loaded machine does not keep.
func TestLandResolveInline_aSlowRuleThatSucceedsIsChargedToTheBudget(t *testing.T) {
	t.Parallel()

	const members = 6
	const timeout = 30 * time.Second
	p := newInlineProbe(t, "", func(c *config.GraftResolver) { c.InlineTimeout = "30s" })
	budget := &landInlineBudget{}
	ctx := withLandInlineBudget(p.fx.ctx, budget)
	var given []time.Duration
	var calls atomic.Int32
	p.s.landInlineCallContext = func(ctx context.Context, allowed time.Duration) (context.Context, context.CancelFunc) {
		calls.Add(1)
		given = append(given, allowed)
		return context.WithTimeout(ctx, allowed)
	}

	// The first member is given the whole timeout, its rule ends, and the time of the call is charged.
	require.Equal(t, landInlineResolved, p.resolve(ctx, t, newLandPreResolution()), "a rule that ends in time is taken")
	require.Positive(t, budget.lost, "the time of a call that succeeds is charged")
	// A slow call would have charged 29 s: the next member is given what is left, 1 s.
	budget.lost = 29 * time.Second
	require.Equal(t, landInlineResolved, p.resolve(ctx, t, newLandPreResolution()))
	require.Equal(t, []time.Duration{timeout, timeout - 29*time.Second}, given, "each member is given the rest of the budget")
	// And that one spent it: the other members go out of the lane without a call.
	budget.lost = timeout
	for i := 2; i < members; i++ {
		require.Equal(t, landInlineSlow, p.resolve(ctx, t, newLandPreResolution()), "member %d", i+1)
	}
	require.EqualValues(t, 2, calls.Load(), "the members that found the budget spent did not call")
}

// TestUserLand_aRuleSlowerThanTheTimeoutLandsOnAHotFile: a rule takes 2 s, longer than inline_timeout, on a file that
// other lands change all the time (each resolved by a fast rule in the lane). Its verdict would be stale at every
// round; once it has lost a round it holds the file with priority, the inline resolutions of the others wait
// behind it, and it lands while the stream of the others is still going. All of them land.
func TestUserLand_aRuleSlowerThanTheTimeoutLandsOnAHotFile(t *testing.T) {
	t.Parallel()

	const stream = 60
	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	// No clock cuts a call, and nothing here waits for a duration: the slow rule is cut by the test, each time the
	// resolver has started it in the lane (it has logged it in inline-slow.log), and the test waits for the lands
	// to end, a hang being the test binary's own timeout to report. (With a real 500 ms a loaded machine cut the
	// fast calls too, and 60 lands then took longer than any bound written here.)
	q := newQueueFixtureMode(t, stream, "slowrule", func(c *config.GraftResolver) { c.InlineTimeout = "30s" })
	require.NotNil(t, s)
	var mu sync.Mutex
	var inflight []context.CancelFunc
	s.landInlineCallContext = func(ctx context.Context, allowed time.Duration) (context.Context, context.CancelFunc) {
		callCtx, cancel := context.WithTimeout(ctx, allowed)
		mu.Lock()
		defer mu.Unlock()
		inflight = append(inflight, cancel)
		return callCtx, cancel
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		cut := 0
		for {
			raw, _ := os.ReadFile(filepath.Join(q.dir, "inline-slow.log"))
			if started := len(raw) / 2; started > cut {
				cut = started
				mu.Lock()
				for _, cancel := range inflight {
					cancel()
				}
				mu.Unlock()
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	slow := gittest.WriteCommit(t, q.cfg, q.repoPath, gittest.WithParents(q.base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "slowrule-1\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"}))

	slowDone := make(chan error, 1)
	go func() {
		_, err := q.land(slow)
		slowDone <- err
	}()
	// The slow land is resolving out of the lane (the lane gave up its rule when the test cut it).
	for unionCalls(t, q.dir)-inlineCalls(t, q.dir) < 1 {
		select {
		case err := <-slowDone:
			t.Fatalf("the slow land ended before it was seen resolving out of the lane: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}

	var landed atomic.Int32
	streamDone := make(chan error, 1)
	go func() {
		for _, c := range q.onA {
			if _, err := q.land(c); err != nil {
				streamDone <- err
				return
			}
			landed.Add(1)
		}
		streamDone <- nil
	}()

	require.NoError(t, <-slowDone, "the slow rule's land")
	require.Less(t, int(landed.Load()), stream, "the slow land landed while the stream of the others was going: they gave way")

	require.NoError(t, <-streamDone)
	got := q.branchBlob(t, "a")
	require.Contains(t, got, "slowrule-1")
	require.Len(t, strings.Split(got, "\n"), stream+2, "ours, the slow rule's line and every other land's")
	require.Empty(t, landTickets.keysOf(q.scope))

	// The holder does not ask its slow rule in the lane again: once for its first lane round, once for the round
	// in which it lost, and no more.
	raw, err := os.ReadFile(filepath.Join(q.dir, "inline-slow.log"))
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw)/2, 2, "the slow rule was asked in the lane by the holder with priority")
}

// TestUserLand_aModelLandThatCannotAskTheRulesReleasesItsTicket: as
// TestUserLand_aModelLandReleasesTheTicketItQueuedFor, but the inline slots stay taken, so the lane round after the
// wait cannot ask the rules at all (no rulesCannot to say that a model is next). The land must not hold the ticket
// of the file during the model call out of the lane.
func TestUserLand_aModelLandThatCannotAskTheRulesReleasesItsTicket(t *testing.T) {
	t.Parallel()

	var s *Server
	operationServerHooks.Store(t.Name(), func(served *Server) { s = served })
	q := newQueueFixtureMode(t, 1, "model", func(c *config.GraftResolver) { c.InlineTimeout = "" })
	require.NotNil(t, s)

	var own []interface{ Release() }
	for {
		slot, ok := s.conflictResolver.TryAcquireInlineSlot()
		if !ok {
			break
		}
		own = append(own, slot)
	}
	defer func() {
		for _, slot := range own {
			slot.Release()
		}
	}()
	holder, err := landTickets.acquire(q.ctx, []string{q.keyOfA})
	require.NoError(t, err)
	defer holder.release()

	model := gittest.WriteCommit(t, q.cfg, q.repoPath, gittest.WithParents(q.base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "model-1\n"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base\n"}))
	done := make(chan error, 1)
	go func() {
		_, err := q.land(model)
		done <- err
	}()
	require.Eventually(t, func() bool { return landTickets.waiting(q.keyOfA) == 1 }, 30*time.Second, 5*time.Millisecond, "the model land queued")
	holder.release()

	require.Eventually(t, func() bool { return unionCalls(t, q.dir)-inlineCalls(t, q.dir) >= 1 }, 30*time.Second, 5*time.Millisecond, "the model call is in flight")
	require.False(t, landTickets.blocked([]string{q.keyOfA}, nil), "the land held the ticket of the file during its model call")

	require.NoError(t, os.WriteFile(filepath.Join(q.dir, "release"), nil, 0o644))
	select {
	case err := <-done:
		requireMergeConflict(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("the model land did not end")
	}
}

// TestLandApplyVerdicts_theHolderWithPriorityDoesNotAskTheRulesInTheLane: a land that holds its files with priority
// and comes back to the lane without a verdict for them (the files changed under it once more) is handed back to
// be resolved out of the lane: it does not ask its rule in the lane again, which would only stall the batch. The
// control, a land without a ticket, asks.
func TestLandApplyVerdicts_theHolderWithPriorityDoesNotAskTheRulesInTheLane(t *testing.T) {
	t.Parallel()

	p := newInlineProbe(t, "", nil)
	req := p.fx.request("Land")
	apply := func(pre *landPreResolution) error {
		_, err := p.s.landApplyVerdicts(p.fx.ctx, req, p.fx.repo, p.fx.tip, p.fx.commit, p.fx.committer(), pre)
		return err
	}

	// Without a ticket the lane asks the rules, and resolves.
	pre := newLandPreResolution()
	_, err := p.s.landApplyVerdicts(p.fx.ctx, req, p.fx.repo, p.fx.tip, p.fx.commit, p.fx.committer(), pre)
	require.NoError(t, err)
	require.Equal(t, 1, inlineCalls(t, p.dir))

	// With the priority it does not: the land is handed back, with the files it needs resolved.
	ticket, err := landTickets.acquire(p.fx.ctx, p.s.landListingKeys(req, p.l))
	require.NoError(t, err)
	defer ticket.release()
	ticket.setPriority()
	held := newLandPreResolution()
	held.ticket = ticket
	var need *landNeedsResolution
	require.ErrorAs(t, apply(held), &need)
	require.NotEmpty(t, need.paths)
	require.False(t, need.queued)
	require.Equal(t, 1, inlineCalls(t, p.dir), "the holder did not ask the rules in the lane")
}
