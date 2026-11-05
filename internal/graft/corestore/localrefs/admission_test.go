package localrefs

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/testhelper"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

func key(i byte) Key { return Key{i} }

// queued returns an admission whose onWait reports every queued writer.
func queued(waitBound, holdBound time.Duration) (*Admission, <-chan string) {
	a := NewAdmission(waitBound, holdBound)
	waiting := make(chan string, 16)
	a.onWait = func(path string) { waiting <- path }
	return a, waiting
}

// acquireAsync starts Acquire in the background, waits until the writer is
// queued, and returns the channel of its result.
func acquireAsync(ctx context.Context, t *testing.T, a *Admission, waiting <-chan string, path string, k Key) <-chan bool {
	t.Helper()
	result := make(chan bool, 1)
	go func() { result <- a.Acquire(ctx, path, k, WriterHook) }()
	require.Equal(t, path, <-waiting)
	return result
}

func TestAdmissionAdmitsInArrivalOrder(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	a, waiting := queued(time.Hour, time.Hour)

	require.True(t, a.Acquire(ctx, "repo", key(0), WriterHook))
	results := []<-chan bool{
		acquireAsync(ctx, t, a, waiting, "repo", key(1)),
		acquireAsync(ctx, t, a, waiting, "repo", key(2)),
		acquireAsync(ctx, t, a, waiting, "repo", key(3)),
	}
	require.Equal(t, 3.0, testutil.ToFloat64(a.waiting))

	// Another repository is not ordered behind them.
	require.True(t, a.Acquire(ctx, "other", key(9), WriterHook))

	for i, result := range results {
		select {
		case <-result:
			t.Fatalf("writer %d admitted before the release of writer %d", i+1, i)
		default:
		}
		a.Release("repo", key(byte(i)))
		require.True(t, <-result, "writer %d", i+1)
	}
	a.Release("repo", key(3))
	a.Release("other", key(9))

	require.Empty(t, a.queues)
	require.Equal(t, 0.0, testutil.ToFloat64(a.waiting))
}

// TestAdmissionReleaseIsIdempotentPerKey verifies that the second release of a key does nothing, and
// that it reports so: A fails, B is admitted, and A's late release does not free B.
func TestAdmissionReleaseIsIdempotentPerKey(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	a, waiting := queued(time.Hour, time.Hour)

	require.True(t, a.Acquire(ctx, "repo", key(0), WriterHook))
	b := acquireAsync(ctx, t, a, waiting, "repo", key(1))

	require.True(t, a.Release("repo", key(0)))
	require.True(t, <-b)

	require.False(t, a.Release("repo", key(0)), "the release of A is late")
	require.False(t, a.ReleaseUnreported("repo", key(0)), "the exit of A is late")
	require.Equal(t, 0.0, testutil.ToFloat64(a.exited), "A released nothing at its exit")

	// B still holds the admission: a third writer queues behind it.
	c := acquireAsync(ctx, t, a, waiting, "repo", key(2))
	select {
	case <-c:
		t.Fatal("the late release of A freed the admission of B")
	default:
	}

	require.True(t, a.Release("repo", key(1)))
	require.True(t, <-c)
	require.True(t, a.Release("repo", key(2)))
	require.False(t, a.Release("repo", key(2)))
	require.Empty(t, a.queues)
}

// TestAdmissionReleaseUnreported verifies that the exit of a process hands the admission over,
// counts only what it released, and does not touch a writer that only has a place in the queue.
func TestAdmissionReleaseUnreported(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	a, waiting := queued(time.Hour, time.Hour)

	require.True(t, a.Acquire(ctx, "repo", key(0), WriterHook))
	b := acquireAsync(ctx, t, a, waiting, "repo", key(1))

	require.False(t, a.ReleaseUnreported("repo", key(1)), "B is only queued")
	require.Equal(t, 0.0, testutil.ToFloat64(a.exited))

	require.True(t, a.ReleaseUnreported("repo", key(0)))
	require.True(t, <-b)
	require.Equal(t, 1.0, testutil.ToFloat64(a.exited))

	require.False(t, a.ReleaseUnreported("repo", key(0)))
	require.Equal(t, 1.0, testutil.ToFloat64(a.exited))

	require.True(t, a.Release("repo", key(1)))
	require.False(t, a.ReleaseUnreported("repo", key(1)), "B reported its end")
	require.Equal(t, 1.0, testutil.ToFloat64(a.exited))

	var nilAdmission *Admission
	require.False(t, nilAdmission.ReleaseUnreported("repo", key(0)))
}

