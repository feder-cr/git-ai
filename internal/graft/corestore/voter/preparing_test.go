package voter_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// countingBlobs counts the Put calls of a BlobStore.
type countingBlobs struct {
	corestore.BlobStore

	mu   sync.Mutex
	puts []string
}

func (c *countingBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	c.mu.Lock()
	c.puts = append(c.puts, key)
	c.mu.Unlock()
	return c.BlobStore.Put(ctx, key, r)
}

func (c *countingBlobs) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.puts)
}

// countPuts makes the fixture's core count its uploads.
func (f *fixture) countPuts() *countingBlobs {
	blobs := &countingBlobs{BlobStore: f.core.Blobs}
	f.core.Blobs = blobs
	return blobs
}

func (f *fixture) preparing(input []byte) {
	f.voter.Preparing(f.ctx, f.payload, input)
}

// TestPreparingUploadsThePackOnce checks the LOCK AND HISTORY RULE: Preparing
// builds and uploads the pack before git takes the locks, and Prepared uses it
// without uploading again; the changeset references the pack Preparing
// uploaded.
func TestPreparingUploadsThePackOnce(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, mode := range []corestore.Mode{corestore.ModeAuthoritative, corestore.ModeShadow} {
			t.Run(string(mode), func(t *testing.T) {
				f := newFixture(t, fixtureOptions{mode: mode, dialect: dialect})
				gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
				f.seed(t, ref, corestore.Target(f.a.String()))
				blobs := f.countPuts()

				input := line(f.a.String(), f.c.String(), ref)
				f.preparing(input)
				require.Equal(t, 1, blobs.count(), "Preparing uploads the pack")
				require.Equal(t, 1, f.voter.PreparedPacks())

				require.NoError(t, f.prepared(input))
				require.Equal(t, 1, blobs.count(), "Prepared does not upload again")
				require.Zero(t, f.voter.PreparedPacks(), "Prepared consumes the prepared pack")

				changeset := f.lastChangeset(t)
				require.Equal(t, blobs.puts[0], changeset.PackKey)
				require.Positive(t, changeset.PackBytes)
				require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref))
				require.Equal(t, float64(changeset.PackBytes), metricValue(t, f.voter, "graft_storage_pack_bytes_total", nil))
				require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_preparing_seconds", nil))
				// Authoritative mode observes every step of the Preparing phase once, as an
				// update (main existed); the shadow mode, which builds its pack another way,
				// none.
				steps := float64(0)
				if mode == corestore.ModeAuthoritative {
					steps = 1
				}
				for _, step := range []string{"record", "walk", "pack", "upload"} {
					require.Equal(t, steps, metricValue(t, f.voter, "graft_storage_preparing_step_seconds", map[string]string{"kind": "update", "step": step}), step)
				}
				require.Zero(t, metricValue(t, f.voter, "graft_storage_preparing_step_seconds", map[string]string{"kind": "create"}))
				// The Prepared phase reads the record and the local references and commits,
				// once each in authoritative mode; the pack was prepared, and the durability
				// of the fixture is not deferred.
				for _, step := range []string{"record", "local_read", "commit"} {
					require.Equal(t, steps, metricValue(t, f.voter, "graft_storage_prepared_step_seconds", map[string]string{"kind": "update", "step": step}), step)
				}
				for _, step := range []string{"pack", "durability"} {
					require.Zero(t, metricValue(t, f.voter, "graft_storage_prepared_step_seconds", map[string]string{"step": step}), step)
				}

				f.voter.Committed(f.ctx, f.payload, input)
				require.Equal(t, []noteCall{{repoPath: f.repoPath, repoID: f.repoID, seq: f.seq(2)}}, f.applier.calls)
			})
		}
	})
}

