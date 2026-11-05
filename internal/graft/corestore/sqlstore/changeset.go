package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
)

// CommitChangeset implements corestore.MetaStore.
func (s *Store) CommitChangeset(ctx context.Context, in corestore.ChangesetInput) (corestore.CommitResult, error) {
	var result corestore.CommitResult

	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		result = corestore.CommitResult{}

		repo, err := s.selectRepository(ctx, tx, "repo_id = ?", []any{in.RepoID}, true)
		if err != nil {
			return err
		}

		result, err = s.commitChangeset(ctx, tx, &repo, in)
		return err
	})
	if err != nil {
		return corestore.CommitResult{}, err
	}

	return result, nil
}

// validateChangeset checks the input of a changeset before anything is read.
func validateChangeset(in corestore.ChangesetInput) error {
	if len(in.Updates) == 0 {
		return errors.New("storage core: changeset has no reference updates")
	}

	seen := make(map[string]struct{}, len(in.Updates))
	for _, u := range in.Updates {
		if u.Name == "" {
			return errors.New("storage core: reference update has an empty name")
		}
		if _, dup := seen[u.Name]; dup {
			return fmt.Errorf("storage core: reference %q is updated twice in one changeset", u.Name)
		}
		seen[u.Name] = struct{}{}

		switch u.Expect {
		case corestore.ExpectValue, corestore.ExpectAny:
		default:
			return fmt.Errorf("storage core: reference %q has an unknown expectation %d", u.Name, u.Expect)
		}

		// The target columns are text: Postgres refuses bytes that are not
		// UTF-8 and NUL bytes, so both dialects refuse such a target (a
		// symbolic reference to a name that is not UTF-8) instead of storing it
		// on SQLite only.
		if !storableText(string(u.New)) {
			return fmt.Errorf("storage core: new target %q of reference %q is not valid UTF-8 text", u.New, u.Name)
		}

		// A deletion is exempt, so that an existing long reference can be cleaned up.
		if !u.New.Absent() {
			if err := git.ValidateReferenceNameLength(u.Name); err != nil {
				return fmt.Errorf("storage core: %w", err)
			}
			if u.New.IsSymbolic() {
				if err := git.ValidateReferenceNameLength(u.New.SymbolicName()); err != nil {
					return fmt.Errorf("storage core: symbolic reference target: %w", err)
				}
			}
		}
	}

	return nil
}

// checkPackDeadline refuses, with ErrPackDeadline, the commit of a row that
// references an uploaded pack once the database clock reached the pack's write
// deadline (PackUpload.DBDeadline), and the commit of one without a deadline.
// Garbage collection deletes an unreferenced pack gc_grace after it observed it
// on the database clock, which is after the record read the deadline starts
// from: a row committed before the deadline is never a row whose pack is gone,
// whatever the writer's own clock did meanwhile (a suspended host, a paused
// VM).
func (s *Store) checkPackDeadline(ctx context.Context, tx *sql.Tx, deadline time.Time) error {
	if deadline.IsZero() {
		return errors.New("storage core: a row that references a pack must carry the pack's write deadline")
	}
	now, err := s.wallTime(ctx, tx)
	if err != nil {
		return err
	}
	if !now.Before(deadline) {
		return fmt.Errorf("storage core: commit at %s, after the pack's write deadline %s: %w", now.Format(time.RFC3339Nano), deadline.UTC().Format(time.RFC3339Nano), corestore.ErrPackDeadline)
	}
	return nil
}

