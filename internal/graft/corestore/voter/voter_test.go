package voter_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// forEachDialect runs fn once per database dialect (SQLite, and Postgres when
// GRAFT_TEST_POSTGRES_DSN is set).
func forEachDialect(t *testing.T, fn func(t *testing.T, dialect sqlstore.Dialect)) {
	t.Helper()

	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			fn(t, dialect)
		})
	}
}

// fakeApplier records the NoteCommitted calls of a voter.
type fakeApplier struct {
	calls []noteCall
}

type noteCall struct {
	repoPath string
	repoID   int64
	seq      uint64
}

func (f *fakeApplier) NoteCommitted(_ context.Context, repoPath string, repoID int64, seq uint64) {
	f.calls = append(f.calls, noteCall{repoPath: repoPath, repoID: repoID, seq: seq})
}

// fixture is a repository, a storage core and a voter of that core.
type fixture struct {
	ctx      context.Context
	cfg      config.Cfg
	repo     *gitalypb.Repository
	repoPath string
	core     *corestore.Core
	// repoID is the database ID of the repository, 0 when it is not tracked.
	repoID int64
	// base is the head sequence of the repository after its registration: 1
	// (changeset 1 records HEAD, as CreateRepository and an import do), or 0
	// for an incomplete import or an untracked repository.
	base    uint64
	voter   *voter.Voter
	applier *fakeApplier
	// logs holds what the voter logged.
	logs    testhelper.LoggerHook
	payload gitcmd.HooksPayload
	zero    string
	// a, b and c are a chain of commits (a <- b <- c) that no reference
	// points to.
	a, b, c git.ObjectID
}

type fixtureOptions struct {
	mode      corestore.Mode
	dialect   sqlstore.Dialect
	untracked bool
	// incomplete registers the repository without changeset 1 (head sequence
	// 0): an import that is not complete (IMPORT RULE).
	incomplete bool
	// files makes the repository use the files reference backend instead of
	// the package's default, reftable (see TestMain).
	files bool
	// localRefs gives the core the admission and the deferred durability of
	// the local reference writes (coretest.EnableLocalRefs).
	localRefs bool
}

func newFixture(t *testing.T, opts fixtureOptions) *fixture {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	if opts.files {
		useFilesBackend(t, cfg, repoPath)
	}
	// The shadow budget is not binding here: these tests check what a shadow
	// write records, which the budget can legitimately prevent under load
	// (doc/graft/PLAN7.md, SHADOW BUDGET requirement). The budget itself is
	// tested with short budgets set explicitly (TestShadowBudgetOverrunInPrepared,
	// TestPreparingShadowTimeoutCoversBothPhases, TestShadowTimeoutNeverFails).
	core := coretest.Open(t, coretest.Options{Mode: opts.mode, Dialect: opts.dialect, ShadowTimeout: time.Minute})

	var repoID int64
	var base uint64
	if !opts.untracked {
		var initial corestore.InitialState
		if !opts.incomplete {
			initial.Changeset = &corestore.ChangesetInput{
				Updates:   []corestore.RefUpdate{{Name: "HEAD", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")}},
				Operation: "create_repository",
				Actor:     "test",
				NodeID:    "node-a",
			}
		}
		record, res, err := core.Meta.RegisterRepository(ctx, corestore.KeyOf(repo), gittest.DefaultObjectHash.Format, initial)
		require.NoError(t, err)
		repoID, base = record.ID, res.Seq
	}

	logger := testhelper.NewLogger(t)
	locator := config.NewLocator(cfg)
	if opts.localRefs {
		coretest.EnableLocalRefs(t, core, cfg)
	}
	cache := catfile.NewCache(cfg)
	t.Cleanup(cache.Stop)
	factory := localrepo.NewFactory(logger, locator, gittest.NewCommandFactory(t, cfg), cache)

	applier := &fakeApplier{}

	a := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("a"))
	b := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("b"), gittest.WithParents(a))
	c := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("c"), gittest.WithParents(b))

	return &fixture{
		ctx:      ctx,
		cfg:      cfg,
		repo:     repo,
		repoPath: repoPath,
		core:     core,
		repoID:   repoID,
		base:     base,
		voter:    voter.New(core, logger, factory, locator, applier),
		applier:  applier,
		logs:     testhelper.AddLoggerHook(logger),
		payload:  gitcmd.HooksPayload{Repo: repo, ObjectFormat: gittest.DefaultObjectHash.Format},
		zero:     gittest.DefaultObjectHash.ZeroOID.String(),
		a:        a,
		b:        b,
		c:        c,
	}
}

// line returns the reference-transaction hook input of one update.
func line(oldValue, newValue, ref string) []byte {
	return []byte(oldValue + " " + newValue + " " + ref + "\n")
}

// prepared runs the Prepared phase with a test operation.
func (f *fixture) prepared(input []byte) error {
	return f.voter.Prepared(f.ctx, f.payload, input, changelog.Meta{Operation: "test", Actor: "tester"})
}

