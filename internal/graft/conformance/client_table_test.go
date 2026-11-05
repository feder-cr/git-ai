package conformance

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/testhelper"
)

// A session runs one row of the client table against one remote and records what a stock client and
// the server can observe: every step's exit code, what the client is told per reference, the
// references on both sides and fsck on both sides. Commits have fixed identities and dates, so that
// the object IDs are the same on every backend and the references compare by value.
type session struct {
	t        *testing.T
	ctx      context.Context
	remote   remote
	protocol int
	root     string
	tick     int
	steps    int
	observed observations
	tracked  []string
	problems []string
	log      strings.Builder
}

// observations maps what was observed to its value, in a form that compares across backends.
type observations map[string]string

// absent is the value of an observation that a backend does not have.
const absent = "<absent>"

func (s *session) env(extra ...string) []string {
	env := stockEnv(s.root)
	date := fmt.Sprintf("%d +0000", 1767225600+s.tick)
	env = append(env,
		"GIT_AUTHOR_NAME=conformance", "GIT_AUTHOR_EMAIL=conformance@example.com", "GIT_AUTHOR_DATE="+date,
		"GIT_COMMITTER_NAME=conformance", "GIT_COMMITTER_EMAIL=conformance@example.com", "GIT_COMMITTER_DATE="+date,
	)
	return append(env, extra...)
}

// local runs a git command of the row's setup, which touches no server and has to succeed.
func (s *session) local(dir string, args ...string) string {
	s.t.Helper()
	return mustStock(s.ctx, s.t, dir, s.env(), nil, args...)
}

// step runs a client operation against the remote and records its exit code, and its standard
// output when compare is set. Its output goes to the log that a failing row prints.
func (s *session) step(direction, dir string, compare bool, args ...string) int {
	s.t.Helper()

	s.steps++
	key := fmt.Sprintf("step %02d git %s", s.steps, strings.ReplaceAll(strings.Join(args, " "), s.remote.url, "<url>"))
	code, stdout, stderr := runStock(s.ctx, s.t, dir, s.env(s.remote.env(direction, s.protocol)...), nil,
		append([]string{"-c", fmt.Sprintf("protocol.version=%d", s.protocol)}, args...)...)
	// git prints an SSH remote without its user.
	stdout = strings.ReplaceAll(stdout, strings.TrimPrefix(s.remote.url, "git@"), "<url>")

	s.observed[key+": exit"] = fmt.Sprint(code)
	if compare {
		s.observeLines(key, stdout)
	}
	fmt.Fprintf(&s.log, "$ %s\nexit %d\n%s%s\n", key, code, stdout, stderr)
	return code
}

// observeLines records the output of a step line by line, so that a difference names the reference
// it is about: `<flag> <from>:<to> <summary>` of a push and `<value> <name>` of ls-remote.
func (s *session) observeLines(key, stdout string) {
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		fields := strings.Split(line, "\t")
		switch len(fields) {
		case 3:
			_, to, _ := strings.Cut(fields[1], ":")
			s.observed[key+": "+to] = fields[0] + " " + fields[2]
		case 2:
			s.observed[key+": "+fields[1]] = fields[0]
		default:
			s.observed[key+": says "+line] = "yes"
		}
	}
}

// work creates a client repository that has the remote as origin.
func (s *session) work(name string) string {
	s.t.Helper()

	dir := filepath.Join(s.root, name)
	require.NoError(s.t, os.MkdirAll(dir, 0o755))
	s.local(dir, "init", "--quiet", "--initial-branch=main")
	s.local(dir, "remote", "add", "origin", s.remote.url)
	return dir
}

