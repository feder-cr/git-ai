package syncer_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestEnsureFreshWaitsForItsOwnRound: a call that arrives during a sync round
// of its repository joins it and, as that round read the head before the call
// arrived, then waits for a round of its own. Each round runs under its own
// sync_timeout. The call must not give up while its own round is within that
// budget, even when, with the round it joined, it waited longer than one
// sync_timeout since it arrived.
//
// The rounds are made slow at their end (the "synced" point, after the sync
// itself returned): the joined round ends 0.7 x sync_timeout after the call
// arrived, and the call's own round 1.2 x sync_timeout after it, which is
// 0.5 x sync_timeout into that round.
func TestEnsureFreshWaitsForItsOwnRound(t *testing.T) {
	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c1 := a.writeCommit(t, src)
	a.register(t, a.key(localRelPath), src, c1)

	b := a.secondNode(t, sqlstore.SQLite)
	const syncTimeout = 2 * time.Second
	b.core.SyncTimeout = syncTimeout

	var (
		rounds atomic.Int32
		// secondArrived keeps the arrival with its monotonic reading: the wall clock
		// may step meanwhile (a virtual machine's time synchronization does by
		// seconds), which would shorten the second round.
		secondArrived  atomic.Pointer[time.Time]
		firstRoundHeld = make(chan struct{})
		releaseFirst   = make(chan struct{})
	)
	b.sy.SetTestHook(func(point string) {
		if point != "synced" {
			return
		}
		switch rounds.Add(1) {
		case 1:
			close(firstRoundHeld)
			<-releaseFirst
		case 2:
			time.Sleep(time.Until(secondArrived.Load().Add(syncTimeout * 12 / 10)))
		}
	})

	first := make(chan error, 1)
	go func() { first <- b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)) }()
	<-firstRoundHeld

	// The second call arrives while the first round is held after it read the head.
	arrived := time.Now()
	secondArrived.Store(&arrived)
	second := make(chan error, 1)
	go func() { second <- b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)) }()

	time.Sleep(time.Until(arrived.Add(syncTimeout * 7 / 10)))
	close(releaseFirst)

	require.NoError(t, <-first)
	require.NoError(t, <-second, "the call gave up while its own round was within its budget")
	require.Greater(t, time.Since(arrived), syncTimeout, "the call waited longer than one sync_timeout since it arrived")
	require.Equal(t, int32(2), rounds.Load(), "the second call ran a round of its own")
}

// TestEnsureFreshBackstop: a sync round that never ends (a step that ignores its
// context, here the "synced" point held) does not hold its callers forever. A
// call stops waiting after twice sync_timeout and the report after each round,
// with Unavailable.
func TestEnsureFreshBackstop(t *testing.T) {
	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c1 := a.writeCommit(t, src)
	a.register(t, a.key(localRelPath), src, c1)

	b := a.secondNode(t, sqlstore.SQLite)
	const syncTimeout = 200 * time.Millisecond
	b.core.SyncTimeout = syncTimeout

	release := make(chan struct{})
	b.sy.SetTestHook(func(point string) {
		if point == "synced" {
			<-release
		}
	})
	// The held round ends when the test does, so that nothing outlives it.
	t.Cleanup(func() { close(release) })

	start := time.Now()
	err := b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath))
	elapsed := time.Since(start)

	require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "no sync ended within")
	require.GreaterOrEqual(t, elapsed, 2*syncTimeout, "the call waited for the two rounds it may need")
}