// seed makes the database hold target for ref, in a changeset of its own.
func (f *fixture) seed(t *testing.T, ref string, target corestore.Target) {
	t.Helper()

	_, err := f.core.Meta.CommitChangeset(f.ctx, corestore.ChangesetInput{
		RepoID:    f.repoID,
		Updates:   []corestore.RefUpdate{{Name: ref, Expect: corestore.ExpectAny, New: target}},
		Operation: "seed",
		Actor:     "test",
		NodeID:    "node-a",
	})
	require.NoError(t, err)
}

// dbRef returns the database value of ref, "" when absent.
func (f *fixture) dbRef(t *testing.T, ref string) corestore.Target {
	t.Helper()

	refs, _, err := f.core.Meta.Refs(f.ctx, f.repoID)
	require.NoError(t, err)
	return refs[ref]
}

// headSeq returns the number of changesets recorded after the registration
// (the head sequence minus base).
func (f *fixture) headSeq(t *testing.T) uint64 {
	t.Helper()

	r, err := f.core.Meta.GetRepository(f.ctx, corestore.KeyOf(f.repo))
	require.NoError(t, err)
	require.GreaterOrEqual(t, r.HeadSeq, f.base)
	return r.HeadSeq - f.base
}

// seq returns the sequence of the n-th changeset recorded after the
// registration.
func (f *fixture) seq(n uint64) uint64 {
	return f.base + n
}

// lastChangeset returns the changeset with the head sequence, which must have
// been recorded after the registration.
func (f *fixture) lastChangeset(t *testing.T) corestore.Changeset {
	t.Helper()

	require.NotZero(t, f.headSeq(t))
	head := f.seq(f.headSeq(t))
	changesets, err := f.core.Meta.ListChangesets(f.ctx, f.repoID, head-1, head)
	require.NoError(t, err)
	require.Len(t, changesets, 1)
	return changesets[0]
}

// referenceBackend returns the reference backend of the fixture's repository.
func (f *fixture) referenceBackend(t *testing.T) git.ReferenceBackend {
	t.Helper()

	backend, err := localrepo.NewTestRepo(t, f.cfg, f.repo).ReferenceBackend(f.ctx)
	require.NoError(t, err)
	return backend
}

// useFilesBackend migrates the repository at repoPath, which has no
// references yet, to the files reference backend.
func useFilesBackend(t *testing.T, cfg config.Cfg, repoPath string) {
	t.Helper()

	gittest.Exec(t, cfg, "-C", repoPath, "refs", "migrate", "--ref-format="+git.ReferenceBackendFiles.Name)

	backend, err := gitcmd.DetectReferenceBackend(testhelper.Context(t), repoPath)
	require.NoError(t, err)
	require.Equal(t, git.ReferenceBackendFiles.Name, backend.Name)
}

// metricValue sums the samples of name whose labels include labels. It registers
// the collector (the voter, the admission) in a pedantic registry, which also
// checks that the collector is consistent.
func metricValue(t *testing.T, collector prometheus.Collector, name string, labels map[string]string) float64 {
	t.Helper()

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(collector))

	families, err := reg.Gather()
	require.NoError(t, err)

	var sum float64
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if !hasLabels(metric.GetLabel(), labels) {
				continue
			}
			if c := metric.GetCounter(); c != nil {
				sum += c.GetValue()
			}
			if g := metric.GetGauge(); g != nil {
				sum += g.GetValue()
			}
			if h := metric.GetHistogram(); h != nil {
				sum += float64(h.GetSampleCount())
			}
		}
	}
	return sum
}