// TestPreparedWithoutPreparing checks that Prepared still does all the work
// when no Preparing phase ran (or it ran for another input).
func TestPreparedWithoutPreparing(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		t.Run("no Preparing", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			blobs := f.countPuts()

			require.NoError(t, f.prepared(line(f.zero, f.b.String(), ref)))
			require.Equal(t, 1, blobs.count(), "Prepared uploads the pack itself")
			require.Zero(t, metricValue(t, f.voter, "graft_storage_preparing_step_seconds", nil),
				"the steps are those of the Preparing phase, not of a pack that Prepared builds")
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_prepared_step_seconds", map[string]string{"kind": "create", "step": "pack"}),
				"the pack that Prepared builds is a step of the Prepared phase")
			require.Equal(t, blobs.puts[0], f.lastChangeset(t).PackKey)
			require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, ref))
		})

		t.Run("no pack step without new tips", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
			f.seed(t, ref, corestore.Target(f.a.String()))

			require.NoError(t, f.prepared(line(f.a.String(), f.zero, ref)))
			require.Zero(t, metricValue(t, f.voter, "graft_storage_prepared_step_seconds", map[string]string{"step": "pack"}),
				"a deletion builds no pack")
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_prepared_step_seconds", map[string]string{"step": "commit"}))
		})

		t.Run("a failed step is not observed", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.b)
			f.seed(t, ref, corestore.Target(f.b.String()))

			// The database holds b: the commit of an update from a fails its compare-and-swap.
			require.Error(t, f.prepared(line(f.a.String(), f.c.String(), ref)))
			for step, want := range map[string]float64{"record": 1, "local_read": 1, "pack": 1, "commit": 0} {
				require.Equal(t, want, metricValue(t, f.voter, "graft_storage_prepared_step_seconds", map[string]string{"step": step}), step)
			}
			require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, ref))
		})

		t.Run("Preparing of another input", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			blobs := f.countPuts()

			f.preparing(line(f.zero, f.a.String(), ref))
			require.Equal(t, 1, blobs.count())
			require.Equal(t, float64(4), metricValue(t, f.voter, "graft_storage_preparing_step_seconds", map[string]string{"kind": "create"}),
				"a transaction that creates its reference is a create in each of the four steps")

			require.NoError(t, f.prepared(line(f.zero, f.c.String(), ref)))
			require.Equal(t, 2, blobs.count(), "the pack of another transaction is not used")
			require.Equal(t, blobs.puts[1], f.lastChangeset(t).PackKey)
			require.Equal(t, 1, f.voter.PreparedPacks(), "the other transaction's pack is still pending")
		})

		t.Run("Preparing in another repository", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			blobs := f.countPuts()

			input := line(f.zero, f.c.String(), ref)
			f.preparing(input)
			require.Equal(t, 1, blobs.count())

			// The repository is removed and created again at the same path: the
			// prepared pack was uploaded under the old repository ID.
			deleted, err := f.core.Meta.DeleteRepository(f.ctx, corestore.KeyOf(f.repo))
			require.NoError(t, err)
			require.True(t, deleted)
			record, _, err := f.core.Meta.RegisterRepository(f.ctx, corestore.KeyOf(f.repo), gittest.DefaultObjectHash.Format, corestore.InitialState{
				Changeset: &corestore.ChangesetInput{
					Updates:   []corestore.RefUpdate{{Name: "HEAD", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")}},
					Operation: "create_repository",
					NodeID:    "node-b",
				},
			})
			require.NoError(t, err)
			require.NotEqual(t, f.repoID, record.ID)

			require.NoError(t, f.prepared(input))
			require.Equal(t, 2, blobs.count(), "the pack is uploaded again for the new repository")
			repoID, _, ok := corestore.ParsePackKey(blobs.puts[1])
			require.True(t, ok)
			require.Equal(t, record.ID, repoID)

			refs, _, err := f.core.Meta.Refs(f.ctx, record.ID)
			require.NoError(t, err)
			require.Equal(t, corestore.Target(f.c.String()), refs[ref])
		})
	})
}

// TestPreparingNeverFails checks that a failing Preparing phase never fails
// the transaction: Prepared then behaves as without it.
func TestPreparingNeverFails(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		t.Run("bucket failure in Preparing", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			blobs := &failingFirstPut{BlobStore: f.core.Blobs}
			f.core.Blobs = blobs

			input := line(f.zero, f.c.String(), ref)
			f.preparing(input)
			require.Equal(t, 1, f.voter.PreparedPacks(), "the failure is remembered")

			require.NoError(t, f.prepared(input), "Prepared uploads the pack itself")
			require.Equal(t, 2, blobs.calls())
			require.NotEmpty(t, f.lastChangeset(t).PackKey)
			require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref))
		})

		t.Run("closed database in authoritative mode", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			require.NoError(t, f.core.Meta.Close())

			input := line(f.zero, f.a.String(), ref)
			f.preparing(input)
			err := f.prepared(input)
			require.Equal(t, codes.Unavailable, status.Code(err), "error %v", err)
		})

		t.Run("closed database in shadow mode", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect})
			require.NoError(t, f.core.Meta.Close())

			input := line(f.zero, f.a.String(), ref)
			f.preparing(input)
			require.NoError(t, f.prepared(input))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil),
				"only the Prepared phase counts a shadow error")
		})

		t.Run("malformed input and untracked repositories prepare nothing", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect, untracked: true})
			blobs := f.countPuts()

			f.preparing([]byte("garbage\n"))
			f.preparing(line(f.zero, f.a.String(), ref))
			require.Zero(t, f.voter.PreparedPacks())
			require.Zero(t, blobs.count())
		})
	})
}

// TestPreparingShadowTimeoutCoversBothPhases checks that in shadow mode the
// shadow timeout is the budget of Preparing and Prepared together: when
// Preparing used it up, Prepared gives up at once and the write proceeds.
func TestPreparingShadowTimeoutCoversBothPhases(t *testing.T) {
	f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow})
	f.core.ShadowTimeout = time.Nanosecond

	input := line(f.zero, f.a.String(), "refs/heads/main")
	f.preparing(input)
	require.NoError(t, f.prepared(input))
	require.Equal(t, uint64(0), f.headSeq(t), "nothing is recorded once the budget ran out")
	require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
}

// TestAbortedDropsThePreparedPack checks that a transaction aborted before its
// Prepared phase leaves no pending pack behind.
func TestAbortedDropsThePreparedPack(t *testing.T) {
	f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative})

	input := line(f.zero, f.a.String(), "refs/heads/main")
	f.preparing(input)
	require.Equal(t, 1, f.voter.PreparedPacks())

	f.voter.Aborted(f.ctx, f.payload, input)
	require.Zero(t, f.voter.PreparedPacks())
	require.Zero(t, metricValue(t, f.voter, "graft_storage_local_commit_failures_total", nil),
		"nothing was committed to the database")
	require.Equal(t, uint64(0), f.headSeq(t))
}

// failingFirstPut fails the first Put.
type failingFirstPut struct {
	corestore.BlobStore

	mu sync.Mutex
	n  int
}

func (f *failingFirstPut) Put(ctx context.Context, key string, r io.Reader) error {
	f.mu.Lock()
	f.n++
	first := f.n == 1
	f.mu.Unlock()

	if first {
		return io.ErrUnexpectedEOF
	}
	return f.BlobStore.Put(ctx, key, r)
}

func (f *failingFirstPut) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}
