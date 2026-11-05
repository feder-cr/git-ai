// Package localrefs orders and makes durable the local reference writes of the
// storage core in authoritative mode (doc/graft/PLAN7.md, LOCAL REFERENCE
// WRITES RULE).
//
// A reftable repository has one lock for all its references (tables.list.lock),
// and git waits for it by polling with a growing, randomized sleep: there is no
// queue, a newcomer can take the lock before a writer that has waited for long,
// and the lock can sit free while the waiters sleep. Admission puts the writers
// of one node in a FIFO queue per repository in front of that lock. It only
// orders: git's lock stays the only mutual exclusion, so every bound of the
// admission falls back to git's own wait.
//
// Durability moves the durability of the local reference writes from each
// transaction to a durable step that runs outside the writers' path; see
// Durability.
package localrefs

import (
	"container/list"
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Key identifies the holder of an admission between Acquire and Release. The
// voter's key is unique to the process of the transaction, so that a release
// never frees the admission of another process (Voter.admit).
type Key [32]byte

// Writers, the label of the admission metrics.
const (
	// WriterHook is a reference transaction of the reference-transaction hook.
	WriterHook = "hook"
	// WriterSyncer is a reference transaction or a durable step of the syncer.
	WriterSyncer = "syncer"
)

// Admission is the FIFO admission of the reference writers of one node, per
// local repository (keyed by its path). A nil *Admission admits nothing and
// never waits.
type Admission struct {
	// waitBound is how long Acquire waits before the writer goes on to git's
	// lock without being admitted. It equals git's own wait for the lock
	// (reftable.lockTimeout), so that a write that today ends in git's lock
	// timeout (another transaction on the same repository held open by the
	// same caller) does not wait forever on the admission instead.
	waitBound time.Duration
	// holdBound is how long an admission is held at most. The end of a
	// transaction's process releases its admission (ReleaseUnreported), so
	// this only bounds a live process that stays between two phases, and a hook
	// call without an invocation, whose process nobody tracks.
	holdBound time.Duration

	mu     sync.Mutex
	queues map[string]*queue
	// unique numbers the keys of AcquireUnique.
	unique atomic.Uint64
	// onWait is called when a writer starts waiting; tests set it to know that
	// a writer is queued.
	onWait func(path string)
	// onGiveUp is called when a writer stopped waiting, before it takes the lock;
	// tests set it to have the queue change at that point.
	onGiveUp func(path string)

	wait     *prometheus.HistogramVec
	waiting  prometheus.Gauge
	bypassed *prometheus.CounterVec
	expired  prometheus.Counter
	exited   prometheus.Counter
}

var _ prometheus.Collector = (*Admission)(nil)

// queue is the admission of one repository: the holder and the writers waiting
// behind it, oldest first.
type queue struct {
	holder  *ticket
	waiters *list.List
}

// ticket is one writer of a queue.
type ticket struct {
	key Key
	// admitted is closed when the ticket becomes the holder.
	admitted chan struct{}
	// element is the ticket in the waiters while it waits.
	element *list.Element
	// expiry ends the admission at the hold bound.
	expiry *time.Timer
}

// NewAdmission returns an admission with the given bounds, both positive.
func NewAdmission(waitBound, holdBound time.Duration) *Admission {
	a := &Admission{
		waitBound: waitBound,
		holdBound: holdBound,
		queues:    map[string]*queue{},
		wait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "graft_storage_ref_admission_wait_seconds",
			Help:    "Time a local reference writer waited for its admission to the reference lock of a repository, by writer (hook, syncer).",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"writer"}),
		waiting: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_storage_ref_admission_waiting",
			Help: "Number of local reference writers waiting for their admission, all repositories.",
		}),
		bypassed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_storage_ref_admission_bypassed_total",
			Help: "Number of local reference writers that went on to git's reference lock without an admission after the wait bound, by writer.",
		}, []string{"writer"}),
		expired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_ref_admission_expired_total",
			Help: "Number of admissions ended by the hold bound instead of their release (a writer that never reported its end).",
		}),
		exited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_ref_admission_released_unreported_total",
			Help: "Number of admissions released because the git process of the transaction ended, or went on to its next transaction, without reporting Committed or Aborted (a prepare that failed in git, a killed process).",
		}),
	}
	for _, writer := range []string{WriterHook, WriterSyncer} {
		a.wait.WithLabelValues(writer)
		a.bypassed.WithLabelValues(writer)
	}
	return a
}