func hasLabels(pairs []*dto.LabelPair, want map[string]string) bool {
	for name, value := range want {
		found := false
		for _, pair := range pairs {
			if pair.GetName() == name && pair.GetValue() == value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func requireMismatch(t *testing.T, err error, ref, expected, actual string) {
	t.Helper()

	var mismatch updateref.MismatchingStateError
	require.True(t, errors.As(err, &mismatch), "error %v", err)
	require.Equal(t, ref, mismatch.ReferenceName)
	require.Equal(t, expected, mismatch.ExpectedObjectID)
	require.Equal(t, actual, mismatch.ActualObjectID)
}

func TestPreparedCompareAndSwap(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		newAuthoritative := func(t *testing.T) *fixture {
			return newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
		}

		t.Run("zero old value and no local reference creates the reference", func(t *testing.T) {
			f := newAuthoritative(t)

			require.NoError(t, f.prepared(line(f.zero, f.a.String(), ref)))
			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref))
			require.Equal(t, uint64(1), f.headSeq(t))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "authoritative", "result": "committed"}))
		})

		t.Run("zero old value and no local reference conflicts with a database value", func(t *testing.T) {
			f := newAuthoritative(t)
			f.seed(t, ref, corestore.Target(f.b.String()))

			err := f.prepared(line(f.zero, f.a.String(), ref))
			requireMismatch(t, err, ref, f.zero, f.b.String())
			require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, ref), "a conflict writes nothing")
			require.Equal(t, uint64(1), f.headSeq(t))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "authoritative", "result": "conflict"}))
		})

		t.Run("old value updates the reference", func(t *testing.T) {
			f := newAuthoritative(t)
			f.seed(t, ref, corestore.Target(f.b.String()))

			require.NoError(t, f.prepared(line(f.b.String(), f.a.String(), ref)))
			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref))
		})

		t.Run("old value conflicts with another database value", func(t *testing.T) {
			f := newAuthoritative(t)
			f.seed(t, ref, corestore.Target(f.b.String()))

			err := f.prepared(line(f.a.String(), f.c.String(), ref))
			requireMismatch(t, err, ref, f.a.String(), f.b.String())
			require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, ref))
		})

		t.Run("old value conflicts with an absent database reference", func(t *testing.T) {
			f := newAuthoritative(t)

			err := f.prepared(line(f.a.String(), f.c.String(), ref))
			requireMismatch(t, err, ref, f.a.String(), f.zero)
			require.Empty(t, f.dbRef(t, ref))
			require.Equal(t, uint64(0), f.headSeq(t))
		})

		t.Run("zero old value with a local reference is forced", func(t *testing.T) {
			f := newAuthoritative(t)
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
			f.seed(t, ref, corestore.Target(f.b.String()))

			require.NoError(t, f.prepared(line(f.zero, f.c.String(), ref)))
			require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref))

			changeset := f.lastChangeset(t)
			require.False(t, changeset.Forced, "a forced git update is not a forced (shadow) changeset")
			require.Equal(t, []corestore.AppliedUpdate{{Name: ref, Old: corestore.Target(f.b.String()), New: corestore.Target(f.c.String())}}, changeset.Updates)
		})

		t.Run("deletion with an old value deletes the reference", func(t *testing.T) {
			f := newAuthoritative(t)
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
			f.seed(t, ref, corestore.Target(f.a.String()))

			require.NoError(t, f.prepared(line(f.a.String(), f.zero, ref)))
			require.Empty(t, f.dbRef(t, ref))
		})

		t.Run("unchanged update is a no-op", func(t *testing.T) {
			f := newAuthoritative(t)
			f.seed(t, ref, corestore.Target(f.a.String()))

			input := line(f.a.String(), f.a.String(), ref)
			require.NoError(t, f.prepared(input))
			require.Equal(t, uint64(1), f.headSeq(t), "a no-op records no changeset")
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "authoritative", "result": "noop"}))

			f.voter.Committed(f.ctx, f.payload, input)
			require.Empty(t, f.applier.calls, "a no-op has no sequence to note")
		})

		t.Run("one conflicting reference rejects the whole transaction", func(t *testing.T) {
			f := newAuthoritative(t)
			f.seed(t, "refs/heads/other", corestore.Target(f.b.String()))

			input := append(line(f.zero, f.a.String(), ref), line(f.a.String(), f.c.String(), "refs/heads/other")...)
			err := f.prepared(input)
			requireMismatch(t, err, "refs/heads/other", f.a.String(), f.b.String())
			require.Empty(t, f.dbRef(t, ref), "nothing of a conflicting transaction is written")
			require.Equal(t, uint64(1), f.headSeq(t))
		})

		t.Run("several references in one changeset", func(t *testing.T) {
			f := newAuthoritative(t)

			input := append(line(f.zero, f.a.String(), ref), line(f.zero, f.c.String(), "refs/tags/v1")...)
			input = append(input, line(f.zero, f.b.String(), "refs/merge-requests/1/head")...)
			require.NoError(t, f.prepared(input))

			changeset := f.lastChangeset(t)
			require.Equal(t, f.seq(1), changeset.Seq)
			require.Equal(t, "test", changeset.Operation)
			require.Equal(t, "tester", changeset.Actor)
			require.Equal(t, "node-a", changeset.NodeID)
			require.Equal(t, []corestore.AppliedUpdate{
				{Name: ref, New: corestore.Target(f.a.String())},
				{Name: "refs/tags/v1", New: corestore.Target(f.c.String())},
				{Name: "refs/merge-requests/1/head", New: corestore.Target(f.b.String())},
			}, changeset.Updates)
		})
	})
}

func TestPreparedSymbolicHead(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
		f.seed(t, "HEAD", corestore.SymbolicTarget("refs/heads/main"))

		// git reports the zero OID as the old value of a symbolic reference. HEAD
		// exists locally, so the update is forced.
		require.NoError(t, f.prepared(line(f.zero, "ref:refs/heads/feature", "HEAD")))
		require.Equal(t, corestore.SymbolicTarget("refs/heads/feature"), f.dbRef(t, "HEAD"))

		changeset := f.lastChangeset(t)
		require.Empty(t, changeset.PackKey, "a symbolic reference brings no objects")

		entries, err := f.core.Meta.ScanChangelog(f.ctx, f.repoID, 0, 0)
		require.NoError(t, err)
		require.Empty(t, entries, "HEAD is not in the changelog")
	})
}

