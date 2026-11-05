package conformance

import (
	"sort"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
)

// An allowedDifference is a difference from stock git that is meant to be there. The diffs are the
// exact text of differences(), so a new difference, or one that went away, fails the row.
type allowedDifference struct {
	row string
	// protocols selects the wire protocol versions it applies to; nil means every version.
	protocols []int
	// modes selects the gitai modes it applies to; nil means every mode.
	modes func(gitaiMode) bool
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

	// In authoritative mode the storage core refuses, in the update hook, a reference that git cannot
	// hold next to one the database holds. receive-pack reports it as a hook's refusal, with the
	// reason on a remote: line, where stock git reports its own refusal of the name; the other
	// references of the push are handled as in stock git.
	refNameHookDecline("conflict: a branch below a branch", "step 03 git push --porcelain origin main:refs/heads/a/b: refs/heads/a/b", "refname conflict"),
	refNameHookDecline("conflict: a branch above a branch", "step 03 git push --porcelain origin main:refs/heads/a: refs/heads/a", "refname conflict"),
	refNameHookDecline("push non-atomic, one refused by the server",
		"step 02 git push --porcelain origin main:refs/heads/one main:refs/heads/side/below: refs/heads/side/below", "refname conflict"),
	// With --atomic, git refuses the whole push in both; it names another reason for each reference.
	refNameHookDecline("push atomic, one refused by the server",
		"step 02 git push --porcelain origin --atomic main:refs/heads/one main:refs/heads/side/below: refs/heads/side/below", "atomic transaction failed",
		`step 02 git push --porcelain origin --atomic main:refs/heads/one main:refs/heads/side/below: refs/heads/one: stock "! [remote rejected] (atomic transaction failed)", gitai "! [remote rejected] (atomic push failure)"`),
	// The update hook runs before the deletion of the same atomic push: it refuses refs/heads/a/b
	// while the database holds refs/heads/a. Stock git refuses the push too, as git does not resolve
	// a file/directory conflict inside one transaction.
	refNameHookDecline("conflict resolved by a delete in the same atomic push",
		"step 03 git push --porcelain origin --atomic :refs/heads/a main:refs/heads/a/b: refs/heads/a/b", "atomic transaction failed",
		`step 03 git push --porcelain origin --atomic :refs/heads/a main:refs/heads/a/b: refs/heads/a: stock "! [remote rejected] (atomic transaction failed)", gitai "! [remote rejected] (atomic push failure)"`),

	// Both names are new in one push, so the update hook cannot see the conflict; git refuses one of
	// them, but still hands both to the reference-transaction hook (bugRejectedUpdateCommitted). The
	// database refuses that changeset, which fails the whole push in its prepared phase. This entry
	// goes away with the bug.
	{
		row:    "conflict inside one push",
		modes:  withAuthoritativeCore,
		reason: bugRejectedUpdateCommitted + ": the database refuses the changeset that holds the rejected update, so receive-pack dies and nothing of the push lands",
		diffs: []string{
			`server ref refs/heads/a/b: stock "<oid>", gitai "<absent>"`,
			`step 02 git push --porcelain origin main:refs/heads/a main:refs/heads/a/b: exit: stock "1", gitai "128"`,
			`step 02 git push --porcelain origin main:refs/heads/a main:refs/heads/a/b: refs/heads/a/b: stock "* [new branch]", gitai "<absent>"`,
			`step 02 git push --porcelain origin main:refs/heads/a main:refs/heads/a/b: refs/heads/a: stock "! [remote rejected] (refname conflict)", gitai "<absent>"`,
			`step 02 git push --porcelain origin main:refs/heads/a main:refs/heads/a/b: says : stock "<absent>", gitai "yes"`,
			`step 02 git push --porcelain origin main:refs/heads/a main:refs/heads/a/b: says Done: stock "yes", gitai "<absent>"`,
			`step 02 git push --porcelain origin main:refs/heads/a main:refs/heads/a/b: says To <url>: stock "yes", gitai "<absent>"`,
			`step 03 git ls-remote --symref <url>: refs/heads/a/b: stock "<oid>", gitai "<absent>"`,
		},
	},
}

// reasonRefNameHookDecline is why the update hook refuses a name git cannot hold next to a reference
// of the storage core's database (doc/graft/STORAGE.md, Failures).
const reasonRefNameHookDecline = "the storage core's database holds every node's references, and the update hook refuses a name git cannot hold next to one of them, so that no node is left with a pair it cannot apply"

// refNameHookDecline is the difference of a push whose reference the update hook declines for the
// storage core, in authoritative mode (shadow mode never declines a git write): line is the step and
// reference of its status, and stockReason the reason stock git gives.
func refNameHookDecline(row, line, stockReason string, more ...string) allowedDifference {
	return allowedDifference{
		row:    row,
		modes:  withAuthoritativeCore,
		reason: reasonRefNameHookDecline,
		diffs: append([]string{
			line + `: stock "! [remote rejected] (` + stockReason + `)", gitai "! [remote rejected] (hook declined)"`,
		}, more...),
	}
}

// allowedFor returns the differences the row may have with the protocol in mode, sorted as
// differences() sorts them.
func allowedFor(row string, mode gitaiMode, protocol int) []string {
	allowed := []string{}
	for _, entry := range allowedDifferences {
		if entry.row != row {
			continue
		}
		if entry.protocols != nil && !containsInt(entry.protocols, protocol) {
			continue
		}
		if entry.modes != nil && !entry.modes(mode) {
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
// receive-pack (REF_TRANSACTION_ALLOW_FAILURE) already rejected. The database refuses a changeset
// that would leave a pair git cannot hold, so it never records such a rejected update; in
// authoritative mode the update hook also keeps a name that conflicts with a reference of the
// database out of the transaction. What is left:
//   - authoritative mode, a push that creates both names of a pair: the update hook cannot see that,
//     git rejects one name, and the database refuses the changeset that still holds both, which fails
//     the whole push (an allowed difference above);
//   - shadow mode, which never declines a git write: the changeset that holds git's rejected name is
//     not recorded, so the database misses the names git did write in that push.
const bugRejectedUpdateCommitted = "a rejected update of a non-atomic push reaches the reference-transaction hook"

func missingAt(reference string) []string {
	return []string{`the database diverges at ` + reference + `: local "<oid>", database ""`}
}

func withShadowCore(mode gitaiMode) bool { return mode.coreMode == corestore.ModeShadow }

func withAuthoritativeCore(mode gitaiMode) bool { return mode.coreMode == corestore.ModeAuthoritative }

var knownProblems = []knownProblem{
	{"conflict inside one push", withShadowCore, bugRejectedUpdateCommitted, missingAt("refs/heads/a/b")},
	{"push non-atomic, one refused by the server", withShadowCore, bugRejectedUpdateCommitted, missingAt("refs/heads/one")},
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
