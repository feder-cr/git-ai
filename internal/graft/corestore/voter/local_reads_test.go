package voter_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/voter"
)

// TestPreparedReadsLocalRefsOfZeroOldValuesOnly checks which local references the Prepared phase
// reads under git's reference lock: only those of the updates with a zero old value, which the
// local reference tells apart ("must not exist" or forced), and none when every update names its
// old value, which the database checks alone. The refusal of a stale create of a receive-pack
// still reads the reference and still refuses, also next to updates that read nothing.
func TestPreparedReadsLocalRefsOfZeroOldValuesOnly(t *testing.T) {
	const (
		main  = "refs/heads/main"
		other = "refs/heads/other"
		added = "refs/heads/new"
	)

	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, mode := range []corestore.Mode{corestore.ModeAuthoritative, corestore.ModeShadow} {
			t.Run(string(mode), func(t *testing.T) {
				// setup returns a fixture whose database and local repository hold main and
				// other at b and HEAD at main, and the names of each local read.
				setup := func(t *testing.T, hooks gitcmd.Hook) (*fixture, *[][]string) {
					f := newFixture(t, fixtureOptions{mode: mode, dialect: dialect})
					for _, ref := range []string{main, other} {
						f.seed(t, ref, corestore.Target(f.b.String()))
						gittest.WriteRef(t, f.cfg, f.repoPath, git.ReferenceName(ref), f.b)
					}
					f.payload.RequestedHooks = hooks

					reads := &[][]string{}
					voter.ObserveLocalReads(f.voter, func(names []string) {
						*reads = append(*reads, slices.Clone(names))
					})
					return f, reads
				}

				for _, tc := range []struct {
					desc  string
					hooks gitcmd.Hook
					input func(f *fixture) []byte
					// reads are the names of each local read.
					reads [][]string
					// refused is the reference of the ReferenceAlreadyExistsError of an
					// authoritative write, "" when the write passes. A shadow write never
					// fails, and records nothing when authoritative mode refuses.
					refused string
					// db are the database values after the write when it passes, "" for an
					// absent reference.
					db map[string]corestore.Target
				}{
					{
						desc:  "an update that names its old value",
						hooks: gitcmd.ReceivePackHooks,
						input: func(f *fixture) []byte { return line(f.b.String(), f.c.String(), main) },
						db:    map[string]corestore.Target{main: "c", other: "b"},
					},
					{
						desc:  "a deletion that names its old value",
						hooks: gitcmd.ReceivePackHooks,
						input: func(f *fixture) []byte { return line(f.b.String(), f.zero, main) },
						db:    map[string]corestore.Target{main: "", other: "b"},
					},
					{
						desc:  "updates of UpdaterWithHooks-like writers that name their old values",
						hooks: gitcmd.ReferenceTransactionHook,
						input: func(f *fixture) []byte {
							return append(line(f.b.String(), f.c.String(), main), line(f.b.String(), f.a.String(), other)...)
						},
						db: map[string]corestore.Target{main: "c", other: "a"},
					},
					{
						desc:  "a symbolic update of HEAD that names its old value",
						hooks: gitcmd.ReferenceTransactionHook,
						input: func(f *fixture) []byte { return line("ref:"+main, "ref:"+other, "HEAD") },
						db:    map[string]corestore.Target{"HEAD": corestore.SymbolicTarget(other)},
					},
					{
						desc:  "a create of a receive-pack",
						hooks: gitcmd.ReceivePackHooks,
						input: func(f *fixture) []byte { return line(f.zero, f.a.String(), added) },
						reads: [][]string{{added}},
						db:    map[string]corestore.Target{added: "a", main: "b"},
					},
					{
						desc:    "a stale create of a receive-pack",
						hooks:   gitcmd.ReceivePackHooks,
						input:   func(f *fixture) []byte { return line(f.zero, f.a.String(), main) },
						reads:   [][]string{{main}},
						refused: main,
					},
					{
						desc:  "an update and a create read the create only",
						hooks: gitcmd.ReceivePackHooks,
						input: func(f *fixture) []byte {
							return append(line(f.b.String(), f.c.String(), main), line(f.zero, f.a.String(), added)...)
						},
						reads: [][]string{{added}},
						db:    map[string]corestore.Target{main: "c", added: "a", other: "b"},
					},
					{
						desc:  "an update next to a stale create is refused with it",
						hooks: gitcmd.ReceivePackHooks,
						input: func(f *fixture) []byte {
							return append(line(f.b.String(), f.c.String(), other), line(f.zero, f.a.String(), main)...)
						},
						reads:   [][]string{{main}},
						refused: main,
					},
					{
						desc:  "a forced update",
						hooks: gitcmd.ReferenceTransactionHook,
						input: func(f *fixture) []byte { return line(f.zero, f.a.String(), main) },
						reads: [][]string{{main}},
						db:    map[string]corestore.Target{main: "a"},
					},
					{
						desc:  "a forced deletion",
						hooks: gitcmd.ReferenceTransactionHook,
						input: func(f *fixture) []byte { return line(f.zero, f.zero, main) },
						reads: [][]string{{main}},
						db:    map[string]corestore.Target{main: "", other: "b"},
					},
					{
						desc:  "a forced symbolic update of HEAD",
						hooks: gitcmd.ReferenceTransactionHook,
						input: func(f *fixture) []byte { return line(f.zero, "ref:"+other, "HEAD") },
						reads: [][]string{{"HEAD"}},
						db:    map[string]corestore.Target{"HEAD": corestore.SymbolicTarget(other)},
					},
				} {
					t.Run(tc.desc, func(t *testing.T) {
						f, reads := setup(t, tc.hooks)
						head := f.headSeq(t)

						err := f.prepared(tc.input(f))
						require.Equal(t, tc.reads, nilIfEmpty(*reads))

						if tc.refused != "" {
							if mode == corestore.ModeAuthoritative {
								var exists updateref.ReferenceAlreadyExistsError
								require.True(t, errors.As(err, &exists), "error: %v", err)
								require.Equal(t, tc.refused, exists.ReferenceName)
							} else {
								require.NoError(t, err)
							}
							require.Equal(t, head, f.headSeq(t), "the refused transaction is not recorded")
							require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, main))
							require.Equal(t, corestore.Target(f.b.String()), f.dbRef(t, other))
							return
						}

						require.NoError(t, err)
						require.Equal(t, head+1, f.headSeq(t))
						commits := map[string]string{"a": f.a.String(), "b": f.b.String(), "c": f.c.String()}
						for ref, want := range tc.db {
							if oid, ok := commits[string(want)]; ok {
								want = corestore.Target(oid)
							}
							require.Equal(t, want, f.dbRef(t, ref), ref)
						}
					})
				}
			})
		}
	})
}

func nilIfEmpty(reads [][]string) [][]string {
	if len(reads) == 0 {
		return nil
	}
	return reads
}