// Acquire waits for the admission of key to the reference lock of the
// repository at path. It returns true when the writer is admitted, and must
// then call Release(path, key) when it no longer needs the lock. It returns
// false without an admission when ctx ends (the writer is gone) or the wait
// bound passes (the writer goes on to git's lock as without admission).
func (a *Admission) Acquire(ctx context.Context, path string, key Key, writer string) bool {
	if a == nil {
		return false
	}

	start := time.Now()
	t := &ticket{key: key, admitted: make(chan struct{})}

	a.mu.Lock()
	q := a.queues[path]
	if q == nil {
		q = &queue{waiters: list.New()}
		a.queues[path] = q
	}
	if q.holder == nil && q.waiters.Len() == 0 {
		a.admitLocked(path, q, t)
		a.mu.Unlock()
		a.wait.WithLabelValues(writer).Observe(time.Since(start).Seconds())
		return true
	}
	t.element = q.waiters.PushBack(t)
	a.waiting.Inc()
	a.mu.Unlock()
	if a.onWait != nil {
		a.onWait(path)
	}

	bound := time.NewTimer(a.waitBound)
	defer bound.Stop()

	var cancelled bool
	select {
	case <-t.admitted:
		a.wait.WithLabelValues(writer).Observe(time.Since(start).Seconds())
		return true
	case <-bound.C:
	case <-ctx.Done():
		cancelled = true
	}

	if a.onGiveUp != nil {
		a.onGiveUp(path)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	select {
	case <-t.admitted:
		// Admitted while giving up. The admission may have ended meanwhile, by a
		// release or the hold bound, and then belongs to a later writer, in this
		// queue or in a newer one of the repository.
		if cancelled {
			if a.queues[path] == q && q.holder == t {
				t.expiry.Stop()
				a.handOffLocked(path, q)
			}
			return false
		}
		a.wait.WithLabelValues(writer).Observe(time.Since(start).Seconds())
		return true
	default:
	}

	q.waiters.Remove(t.element)
	t.element = nil
	a.waiting.Dec()
	if q.holder == nil && q.waiters.Len() == 0 {
		delete(a.queues, path)
	}
	if !cancelled {
		a.bypassed.WithLabelValues(writer).Inc()
		a.wait.WithLabelValues(writer).Observe(time.Since(start).Seconds())
	}
	return false
}

// AcquireUnique is Acquire with a key of its own. It returns the release
// function, nil when the writer was not admitted.
func (a *Admission) AcquireUnique(ctx context.Context, path, writer string) func() {
	if a == nil {
		return nil
	}

	// The keys of the voter are SHA-256 sums; a counter in the first eight bytes
	// and a marker byte that is set make a key that no other writer uses.
	var key Key
	binary.BigEndian.PutUint64(key[:8], a.unique.Add(1))
	key[31] = 0xff
	key[30] = 0xfe

	if !a.Acquire(ctx, path, key, writer) {
		return nil
	}
	return func() { a.Release(path, key) }
}

// Release ends the admission of key to the reference lock of the repository at
// path, and reports whether key held it. It does nothing when key does not hold
// it: the admission ended already (released before, or at the hold bound), the
// writer was not admitted, or it holds no more than a place in the queue. So a
// release is idempotent per key and never frees the admission of another key.
// Two writers with the same key are not told apart; only a hook call without an
// invocation has a key that another writer can share (Voter.admit).
func (a *Admission) Release(path string, key Key) bool {
	if a == nil {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	q := a.queues[path]
	if q == nil || q.holder == nil || q.holder.key != key {
		return false
	}
	q.holder.expiry.Stop()
	a.handOffLocked(path, q)
	return true
}

// ReleaseUnreported is Release for a transaction known to have ended without a
// report: its process ended, or went on to its next transaction. When key still
// held the admission, git did not report Committed or Aborted: that is counted,
// and true tells the caller to log it.
func (a *Admission) ReleaseUnreported(path string, key Key) bool {
	if !a.Release(path, key) {
		return false
	}
	a.exited.Inc()
	return true
}

// admitLocked makes t the holder of q.
func (a *Admission) admitLocked(path string, q *queue, t *ticket) {
	q.holder = t
	close(t.admitted)
	t.expiry = time.AfterFunc(a.holdBound, func() { a.expire(path, t) })
}

// handOffLocked ends the admission of the holder of q and admits the oldest
// waiter, if any.
func (a *Admission) handOffLocked(path string, q *queue) {
	q.holder = nil
	front := q.waiters.Front()
	if front == nil {
		delete(a.queues, path)
		return
	}
	next := q.waiters.Remove(front).(*ticket)
	next.element = nil
	a.waiting.Dec()
	a.admitLocked(path, q, next)
}

// expire ends the admission of t at the hold bound, if t still holds it.
func (a *Admission) expire(path string, t *ticket) {
	a.mu.Lock()
	defer a.mu.Unlock()

	q := a.queues[path]
	if q == nil || q.holder != t {
		return
	}
	a.expired.Inc()
	a.handOffLocked(path, q)
}

// Describe implements prometheus.Collector.
func (a *Admission) Describe(descs chan<- *prometheus.Desc) {
	a.wait.Describe(descs)
	a.waiting.Describe(descs)
	a.bypassed.Describe(descs)
	a.expired.Describe(descs)
	a.exited.Describe(descs)
}

// Collect implements prometheus.Collector.
func (a *Admission) Collect(metrics chan<- prometheus.Metric) {
	a.wait.Collect(metrics)
	a.waiting.Collect(metrics)
	a.bypassed.Collect(metrics)
	a.expired.Collect(metrics)
	a.exited.Collect(metrics)
}
