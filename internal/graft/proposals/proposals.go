// Package proposals keeps the proposals of a land the server did not resolve (PLAN10 14): one
// reference per branch and landed commit under refs/graft/proposals/, written only by the server.
// It is the one writer of those references (Update) and the one place they expire (Prune), used
// by UserLand, one sweep at a time per branch (Sweeper), and by the housekeeping pass.
package proposals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/log"
)

const (
	// RefPrefix is the hidden namespace of the proposals (git.InternalRefPrefixes).
	RefPrefix = "refs/graft/proposals/"
	// MaxPerBranch bounds the proposal references kept for one branch.
	MaxPerBranch = 20
)

// BranchDir is the directory of the proposal references of a branch: a hash of the branch name,
// so that any branch name gives a valid, fixed-length component.
func BranchDir(branch string) string {
	sum := sha256.Sum256([]byte(branch))
	return RefPrefix + hex.EncodeToString(sum[:])[:16] + "/"
}

// Ref is the reference that keeps the proposals of the land of commit onto branch.
func Ref(branch string, commit git.ObjectID) git.ReferenceName {
	return git.ReferenceName(BranchDir(branch) + commit.String())
}

// RefNameValid reports whether name is refs/graft/proposals/<16 hex>/<object ID>.
func RefNameValid(name git.ReferenceName) bool {
	rest, ok := strings.CutPrefix(name.String(), RefPrefix)
	if !ok {
		return false
	}
	dir, commit, ok := strings.Cut(rest, "/")
	return ok && len(dir) == 16 && isHex(dir) && (len(commit) == 40 || len(commit) == 64) && isHex(commit)
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Op is one update of a proposal reference, from OldOID to NewOID; a NewOID that is the zero
// object ID deletes it, and an OldOID that is the zero object ID creates it. Every update is a
// compare-and-swap against OldOID.
type Op struct {
	Name           git.ReferenceName
	NewOID, OldOID git.ObjectID
}

// Update is the one writer of the proposal references: it refuses any name but
// refs/graft/proposals/<16 hex>/<object ID> before git runs, and updates them as the server,
// without the identity of the request, through the reference transaction like any update (and so
// the voter in authoritative storage mode). The agent whose land made the proposals may not write
// refs/graft/ itself (the reference-transaction hook reserves it), and its policy does not judge
// the server's write; who caused it is in the proposals' commit.
func Update(ctx context.Context, repo *localrepo.Repo, ops []Op) error {
	for _, op := range ops {
		if !RefNameValid(op.Name) {
			return fmt.Errorf("refusing to update %q: not a proposal reference", op.Name)
		}
	}
	ctx = identity.NewContext(ctx, identity.Identity{})
	updater, err := updateref.New(ctx, repo)
	if err != nil {
		return fmt.Errorf("updating proposal references: %w", err)
	}
	defer func() { _ = updater.Close() }()
	if err := updater.Start(); err != nil {
		return fmt.Errorf("updating proposal references: %w", err)
	}
	for _, op := range ops {
		if err := updater.Update(op.Name, op.NewOID, op.OldOID); err != nil {
			return fmt.Errorf("updating %s: %w", op.Name, err)
		}
	}
	if err := updater.Commit(); err != nil {
		return fmt.Errorf("updating proposal references: %w", err)
	}
	return nil
}

// pruneSkipped counts the proposal references that Prune left because they point to no commit.
var pruneSkipped = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_proposal_prune_skipped_total",
	Help: "Proposal references left by the pruning because they point to no commit.",
})

// Prune deletes, among the proposal references under dir (a branch's BranchDir, or RefPrefix for
// every branch), those older than ttl and those beyond the newest MaxPerBranch of each branch. The
// age is the committer date of the proposals' commit, which the server writes with its own clock
// (never the request's). It returns the number of references deleted. A reference to no commit is
// left, logged and counted (graft_proposal_prune_skipped_total), and does not stop the pruning of
// the others.
//
// Each deletion expects the target it read, and this protects data: the name of a proposal
// reference is (branch, commit) and landing that commit again replaces its target, so a reference
// listed as expired or surplus may hold the branch's newest proposal by the time it is deleted. The
// transaction then fails and deletes nothing, and a later Prune decides from what it lists then.
func Prune(ctx context.Context, repo *localrepo.Repo, dir string, ttl time.Duration, now time.Time) (int, error) {
	return prune(ctx, repo, dir, ttl, now, nil)
}

