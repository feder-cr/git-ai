package conformance

import "sort"

// An allowedDifference is a difference from stock git that is meant to be there. The diffs are the
// exact text of differences(), so a new difference, or one that went away, fails the row.
type allowedDifference struct {
	row string
	// protocols selects the wire protocol versions it applies to; nil means every version.
	protocols []int
	// reason says why gitai differs on purpose.
	reason string
	diffs  []string
}

const (
	reasonInternalRefs = "git.InternalRefPrefixes, inherited from Gitaly: receive.hideRefs makes the server's internal namespaces read-only for every client"
	reasonReservedRefs = "refs/graft/ is the server's in every mode (doc/graft/AGENT-SECURITY.md): receive.hideRefs refuses it to agents"
)

// hiddenRefPush is the difference of a push of main to reference, which the server hides.
func hiddenRefPush(row, reference, reason string) allowedDifference {
	step := "step 02 git push --porcelain origin main:" + reference + ": "
	return allowedDifference{
		row:    row,
		reason: reason,
		diffs: []string{
			"server ref " + reference + `: stock "<oid>", gitai "<absent>"`,
			step + `exit: stock "0", gitai "1"`,
			step + reference + `: stock "* [new reference]", gitai "! [remote rejected] (deny updating a hidden ref)"`,
			"step 03 git ls-remote --symref <url>: " + reference + `: stock "<oid>", gitai "<absent>"`,
		},
	}
}

// allowedDifferences are all the differences from stock git that are intentional.
var allowedDifferences = []allowedDifference{
	hiddenRefPush("push to refs/graft/", "refs/graft/x", reasonReservedRefs),
	hiddenRefPush("push to refs/keep-around/", "refs/keep-around/x", reasonInternalRefs),
	hiddenRefPush("push to refs/merge-requests/", "refs/merge-requests/1/head", reasonInternalRefs),
	hiddenRefPush("push to refs/pipelines/", "refs/pipelines/1", reasonInternalRefs),
	hiddenRefPush("push to refs/environments/", "refs/environments/1", reasonInternalRefs),
	hiddenRefPush("push to refs/remotes/", "refs/remotes/origin/x", reasonInternalRefs),
	{
		row:    "push mirror",
		reason: reasonInternalRefs + ": --mirror also pushes the client's refs/remotes/, so the push fails for those references and goes through for the others",
		diffs: []string{
			`server ref refs/remotes/origin/main: stock "<oid>", gitai "<absent>"`,
			`server ref refs/remotes/origin/side: stock "<oid>", gitai "<absent>"`,
			`step 02 git push --porcelain --mirror origin: exit: stock "0", gitai "1"`,
			`step 02 git push --porcelain --mirror origin: refs/remotes/origin/main: stock "* [new reference]", gitai "! [remote rejected] (deny updating a hidden ref)"`,
			`step 02 git push --porcelain --mirror origin: refs/remotes/origin/side: stock "* [new reference]", gitai "! [remote rejected] (deny updating a hidden ref)"`,
			`step 03 git ls-remote --symref <url>: refs/remotes/origin/main: stock "<oid>", gitai "<absent>"`,
			`step 03 git ls-remote --symref <url>: refs/remotes/origin/side: stock "<oid>", gitai "<absent>"`,
		},
	},
	{
		row:    "push of a malformed commit",
		reason: "inherited from Gitaly: receive-pack runs fsck on what it receives; stock git accepts the commit and its own fsck then fails on the server",
		diffs: []string{
			`server fsck: exit: stock "1", gitai "0"`,
			`server ref refs/heads/malformed: stock "<oid>", gitai "<absent>"`,
			`step 02 git push --porcelain origin malformed: exit: stock "0", gitai "1"`,
			`step 02 git push --porcelain origin malformed: refs/heads/malformed: stock "* [new branch]", gitai "! [remote rejected] (unpacker error)"`,
			`step 02 git push --porcelain origin malformed: says Done: stock "yes", gitai "<absent>"`,
			`step 03 git ls-remote --symref <url>: refs/heads/malformed: stock "<oid>", gitai "<absent>"`,
		},
	},
	{
		row:       "clone partial, then checkout fetches the blobs",
		protocols: []int{0},
		reason:    "inherited from Gitaly: upload-pack allows any object in a want, which the lazy fetch of a partial clone needs on protocol v0; stock git refuses it there unless configured to allow it",
		diffs: []string{
			`client 0 objects: stock "7 present, 3 missing", gitai "10 present, 0 missing"`,
			`step 02 git clone --quiet --filter=blob:none <url> c: exit: stock "128", gitai "0"`,
		},
	},
}

// allowedFor returns the differences the row may have with the protocol, sorted as differences()
// sorts them.
func allowedFor(row string, protocol int) []string {
	allowed := []string{}
	for _, entry := range allowedDifferences {
		if entry.row != row {
			continue
		}
		if entry.protocols != nil && !containsInt(entry.protocols, protocol) {
			continue
		}
		allowed = append(allowed, entry.diffs...)
	}
	sort.Strings(allowed)
	return allowed
}

func containsInt(values []int, value int) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// A knownProblem is a failure of a mode's invariant that a suspected bug of the product causes. It is
// listed so that the harness stays usable while the bug is open; the entry fails the row as soon as
// the problem goes away, so that it does not outlive the bug.
type knownProblem struct {
	row   string
	modes func(gitaiMode) bool
	bug   string
	// problems are the exact texts of the invariants, sorted.
	problems []string
}

func withStorageCore(mode gitaiMode) bool { return mode.coreMode != "" }

// bugRejectedUpdateCommitted: git's reference-transaction hook is fed the updates that a non-atomic
// receive-pack (REF_TRANSACTION_ALLOW_FAILURE) already rejected, so the voter commits a rejected update
// to the database. The client is told that the reference was refused; the database holds it. With a
// directory/file conflict the database then holds refs/heads/a and refs/heads/a/b, which no git
// repository can hold: the local copy diverges for good, and a node without a copy fails to hydrate
// the repository with "conflicting reference updates in the same transaction".
const bugRejectedUpdateCommitted = "a rejected update of a non-atomic push is committed to the database"

func divergedAt(reference string) []string {
	return []string{`the database diverges at ` + reference + `: local "", database "<oid>"`}
}

var knownProblems = []knownProblem{
	{"conflict: a branch below a branch", withStorageCore, bugRejectedUpdateCommitted, divergedAt("refs/heads/a/b")},
	{"conflict: a branch above a branch", withStorageCore, bugRejectedUpdateCommitted, divergedAt("refs/heads/a")},
	{"conflict inside one push", withStorageCore, bugRejectedUpdateCommitted, divergedAt("refs/heads/a")},
	{"push non-atomic, one refused by the server", withStorageCore, bugRejectedUpdateCommitted, divergedAt("refs/heads/side/below")},
}

// knownProblemsFor returns the problems the row may have in mode.
func knownProblemsFor(row string, mode gitaiMode) []string {
	known := []string{}
	for _, entry := range knownProblems {
		if entry.row == row && entry.modes(mode) {
			known = append(known, entry.problems...)
		}
	}
	return known
}