// commitChangeset records a changeset in the open transaction tx, for the
// locked repository repo, and advances repo to the new sequences. It is shared
// by CommitChangeset and the initial changeset of RegisterRepository.
//
// It returns *corestore.ErrRefConflict, writing nothing, when an expectation
// fails and in.Force is not set, and *corestore.ErrRefNameConflict, in which case
// the caller's rollback undoes the writes, when a reference it creates cannot
// exist in git next to another one (refuseRefNameConflicts).
func (s *Store) commitChangeset(ctx context.Context, tx *sql.Tx, repo *repoState, in corestore.ChangesetInput) (corestore.CommitResult, error) {
	if err := validateChangeset(in); err != nil {
		return corestore.CommitResult{}, err
	}

	names := make([]string, len(in.Updates))
	for i, u := range in.Updates {
		names[i] = u.Name
	}

	current, err := s.currentTargets(ctx, tx, repo.id, names)
	if err != nil {
		return corestore.CommitResult{}, err
	}

	if in.OnlyConflictingNames {
		if err := s.refuseRepairOfApplicableRefs(ctx, tx, repo.id, names, current); err != nil {
			return corestore.CommitResult{}, err
		}
	}

	var (
		conflicts []corestore.RefConflict
		effective []corestore.AppliedUpdate
	)
	for _, u := range in.Updates {
		actual := current[u.Name]

		if u.Expect == corestore.ExpectValue && actual != u.Old {
			conflicts = append(conflicts, corestore.RefConflict{Name: u.Name, Expected: u.Old, Actual: actual})
		}

		if actual != u.New {
			effective = append(effective, corestore.AppliedUpdate{Name: u.Name, Old: actual, New: u.New})
		}
	}

	if len(conflicts) > 0 && !in.Force {
		return corestore.CommitResult{}, &corestore.ErrRefConflict{Conflicts: conflicts}
	}
	if !in.Force {
		if err := s.refuseReleasedRoots(ctx, tx, repo.id, effective); err != nil {
			return corestore.CommitResult{}, err
		}
	}

	if len(effective) == 0 {
		return corestore.CommitResult{NoOp: true, Conflicts: conflicts}, nil
	}

	now := time.Now().UTC()
	seq := repo.headSeq + 1
	forced := in.Force && len(conflicts) > 0

	var packKey any
	if in.PackKey != "" {
		if err := s.checkPackDeadline(ctx, tx, in.PackDeadline); err != nil {
			return corestore.CommitResult{}, err
		}
		packKey = in.PackKey
	}

	if _, err := tx.ExecContext(ctx, s.bind(`
		INSERT INTO graft_changesets (repo_id, seq, operation, actor, node_id, pack_key, pack_bytes, forced, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), repo.id, int64(seq), validUTF8(in.Operation), validUTF8(in.Actor), validUTF8(in.NodeID),
		packKey, in.PackBytes, forced, s.timeArg(now)); err != nil {
		return corestore.CommitResult{}, fmt.Errorf("insert changeset: %w", err)
	}

	for ordinal, u := range effective {
		if _, err := tx.ExecContext(ctx, s.bind(`
			INSERT INTO graft_changeset_refs (repo_id, seq, ordinal, name, old_target, new_target)
			VALUES (?, ?, ?, ?, ?, ?)
		`), repo.id, int64(seq), ordinal, bytesArg([]byte(u.Name)), targetArg(u.Old), targetArg(u.New)); err != nil {
			return corestore.CommitResult{}, fmt.Errorf("insert changeset reference: %w", err)
		}

		if u.New == "" {
			_, err = tx.ExecContext(ctx, s.bind("DELETE FROM graft_refs WHERE repo_id = ? AND name = ?"),
				repo.id, bytesArg([]byte(u.Name)))
		} else {
			_, err = tx.ExecContext(ctx, s.bind(`
				INSERT INTO graft_refs (repo_id, name, target, seq)
				VALUES (?, ?, ?, ?)
				ON CONFLICT (repo_id, name) DO UPDATE SET
					target = excluded.target,
					seq = excluded.seq
			`), repo.id, bytesArg([]byte(u.Name)), string(u.New), int64(seq))
		}
		if err != nil {
			return corestore.CommitResult{}, fmt.Errorf("update reference %q: %w", u.Name, err)
		}
	}

	if err := s.refuseRefNameConflicts(ctx, tx, repo.id, effective); err != nil {
		return corestore.CommitResult{}, err
	}

	var entries []changelog.Entry
	if in.Changelog != nil {
		entries = buildChangelogEntries(repo, in.Changelog, now, effective)
		if err := insertChangelog(ctx, s, tx, repo.id, entries); err != nil {
			return corestore.CommitResult{}, err
		}
		if n := len(entries); n > 0 {
			repo.logSeq = entries[n-1].Sequence
		}
	}

	repo.headSeq = seq

	if _, err := tx.ExecContext(ctx, s.bind("UPDATE graft_repositories SET head_seq = ?, log_seq = ? WHERE repo_id = ?"),
		int64(repo.headSeq), int64(repo.logSeq), repo.id); err != nil {
		return corestore.CommitResult{}, fmt.Errorf("update repository sequences: %w", err)
	}

	return corestore.CommitResult{
		Seq:       seq,
		Forced:    forced,
		Conflicts: conflicts,
		Updates:   effective,
		Changelog: entries,
	}, nil
}

// refuseRefNameConflicts returns *corestore.ErrRefNameConflict when a reference
// that updates created cannot exist in git next to another reference of the
// repository: one of its parent directories is a reference, or a reference
// exists below it. git itself checks this only against its local references,
// which may be behind the database, so without this check two nodes could each
// commit one side of a pair that no node can then apply.
//
// It runs in tx after the updates were written, so it reads the state the
// changeset leaves: a reference the changeset deletes no longer counts (refs/a
// deleted and refs/a/b created in one changeset is legal), and a reference it
// creates does. The caller rolls back on an error.
//
// It sees every commit of the repository before its own. CommitChangeset holds
// the row lock of the repository from before the first read (Postgres; READ
// COMMITTED, so each statement here reads what the commits before the lock
// committed) or the database write lock (SQLite, BEGIN IMMEDIATE), and the
// changesets are the only writers of graft_refs. Two commits of a pair therefore
// run one after the other, and the second one finds the first one's row.
//
// A reference that already exists is not checked, whatever its value changes
// to: an update or a deletion creates no new name, and updates of existing
// references, the common case (a land), cost no query here. The parent names of
// the created references, deduplicated, are read together, in one primary key
// lookup per maxNamesPerQuery names, and the ranges below the created references
// in one primary key range query per maxNamesPerQuery names that reads at most
// one row. Neither reads the other references of the repository, and a push that
// creates many references holds the lock of the repository for a few statements
// more, not one per reference.
func (s *Store) refuseRefNameConflicts(ctx context.Context, tx *sql.Tx, repoID int64, updates []corestore.AppliedUpdate) error {
	var created, parents []string
	seen := map[string]struct{}{}
	for _, u := range updates {
		if !u.Old.Absent() || u.New.Absent() {
			continue
		}
		created = append(created, u.Name)
		for _, parent := range corestore.ParentRefNames(u.Name) {
			if _, dup := seen[parent]; !dup {
				seen[parent] = struct{}{}
				parents = append(parents, parent)
			}
		}
	}

	existing, err := s.currentTargets(ctx, tx, repoID, parents)
	if err != nil {
		return err
	}

	for _, name := range created {
		for _, parent := range corestore.ParentRefNames(name) {
			if _, ok := existing[parent]; ok {
				return &corestore.ErrRefNameConflict{Name: name, Existing: parent}
			}
		}
	}

	for start := 0; start < len(created); start += maxNamesPerQuery {
		chunk := created[start:min(start+maxNamesPerQuery, len(created))]
		args := make([]any, 0, 1+2*len(chunk))
		args = append(args, repoID)
		for _, name := range chunk {
			lower, upper := refChildrenRange(name)
			args = append(args, lower, upper)
		}

		var child []byte
		err := tx.QueryRowContext(ctx, s.bind(refInRangesQuery(len(chunk))), args...).Scan(&child)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return fmt.Errorf("read the references below the created references: %w", err)
		}
		for _, name := range chunk {
			if strings.HasPrefix(string(child), name+"/") {
				return &corestore.ErrRefNameConflict{Name: name, Existing: string(child)}
			}
		}
		return fmt.Errorf("storage core: reference %q is below none of the created references", child)
	}

	return nil
}

// refInRangesQuery returns a reference of a repository in any of n ranges of
// names, which are bound after the repository as lower and upper bound of each
// range. Each range is on the primary key (repo_id, name). There is no ORDER BY:
// any reference in a range is a conflict, and an order would let a planner walk
// the whole primary key of the repository instead of the ranges.
func refInRangesQuery(n int) string {
	var b strings.Builder
	b.WriteString("SELECT name FROM graft_refs WHERE repo_id = ? AND (")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(" OR ")
		}
		b.WriteString("(name >= ? AND name < ?)")
	}
	b.WriteString(") LIMIT 1")
	return b.String()
}

// firstRefBelow returns the first reference of the repository below the
// directory name, and whether there is one.
func (s *Store) firstRefBelow(ctx context.Context, q querier, repoID int64, name string) (string, bool, error) {
	var child []byte
	lower, upper := refChildrenRange(name)
	err := q.QueryRowContext(ctx, s.bind(firstRefInRangeQuery), repoID, lower, upper).Scan(&child)
	switch {
	case err == nil:
		return string(child), true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("read the references below %q: %w", name, err)
	}
}

// conflictingRef returns a reference of the repository, as q reads it, that git
// cannot hold next to the reference name: a parent directory of name that is a
// reference, else the first reference below name. The reference name itself
// does not count. It costs one primary key lookup of the parent names and one
// primary key range probe.
func (s *Store) conflictingRef(ctx context.Context, q querier, repoID int64, name string) (string, bool, error) {
	parents := corestore.ParentRefNames(name)
	existing, err := s.currentTargets(ctx, q, repoID, parents)
	if err != nil {
		return "", false, err
	}
	for _, parent := range parents {
		if _, ok := existing[parent]; ok {
			return parent, true, nil
		}
	}
	return s.firstRefBelow(ctx, q, repoID, name)
}

// ConflictingRef implements corestore.MetaStore.
func (s *Store) ConflictingRef(ctx context.Context, repoID int64, name string) (string, bool, error) {
	var (
		existing string
		found    bool
	)
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		existing, found, err = s.conflictingRef(ctx, tx, repoID, name)
		return err
	})
	if err != nil {
		return "", false, err
	}
	return existing, found, nil
}

// refuseRepairOfApplicableRefs returns corestore.ErrRefNotConflicting unless
// every reference of names exists in current and conflicts by name with another
// reference of the repository (ChangesetInput.OnlyConflictingNames). It reads
// in tx, under the lock of the repository, so the check holds for the commit it
// guards: once one side of a pair is deleted, the other is no longer accepted.
func (s *Store) refuseRepairOfApplicableRefs(ctx context.Context, tx *sql.Tx, repoID int64, names []string, current map[string]corestore.Target) error {
	for _, name := range names {
		if current[name].Absent() {
			return fmt.Errorf("reference %q does not exist: %w", name, corestore.ErrRefNotConflicting)
		}
		_, found, err := s.conflictingRef(ctx, tx, repoID, name)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("reference %q: %w", name, corestore.ErrRefNotConflicting)
		}
	}
	return nil
}

// firstRefInRangeQuery returns the first reference of a repository in a range of
// names. The range is on the primary key (repo_id, name), whose names compare as
// bytes in both dialects (BLOB and BYTEA), whatever the collation.
const firstRefInRangeQuery = "SELECT name FROM graft_refs WHERE repo_id = ? AND name >= ? AND name < ? ORDER BY name LIMIT 1"

// refChildrenRange returns the half-open range of the names below the reference
// name: from name + "/" up to name + "0", '0' being the byte after '/'. refs/a-b
// and refs/a0 are outside the range of refs/a.
func refChildrenRange(name string) (lower, upper []byte) {
	return []byte(name + "/"), []byte(name + "0")
}

// buildChangelogEntries returns the changelog entries of the effective updates
// of a changeset: one per branch or tag that is not symbolic, with the
// sequences that follow repo's last changelog sequence.
func buildChangelogEntries(repo *repoState, meta *changelog.Meta, now time.Time, updates []corestore.AppliedUpdate) []changelog.Entry {
	zero := zeroOID(repo.objectFormat)

	recordedAt := meta.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = now
	}
	recordedAt = recordedAt.UTC()

	var entries []changelog.Entry
	seq := repo.logSeq
	for _, u := range updates {
		if !corestore.IsChangelogRef(u.Name) || u.Old.IsSymbolic() || u.New.IsSymbolic() {
			continue
		}

		seq++

		oldOID := string(u.Old)
		if oldOID == "" {
			oldOID = zero
		}
		newOID := string(u.New)
		if newOID == "" {
			newOID = zero
		}

		entries = append(entries, changelog.Entry{
			Sequence:         seq,
			Reference:        u.Name,
			OldOID:           oldOID,
			NewOID:           newOID,
			Operation:        validUTF8(meta.Operation),
			Actor:            validUTF8(meta.Actor),
			RecordedAt:       recordedAt,
			Rewritten:        meta.Rewritten,
			Members:          slices.Clone(meta.Members),
			MembersTruncated: meta.MembersTruncated,
			Lands:            changelog.LandsOf(*meta, u.Name),
			LandInvocation:   changelog.LandInvocationOf(*meta, u.Name),
		})
	}

	return entries
}

// currentTargets returns the current targets of the named references of the
// repository. Absent references are missing from the map.
func (s *Store) currentTargets(ctx context.Context, q querier, repoID int64, names []string) (map[string]corestore.Target, error) {
	current := make(map[string]corestore.Target, len(names))

	for start := 0; start < len(names); start += maxNamesPerQuery {
		chunk := names[start:min(start+maxNamesPerQuery, len(names))]

		args := make([]any, 0, len(chunk)+1)
		args = append(args, repoID)
		placeholders := make([]string, len(chunk))
		for i, name := range chunk {
			placeholders[i] = "?"
			args = append(args, bytesArg([]byte(name)))
		}

		query := "SELECT name, target FROM graft_refs WHERE repo_id = ? AND name IN (" + strings.Join(placeholders, ", ") + ")"
		if err := readReferences(ctx, s, q, query, args, current); err != nil {
			return nil, err
		}
	}

	return current, nil
}

// readReferences runs a query of (name, target) rows and adds them to refs.
func readReferences(ctx context.Context, s *Store, q querier, query string, args []any, refs map[string]corestore.Target) error {
	rows, err := q.QueryContext(ctx, s.bind(query), args...)
	if err != nil {
		return fmt.Errorf("read references: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name []byte
		var target string
		if err := rows.Scan(&name, &target); err != nil {
			return fmt.Errorf("scan reference: %w", err)
		}
		refs[string(name)] = corestore.Target(target)
	}

	return rows.Err()
}

// ListChangesets implements corestore.MetaStore.
func (s *Store) ListChangesets(ctx context.Context, repoID int64, after, upTo uint64) ([]corestore.Changeset, error) {
	// The sequences are BIGINT: no sequence is above math.MaxInt64.
	if after >= math.MaxInt64 {
		return nil, nil
	}
	upTo = min(upTo, math.MaxInt64)
	if upTo <= after {
		return nil, nil
	}

	var changesets []corestore.Changeset
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		changesets = nil

		rows, err := tx.QueryContext(ctx, s.bind(`
			SELECT seq, operation, actor, node_id, pack_key, pack_bytes, forced, created_at
			FROM graft_changesets
			WHERE repo_id = ? AND seq > ? AND seq <= ?
			ORDER BY seq
		`), repoID, int64(after), int64(upTo))
		if err != nil {
			return fmt.Errorf("list changesets: %w", err)
		}

		index := map[uint64]int{}
		for rows.Next() {
			var (
				seq        int64
				cs         corestore.Changeset
				packKey    sql.NullString
				createdRaw any
			)
			if err := rows.Scan(&seq, &cs.Operation, &cs.Actor, &cs.NodeID, &packKey, &cs.PackBytes, &cs.Forced, &createdRaw); err != nil {
				rows.Close()
				return fmt.Errorf("scan changeset: %w", err)
			}

			cs.RepoID = repoID
			cs.Seq = uint64(seq)
			cs.PackKey = packKey.String
			cs.CreatedAt, err = scanTime(createdRaw)
			if err != nil {
				rows.Close()
				return err
			}

			index[cs.Seq] = len(changesets)
			changesets = append(changesets, cs)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list changesets: %w", err)
		}
		rows.Close()

		refRows, err := tx.QueryContext(ctx, s.bind(`
			SELECT seq, name, old_target, new_target
			FROM graft_changeset_refs
			WHERE repo_id = ? AND seq > ? AND seq <= ?
			ORDER BY seq, ordinal
		`), repoID, int64(after), int64(upTo))
		if err != nil {
			return fmt.Errorf("list changeset references: %w", err)
		}
		defer refRows.Close()

		for refRows.Next() {
			var (
				seq      int64
				name     []byte
				oldValue sql.NullString
				newValue sql.NullString
			)
			if err := refRows.Scan(&seq, &name, &oldValue, &newValue); err != nil {
				return fmt.Errorf("scan changeset reference: %w", err)
			}

			i, ok := index[uint64(seq)]
			if !ok {
				return fmt.Errorf("changeset reference of unknown sequence %d", seq)
			}

			changesets[i].Updates = append(changesets[i].Updates, corestore.AppliedUpdate{
				Name: string(name),
				Old:  corestore.Target(oldValue.String),
				New:  corestore.Target(newValue.String),
			})
		}

		return refRows.Err()
	})
	if err != nil {
		return nil, err
	}

	return changesets, nil
}
