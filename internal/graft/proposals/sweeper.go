package proposals

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/transaction/txinfo"
)

// SweepTimeout bounds what a sweep may cost one caller, waiting and running together. A land's
// response, for a land that already landed, waits for it. It is about 800 times the uncontended
// sweep (6 ms p50) and over 3 times the p50 of 64 concurrent writers on reftable (1.4 s), the worst
// measured contention, whose maximum (13.3 s) it cuts: the proposals are an aid, and what a sweep
// that ran out of time left is removed by a later sweep or by the housekeeping pass.
const SweepTimeout = 5 * time.Second

// ErrSweepTimedOut is the cause of a sweep that ran out of SweepTimeout, as opposed to a caller
// that gave up.
var ErrSweepTimedOut = errors.New("the sweep of the proposals ran out of time")

// ProcessSweeper is the sweeper of the process: the servers of a process share a repository's
// references, so they share the guard. Its map holds only the keys in use.
var ProcessSweeper = NewSweeper()

// Sweeper runs at most one Prune at a time per (repository, branch), so that concurrent lands of a
// branch do not issue the same deletions: with the old object IDs they carry, all but one would
// fail. Housekeeping does not go through it: its Prune covers every branch and is rare, and a
// collision with a sweep ends in a failed, counted transaction that the next pass repeats.
//
// A nil return of Sweep means that a sweep that began listing after the call began succeeded, so
// that the proposal a caller wrote just before calling is never left to a sweep that could not see
// it. A caller waits for such a sweep or runs it, always under its own context and for at most
// SweepTimeout, however many others come: a sweep that fails or is cancelled answers only the caller
// that ran it, and a waiting caller then runs its own. No goroutine outlives its caller. When nobody
// is waiting, what a failed sweep left is cleaned up by the next sweep of the branch, or by the
// housekeeping pass.
//
// That holds only where a sweep is a fact all callers share. It is not one in two cases, which go
// straight to Prune (sweepsAreShared).
type Sweeper struct {
	mu     sync.Mutex
	sweeps map[sweepKey]*sweepState
	// run is one sweep.
	run func(ctx context.Context, repo *localrepo.Repo, dir string, ttl time.Duration, now time.Time) (int, error)
	// beforeDelete, when set, is called between deciding what to delete and deleting it.
	beforeDelete func()
	// deadline gives a caller its context with the bound of SweepTimeout.
	deadline func(ctx context.Context) (context.Context, context.CancelFunc)
}

type sweepKey struct {
	storage, relativePath, dir string
}

// sweepState is the state of a key while it has callers; mu guards it.
type sweepState struct {
	// users is the number of callers inside the guard: the state goes with the last of them.
	users int
	// arrivals numbers the callers: each takes the current value as its ticket.
	arrivals uint64
	// covered is the first ticket that no successful sweep has covered yet.
	covered uint64
	// running is closed when the running sweep ends, and nil when none runs.
	running chan struct{}
}

// SweeperOption changes a Sweeper. Only tests use them.
type SweeperOption func(*Sweeper)

// WithBeforeDelete makes the sweeper call fn between deciding what to delete and deleting it, in a
// sweep that has something to delete and in Drop, to interleave a writer there.
func WithBeforeDelete(fn func()) SweeperOption {
	return func(s *Sweeper) { s.beforeDelete = fn }
}

// WithDeadlineContext replaces the bound of SweepTimeout, so that a test decides when it passes: fn
// returns the context a caller goes on with, whose cause is ErrSweepTimedOut when it has passed.
func WithDeadlineContext(fn func(ctx context.Context) (context.Context, context.CancelFunc)) SweeperOption {
	return func(s *Sweeper) { s.deadline = fn }
}

// NewSweeper returns a sweeper with no sweep running.
func NewSweeper(opts ...SweeperOption) *Sweeper {
	s := &Sweeper{
		sweeps: make(map[sweepKey]*sweepState),
		deadline: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return sweepDeadline(ctx, SweepTimeout)
		},
	}
	s.run = func(ctx context.Context, repo *localrepo.Repo, dir string, ttl time.Duration, now time.Time) (int, error) {
		return prune(ctx, repo, dir, ttl, now, s.beforeDelete)
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// sweepDeadline bounds ctx by timeout, with ErrSweepTimedOut as the cause.
func sweepDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, timeout, ErrSweepTimedOut)
}

