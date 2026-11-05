package voter

import (
	"time"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// SetBeforeAcquire sets the function called right before a transaction with an invocation takes its
// admission.
func SetBeforeAcquire(v *Voter, f func()) { v.beforeAcquire = f }

// The pending set is exported to the external tests of the package.

// PendingTTL and MaxPendingEntries bound the pending set.
const (
	PendingTTL        = pendingTTL
	MaxPendingEntries = maxPendingEntries
)

// PendingSet is the pending set of a voter, for tests.
type PendingSet struct{ set *pendingSet[committedEntry] }

// NewPendingSet returns an empty pending set.
func NewPendingSet() PendingSet { return PendingSet{set: newPendingSet[committedEntry]()} }

// Add remembers the transaction of input in the repository at relativePath of
// the storage.
func (p PendingSet) Add(storageName, relativePath string, input []byte, repoID int64, seq uint64, now time.Time) {
	p.set.add(pendingKey(testRepo(storageName, relativePath), input), committedEntry{repoID: repoID, seq: seq}, now)
}

// Take pops the transaction of input in the repository at relativePath of the
// storage.
func (p PendingSet) Take(storageName, relativePath string, input []byte, now time.Time) (repoID int64, seq uint64, ok bool) {
	e, ok := p.set.take(pendingKey(testRepo(storageName, relativePath), input), now)
	return e.repoID, e.seq, ok
}

// Has reports whether the transaction of input in the repository at
// relativePath of the storage is pending, without popping it.
func (p PendingSet) Has(storageName, relativePath string, input []byte, now time.Time) bool {
	return p.set.has(pendingKey(testRepo(storageName, relativePath), input), now)
}

// Len returns the number of entries, expired ones included.
func (p PendingSet) Len() int { return p.set.len() }

// PreparedPacks returns the number of packs prepared in Preparing whose
// Prepared or Aborted phase has not run yet.
func (v *Voter) PreparedPacks() int { return v.packs.len() }

// PendingEntries returns the number of transactions committed in Prepared whose
// Committed or Aborted phase has not run yet.
func (v *Voter) PendingEntries() int { return v.pending.len() }

func testRepo(storageName, relativePath string) *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: storageName, RelativePath: relativePath}
}