// givingUpWriter queues a writer with a context of its own, cancels it, and returns once the writer
// stopped waiting and is paused before it takes the lock of the admission. resume lets it go on and
// returns what Acquire returned.
func givingUpWriter(ctx context.Context, t *testing.T, a *Admission, waiting <-chan string, path string, k Key) (resume func() bool) {
	t.Helper()

	paused, proceed := make(chan struct{}), make(chan struct{})
	a.onGiveUp = func(string) {
		close(paused)
		<-proceed
	}

	writerCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	result := acquireAsync(writerCtx, t, a, waiting, path, k)
	cancel()
	<-paused

	return func() bool {
		close(proceed)
		return <-result
	}
}

// TestAdmissionWriterGivingUpAfterItsAdmissionEnded covers a writer that is admitted while it gives
// up, and whose admission ends before it takes the lock: the exit of its process cancels its context
// and releases it. Its turn has gone, so it must not hand anything over again.
func TestAdmissionWriterGivingUpAfterItsAdmissionEnded(t *testing.T) {
	t.Parallel()

	t.Run("the next writer holds it, a later one stays queued", func(t *testing.T) {
		t.Parallel()
		ctx := testhelper.Context(t)
		a, waiting := queued(time.Hour, time.Hour)

		require.True(t, a.Acquire(ctx, "repo", key(0), WriterHook))
		resume := givingUpWriter(ctx, t, a, waiting, "repo", key(1))
		x := acquireAsync(ctx, t, a, waiting, "repo", key(2))
		y := acquireAsync(ctx, t, a, waiting, "repo", key(3))

		require.True(t, a.Release("repo", key(0)), "the holder ends, W is admitted")
		require.True(t, a.Release("repo", key(1)), "the process of W ends, X is admitted")
		require.True(t, <-x)

		require.False(t, resume())

		require.Equal(t, 1.0, testutil.ToFloat64(a.waiting), "Y still waits behind X")
		select {
		case <-y:
			t.Fatal("two live writers hold the admission")
		default:
		}

		require.True(t, a.Release("repo", key(2)))
		require.True(t, <-y)
		require.True(t, a.Release("repo", key(3)))
		require.Empty(t, a.queues)
	})

	t.Run("a newer queue of the repository stays", func(t *testing.T) {
		t.Parallel()
		ctx := testhelper.Context(t)
		a, waiting := queued(time.Hour, time.Hour)

		require.True(t, a.Acquire(ctx, "repo", key(0), WriterHook))
		resume := givingUpWriter(ctx, t, a, waiting, "repo", key(1))

		require.True(t, a.Release("repo", key(0)))
		require.True(t, a.Release("repo", key(1)))
		require.Empty(t, a.queues, "the queue ended with W")

		require.True(t, a.Acquire(ctx, "repo", key(2), WriterHook), "a new writer has a new queue")

		require.False(t, resume())

		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		a.onGiveUp = nil
		require.False(t, a.Acquire(cancelled, "repo", key(3), WriterHook), "the new writer still holds it")
		require.True(t, a.Release("repo", key(2)))
	})

	t.Run("while it still holds it, it hands it over", func(t *testing.T) {
		t.Parallel()
		ctx := testhelper.Context(t)
		a, waiting := queued(time.Hour, time.Hour)

		require.True(t, a.Acquire(ctx, "repo", key(0), WriterHook))
		resume := givingUpWriter(ctx, t, a, waiting, "repo", key(1))
		x := acquireAsync(ctx, t, a, waiting, "repo", key(2))

		require.True(t, a.Release("repo", key(0)), "W is admitted, with nobody using it")
		require.False(t, resume())
		require.True(t, <-x)
		require.False(t, a.Release("repo", key(1)))
		require.True(t, a.Release("repo", key(2)))
		require.Empty(t, a.queues)
	})
}

