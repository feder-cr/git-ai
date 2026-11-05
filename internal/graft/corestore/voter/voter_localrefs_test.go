package voter_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// admissionHeld reports whether a writer holds the admission of the fixture's
// repository: an Acquire with a cancelled context is admitted at once when
// nobody holds it, and gives up otherwise.
func (f *fixture) admissionHeld(t *testing.T) bool {
	t.Helper()

	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	probe := localrefs.Key{0xaa, 0xbb}
	if f.core.RefAdmission.Acquire(ctx, f.repoPath, probe, localrefs.WriterSyncer) {
		f.core.RefAdmission.Release(f.repoPath, probe)
		return false
	}
	return true
}

// markers returns the dirty markers of the fixture's storage.
func (f *fixture) markers(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(f.cfg.Storages[0].Path, localrefs.MarkerDirName))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// deferred starts a deferred git process for the fixture's repository, as
// UpdateWithHooks does before its update-ref.
func (f *fixture) deferred(t *testing.T) (end func()) {
	t.Helper()
	end, ok := f.voter.DeferReferenceWrites(f.ctx, f.repo)
	require.True(t, ok)
	return end
}

// TestLocalReferenceWritesOfACommittedTransaction follows one authoritative
// transaction through the LOCAL REFERENCE WRITES RULE (doc/graft/PLAN7.md):
// its git process is live from before it starts until it exits, it is
// admitted at the end of Preparing and released in Committed, and its
// repository is marked dirty after the database commit in Prepared.
func TestLocalReferenceWritesOfACommittedTransaction(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	const ref = "refs/heads/main"
	input := line(f.zero, f.a.String(), ref)

	require.True(t, f.voter.DeferredReferenceDurability())
	require.False(t, f.admissionHeld(t))

	end := f.deferred(t)
	f.voter.Preparing(f.ctx, f.payload, input)
	require.True(t, f.admissionHeld(t), "the transaction is admitted at the end of Preparing")
	require.Empty(t, f.markers(t), "nothing is dirty before the database commit")

	require.NoError(t, f.prepared(input))
	require.Len(t, f.markers(t), 1)
	require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_prepared_step_seconds", map[string]string{"kind": "create", "step": "durability"}),
		"the dirty marker is a step of the Prepared phase")

	gittest.WriteRef(t, f.cfg, f.repoPath, git.ReferenceName(ref), f.a)
	f.voter.Committed(f.ctx, f.payload, input)
	require.False(t, f.admissionHeld(t), "Committed releases the admission")
	require.Len(t, f.applier.calls, 1, "the local commit is still noted")

	// The git process may still compact until it exits.
	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, true))
	require.Len(t, f.markers(t), 1)

	end()
	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, false))
	require.Empty(t, f.markers(t))
}

// TestLocalReferenceWritesOfAnAbortedTransaction verifies that an abort after
// Prepared releases the admission, and that the repository is settled once its
// git process exited.
func TestLocalReferenceWritesOfAnAbortedTransaction(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	input := line(f.zero, f.a.String(), "refs/heads/main")

	end := f.deferred(t)
	f.voter.Preparing(f.ctx, f.payload, input)
	require.NoError(t, f.prepared(input))
	f.voter.Aborted(f.ctx, f.payload, input)
	require.False(t, f.admissionHeld(t))
	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, false))
	require.Len(t, f.markers(t), 1, "the git process has not exited yet")

	end()
	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, false))
	require.Empty(t, f.markers(t))
}

// TestLocalReferenceWritesWhenGitSkipsItsHooks covers the transactions whose
// later phases git never reports: a prepare that fails (a stale old value of a
// writer that lost a race, a lock that stayed busy, a name conflict), a commit
// whose finish fails, and the Committed phase of a symbolic reference that
// names its target instead of the reference of Preparing. The live count
// follows the git processes, so none of them leaks: the repository settles once
// both processes exited.
func TestLocalReferenceWritesWhenGitSkipsItsHooks(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	winner := line(f.zero, f.a.String(), "refs/heads/main")
	loser := line(f.zero, f.b.String(), "refs/heads/main")

	endWinner := f.deferred(t)
	endLoser := f.deferred(t)
	f.voter.Preparing(f.ctx, f.payload, winner)
	f.voter.Preparing(f.ctx, f.payload, loser)

	require.NoError(t, f.prepared(winner))
	gittest.WriteRef(t, f.cfg, f.repoPath, "refs/heads/main", f.a)
	// The Committed phase names another reference than Preparing (a symbolic
	// reference split into its target).
	f.voter.Committed(f.ctx, f.payload, line(f.zero, f.a.String(), "HEAD"))
	endWinner()

	// The loser's prepare failed in git: no Prepared, no Aborted, no Committed.
	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, false))
	require.Len(t, f.markers(t), 1, "the loser's git process is still live")
	endLoser()
	require.NoError(t, f.core.RefDurability.Settle(f.repoPath, false))
	require.Empty(t, f.markers(t))
}