// prune is Prune, calling beforeDelete, when set, once the deletions are decided, if there are any.
func prune(ctx context.Context, repo *localrepo.Repo, dir string, ttl time.Duration, now time.Time, beforeDelete func()) (int, error) {
	if !strings.HasPrefix(dir, RefPrefix) {
		return 0, fmt.Errorf("pruning proposals: %q is not under %s", dir, RefPrefix)
	}
	refs, err := repo.GetReferences(ctx, dir)
	if err != nil {
		return 0, fmt.Errorf("listing the proposals under %s: %w", dir, err)
	}
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return 0, fmt.Errorf("detecting object hash: %w", err)
	}
	type dated struct {
		ref  git.Reference
		when time.Time
	}
	byBranch := map[string][]dated{}
	var remove []Op
	for _, r := range refs {
		if !RefNameValid(r.Name) || r.IsSymbolic {
			continue // not written by Update; nothing of the server's to expire
		}
		c, err := repo.ReadCommit(ctx, git.Revision(r.Target))
		if errors.Is(err, localrepo.ErrObjectNotFound) {
			// A reference to no commit was not written by Update: it is left as it is, and the
			// others are pruned.
			pruneSkipped.Inc()
			log.FromContext(ctx).WithField("reference", r.Name.String()).WarnContext(ctx,
				"graft: a proposal reference points to no commit; it is not pruned")
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("reading %s: %w", r.Name, err)
		}
		when := c.GetCommitter().GetDate().AsTime()
		if now.Sub(when) > ttl {
			remove = append(remove, Op{Name: r.Name, NewOID: objectHash.ZeroOID, OldOID: git.ObjectID(r.Target)})
			continue
		}
		branch := strings.SplitN(strings.TrimPrefix(r.Name.String(), RefPrefix), "/", 2)[0]
		byBranch[branch] = append(byBranch[branch], dated{r, when})
	}
	for _, kept := range byBranch {
		sort.Slice(kept, func(i, j int) bool { return kept[i].when.After(kept[j].when) })
		for _, d := range kept[min(MaxPerBranch, len(kept)):] {
			remove = append(remove, Op{Name: d.ref.Name, NewOID: objectHash.ZeroOID, OldOID: git.ObjectID(d.ref.Target)})
		}
	}
	if len(remove) == 0 {
		return 0, nil
	}
	if beforeDelete != nil {
		beforeDelete()
	}
	if err := Update(ctx, repo, remove); err != nil {
		return 0, fmt.Errorf("pruning proposals: %w", err)
	}
	return len(remove), nil
}

// drop deletes the proposal reference name, the proposal of a change that landed. It expects the
// target it reads just before deleting, so that a proposal that a land of the same commit wrote
// since stays, and that fails the drop. A reference already deleted, before the read or by a sweep
// between the read and the deletion, is what it wants, and not an error. A symbolic reference was
// not written by Update and is left, as Prune leaves it. It calls beforeDelete, when set, between
// the read and the deletion.
func drop(ctx context.Context, repo *localrepo.Repo, name git.ReferenceName, beforeDelete func()) error {
	if !RefNameValid(name) {
		return fmt.Errorf("refusing to drop %q: not a proposal reference", name)
	}
	ref, err := repo.GetReference(ctx, name)
	if errors.Is(err, git.ErrReferenceNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}
	if ref.IsSymbolic {
		return nil
	}
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}
	if beforeDelete != nil {
		beforeDelete()
	}
	updateErr := Update(ctx, repo, []Op{{Name: name, NewOID: objectHash.ZeroOID, OldOID: git.ObjectID(ref.Target)}})
	if updateErr == nil {
		return nil
	}
	// The deletion failed: it is done only if the reference is gone now. Any other state, a new
	// target or a read that fails too, leaves the failure standing.
	if _, err := repo.GetReference(ctx, name); errors.Is(err, git.ErrReferenceNotFound) {
		return nil
	}
	return fmt.Errorf("dropping %s: %w", name, updateErr)
}
