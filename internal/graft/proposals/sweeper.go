package proposals

import (
	"context"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/git/localrepo"
)

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
// it. A caller waits for such a sweep or runs it, always under its own context: a sweep that fails
// or is cancelled answers only the caller that ran it, and a waiting caller then runs its own. No
// goroutine outlives its caller. When nobody is waiting, what a failed sweep left is cleaned up by
// the next sweep of the branch, or by the housekeeping pass.
type Sweeper struct {
	mu     sync.Mutex
	sweeps map[sweepKey]*sweepState
	// run is one sweep.
	run func(ctx context.Context, repo *localrepo.Repo, dir string, ttl time.Duration, now time.Time) (int, error)
}

type sweepKey struct {
	storage, relativePath, dir string
}

// sweepState is the state of a key while it has callers; mu guards it.
type sweepState struct {
	// users is the number of callers inside Sweep: the state goes with the last of them.
	users int
	// arrivals numbers the callers: each takes the current value as its ticket.
	arrivals uint64
	// covered is the first ticket that no successful sweep has covered yet.
	covered uint64
	// running is closed when the running sweep ends, and nil when none runs.
	running chan struct{}
}

// NewSweeper returns a sweeper with no sweep running.
func NewSweeper() *Sweeper {
	return NewSweeperWithHook(nil)
}

// NewSweeperWithHook returns a sweeper whose sweeps call beforeDelete between deciding what to
// delete and deleting it, when they have something to delete. Only tests pass a hook, to
// interleave a writer there.
func NewSweeperWithHook(beforeDelete func()) *Sweeper {
	return &Sweeper{
		sweeps: make(map[sweepKey]*sweepState),
		run: func(ctx context.Context, repo *localrepo.Repo, dir string, ttl time.Duration, now time.Time) (int, error) {
			return prune(ctx, repo, dir, ttl, now, beforeDelete)
		},
	}
}

// Sweep prunes the proposals of branch (Prune), coalescing with the concurrent sweeps of the same
// repository and branch. See Sweeper for what its result means.
func (s *Sweeper) Sweep(ctx context.Context, repo *localrepo.Repo, branch string, ttl time.Duration, now time.Time) error {
	dir := BranchDir(branch)
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