// commit commits content as path on the current branch of dir and returns the commit.
func (s *session) commit(dir, path, content string) string {
	s.t.Helper()

	s.tick++
	full := filepath.Join(dir, path)
	require.NoError(s.t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(s.t, os.WriteFile(full, []byte(content), 0o644))
	s.local(dir, "add", "--", path)
	s.local(dir, "commit", "--quiet", "--message", "write "+path)
	return s.local(dir, "rev-parse", "HEAD")
}

func (s *session) push(dir string, args ...string) int {
	s.t.Helper()
	return s.step(receivePack, dir, true, append([]string{"push", "--porcelain", "origin"}, args...)...)
}

func (s *session) fetch(dir string, args ...string) int {
	s.t.Helper()
	return s.step(uploadPack, dir, false, append([]string{"fetch", "origin"}, args...)...)
}

// clone clones the remote into a client that the final state of the row includes.
func (s *session) clone(name string, args ...string) string {
	s.t.Helper()

	dir := filepath.Join(s.root, name)
	s.step(uploadPack, s.root, false, append(append([]string{"clone", "--quiet"}, args...), s.remote.url, name)...)
	s.track(dir)
	return dir
}

// track includes the client repository at dir in the final state of the row.
func (s *session) track(dir string) {
	s.tracked = append(s.tracked, dir)
}

// seeded returns a client that pushed the seed: main with three commits, side from the second, a
// lightweight tag v1 and an annotated tag v2. The client's main is at the third commit.
func (s *session) seeded() string {
	s.t.Helper()

	dir := s.work("seed")
	s.commit(dir, "a.txt", "one\n")
	s.local(dir, "tag", "v1")
	s.commit(dir, "b.txt", "two\n")
	s.local(dir, "tag", "--annotate", "--message", "v2", "v2")
	s.local(dir, "branch", "side")
	s.commit(dir, "c.txt", "three\n")
	s.push(dir, "main", "side", "v1", "v2")
	return dir
}

// recordRefs records the references and HEAD of the repository at dir under prefix.
func (s *session) recordRefs(prefix, dir string) {
	s.t.Helper()

	output := s.local(dir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")
	for _, line := range strings.Split(output, "\n") {
		if name, value, ok := strings.Cut(line, " "); ok {
			s.observed[prefix+" ref "+name] = strings.TrimSpace(value)
		}
	}
	_, head, _ := runStock(s.ctx, s.t, dir, s.env(), nil, "symbolic-ref", "HEAD")
	s.observed[prefix+" HEAD"] = strings.TrimSpace(head)
}

// finish records the final state: what a client sees of the server, the references on both sides,
// and fsck on both sides.
func (s *session) finish() {
	s.t.Helper()

	s.step(uploadPack, s.root, true, "ls-remote", "--symref", s.remote.url)

	s.recordRefs("server", s.remote.path)
	code, _, stderr := runStock(s.ctx, s.t, s.remote.path, s.env(), nil, "fsck", "--no-dangling")
	s.observed["server fsck: exit"] = fmt.Sprint(code)
	fmt.Fprintf(&s.log, "server fsck: exit %d\n%s\n", code, stderr)

	for i, dir := range s.tracked {
		prefix := fmt.Sprintf("client %d", i)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
				s.observed[prefix] = "not created"
				continue
			}
		}
		s.recordRefs(prefix, dir)

		// fsck does not fetch what a partial clone lacks, so it needs no credentials; rev-list counts
		// what the filter left out without fetching it either.
		code, _, stderr := runStock(s.ctx, s.t, dir, s.env(), nil, "fsck", "--no-dangling")
		s.observed[prefix+" fsck: exit"] = fmt.Sprint(code)
		fmt.Fprintf(&s.log, "%s fsck: exit %d\n%s\n", prefix, code, stderr)

		shallow, err := os.ReadFile(filepath.Join(s.gitDir(dir), "shallow"))
		if err != nil {
			require.ErrorIs(s.t, err, os.ErrNotExist)
		}
		s.observed[prefix+" shallow"] = strings.TrimSpace(string(shallow))

		objects := s.local(dir, "rev-list", "--objects", "--all", "--missing=print")
		present, missing := 0, 0
		for _, line := range strings.Split(objects, "\n") {
			switch {
			case line == "":
			case strings.HasPrefix(line, "?"):
				missing++
			default:
				present++
			}
		}
		s.observed[prefix+" objects"] = fmt.Sprintf("%d present, %d missing", present, missing)
	}

	s.problems = s.remote.invariants(s.t)
}

func (s *session) gitDir(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return filepath.Join(dir, ".git")
	}
	return dir
}

// A clientRow is one operation, or a short story of them, that a stock client runs.
type clientRow struct {
	name string
	// protocols is the wire protocol versions the row runs with. Pushes speak v0 whatever the client
	// asks for, so most of them run once.
	protocols []int
	run       func(s *session)
}

