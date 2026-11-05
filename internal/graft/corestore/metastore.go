package corestore

import (
	"context"
	"errors"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// ChangesetInput is a changeset to commit.
type ChangesetInput struct {
	// RepoID is the repository. RegisterRepository ignores it.
	RepoID int64
	// OnlyConflictingNames refuses the changeset with ErrRefNotConflicting,
	// writing nothing, unless every reference it updates exists and is one side
	// of a pair that git cannot hold together (RefNameConflicts). It is checked
	// in the transaction of the commit, under the lock of the repository. Only
	// the operator's repair of such a pair sets it.
	OnlyConflictingNames bool
	// Updates are the reference changes, in order. A name appears at most once.
	Updates []RefUpdate
	// Operation, Actor and NodeID describe who made the changeset.
	Operation string
	Actor     string
	NodeID    string
	// PackKey is the bucket key of the already uploaded pack with the new
	// objects, empty when there are none. PackBytes is its size.
	PackKey   string
	PackBytes int64
	// PackDeadline is the write deadline of the pack on the database clock
	// (PackUpload.DBDeadline). A changeset with a pack is committed only before
	// it, checked in the commit's transaction; one without it is refused.
	PackDeadline time.Time
	// Changelog, when not nil, appends one changelog entry per effective update
	// of a branch or tag (IsChangelogRef), in the order of Updates, in the same
	// transaction. The entries get the next changelog sequences, the actual
	// previous value as OldOID (the zero OID of the repository's object format
	// when the reference was absent) and the new value as NewOID (the zero OID
	// when deleted). A symbolic value is never a branch or tag value; such an
	// update is not logged. RecordedAt zero means the commit time.
	Changelog *changelog.Meta
	// Force records the changeset even when references do not satisfy their
	// expectations (shadow mode): the changeset is marked forced and the
	// conflicts are returned in CommitResult.Conflicts.
	Force bool
}

// CommitResult is the outcome of a committed changeset.
type CommitResult struct {
	// Seq is the sequence of the new changeset, 0 when NoOp.
	Seq uint64
	// NoOp is true when no update changed the database; nothing was written.
	NoOp bool
	// Forced and Conflicts report a changeset recorded with Force despite
	// conflicts.
	Forced    bool
	Conflicts []RefConflict
	// Updates are the effective updates that were recorded.
	Updates []AppliedUpdate
	// Changelog are the changelog entries that were appended.
	Changelog []changelog.Entry
}

// InitialState is what RegisterRepository writes together with the new
// repository, in the same transaction.
type InitialState struct {
	// Changeset, when not nil, is recorded as changeset 1 (its RepoID is
	// ignored; every update must expect the reference to be absent).
	Changeset *ChangesetInput
	// Changelog is imported changelog history. Sequences are kept as they are
	// and must be strictly increasing; entries appended by Changeset follow
	// the last one.
	Changelog []changelog.Entry
	// Reviews are imported reviews, keeping their IDs (decimal strings). The
	// next review ID is one more than the largest imported one.
	Reviews []*gitalypb.Review
}

// CheckpointInput is the state MetaStore.CreateCheckpoint records.
type CheckpointInput struct {
	// Seq is the changeset sequence of the reference snapshot: 1..head sequence.
	// A newer checkpoint covers it (CreateCheckpoint returns that one).
	Seq uint64
	// Refs is the reference snapshot at Seq (every existing reference, HEAD
	// included); it holds no absent value and is not empty.
	Refs map[string]Target
	// PackKey is the owned key (NewPackKey) of the already uploaded consolidated
	// pack, PackBytes its size.
	PackKey   string
	PackBytes int64
	// PackDeadline is the write deadline of the pack on the database clock
	// (PackUpload.DBDeadline), checked in the record's transaction.
	PackDeadline time.Time
}

// ReviewInsert are the conditions under which MetaStore.InsertReview writes a
// review; the zero value has none.
type ReviewInsert struct {
	// RequireRoots requires the keep-around reference (KeepAroundRef) of the
	// review's head and base commits to exist with that value, else
	// ErrReviewRootsMissing and nothing is written (doc/graft/PLAN7.md, REVIEW
	// ROOTS RULE). Together with CommitChangeset's refusal to delete such a
	// reference, every open review written so has its commits referenced.
	RequireRoots bool
	// MaxOpen, when positive, refuses an open review with
	// ErrTooManyOpenReviews when the repository has MaxOpen open reviews
	// already, and nothing is written.
	MaxOpen int
}

var (
	// ErrTooManyOpenReviews is returned by InsertReview for an open review
	// beyond ReviewInsert.MaxOpen.
	ErrTooManyOpenReviews = errors.New("storage core: too many open reviews")
	// ErrReviewNotFound is returned for an unknown review ID.
	ErrReviewNotFound = errors.New("storage core: review not found")
	// ErrReviewVersionConflict is returned by UpdateReview when the review
	// changed since it was read.
	ErrReviewVersionConflict = errors.New("storage core: review changed concurrently")
)

// MetaStore is the strongly consistent database of the storage core. Every
// method is safe for concurrent use by any number of processes sharing the
// database. Implementations: sqlstore (SQLite and Postgres).
type MetaStore interface {
	// RegisterRepository records a new repository with its initial state in
	// one transaction. It returns ErrRepositoryExists when the key is taken.
	RegisterRepository(ctx context.Context, key RepoKey, objectFormat string, init InitialState) (Repository, CommitResult, error)
	// GetRepository returns ErrRepositoryNotFound for an unknown key.
	GetRepository(ctx context.Context, key RepoKey) (Repository, error)
	// DeleteRepository removes the repository and everything recorded for it,
	// and records a tombstone (ListRemovedRepositories) in the same transaction
	// so that garbage collection deletes its objects from the bucket. It returns
	// false, writing nothing, when the repository was not known.
	DeleteRepository(ctx context.Context, key RepoKey) (bool, error)
	// ListRepositories returns the repositories of the storage whose import is
	// complete (head sequence > 0), ascending by ID; every storage's when storage
	// is empty.
	ListRepositories(ctx context.Context, storage string) ([]Repository, error)

	// CommitChangeset checks every update against the current references and
	// records the changeset, the reference changes and the changelog entries
	// in one transaction (see the CAS table of doc/graft/PLAN6.md). It returns
	// *ErrRefConflict, writing nothing, when an expectation fails and Force is
	// not set, and ErrRepositoryNotFound for an unknown repository. Unless Force
	// is set, a changeset that deletes or moves the keep-around reference of a
	// commit that an open review uses as its head or base is refused with
	// ErrReviewRootInUse (doc/graft/PLAN7.md, REVIEW ROOTS RULE); shadow mode,
	// whose changesets are forced, does not enforce it. A changeset that creates
	// a reference that cannot exist in git next to another one of the repository
	// (refs/heads/a and refs/heads/a/b), counting the changeset's own deletions
	// and creations, is refused with *ErrRefNameConflict, writing nothing, also
	// when Force is set: the database must stay something git can apply.
	CommitChangeset(ctx context.Context, in ChangesetInput) (CommitResult, error)
	// ConflictingRef returns a reference of the repository that git cannot hold
	// next to a reference named name (a parent directory of name that is a
	// reference, or a reference below name), and whether there is one; name
	// itself does not count. It reads the committed state, outside any commit, so
	// a commit may change the answer right after: CommitChangeset decides. It
	// costs one primary key lookup and one primary key range probe.
	ConflictingRef(ctx context.Context, repoID int64, name string) (string, bool, error)
	// ListChangesets returns the changesets with after < seq <= upTo in
	// ascending order, with their updates.
	ListChangesets(ctx context.Context, repoID int64, after, upTo uint64) ([]Changeset, error)
	// Refs returns every current reference and the head sequence they
	// correspond to, read in one consistent snapshot.
	Refs(ctx context.Context, repoID int64) (map[string]Target, uint64, error)
	// ReportApplied records how far a node's cache is (observability only).
	ReportApplied(ctx context.Context, repoID int64, nodeID string, seq uint64) error

	// ScanChangelog returns up to limit changelog entries with a sequence
	// greater than after, in ascending order. limit <= 0 means no limit.
	ScanChangelog(ctx context.Context, repoID int64, after uint64, limit int) ([]changelog.Entry, error)
	// LookupLand returns the last land of the original commit on the branch
	// reference that the changelog recorded (changelog.Entry.Lands,
	// doc/graft/PLAN11.md): the sequence of its entry and the commit it landed
	// as. found is false when the changelog recorded no such land. The record is
	// written with its entry, in the same transaction.
	LookupLand(ctx context.Context, repoID int64, reference, original string) (record changelog.LandRecord, found bool, err error)
	// ImportHistory brings history written outside the database (shadow mode
	// writes the file changelog and file reviews) into a tracked repository, in
	// one transaction (doc/graft/PLAN6.md, LOCK AND HISTORY RULE). The changelog
	// entries keep their sequences, which must be strictly increasing and
	// greater than the repository's log sequence, else nothing is written and an
	// error is returned; the log sequence becomes the last one. A review is
	// written when ReviewNeedsImport says so (its ID is new, or it is the
	// database review changed in the file since an earlier import; a replaced
	// review gets the next version) and skipped otherwise; the next review ID
	// becomes one more than the largest review ID. References are not touched. It returns the numbers of
	// entries and reviews written, and ErrRepositoryNotFound for an unknown
	// repository.
	ImportHistory(ctx context.Context, repoID int64, entries []changelog.Entry, reviews []*gitalypb.Review) (importedEntries, importedReviews int, err error)

	// InsertReview allocates the next review ID of the repository, calls build
	// with it and stores the returned review (version 1), atomically. build runs
	// inside the write transaction and must not call the MetaStore.
	//
	// The transaction locks the repository row first, so the conditions of
	// opts hold against every concurrent insert (ReviewInsert).
	InsertReview(ctx context.Context, repoID int64, build func(id string) (*gitalypb.Review, error), opts ReviewInsert) (*gitalypb.Review, error)
	// GetReview returns the review and its version, ErrReviewNotFound if unknown.
	GetReview(ctx context.Context, repoID int64, id string) (*gitalypb.Review, int64, error)
	// ListReviews returns the reviews of the repository newest first
	// (descending numeric ID), keeping only those targeting targetBranch when
	// it is not empty and, unless includeClosed, only those whose state is
	// Review_STATE_OPEN_UNSPECIFIED; at most limit of them (limit <= 0 means
	// no limit). This is exactly the contract of review/store.Store.List.
	ListReviews(ctx context.Context, repoID int64, targetBranch string, includeClosed bool, limit int) ([]*gitalypb.Review, error)
	// UpdateReview stores r if the review is still at version, which becomes
	// version+1. It returns ErrReviewVersionConflict otherwise.
	//
	// requireRoots is as for InsertReview, for the head and base of r: an update
	// that may move the head requires them. Updates that move no commit
	// (comments, verdicts, closes) do not, and do not lock the repository row
	// (LOCK ORDER).
	UpdateReview(ctx context.Context, repoID int64, id string, version int64, r *gitalypb.Review, requireRoots bool) error

	// CreateCheckpoint records a checkpoint and its reference snapshot in one
	// transaction, with the database time as its creation time. It does not lock
	// the repository row (doc/graft/PLAN7.md, LOCK ORDER). It requires Seq <=
	// head sequence, an owned pack key of the repository, a valid snapshot and
	// the pack's write deadline not reached on the database clock, else an error
	// and nothing is written. A checkpoint that covers the request (one at Seq,
	// or a newer one) is returned unchanged with created false: its pack is
	// kept, and the caller's becomes an orphan. ErrRepositoryNotFound for an
	// unknown repository.
	CreateCheckpoint(ctx context.Context, repoID int64, in CheckpointInput) (cp Checkpoint, created bool, err error)
	// NewestCheckpoint returns the checkpoint with the greatest sequence; ok is
	// false when the repository has none.
	NewestCheckpoint(ctx context.Context, repoID int64) (cp Checkpoint, ok bool, err error)
	// SyncBoundary returns the sequence of the newest checkpoint created at
	// least age ago on the database clock, 0 when there is none
	// (doc/graft/PLAN7.md, GC BOUNDARY rule). ErrRepositoryNotFound for an unknown
	// repository.
	SyncBoundary(ctx context.Context, repoID int64, age time.Duration) (uint64, error)
	// CheckpointRefs returns the reference snapshot of the checkpoint at seq,
	// ErrCheckpointNotFound when there is none.
	CheckpointRefs(ctx context.Context, repoID int64, seq uint64) (map[string]Target, error)
	// DeleteCheckpoint removes the checkpoint at seq and its snapshot; false when
	// there was none. The caller deletes the pack afterwards.
	DeleteCheckpoint(ctx context.Context, repoID int64, seq uint64) (bool, error)

	// GCSnapshot reads what garbage collection decides on, in one read
	// transaction (see GCState). ErrRepositoryNotFound for an unknown repository.
	GCSnapshot(ctx context.Context, repoID int64) (GCState, error)
	// RecordOrphans records the database time as the first observation of every
	// key that has none; existing observations are kept.
	RecordOrphans(ctx context.Context, repoID int64, keys []string) error
	// ForgetOrphans removes the observations of the keys.
	ForgetOrphans(ctx context.Context, repoID int64, keys []string) error

	// AcquireMaintenanceLease takes the repository's maintenance lease for
	// holder until the database time plus ttl, by compare-and-swap: it succeeds
	// when there is no lease, when it has expired, or when holder holds it.
	// RenewMaintenanceLease extends it when holder holds it (held false
	// otherwise); ReleaseMaintenanceLease frees it when holder holds it. A lease
	// only keeps two nodes from doing the same maintenance at once: no read,
	// write or correctness argument depends on it. ErrRepositoryNotFound for an
	// unknown repository, except from ReleaseMaintenanceLease, which has nothing
	// to free then.
	AcquireMaintenanceLease(ctx context.Context, repoID int64, holder string, ttl time.Duration) (acquired bool, err error)
	RenewMaintenanceLease(ctx context.Context, repoID int64, holder string, ttl time.Duration) (held bool, err error)
	ReleaseMaintenanceLease(ctx context.Context, repoID int64, holder string) error

	// ListRemovedRepositories returns the tombstones of the storage (of every
	// storage when it is empty), and those of an unknown key (RecordLeak),
	// recorded at least minAge ago on the database clock, ascending by
	// repository ID. A tombstone whose ID has a row (a reconciled ID whose
	// creation committed after all) is not returned.
	ListRemovedRepositories(ctx context.Context, storage string, minAge time.Duration) ([]RemovedRepository, error)
	// DeleteRemovedRepository removes a tombstone; a missing one is success.
	DeleteRemovedRepository(ctx context.Context, repoID int64) error

	// CheckpointsBelow returns the number of checkpoints of the repository
	// below seq.
	CheckpointsBelow(ctx context.Context, repoID int64, seq uint64) (int, error)

	// ObserveAllocator reads the repository ID allocator and the
	// reconciliation cursor and, unless dryRun, records the observation
	// (AllocatorView.Now, Position) and drops the observations older than the
	// one Bound comes from, in one transaction (doc/graft/PLAN7.md, "Removed
	// repositories", reconciliation).
	ObserveAllocator(ctx context.Context, minAge time.Duration, dryRun bool) (AllocatorView, error)
	// UnrecordedIDs returns the repository IDs in (after, upTo] that have
	// neither a row nor a tombstone, ascending.
	UnrecordedIDs(ctx context.Context, after, upTo int64) ([]int64, error)
	// RecordLeak records a tombstone for repoID, with an unknown key (empty
	// storage and relative path) and the database time, unless the ID has a
	// row or a tombstone; it reports whether it recorded one.
	RecordLeak(ctx context.Context, repoID int64) (bool, error)
	// AdvanceReconcile moves the reconciliation cursor to doneThrough unless
	// it is there or further already.
	AdvanceReconcile(ctx context.Context, doneThrough int64) error

	// Close releases the database.
	Close() error
}