// TestPreparedDanglingSymbolicReference checks that a symbolic reference whose
// target does not exist, which for-each-ref leaves out, is still known to
// exist locally: git reports the zero value as its old value, so its update
// and its deletion are forced instead of conflicting with the database row.
func TestPreparedDanglingSymbolicReference(t *testing.T) {
	const ref = "refs/heads/sym"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		newDangling := func(t *testing.T) *fixture {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			gittest.Exec(t, f.cfg, "-C", f.repoPath, "symbolic-ref", ref, "refs/heads/missing")
			f.seed(t, ref, corestore.SymbolicTarget("refs/heads/missing"))
			return f
		}

		t.Run("retargeting is forced", func(t *testing.T) {
			f := newDangling(t)

			require.NoError(t, f.prepared(line(f.zero, "ref:refs/heads/other", ref)))
			require.Equal(t, corestore.SymbolicTarget("refs/heads/other"), f.dbRef(t, ref))
			require.Equal(t, uint64(2), f.headSeq(t))
		})

		t.Run("deletion is forced", func(t *testing.T) {
			f := newDangling(t)

			require.NoError(t, f.prepared(line(f.zero, f.zero, ref)))
			require.Empty(t, f.dbRef(t, ref))
			require.Equal(t, uint64(2), f.headSeq(t))
		})

		t.Run("a symbolic creation still requires an absent database reference", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			f.seed(t, ref, corestore.SymbolicTarget("refs/heads/missing"))

			err := f.prepared(line(f.zero, "ref:refs/heads/other", ref))
			var mismatch updateref.MismatchingStateError
			require.True(t, errors.As(err, &mismatch), "error %v", err)
			require.Equal(t, ref, mismatch.ReferenceName)
			require.Equal(t, corestore.SymbolicTarget("refs/heads/missing"), f.dbRef(t, ref))
			require.Equal(t, uint64(1), f.headSeq(t))
		})
	})
}

func TestPreparedForceDeletion(t *testing.T) {
	const ref = "refs/heads/gone"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		t.Run("authoritative reftable deletes the reference", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			require.Equal(t, git.ReferenceBackendReftables.Name, f.referenceBackend(t).Name)
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
			f.seed(t, ref, corestore.Target(f.a.String()))

			require.NoError(t, f.prepared(line(f.zero, f.zero, ref)))
			require.Empty(t, f.dbRef(t, ref))
			require.Equal(t, uint64(2), f.headSeq(t))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "authoritative", "result": "committed"}))
		})

		t.Run("authoritative files backend is a FailedPrecondition", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect, files: true})
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
			f.seed(t, ref, corestore.Target(f.a.String()))

			for _, input := range [][]byte{
				line(f.zero, f.zero, ref),
				line(f.a.String(), f.zero, ref),
				line(f.a.String(), f.b.String(), ref),
				line(f.zero, f.c.String(), "refs/heads/new"),
			} {
				err := f.prepared(input)
				require.Equal(t, codes.FailedPrecondition, status.Code(err), "error %v", err)
				require.ErrorContains(t, err, "authoritative storage mode requires a reftable repository")
			}

			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref), "nothing is committed")
			require.Equal(t, uint64(1), f.headSeq(t))
			require.Equal(t, float64(4), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "authoritative", "result": "error"}))
		})

		// On the files backend git deletes a packed reference in two transactions,
		// the packed-refs half and then the loose half. Without an expected old value
		// both have the same input.
		t.Run("shadow files backend forced deletion in both halves", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect, files: true})
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
			f.seed(t, ref, corestore.Target(f.a.String()))

			input := line(f.zero, f.zero, ref)
			require.NoError(t, f.prepared(input))
			require.Empty(t, f.dbRef(t, ref), "the first half deletes the reference")
			require.Equal(t, uint64(2), f.headSeq(t))

			require.NoError(t, f.prepared(input))
			require.Equal(t, uint64(2), f.headSeq(t), "the second half is a no-op")

			f.voter.Committed(f.ctx, f.payload, input)
			f.voter.Committed(f.ctx, f.payload, input)
			require.Equal(t, []noteCall{{repoPath: f.repoPath, repoID: f.repoID, seq: f.seq(2)}}, f.applier.calls)

			require.Zero(t, metricValue(t, f.voter, "graft_storage_shadow_divergences_total", nil))
			require.Zero(t, metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "committed"}))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "noop"}))
		})

		t.Run("shadow files backend checked deletion after the packed-refs half is no divergence", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect, files: true})
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
			f.seed(t, ref, corestore.Target(f.a.String()))

			require.NoError(t, f.prepared(line(f.zero, f.zero, ref)))
			require.NoError(t, f.prepared(line(f.a.String(), f.zero, ref)))

			require.Empty(t, f.dbRef(t, ref))
			require.Equal(t, uint64(2), f.headSeq(t))
			require.Zero(t, metricValue(t, f.voter, "graft_storage_shadow_divergences_total", nil))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "noop"}))
		})

		t.Run("shadow deletion of a reference at another database value is a divergence", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect, files: true})
			gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
			f.seed(t, ref, corestore.Target(f.b.String()))

			require.NoError(t, f.prepared(line(f.a.String(), f.zero, ref)))
			require.Empty(t, f.dbRef(t, ref))
			require.True(t, f.lastChangeset(t).Forced)
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_divergences_total", nil))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "conflict"}))
		})

		t.Run("shadow creation over an absent reference that is deleted elsewhere is a divergence", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect, files: true})

			// git moves the reference from a to b, the database does not have it: only a
			// deletion finds an absent reference without diverging.
			require.NoError(t, f.prepared(line(f.a.String(), f.b.String(), ref)))
			require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, ref))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_divergences_total", nil))
		})
	})
}