var (
	v0Only = []int{0}
	v0v2   = []int{0, 2}
)

// clientRows is the client table.
var clientRows = []clientRow{
	// An empty repository.
	{"empty clone", v0v2, func(s *session) { s.clone("c") }},
	{"empty shallow clone", v0v2, func(s *session) { s.clone("c", "--depth=1") }},
	{"empty fetch", v0v2, func(s *session) {
		c := s.work("c")
		s.track(c)
		s.fetch(c)
	}},
	{"empty first push", v0Only, func(s *session) {
		c := s.work("c")
		s.commit(c, "a.txt", "one\n")
		s.push(c, "main")
	}},
	{"empty first push of another branch", v0Only, func(s *session) {
		c := s.work("c")
		s.commit(c, "a.txt", "one\n")
		s.push(c, "main:refs/heads/other")
		s.clone("d")
	}},

	// Clones.
	{"clone full", v0v2, func(s *session) { s.seeded(); s.clone("c") }},
	{"clone shallow", v0v2, func(s *session) { s.seeded(); s.clone("c", "--depth=1") }},
	{"clone shallow of every branch", v0v2, func(s *session) { s.seeded(); s.clone("c", "--depth=1", "--no-single-branch") }},
	{"clone partial without blobs", v0v2, func(s *session) { s.seeded(); s.clone("c", "--filter=blob:none", "--no-checkout") }},
	{"clone partial without trees", v0v2, func(s *session) { s.seeded(); s.clone("c", "--filter=tree:0", "--no-checkout") }},
	{"clone partial, then checkout fetches the blobs", v0v2, func(s *session) { s.seeded(); s.clone("c", "--filter=blob:none") }},
	{"clone single branch", v0v2, func(s *session) { s.seeded(); s.clone("c", "--single-branch", "--branch=side") }},
	{"clone at a tag", v0v2, func(s *session) { s.seeded(); s.clone("c", "--branch=v2") }},
	{"clone of a missing branch", v0v2, func(s *session) { s.seeded(); s.clone("c", "--branch=missing") }},
	{"clone mirror", v0v2, func(s *session) { s.seeded(); s.clone("c", "--mirror") }},

	// Fetches.
	{"fetch new commits", v0v2, func(s *session) {
		seed := s.seeded()
		c := s.clone("c")
		s.commit(seed, "d.txt", "four\n")
		s.push(seed, "main")
		s.fetch(c)
	}},
	{"fetch prunes a deleted branch", v0v2, func(s *session) {
		seed := s.seeded()
		c := s.clone("c")
		s.push(seed, ":side")
		s.fetch(c, "--prune")
	}},
	{"fetch one refspec", v0v2, func(s *session) {
		s.seeded()
		c := s.work("c")
		s.track(c)
		s.fetch(c, "+refs/heads/side:refs/remotes/origin/side")
	}},
	{"fetch a tag", v0v2, func(s *session) {
		s.seeded()
		c := s.work("c")
		s.track(c)
		s.fetch(c, "refs/tags/v2:refs/tags/v2", "--no-tags")
	}},
	{"fetch a missing reference", v0v2, func(s *session) {
		s.seeded()
		c := s.work("c")
		s.track(c)
		s.fetch(c, "refs/heads/missing")
	}},
	{"fetch deepens a shallow clone", v0v2, func(s *session) {
		s.seeded()
		c := s.clone("c", "--depth=1")
		s.fetch(c, "--deepen=1")
	}},
	{"fetch unshallows", v0v2, func(s *session) {
		s.seeded()
		c := s.clone("c", "--depth=1")
		s.fetch(c, "--unshallow")
	}},
	{"fetch the tip by object ID", v0v2, func(s *session) {
		seed := s.seeded()
		c := s.work("c")
		s.track(c)
		s.fetch(c, s.local(seed, "rev-parse", "main")+":refs/heads/fetched")
	}},
	{"fetch a reachable non-tip by object ID", v0v2, func(s *session) {
		seed := s.seeded()
		c := s.work("c")
		s.track(c)
		s.fetch(c, s.local(seed, "rev-parse", "main~2")+":refs/heads/fetched")
	}},

	// Pushes of branches.
	{"push new branch", v0Only, func(s *session) { s.push(s.seeded(), "main:refs/heads/feature/new") }},
	{"push with protocol 2 configured", []int{2}, func(s *session) { s.push(s.seeded(), "main:refs/heads/feature/new") }},
	{"push nothing new", v0Only, func(s *session) { s.push(s.seeded(), "main") }},
	{"push fast-forward", v0Only, func(s *session) {
		seed := s.seeded()
		s.commit(seed, "d.txt", "four\n")
		s.push(seed, "main")
	}},
	{"push non-fast-forward is rejected", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "reset", "--quiet", "--hard", "HEAD~1")
		s.commit(seed, "other.txt", "other\n")
		s.push(seed, "main")
	}},
	{"push force", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "reset", "--quiet", "--hard", "HEAD~1")
		s.commit(seed, "other.txt", "other\n")
		s.push(seed, "--force", "main")
	}},
	{"push force-with-lease", v0Only, func(s *session) {
		seed := s.seeded()
		old := s.local(seed, "rev-parse", "main")
		s.local(seed, "reset", "--quiet", "--hard", "HEAD~1")
		s.commit(seed, "other.txt", "other\n")
		s.push(seed, "--force-with-lease=main:"+old, "main")
	}},
	{"push force-with-lease that is stale", v0Only, func(s *session) {
		seed := s.seeded()
		stale := s.local(seed, "rev-parse", "main~1")
		s.local(seed, "reset", "--quiet", "--hard", "HEAD~1")
		s.commit(seed, "other.txt", "other\n")
		s.push(seed, "--force-with-lease=main:"+stale, "main")
	}},
	{"push delete", v0Only, func(s *session) { s.push(s.seeded(), ":side") }},
	{"push delete of a missing branch", v0Only, func(s *session) { s.push(s.seeded(), ":refs/heads/missing") }},
	{"push delete of the default branch", v0Only, func(s *session) { s.push(s.seeded(), ":main") }},
	{"push everything", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "branch", "more")
		s.step(receivePack, seed, true, "push", "--porcelain", "--all", "origin")
	}},
	{"push mirror", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "branch", "--delete", "--force", "side")
		s.local(seed, "branch", "more")
		s.step(receivePack, seed, true, "push", "--porcelain", "--mirror", "origin")
	}},

	// Pushes of tags.
	{"push lightweight tag", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "tag", "light")
		s.push(seed, "light")
	}},
	{"push annotated tag", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "tag", "--annotate", "--message", "annotated", "annotated")
		s.push(seed, "annotated")
	}},
	{"push moves a tag without force", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "tag", "--force", "v1", "main")
		s.push(seed, "refs/tags/v1")
	}},
	{"push moves a tag with force", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "tag", "--force", "v1", "main")
		s.push(seed, "--force", "refs/tags/v1")
	}},
	{"push deletes a tag", v0Only, func(s *session) { s.push(s.seeded(), ":refs/tags/v2") }},
	{"push tags", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "tag", "more")
		s.step(receivePack, seed, true, "push", "--porcelain", "--tags", "origin")
	}},

	// Pushes of several references.
	{"push two references", v0Only, func(s *session) {
		seed := s.seeded()
		s.push(seed, "main:refs/heads/one", "main:refs/heads/two")
	}},
	{"push two references atomically", v0Only, func(s *session) {
		seed := s.seeded()
		s.push(seed, "--atomic", "main:refs/heads/one", "main:refs/heads/two")
	}},
	{"push non-atomic, one rejected by the client", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "tag", "--force", "v1", "main")
		s.push(seed, "main:refs/heads/one", "refs/tags/v1")
	}},
	{"push atomic, one rejected by the client", v0Only, func(s *session) {
		seed := s.seeded()
		s.local(seed, "tag", "--force", "v1", "main")
		s.push(seed, "--atomic", "main:refs/heads/one", "refs/tags/v1")
	}},
	{"push non-atomic, one refused by the server", v0Only, func(s *session) {
		seed := s.seeded()
		s.push(seed, "main:refs/heads/one", "main:refs/heads/side/below")
	}},
	{"push atomic, one refused by the server", v0Only, func(s *session) {
		seed := s.seeded()
		s.push(seed, "--atomic", "main:refs/heads/one", "main:refs/heads/side/below")
	}},

	// Directory/file conflicts of references.
	{"conflict: a branch below a branch", v0Only, func(s *session) {
		seed := s.seeded()
		s.push(seed, "main:refs/heads/a")
		s.push(seed, "main:refs/heads/a/b")
	}},
	{"conflict: a branch above a branch", v0Only, func(s *session) {
		seed := s.seeded()
		s.push(seed, "main:refs/heads/a/b")
		s.push(seed, "main:refs/heads/a")
	}},
	{"conflict inside one push", v0Only, func(s *session) {
		s.push(s.seeded(), "main:refs/heads/a", "main:refs/heads/a/b")
	}},
	{"conflict inside one atomic push", v0Only, func(s *session) {
		s.push(s.seeded(), "--atomic", "main:refs/heads/a", "main:refs/heads/a/b")
	}},
	{"conflict resolved by a delete in the same push", v0Only, func(s *session) {
		seed := s.seeded()
		s.push(seed, "main:refs/heads/a")
		s.push(seed, ":refs/heads/a", "main:refs/heads/a/b")
	}},
	{"conflict resolved by a delete in the same atomic push", v0Only, func(s *session) {
		seed := s.seeded()
		s.push(seed, "main:refs/heads/a")
		s.push(seed, "--atomic", ":refs/heads/a", "main:refs/heads/a/b")
	}},
	{"conflict resolved by a delete first", v0Only, func(s *session) {
		seed := s.seeded()
		s.push(seed, "main:refs/heads/a")
		s.push(seed, ":refs/heads/a")
		s.push(seed, "main:refs/heads/a/b")
	}},

	// References outside of branches and tags.
	{"push to refs/notes/", v0Only, func(s *session) { s.push(s.seeded(), "main:refs/notes/commits") }},
	{"push to a custom namespace", v0Only, func(s *session) { s.push(s.seeded(), "main:refs/custom/x") }},
	{"push to refs/graft/", v0Only, func(s *session) { s.push(s.seeded(), "main:refs/graft/x") }},
	{"push to refs/keep-around/", v0Only, func(s *session) { s.push(s.seeded(), "main:refs/keep-around/x") }},
	{"push to refs/merge-requests/", v0Only, func(s *session) { s.push(s.seeded(), "main:refs/merge-requests/1/head") }},
	{"push to refs/pipelines/", v0Only, func(s *session) { s.push(s.seeded(), "main:refs/pipelines/1") }},
	{"push to refs/environments/", v0Only, func(s *session) { s.push(s.seeded(), "main:refs/environments/1") }},
	{"push to refs/remotes/", v0Only, func(s *session) { s.push(s.seeded(), "main:refs/remotes/origin/x") }},

	// Objects.
	{"push of a malformed commit", v0Only, func(s *session) {
		seed := s.seeded()
		tree := s.local(seed, "rev-parse", "main^{tree}")
		malformed := "tree " + tree + "\nauthor no email\ncommitter no email\n\nmalformed\n"
		commit := mustStock(s.ctx, s.t, seed, s.env(), []byte(malformed), "hash-object", "-t", "commit", "-w", "--stdin", "--literally")
		s.local(seed, "update-ref", "refs/heads/malformed", commit)
		s.push(seed, "malformed")
	}},
	{"large-ish pack", v0v2, func(s *session) {
		seed := s.work("seed")
		// 4 MiB that do not compress, and enough small files to stay above receive.unpackLimit, so
		// that the server keeps the pack.
		random := rand.New(rand.NewSource(1))
		large := make([]byte, 4<<20)
		_, err := random.Read(large)
		require.NoError(s.t, err)
		require.NoError(s.t, os.WriteFile(filepath.Join(seed, "large.bin"), large, 0o644))
		for i := 0; i < 300; i++ {
			name := filepath.Join(seed, "small", fmt.Sprintf("%03d.txt", i))
			require.NoError(s.t, os.MkdirAll(filepath.Dir(name), 0o755))
			require.NoError(s.t, os.WriteFile(name, []byte(fmt.Sprintf("small %d\n", i)), 0o644))
		}
		s.local(seed, "add", ".")
		s.tick++
		s.local(seed, "commit", "--quiet", "--message", "large")
		s.push(seed, "main")
		s.clone("c")
	}},
}

