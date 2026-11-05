package syncer_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// createRef returns the update that creates the reference at target.
func createRef(name string, target corestore.Target) corestore.RefUpdate {
	return corestore.RefUpdate{Name: name, Expect: corestore.ExpectValue, New: target}
}

// TestSyncerImpliedAbsenceStillDetectsLocalWrite covers the name whose absence
// the catch-up does not verify, because the reference it creates implies it: a
// local value that appears there between the check and the apply fails the
// transaction, the check runs again and the value is removed, instead of being
// left next to the new reference or being taken for a stuck repository.
func TestSyncerImpliedAbsenceStillDetectsLocalWrite(t *testing.T) {
	const (
		a  = "refs/heads/a"
		ab = "refs/heads/a/b"
	)

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		writer := newEnv(t, envOptions{dialect: dialect})
		node := writer.secondNode(t, dialect)
		src := writer.newSource(t)
		c1 := writer.writeCommit(t, src)
		repo := writer.register(t, writer.key(localRelPath), src, c1)
		require.NoError(t, node.sy.EnsureFresh(node.ctx, node.pbRepo(localRelPath)))

		writer.changeset(t, repo.ID, src, []corestore.RefUpdate{createRef(a, corestore.Target(c1))})
		writer.changeset(t, repo.ID, src, []corestore.RefUpdate{
			{Name: a, Expect: corestore.ExpectValue, Old: corestore.Target(c1)},
			createRef(ab, corestore.Target(c1)),
		})

		wrote := false
		node.sy.SetTestHook(once("checked", func() {
			wrote = true
			require.NoError(t, gitops.ApplyRefs(node.ctx, node.localRepo(), map[string]corestore.Target{a: corestore.Target(c1)}))
		}))
		require.NoError(t, node.sy.EnsureFresh(node.ctx, node.pbRepo(localRelPath)))
		require.True(t, wrote)
		node.verifySynced(t, repo)
	})
}

// TestSyncerCatchesUpOverRenamedDirectories catches a node up, in one range,
// over a history in which conflicting names existed at different times. Each
// changeset is legal, and the history ends in a state git can hold. The node
// misses the intermediate states, so a name can be absent both before and after
// the range: git is asked to verify its absence in the same catch-up that creates
// a name it conflicts with, which it refuses within one transaction.
func TestSyncerCatchesUpOverRenamedDirectories(t *testing.T) {
	const (
		a  = "refs/heads/a"
		ab = "refs/heads/a/b"
		ac = "refs/heads/a/c"
		// abc conflicts with both a and ab.
		abc = "refs/heads/a/b/c"
	)
	type step struct{ deletes, creates []string }

	for _, tc := range []struct {
		desc string
		// seen are the changesets the node applied before it falls behind.
		seen []step
		// behind are the changesets it catches up over, in one range.
		behind []step
	}{
		{
			desc:   "a created, then replaced by a/b in one changeset",
			behind: []step{{creates: []string{a}}, {deletes: []string{a}, creates: []string{ab}}},
		},
		{
			desc:   "a created, deleted, then a/b created",
			behind: []step{{creates: []string{a}}, {deletes: []string{a}}, {creates: []string{ab}}},
		},
		{
			desc:   "a/b created, deleted, then a created",
			behind: []step{{creates: []string{ab}}, {deletes: []string{ab}}, {creates: []string{a}}},
		},
		{
			desc:   "a/b created, then replaced by a in one changeset",
			behind: []step{{creates: []string{ab}}, {deletes: []string{ab}, creates: []string{a}}},
		},
		{
			desc: "a and a/b each existed for a while, and nothing is left",
			behind: []step{
				{creates: []string{a}}, {deletes: []string{a}, creates: []string{ab}}, {deletes: []string{ab}},
			},
		},
		{
			desc: "a, a/b and a/b/c each existed for a while, and only a/b/c is left",
			behind: []step{
				{creates: []string{a}},
				{deletes: []string{a}, creates: []string{ab}},
				{deletes: []string{ab}, creates: []string{abc}},
			},
		},
		{
			desc: "a, a/b and a/b/c each existed for a while, and only a is left",
			behind: []step{
				{creates: []string{abc}},
				{deletes: []string{abc}, creates: []string{ab}},
				{deletes: []string{ab}, creates: []string{a}},
			},
		},
		{
			desc:   "the node has a/b, a existed for a while, and nothing is left",
			seen:   []step{{creates: []string{ab}}},
			behind: []step{{deletes: []string{ab}, creates: []string{a}}, {deletes: []string{a}}},
		},
		{
			desc: "the node has a, a/b existed for a while, and a/c is left",
			seen: []step{{creates: []string{a}}},
			behind: []step{
				{deletes: []string{a}, creates: []string{ab}},
				{deletes: []string{ab}, creates: []string{ac}},
			},
		},
		{
			desc:   "the node has a/b, a/b/c existed for a while, and a is left",
			seen:   []step{{creates: []string{ab}}},
			behind: []step{{deletes: []string{ab}, creates: []string{abc}}, {deletes: []string{abc}, creates: []string{a}}},
		},
		{
			desc: "the node has a/b, a existed for a while, and a/b is back at the same value",
			seen: []step{{creates: []string{ab}}},
			behind: []step{
				{deletes: []string{ab}, creates: []string{a}},
				{deletes: []string{a}, creates: []string{ab}},
			},
		},
		{
			// a is implied by a/c; a/b does not conflict with a/c, so it stays verified.
			desc: "a and a/b each existed for a while, and the sibling a/c is left",
			behind: []step{
				{creates: []string{a}},
				{deletes: []string{a}, creates: []string{ab, ac}},
				{deletes: []string{ab}},
			},
		},
		{
			desc: "a/b and a/c each existed for a while, and nothing is left",
			behind: []step{
				{creates: []string{ab, ac}},
				{deletes: []string{ab, ac}},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
				writer := newEnv(t, envOptions{dialect: dialect})
				node := writer.secondNode(t, dialect)
				src := writer.newSource(t)
				c1 := writer.writeCommit(t, src)
				repo := writer.register(t, writer.key(localRelPath), src, c1)

				apply := func(s step) {
					var updates []corestore.RefUpdate
					for _, name := range s.deletes {
						updates = append(updates, corestore.RefUpdate{Name: name, Expect: corestore.ExpectValue, Old: corestore.Target(c1)})
					}
					for _, name := range s.creates {
						updates = append(updates, createRef(name, corestore.Target(c1)))
					}
					writer.changeset(t, repo.ID, src, updates)
				}

				for _, s := range tc.seen {
					apply(s)
				}
				require.NoError(t, node.sy.EnsureFresh(node.ctx, node.pbRepo(localRelPath)))
				node.verifySynced(t, repo)

				for _, s := range tc.behind {
					apply(s)
				}
				require.NoError(t, node.sy.EnsureFresh(node.ctx, node.pbRepo(localRelPath)))
				node.verifySynced(t, repo)
			})
		})
	}
}