func TestPreparedForceDeletionWithUpdateIsRecorded(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
		gittest.WriteRef(t, f.cfg, f.repoPath, "refs/heads/gone", f.a)
		f.seed(t, "refs/heads/gone", corestore.Target(f.a.String()))

		// A transaction that also updates a reference is never a packed-refs cleanup.
		input := append(line(f.zero, f.zero, "refs/heads/gone"), line(f.zero, f.b.String(), "refs/heads/new")...)
		require.NoError(t, f.prepared(input))
		require.Empty(t, f.dbRef(t, "refs/heads/gone"))
		require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, "refs/heads/new"))
	})
}

func TestPreparedPack(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
		gittest.WriteRef(t, f.cfg, f.repoPath, ref, f.a)
		f.seed(t, ref, corestore.Target(f.a.String()))

		require.NoError(t, f.prepared(line(f.a.String(), f.c.String(), ref)))

		changeset := f.lastChangeset(t)
		require.NotEmpty(t, changeset.PackKey)
		require.Positive(t, changeset.PackBytes)
		repoID, _, ok := corestore.ParsePackKey(changeset.PackKey)
		require.True(t, ok, "pack key %q", changeset.PackKey)
		require.Equal(t, f.repoID, repoID)
		require.Equal(t, float64(changeset.PackBytes), metricValue(t, f.voter, "graft_storage_pack_bytes_total", nil))

		blob, err := f.core.Blobs.Get(f.ctx, changeset.PackKey)
		require.NoError(t, err)
		defer func() { require.NoError(t, blob.Close()) }()

		emptyProto, _ := gittest.CreateRepository(t, f.ctx, f.cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
		empty := localrepo.NewTestRepo(t, f.cfg, emptyProto)
		require.NoError(t, gitops.IndexPack(f.ctx, empty, blob))

		for _, oid := range []git.ObjectID{f.b, f.c} {
			ok, err := empty.HasRevision(f.ctx, git.Revision(oid.String()+"^{commit}"))
			require.NoError(t, err)
			require.True(t, ok, "the pack holds the new commit %s", oid)
		}

		ok, err = empty.HasRevision(f.ctx, git.Revision(f.a.String()+"^{commit}"))
		require.NoError(t, err)
		require.False(t, ok, "the pack does not hold what a reference already reaches")
	})
}

func TestPreparedWithoutNewObjectsHasNoPack(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
		gittest.WriteRef(t, f.cfg, f.repoPath, "refs/heads/main", f.c)

		require.NoError(t, f.prepared(line(f.zero, f.b.String(), "refs/heads/old")))

		changeset := f.lastChangeset(t)
		require.Empty(t, changeset.PackKey)
		require.Zero(t, changeset.PackBytes)
	})
}

func TestShadowDivergenceIsForced(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect})
		f.seed(t, ref, corestore.Target(f.b.String()))

		require.NoError(t, f.prepared(line(f.a.String(), f.c.String(), ref)))
		require.Equal(t, corestore.Target(f.c.String()), f.dbRef(t, ref), "shadow mode records the git value")

		changeset := f.lastChangeset(t)
		require.True(t, changeset.Forced)
		require.Equal(t, []corestore.AppliedUpdate{{Name: ref, Old: corestore.Target(f.b.String()), New: corestore.Target(f.c.String())}}, changeset.Updates)

		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_divergences_total", nil))
		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "conflict"}))
		require.Zero(t, metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))

		entries, err := f.core.Meta.ScanChangelog(f.ctx, f.repoID, 0, 0)
		require.NoError(t, err)
		require.Empty(t, entries, "the file changelog stays the source of truth in shadow mode")
	})
}

func TestShadowDivergenceWithoutChange(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect})
		f.seed(t, ref, corestore.Target(f.c.String()))

		// git moves the reference from a to c, the database already has c.
		require.NoError(t, f.prepared(line(f.a.String(), f.c.String(), ref)))
		require.Equal(t, uint64(1), f.headSeq(t))
		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_divergences_total", nil))
		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "noop"}))
	})
}

func TestShadowCommits(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect})

		input := line(f.zero, f.a.String(), ref)
		require.NoError(t, f.prepared(input))
		f.voter.Committed(f.ctx, f.payload, input)

		require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref))
		require.False(t, f.lastChangeset(t).Forced)
		require.Equal(t, []noteCall{{repoPath: f.repoPath, repoID: f.repoID, seq: f.seq(1)}}, f.applier.calls)
		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "committed"}))
		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_prepared_seconds", nil))
	})
}

func TestShadowClosedDatabaseNeverFails(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect})
		require.NoError(t, f.core.Meta.Close())

		input := line(f.zero, f.a.String(), ref)
		require.NoError(t, f.prepared(input))
		f.voter.Committed(f.ctx, f.payload, input)

		require.Empty(t, f.applier.calls)
		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "error"}))
	})
}

func TestShadowTimeoutNeverFails(t *testing.T) {
	f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow})

	// A context that is already done stands for a shadow timeout.
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()

	require.NoError(t, f.voter.Prepared(ctx, f.payload, line(f.zero, f.a.String(), "refs/heads/main"), changelog.Meta{Operation: "test"}))
	require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
}

func TestAuthoritativeClosedDatabaseIsUnavailable(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
		require.NoError(t, f.core.Meta.Close())

		err := f.prepared(line(f.zero, f.a.String(), "refs/heads/main"))
		require.Equal(t, codes.Unavailable, status.Code(err), "error %v", err)
		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "authoritative", "result": "error"}))
	})
}