// runClientRow runs row against r and returns what was observed, what the mode's invariants say about
// the outcome, and the log of the row.
func runClientRow(t *testing.T, r remote, row clientRow, protocol int) (observations, []string, string) {
	t.Helper()

	s := &session{
		t: t, ctx: testhelper.Context(t), remote: r, protocol: protocol,
		root: testhelper.TempDir(t), observed: observations{},
	}
	row.run(s)
	s.finish()
	return s.observed, normalizedProblems(s.problems), s.log.String()
}

// objectID is an object ID in a difference, which an allowlist entry does not spell out.
var objectID = regexp.MustCompile(`\b[0-9a-f]{40}([0-9a-f]{24})?\b`)

// differences returns what gitai did differently from the reference, sorted.
func differences(reference, gitai observations) []string {
	keys := map[string]struct{}{}
	for key := range reference {
		keys[key] = struct{}{}
	}
	for key := range gitai {
		keys[key] = struct{}{}
	}

	var diffs []string
	for key := range keys {
		want, ok := reference[key]
		if !ok {
			want = absent
		}
		got, ok := gitai[key]
		if !ok {
			got = absent
		}
		if want != got {
			diffs = append(diffs, objectID.ReplaceAllString(fmt.Sprintf("%s: stock %q, gitai %q", key, want, got), "<oid>"))
		}
	}
	sort.Strings(diffs)
	return diffs
}