// sweepsAreShared reports whether callers can share a sweep, which they cannot in two cases:
//   - On MVCC, each RPC reads the snapshot it pinned and publishes its changes at its end, if it
//     succeeds. A sweep of another RPC may not see what this one wrote, and its deletions are thrown
//     away if that RPC fails, so it can answer for nobody else. Two RPCs cannot collide either: each
//     deletes in its own snapshot.
//   - In a Praefect transaction, the replicas must vote on the same updates, and which caller
//     coalesces with which depends on the timing of each node: one would vote on a sweep that another
//     skipped. A vote may also block for a long time, which must not hold the guard. A transaction
//     that cannot be read counts as one.
//
// Neither gets the bound of SweepTimeout: they wait for nobody, and killing a git process that is
// voting or working on a pinned snapshot would only add a way for the replicas to differ.
func sweepsAreShared(ctx context.Context) bool {
	if mvcc.CacheFromContext(ctx) != nil {
		return false
	}
	_, err := txinfo.TransactionFromContext(ctx)
	return errors.Is(err, txinfo.ErrTransactionNotFound)
}

// Sweep prunes the proposals of branch (Prune), coalescing with the concurrent sweeps of the same
// repository and branch where callers can share them. See Sweeper for what its result means. It
// wraps ErrSweepTimedOut when it ran out of SweepTimeout.
func (s *Sweeper) Sweep(ctx context.Context, repo *localrepo.Repo, branch string, ttl time.Duration, now time.Time) error {
	dir := BranchDir(branch)
	if !sweepsAreShared(ctx) {
		_, err := s.run(ctx, repo, dir, ttl, now)
		return err
	}

	ctx, cancel := s.deadline(ctx)
	defer cancel()
	err := s.sweepGuarded(ctx, repo, dir, ttl, now)
	if err != nil && errors.Is(context.Cause(ctx), ErrSweepTimedOut) {
		return fmt.Errorf("%w: %w", ErrSweepTimedOut, err)
	}
	return err
}

// Drop deletes the proposal reference name of a change that landed (see drop). It is a deletion
// that no other land shares, and goes through no guard.
func (s *Sweeper) Drop(ctx context.Context, repo *localrepo.Repo, name git.ReferenceName) error {
	return drop(ctx, repo, name, s.beforeDelete)
}

// sweepGuarded waits for a sweep that covers the caller, or runs it.
func (s *Sweeper) sweepGuarded(ctx context.Context, repo *localrepo.Repo, dir string, ttl time.Duration, now time.Time) error {
	key := sweepKey{storage: repo.GetStorageName(), relativePath: repo.GetRelativePath(), dir: dir}

	s.mu.Lock()
	state := s.sweeps[key]
	if state == nil {
		state = &sweepState{}
		s.sweeps[key] = state
	}
	state.users++
	ticket := state.arrivals
	state.arrivals++
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		state.users--
		if state.users == 0 {
			delete(s.sweeps, key)
		}
	}()

	for {
		s.mu.Lock()
		if state.covered > ticket {
			s.mu.Unlock()
			return nil
		}
		running := state.running
		if running == nil {
			done := make(chan struct{})
			state.running = done
			// Every caller that has arrived by now is covered by this sweep, which lists after this.
			covers := state.arrivals
			s.mu.Unlock()
			return s.runSweep(ctx, repo, dir, ttl, now, state, done, covers)
		}
		s.mu.Unlock()

		select {
		case <-running:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runSweep runs one sweep and always gives up the running slot, even if the sweep panics, so that a
// sweep can never leave the callers of its key waiting.
func (s *Sweeper) runSweep(ctx context.Context, repo *localrepo.Repo, dir string, ttl time.Duration, now time.Time, state *sweepState, done chan struct{}, covers uint64) error {
	succeeded := false
	defer func() {
		s.mu.Lock()
		state.running = nil
		if succeeded {
			state.covered = covers
		}
		s.mu.Unlock()
		close(done)
	}()

	_, err := s.run(ctx, repo, dir, ttl, now)
	succeeded = err == nil
	return err
}