// TestLocalReferenceWritesOfARejectedTransaction verifies that a transaction
// the database rejects marks nothing: git writes nothing for it.
func TestLocalReferenceWritesOfARejectedTransaction(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	const ref = "refs/heads/main"
	f.seed(t, ref, corestore.Target(f.b.String()))
	input := line(f.a.String(), f.c.String(), ref)

	end := f.deferred(t)
	f.voter.Preparing(f.ctx, f.payload, input)
	requireMismatch(t, f.prepared(input), ref, f.a.String(), f.b.String())
	f.voter.Aborted(f.ctx, f.payload, input)
	end()

	require.False(t, f.admissionHeld(t))
	require.Empty(t, f.markers(t))
}

// TestLocalReferenceWritesWhenTheMarkerCannotBeWritten verifies that git does
// not write references without fsync when the repository cannot be marked
// dirty: the transaction is aborted after the database commit, which catch-up
// repairs like any local failure.
func TestLocalReferenceWritesWhenTheMarkerCannotBeWritten(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	// The marker directory exists (the ownership created it); no file can be
	// created in it.
	markerDir := filepath.Join(f.cfg.Storages[0].Path, localrefs.MarkerDirName)
	require.NoError(t, os.Chmod(markerDir, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(markerDir, 0o700)) })

	input := line(f.zero, f.a.String(), "refs/heads/main")
	err := f.prepared(input)
	require.Equal(t, codes.Unavailable, status.Code(err), "error %v", err)
	require.ErrorContains(t, err, "write the dirty marker")
	require.Equal(t, uint64(1), f.headSeq(t), "the database committed the changeset before the marker")
}

// TestDeferredReferenceDurabilityByMode verifies that references are written
// without fsync in authoritative mode by the process that owns the deferred
// durability only: off and shadow modes, authoritative mode without the boot
// identity, and a process that does not own the storage (the new process of a
// graceful upgrade) keep git's fsync of references.
func TestDeferredReferenceDurabilityByMode(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	var nilVoter *voter.Voter
	require.False(t, nilVoter.DeferredReferenceDurability())

	off := voter.New(&corestore.Core{Mode: corestore.ModeOff}, testhelper.NewLogger(t), localrepo.Factory{}, nil, nil)
	require.False(t, off.DeferredReferenceDurability())

	shadow := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, localRefs: true})
	require.False(t, shadow.voter.DeferredReferenceDurability())
	_, ok := shadow.voter.DeferReferenceWrites(ctx, shadow.repo)
	require.False(t, ok)

	authoritative := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative})
	require.False(t, authoritative.voter.DeferredReferenceDurability())
	_, ok = authoritative.voter.DeferReferenceWrites(ctx, authoritative.repo)
	require.False(t, ok)

	deferred := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, localRefs: true})
	require.True(t, deferred.voter.DeferredReferenceDurability())

	// Another process owns the storage: this one's durability holds no lock.
	notOwner := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative})
	durability, err := localrefs.NewDurability(config.NewLocator(notOwner.cfg), 0, localrefs.WithBootID("boot"))
	require.NoError(t, err)
	notOwner.core.RefDurability = durability
	require.False(t, notOwner.voter.DeferredReferenceDurability())
	_, ok = notOwner.voter.DeferReferenceWrites(ctx, notOwner.repo)
	require.False(t, ok)
	// Its Prepared phase marks nothing: its writes are made with fsync.
	require.NoError(t, notOwner.prepared(line(notOwner.zero, notOwner.a.String(), "refs/heads/main")))
	require.Empty(t, notOwner.markers(t))
}

// TestLocalReferenceWritesNotInShadowMode verifies that shadow mode, where git
// is the source of truth, neither admits nor marks.
func TestLocalReferenceWritesNotInShadowMode(t *testing.T) {
	t.Parallel()

	f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, localRefs: true})
	input := line(f.zero, f.a.String(), "refs/heads/main")

	f.voter.Preparing(f.ctx, f.payload, input)
	require.False(t, f.admissionHeld(t))
	require.NoError(t, f.prepared(input))
	require.Empty(t, f.markers(t))
	f.voter.Committed(f.ctx, f.payload, input)
}
