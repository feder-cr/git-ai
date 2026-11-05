package sqlstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
)

// OpenV010 opens the database with only the schema of v0.1.0 (the first
// migration), as a v0.1.0 binary left it: no checkpoints, no tombstones and no
// owned-key trigger.
func OpenV010(ctx context.Context, dsn string) (*Store, error) {
	return open(ctx, dsn, 1, DefaultPool)
}

// SetSnapshotWrittenHook installs fn as the hook that CreateCheckpoint of s
// calls once the reference snapshot is written, before created_at is stamped.
func (s *Store) SetSnapshotWrittenHook(fn func()) {
	s.snapshotWritten = fn
}

// SetRetryHook installs fn as the hook called for every retried transaction and
// returns the function that removes it.
func SetRetryHook(fn func(err error)) (restore func()) {
	retryHook = fn
	return func() { retryHook = nil }
}

// SetQueuedHook installs fn as the hook called each time a writer joins the
// write queue of a Store, and returns the function that removes it.
func SetQueuedHook(fn func(waiting int)) (restore func()) {
	queuedHook = fn
	return func() { queuedHook = nil }
}

// HoldWriteAdmission takes the write admission of the Store, as a writer
// would, and returns the function that releases it. SQLite stores only.
func (s *Store) HoldWriteAdmission(ctx context.Context) (release func(), _ error) {
	if s.writers == nil {
		return nil, errors.New("only a SQLite store has a write queue")
	}
	if err := s.writers.acquire(ctx); err != nil {
		return nil, err
	}
	return s.writers.release, nil
}

// WaitingWriters returns the number of writers waiting for the write admission.
func (s *Store) WaitingWriters() int {
	return s.writers.waiting()
}

// IsDeadlock reports whether err is a Postgres deadlock (40P01).
func IsDeadlock(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "40P01"
}

// NotOwnedPackKeyMessage is the documented error of the owned-key trigger.
const NotOwnedPackKeyMessage = notOwnedPackKeyMessage

// SeedReference inserts a reference row directly, as a reference recorded before a validation existed.
func (s *Store) SeedReference(ctx context.Context, repoID int64, name string, target corestore.Target) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO graft_refs (repo_id, name, target, seq) VALUES ($1, $2, $3, 0)", repoID, []byte(name), string(target))
	return err
}

// FirstRefInRangeQuery is the range probe of a created reference.
const FirstRefInRangeQuery = firstRefInRangeQuery

// RefChildrenRange returns the range of the names below the reference name.
var RefChildrenRange = refChildrenRange

// RefInRangesQuery is the range query of the references a changeset creates.
var RefInRangesQuery = refInRangesQuery

// Bind rewrites the placeholders of a query for the dialect of s.
func (s *Store) Bind(query string) string { return s.bind(query) }