func TestAdmissionCancelledWriterLeavesTheQueue(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	a, waiting := queued(time.Hour, time.Hour)

	require.True(t, a.Acquire(ctx, "repo", key(0), WriterHook))
	cancelCtx, cancel := context.WithCancel(ctx)
	cancelled := acquireAsync(cancelCtx, t, a, waiting, "repo", key(1))
	next := acquireAsync(ctx, t, a, waiting, "repo", key(2))

	cancel()
	require.False(t, <-cancelled)

	a.Release("repo", key(0))
	require.True(t, <-next)
	a.Release("repo", key(2))
	require.Empty(t, a.queues)
	require.Equal(t, 0.0, testutil.ToFloat64(a.bypassed.WithLabelValues(WriterHook)))
}

func TestAdmissionWaitBoundFallsBackToGitsLock(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	a := NewAdmission(20*time.Millisecond, time.Hour)

	require.True(t, a.Acquire(ctx, "repo", key(0), WriterHook))
	require.False(t, a.Acquire(ctx, "repo", key(1), WriterSyncer))
	require.Equal(t, 1.0, testutil.ToFloat64(a.bypassed.WithLabelValues(WriterSyncer)))

	// The writer that went on without an admission releases nothing.
	a.Release("repo", key(1))
	require.Equal(t, key(0), a.queues["repo"].holder.key)

	a.Release("repo", key(0))
	require.Empty(t, a.queues)
}

func TestAdmissionHoldBoundEndsALostAdmission(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	a, waiting := queued(time.Hour, time.Hour)

	// Writer 0 never releases (git died between its phases); the hold bound
	// ends its admission, which is what its timer calls.
	require.True(t, a.Acquire(ctx, "repo", key(0), WriterHook))
	second := acquireAsync(ctx, t, a, waiting, "repo", key(1))
	a.mu.Lock()
	lost := a.queues["repo"].holder
	a.mu.Unlock()
	a.expire("repo", lost)
	require.True(t, <-second)
	require.Equal(t, 1.0, testutil.ToFloat64(a.expired))

	// The late release of writer 0 does not end the admission of writer 1, and
	// a second expiry of the lost admission does nothing.
	a.Release("repo", key(0))
	a.expire("repo", lost)
	a.mu.Lock()
	require.Equal(t, key(1), a.queues["repo"].holder.key)
	a.mu.Unlock()
	require.Equal(t, 1.0, testutil.ToFloat64(a.expired))

	a.Release("repo", key(1))
	require.Empty(t, a.queues)
}

func TestAdmissionHoldBoundTimer(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	a := NewAdmission(time.Hour, 20*time.Millisecond)

	// The first writer never releases: only the hold bound ends its admission.
	// The bound may pass before the second writer queues (it is then admitted
	// at once) or while it waits (it is then handed the admission): either way
	// it is admitted, by the expiry, and long before its own wait bound.
	require.True(t, a.Acquire(ctx, "repo", key(0), WriterHook))
	require.True(t, a.Acquire(ctx, "repo", key(1), WriterHook))
	require.Equal(t, 1.0, testutil.ToFloat64(a.expired))
}

func TestAdmissionUniqueKeysAreOrdered(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)
	a, waiting := queued(time.Hour, time.Hour)

	release := a.AcquireUnique(ctx, "repo", WriterSyncer)
	require.NotNil(t, release)

	second := make(chan func(), 1)
	go func() { second <- a.AcquireUnique(ctx, "repo", WriterSyncer) }()
	require.Equal(t, "repo", <-waiting)

	release()
	releaseSecond := <-second
	require.NotNil(t, releaseSecond)
	releaseSecond()
	require.Empty(t, a.queues)
}

func TestNilAdmissionAdmitsNothing(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	var a *Admission
	require.False(t, a.Acquire(ctx, "repo", key(0), WriterHook))
	require.Nil(t, a.AcquireUnique(ctx, "repo", WriterSyncer))
	a.Release("repo", key(0))
}