func TestMalformedInput(t *testing.T) {
	zero := gittest.DefaultObjectHash.ZeroOID.String()
	inputs := map[string][]byte{
		"not a hook line":             []byte("this is not a hook line\n"),
		"two fields":                  []byte(zero + " refs/heads/main\n"),
		"invalid object ID":           line(zero, "not-an-object-id", "refs/heads/main"),
		"object ID of the other hash": line(zero, otherFormatOID(len(zero)), "refs/heads/main"),
		"invalid symbolic target":     line(zero, "ref:refs/../x", "HEAD"),
	}

	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			t.Run("shadow is counted and skipped", func(t *testing.T) {
				f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow})

				require.NoError(t, f.prepared(input))
				require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
				require.Equal(t, uint64(0), f.headSeq(t))
			})

			t.Run("authoritative aborts", func(t *testing.T) {
				f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative})

				require.Error(t, f.prepared(input))
				require.Equal(t, uint64(0), f.headSeq(t))
			})
		})
	}
}

// TestInvalidReferenceNames checks that names that git's reference rules
// reject never reach the database, where the catch-up of every node would
// refuse them. The updates are otherwise valid: they point to an existing
// commit.
func TestInvalidReferenceNames(t *testing.T) {
	for _, name := range []string{
		"refs/../x",
		"HEAD:x",
		"refs/heads/x.lock",
		"refs/heads/a..b",
		"ORIG_HEAD",
		"-x",
		"refs/heads/",
		"refs/heads/.hidden",
		"refs/heads/a@{1}",
		"refs/heads/a\x01b",
	} {
		// refchange.ParseHookLine already rejects a line whose name has a
		// control character, before the name itself is validated.
		lineRejected := strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f })

		t.Run(name, func(t *testing.T) {
			t.Run("shadow is counted and skipped", func(t *testing.T) {
				f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow})

				// The valid update of the same transaction is not recorded either.
				input := append(line(f.zero, f.a.String(), "refs/heads/valid"), line(f.zero, f.a.String(), name)...)
				require.NoError(t, f.prepared(input))
				require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
				require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "error"}))
				require.Equal(t, uint64(0), f.headSeq(t))
			})

			t.Run("authoritative aborts", func(t *testing.T) {
				f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative})

				err := f.prepared(line(f.zero, f.a.String(), name))
				require.ErrorContains(t, err, "malformed reference transaction input")
				if lineRejected {
					require.ErrorContains(t, err, "is not <old> <new> <ref>")
				} else {
					require.ErrorContains(t, err, "reference name")
				}
				require.Equal(t, uint64(0), f.headSeq(t))
			})
		})
	}

	t.Run("HEAD and fully qualified names are accepted", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative})

		input := append(line(f.zero, "ref:refs/heads/main", "HEAD"), line(f.zero, f.a.String(), "refs/heads/main")...)
		input = append(input, line(f.zero, f.a.String(), "refs/merge-requests/1/head")...)
		require.NoError(t, f.prepared(input))
		require.Equal(t, corestore.SymbolicTarget("refs/heads/main"), f.dbRef(t, "HEAD"))
		require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, "refs/merge-requests/1/head"))
	})
}

// otherFormatOID returns a hex object ID of the other object format than the
// one whose IDs have length n.
func otherFormatOID(n int) string {
	if n == 64 {
		return strings.Repeat("a", 40)
	}
	return strings.Repeat("a", 64)
}

func TestUntrackedRepository(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		t.Run("authoritative is a FailedPrecondition", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect, untracked: true})

			err := f.prepared(line(f.zero, f.a.String(), "refs/heads/main"))
			require.Equal(t, codes.FailedPrecondition, status.Code(err), "error %v", err)
		})

		t.Run("shadow skips silently and counts", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect, untracked: true})

			require.NoError(t, f.prepared(line(f.zero, f.a.String(), "refs/heads/main")))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_untracked_writes_total", nil))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "skipped"}))
			require.Zero(t, metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
		})
	})
}

// TestIncompleteImport checks the IMPORT RULE: a repository registered without
// changeset 1 (head sequence 0) is an import that is not complete, treated like
// an untracked one. A write recorded now would make the import changeset, which
// expects every reference to be absent, conflict forever.
func TestIncompleteImport(t *testing.T) {
	const ref = "refs/heads/slipped"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		t.Run("authoritative is a FailedPrecondition", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect, incomplete: true})

			input := line(f.zero, f.a.String(), ref)
			err := f.prepared(input)
			require.Equal(t, codes.FailedPrecondition, status.Code(err), "error %v", err)
			require.ErrorContains(t, err, "repository import is not complete; run ImportRepository again")

			f.voter.Committed(f.ctx, f.payload, input)
			require.Empty(t, f.applier.calls)

			refs, head, err := f.core.Meta.Refs(f.ctx, f.repoID)
			require.NoError(t, err)
			require.Zero(t, head, "nothing is committed")
			require.Empty(t, refs)
			require.Zero(t, metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"result": "committed"}))
		})

		t.Run("shadow skips and counts as untracked", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect, incomplete: true})

			input := line(f.zero, f.a.String(), ref)
			require.NoError(t, f.prepared(input))
			f.voter.Committed(f.ctx, f.payload, input)
			require.Empty(t, f.applier.calls)

			refs, head, err := f.core.Meta.Refs(f.ctx, f.repoID)
			require.NoError(t, err)
			require.Zero(t, head)
			require.Empty(t, refs)
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_untracked_writes_total", nil))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_changesets_total", map[string]string{"mode": "shadow", "result": "skipped"}))
			require.Zero(t, metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
		})

		t.Run("a completed registration is written to", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})

			require.NoError(t, f.prepared(line(f.zero, f.a.String(), ref)))
			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref))
		})
	})
}