type rowKey struct {
	row      string
	protocol int
}

// TestConformanceClientTable runs the client table against the reference and against every mode of
// gitai. A difference that allowedDifferences does not list fails, and so does an entry of the list
// that no longer matches, so that the list stays what really differs.
func TestConformanceClientTable(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	reference := map[rowKey]observations{}
	referenceLogs := map[rowKey]string{}

	stock := newReferenceBackend(t)
	t.Run("reference", func(t *testing.T) {
		for _, row := range clientRows {
			for _, protocol := range row.protocols {
				t.Run(fmt.Sprintf("%s/v%d", row.name, protocol), func(t *testing.T) {
					t.Parallel()
					observed, _, log := runClientRow(t, stock.newRemote(t), row, protocol)
					mu.Lock()
					defer mu.Unlock()
					reference[rowKey{row.name, protocol}] = observed
					referenceLogs[rowKey{row.name, protocol}] = log
				})
			}
		}
	})
	require.False(t, t.Failed(), "the reference has to run for the comparison to mean anything")

	for _, mode := range gitaiModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			gitai := newGitaiBackend(t, mode, agentGraft())
			for _, row := range clientRows {
				for _, protocol := range row.protocols {
					t.Run(fmt.Sprintf("%s/v%d", row.name, protocol), func(t *testing.T) {
						t.Parallel()

						observed, problems, log := runClientRow(t, gitai.newRemote(t), row, protocol)
						key := rowKey{row.name, protocol}
						got := differences(reference[key], observed)
						want := allowedFor(row.name, mode, protocol)
						if !equalStrings(want, got) {
							t.Errorf("unlisted difference from stock git\nmode %s, row %q, protocol %d\ndifferences:\n  %s\nallowed:\n  %s\n--- gitai\n%s\n--- stock\n%s",
								mode.name, row.name, protocol, strings.Join(got, "\n  "), strings.Join(want, "\n  "), log, referenceLogs[key])
						}
						if known := knownProblemsFor(row.name, mode); !equalStrings(known, problems) {
							t.Errorf("unlisted problem of the mode\nmode %s, row %q, protocol %d\nproblems:\n  %s\nknown:\n  %s\n--- gitai\n%s",
								mode.name, row.name, protocol, strings.Join(problems, "\n  "), strings.Join(known, "\n  "), log)
						}
					})
				}
			}
		})
	}
}

// normalizedProblems returns problems without object IDs, sorted.
func normalizedProblems(problems []string) []string {
	normalized := []string{}
	for _, problem := range problems {
		normalized = append(normalized, objectID.ReplaceAllString(problem, "<oid>"))
	}
	sort.Strings(normalized)
	return normalized
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