func TestStorageCoreOff(t *testing.T) {
	ctx := testhelper.Context(t)
	payload := gitcmd.HooksPayload{Repo: &gitalypb.Repository{StorageName: "default", RelativePath: "x.git"}}
	input := []byte("garbage\n")

	var nilVoter *voter.Voter
	require.NoError(t, nilVoter.Prepared(ctx, payload, input, changelog.Meta{}))
	nilVoter.Committed(ctx, payload, input)
	nilVoter.Aborted(ctx, payload, input)
	require.False(t, nilVoter.Authoritative())

	off := voter.New(&corestore.Core{Mode: corestore.ModeOff}, testhelper.NewLogger(t), localrepo.Factory{}, nil, nil)
	require.NoError(t, off.Prepared(ctx, payload, input, changelog.Meta{}))
	require.False(t, off.Authoritative())
}

func TestAuthoritativeChangelogEntriesWithMeta(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
		require.True(t, f.voter.Authoritative())

		notified, unsubscribe := corestore.SubscribeChangelog(f.repoID)
		defer unsubscribe()

		recordedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		members := []changelog.Member{
			{OriginalCommit: f.a.String(), Commit: f.a.String(), Actor: "bob"},
			{OriginalCommit: f.c.String(), Commit: f.b.String(), Actor: "carol", Rewritten: true},
		}
		meta := changelog.Meta{
			Operation:        "land_batch",
			Actor:            "alice",
			Rewritten:        true,
			Members:          members,
			MembersTruncated: true,
			RecordedAt:       recordedAt,
		}
		input := append(line(f.zero, f.b.String(), ref), line(f.zero, f.a.String(), "refs/keep-around/x")...)
		require.NoError(t, f.voter.Prepared(f.ctx, f.payload, input, meta))

		select {
		case <-notified:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "no changelog notification")
		}

		entries, err := f.core.Meta.ScanChangelog(f.ctx, f.repoID, 0, 0)
		require.NoError(t, err)
		require.Len(t, entries, 1, "only branches and tags are logged")
		require.Equal(t, uint64(1), entries[0].Sequence)
		require.Equal(t, ref, entries[0].Reference)
		require.Equal(t, "land_batch", entries[0].Operation)
		require.Equal(t, "alice", entries[0].Actor)
		require.True(t, entries[0].Rewritten)
		require.Equal(t, members, entries[0].Members)
		require.True(t, entries[0].MembersTruncated)
		require.Equal(t, f.zero, entries[0].OldOID)
		require.Equal(t, f.b.String(), entries[0].NewOID)
		require.True(t, recordedAt.Equal(entries[0].RecordedAt), "recorded at %v", entries[0].RecordedAt)
	})
}

func TestCommittedNotesTheSequence(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})

		first := line(f.zero, f.a.String(), ref)
		require.NoError(t, f.prepared(first))
		f.voter.Committed(f.ctx, f.payload, first)

		// The hooks payload of a quarantined write carries the quarantine
		// directory; the noted path is the repository's own.
		quarantined := f.payload
		quarantined.Repo = &gitalypb.Repository{
			StorageName:                   f.repo.GetStorageName(),
			RelativePath:                  f.repo.GetRelativePath(),
			GitObjectDirectory:            "objects/tmp_quarantine",
			GitAlternateObjectDirectories: []string{"objects"},
		}
		second := line(f.a.String(), f.b.String(), ref)
		require.NoError(t, f.prepared(second))
		f.voter.Committed(f.ctx, quarantined, second)

		// Committed of a transaction that is not pending, or a second time, notes nothing.
		f.voter.Committed(f.ctx, f.payload, second)
		f.voter.Committed(f.ctx, f.payload, line(f.b.String(), f.c.String(), ref))

		require.Equal(t, []noteCall{
			{repoPath: f.repoPath, repoID: f.repoID, seq: f.seq(1)},
			{repoPath: f.repoPath, repoID: f.repoID, seq: f.seq(2)},
		}, f.applier.calls)
	})
}

func TestAbortedAfterCommit(t *testing.T) {
	const ref = "refs/heads/main"

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})

		// Aborted of a transaction that the database did not commit is not a failure.
		f.voter.Aborted(f.ctx, f.payload, line(f.zero, f.c.String(), ref))
		require.Zero(t, metricValue(t, f.voter, "graft_storage_local_commit_failures_total", nil))

		input := line(f.zero, f.a.String(), ref)
		require.NoError(t, f.prepared(input))
		f.voter.Aborted(f.ctx, f.payload, input)
		require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_local_commit_failures_total", nil))
		require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref), "the database stays the truth")

		// The aborted transaction is no longer pending.
		f.voter.Committed(f.ctx, f.payload, input)
		require.Empty(t, f.applier.calls)
	})
}

func TestFaultHookPoints(t *testing.T) {
	const ref = "refs/heads/main"
	boom := errors.New("boom")

	setFault := func(t *testing.T, at string) {
		voter.FaultHook = func(point string) error {
			if point == at {
				return boom
			}
			return nil
		}
		t.Cleanup(func() { voter.FaultHook = nil })
	}

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		t.Run("after upload leaves an orphan pack and no changeset", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			setFault(t, "after_upload")

			err := f.prepared(line(f.zero, f.a.String(), ref))
			require.ErrorIs(t, err, boom)
			require.Equal(t, uint64(0), f.headSeq(t))
			require.Empty(t, f.dbRef(t, ref))
		})

		t.Run("after commit leaves the changeset and an aborted git transaction", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeAuthoritative, dialect: dialect})
			setFault(t, "after_commit")

			input := line(f.zero, f.a.String(), ref)
			err := f.prepared(input)
			require.ErrorIs(t, err, boom)
			require.Equal(t, uint64(1), f.headSeq(t))
			require.Equal(t, corestore.Target(f.a.String()), f.dbRef(t, ref))

			f.voter.Aborted(f.ctx, f.payload, input)
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_local_commit_failures_total", nil))
		})

		t.Run("shadow never fails", func(t *testing.T) {
			f := newFixture(t, fixtureOptions{mode: corestore.ModeShadow, dialect: dialect})
			setFault(t, "after_upload")

			require.NoError(t, f.prepared(line(f.zero, f.a.String(), ref)))
			require.Equal(t, uint64(0), f.headSeq(t))
			require.Equal(t, float64(1), metricValue(t, f.voter, "graft_storage_shadow_errors_total", nil))
		})
	})
}

func TestPendingSet(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("take pops once", func(t *testing.T) {
		p := voter.NewPendingSet()
		p.Add("default", "a.git", []byte("x"), 1, 7, now)

		_, _, ok := p.Take("default", "b.git", []byte("x"), now)
		require.False(t, ok, "the relative path is part of the key")
		_, _, ok = p.Take("other", "a.git", []byte("x"), now)
		require.False(t, ok, "the storage is part of the key")
		require.True(t, p.Has("default", "./a.git/", []byte("x"), now), "the relative path is normalized like the database key")
		_, _, ok = p.Take("default", "a.git", []byte("y"), now)
		require.False(t, ok, "the input is part of the key")

		repoID, seq, ok := p.Take("default", "a.git", []byte("x"), now)
		require.True(t, ok)
		require.Equal(t, int64(1), repoID)
		require.Equal(t, uint64(7), seq)

		_, _, ok = p.Take("default", "a.git", []byte("x"), now)
		require.False(t, ok)
	})

	t.Run("entries expire", func(t *testing.T) {
		p := voter.NewPendingSet()
		p.Add("default", "a.git", []byte("old"), 1, 1, now)
		p.Add("default", "a.git", []byte("young"), 1, 2, now.Add(voter.PendingTTL/2))

		_, _, ok := p.Take("default", "a.git", []byte("old"), now.Add(voter.PendingTTL))
		require.False(t, ok)
		_, seq, ok := p.Take("default", "a.git", []byte("young"), now.Add(voter.PendingTTL))
		require.True(t, ok)
		require.Equal(t, uint64(2), seq)
	})

	t.Run("adding drops expired entries", func(t *testing.T) {
		p := voter.NewPendingSet()
		for i := 0; i < 10; i++ {
			p.Add("default", "a.git", []byte{byte(i)}, 1, uint64(i), now)
		}
		p.Add("default", "a.git", []byte("new"), 1, 100, now.Add(voter.PendingTTL))
		require.Equal(t, 1, p.Len())
	})

	t.Run("the oldest entries are evicted beyond the limit", func(t *testing.T) {
		p := voter.NewPendingSet()
		for i := 0; i < voter.MaxPendingEntries+2; i++ {
			p.Add("default", "a.git", []byte{byte(i), byte(i >> 8), byte(i >> 16)}, 1, uint64(i), now)
		}
		require.Equal(t, voter.MaxPendingEntries, p.Len())

		for _, i := range []int{0, 1} {
			_, _, ok := p.Take("default", "a.git", []byte{byte(i), byte(i >> 8), byte(i >> 16)}, now)
			require.False(t, ok, "entry %d was evicted", i)
		}
		last := voter.MaxPendingEntries + 1
		_, seq, ok := p.Take("default", "a.git", []byte{byte(last), byte(last >> 8), byte(last >> 16)}, now)
		require.True(t, ok)
		require.Equal(t, uint64(last), seq)
	})

	t.Run("adding the same transaction again replaces it", func(t *testing.T) {
		p := voter.NewPendingSet()
		p.Add("default", "a.git", []byte("x"), 1, 1, now)
		p.Add("default", "a.git", []byte("x"), 1, 2, now)
		require.Equal(t, 1, p.Len())

		_, seq, ok := p.Take("default", "a.git", []byte("x"), now)
		require.True(t, ok)
		require.Equal(t, uint64(2), seq)
	})
}
