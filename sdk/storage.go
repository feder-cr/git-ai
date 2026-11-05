package sdk

import (
	"context"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// StorageImport is the outcome of importing a repository into the storage core.
//
// A first import records the references in changeset 1 and copies the history
// of the files. The import of a repository that is already imported (a
// history import) only brings in the changelog entries and the reviews
// written into the files since then: References and PackBytes are 0 and
// Sequence is the current head. References is never 0 for a first import, which
// always records HEAD.
type StorageImport struct {
	// Sequence is the sequence of the import changeset (1) for a first import,
	// and the current head sequence for a history import.
	Sequence uint64 `json:"sequence"`
	// References is the number of references imported, HEAD included; 0 for a
	// history import, which does not touch the references.
	References uint32 `json:"references"`
	// PackBytes is the size of the uploaded pack; 0 for a history import.
	PackBytes int64 `json:"pack_bytes"`
	// ChangelogEntries is the number of changelog entries imported.
	ChangelogEntries uint32 `json:"changelog_entries"`
	// Reviews is the number of reviews imported.
	Reviews uint32 `json:"reviews"`
}

// StorageState is how the storage core sees a repository and how far this
// node's local copy is.
type StorageState struct {
	// Mode is the storage core mode of the node: "off", "shadow" or "authoritative".
	Mode string `json:"mode"`
	// Tracked is true when the database knows the repository.
	Tracked bool `json:"tracked"`
	// RepositoryID is the database ID of the repository, 0 when untracked.
	RepositoryID int64 `json:"repository_id"`
	// HeadSequence is the sequence of the repository's last changeset.
	HeadSequence uint64 `json:"head_sequence"`
	// AppliedSequence is how far the local copy is, 0 when it has no watermark.
	AppliedSequence uint64 `json:"applied_sequence"`
	// NodeID is the name of the node in the database.
	NodeID string `json:"node_id"`
}

// StorageDivergence is a reference whose local value differs from the database.
type StorageDivergence struct {
	// Reference is the fully qualified reference name, or HEAD.
	Reference string `json:"reference"`
	// LocalTarget is the local value: an object ID, "ref:<name>" for a symbolic
	// reference, empty when absent.
	LocalTarget string `json:"local_target"`
	// DatabaseTarget is the database value, in the same format.
	DatabaseTarget string `json:"database_target"`
}

// StorageVerify is the outcome of comparing a repository's local references
// with the database.
type StorageVerify struct {
	// HeadSequence is the database sequence the references were compared at.
	HeadSequence uint64 `json:"head_sequence"`
	// Divergences lists every reference that differs; empty when consistent.
	Divergences []StorageDivergence `json:"divergences"`
	// ConnectivityOK is true when the connectivity check ran and passed.
	ConnectivityOK bool `json:"connectivity_ok"`
	// ConnectivityError describes a failed connectivity check.
	ConnectivityError string `json:"connectivity_error"`
}

// StorageImport imports the repository into the storage core: its references,
// one pack of every reachable object, its changelog history and its reviews.
// On a repository that is already imported it imports the history written into
// the files since then instead (the file changelog entries beyond the
// database's last sequence and the file reviews the database does not have),
// without touching the references; running it again imports nothing new. It
// fails with AlreadyExists when the server's local copy is not the one that
// was imported (its watermark names another repository ID, or it has none),
// or when another import of the repository is in progress, and with
// FailedPrecondition when the storage core is off.
func (r *Repo) StorageImport(ctx context.Context) (*StorageImport, error) {
	resp, err := r.storageCore().ImportRepository(ctx, &gitalypb.ImportRepositoryRequest{
		Repository: r.repository(),
	})
	if err != nil {
		return nil, err
	}
	return &StorageImport{
		Sequence:         resp.GetSequence(),
		References:       resp.GetReferences(),
		PackBytes:        resp.GetPackBytes(),
		ChangelogEntries: resp.GetChangelogEntries(),
		Reviews:          resp.GetReviews(),
	}, nil
}

// StorageState returns how the storage core sees the repository and how far
// the local copy on the server is.
func (r *Repo) StorageState(ctx context.Context) (*StorageState, error) {
	resp, err := r.storageCore().GetStorageState(ctx, &gitalypb.GetStorageStateRequest{
		Repository: r.repository(),
	})
	if err != nil {
		return nil, err
	}
	return &StorageState{
		Mode:            resp.GetMode(),
		Tracked:         resp.GetTracked(),
		RepositoryID:    resp.GetRepositoryId(),
		HeadSequence:    resp.GetHeadSequence(),
		AppliedSequence: resp.GetAppliedSequence(),
		NodeID:          resp.GetNodeId(),
	}, nil
}

// StorageVerify compares the local references of the repository with the
// database. With connectivity set, it also checks the connectivity of the
// local repository and that the object storage holds every object the
// database references reach (the server downloads every pack of the
// repository for that). In authoritative mode the server catches the local
// repository up first.
func (r *Repo) StorageVerify(ctx context.Context, connectivity bool) (*StorageVerify, error) {
	resp, err := r.storageCore().VerifyRepository(ctx, &gitalypb.VerifyRepositoryRequest{
		Repository:        r.repository(),
		CheckConnectivity: connectivity,
	})
	if err != nil {
		return nil, err
	}

	out := &StorageVerify{
		HeadSequence: resp.GetHeadSequence(),
		// Never nil, so that the JSON form lists no divergences as [].
		Divergences:       make([]StorageDivergence, 0, len(resp.GetDivergences())),
		ConnectivityOK:    resp.GetConnectivityOk(),
		ConnectivityError: resp.GetConnectivityError(),
	}
	for _, d := range resp.GetDivergences() {
		out.Divergences = append(out.Divergences, StorageDivergence{
			Reference:      string(d.GetReference()),
			LocalTarget:    d.GetLocalTarget(),
			DatabaseTarget: d.GetDatabaseTarget(),
		})
	}
	return out, nil
}

// StorageCheckpoint is the outcome of checkpointing a repository.
type StorageCheckpoint struct {
	// Sequence is the changeset sequence the checkpoint is at; 0 when the
	// repository has no object to consolidate (every reference is symbolic).
	Sequence uint64 `json:"sequence"`
	// Created is false when a checkpoint at the head existed already.
	Created bool `json:"created"`
	// PackBytes is the size of the checkpoint's pack.
	PackBytes int64 `json:"pack_bytes"`
	// References is the number of references in the checkpoint's snapshot.
	References uint32 `json:"references"`
}

// StorageGC is what garbage collection of one repository deleted, or would
// delete in a dry run.
type StorageGC struct {
	// Boundary is the sequence of the newest checkpoint at least gc_grace old,
	// 0 when there is none.
	Boundary uint64 `json:"boundary"`
	// CoveredDeleted are the changeset packs covered by the boundary.
	CoveredDeleted uint32 `json:"covered_deleted"`
	// CheckpointsDropped are the checkpoints superseded by the boundary.
	CheckpointsDropped uint32 `json:"checkpoints_dropped"`
	// OrphansDeleted are the unreferenced objects observed at least gc_grace ago.
	OrphansDeleted uint32 `json:"orphans_deleted"`
	// OrphansObserved are the unreferenced objects seen for the first time.
	OrphansObserved uint32 `json:"orphans_observed"`
	// OrphansInGrace are the unreferenced objects observed less than gc_grace ago.
	OrphansInGrace uint32 `json:"orphans_in_grace"`
	// PacksDeleted counts every object deleted.
	PacksDeleted uint32 `json:"packs_deleted"`
	// BytesReclaimed is the size of those objects.
	BytesReclaimed int64 `json:"bytes_reclaimed"`
	// DryRun tells that nothing was deleted or recorded.
	DryRun bool `json:"dry_run"`
}

// StorageGCRemoved is what garbage collection of a storage's removed
// repositories deleted, or would delete in a dry run.
type StorageGCRemoved struct {
	// Repositories is the number of removed repositories whose objects were
	// deleted.
	Repositories uint32 `json:"repositories"`
	// PacksDeleted counts every object deleted.
	PacksDeleted uint32 `json:"packs_deleted"`
	// BytesReclaimed is the size of those objects.
	BytesReclaimed int64 `json:"bytes_reclaimed"`
	// DryRun tells that nothing was deleted or recorded.
	DryRun bool `json:"dry_run"`
	// LeaksFound is the number of repository IDs the reconciliation found with
	// objects in the bucket and neither a row nor a tombstone.
	LeaksFound uint32 `json:"leaks_found"`
	// LeaksTombstoned is the number of tombstones it recorded for them; their
	// objects are deleted once the tombstone is gc_grace old.
	LeaksTombstoned uint32 `json:"leaks_tombstoned"`
	// IDsAheadOfAllocator is the number of repository IDs above the
	// database's allocator that have objects in the bucket (a database
	// restored from an older backup); they are never touched.
	IDsAheadOfAllocator uint32 `json:"ids_ahead_of_allocator"`
}

// StorageCheckpoint checkpoints the repository at its current database head:
// one pack of every object reachable from its references, with the reference
// snapshot, so that hydration starts from it and garbage collection can delete
// the packs it covers. It fails with FailedPrecondition outside authoritative
// mode.
func (r *Repo) StorageCheckpoint(ctx context.Context) (*StorageCheckpoint, error) {
	resp, err := r.storageCore().CreateCheckpoint(ctx, &gitalypb.CreateCheckpointRequest{
		Repository: r.repository(),
	})
	if err != nil {
		return nil, err
	}
	return &StorageCheckpoint{
		Sequence:   resp.GetCheckpointSequence(),
		Created:    resp.GetCreated(),
		PackBytes:  resp.GetPackBytes(),
		References: resp.GetReferences(),
	}, nil
}

// StorageDeleteConflictingRef deletes one reference of a pair that git cannot
// hold together (refs/heads/a and refs/heads/a/b), which is the way out for a
// repository whose database an older version left with such a pair, and returns
// the sequence of the changeset. expectedTarget is the value the database must
// hold: StorageVerify's error names both. It fails with FailedPrecondition
// outside authoritative mode, when the reference is not part of such a pair, or
// when it has another value.
func (r *Repo) StorageDeleteConflictingRef(ctx context.Context, reference, expectedTarget string) (uint64, error) {
	resp, err := r.storageCore().DeleteConflictingReference(ctx, &gitalypb.DeleteConflictingReferenceRequest{
		Repository:     r.repository(),
		Reference:      []byte(reference),
		ExpectedTarget: expectedTarget,
	})
	if err != nil {
		return 0, err
	}
	return resp.GetSequence(), nil
}

// StorageGC deletes the packs of the repository that no reader needs any more
// (with dryRun, only reports them). It touches no other repository. It fails
// with FailedPrecondition when the storage core is off or another maintenance
// of the repository is in progress.
func (r *Repo) StorageGC(ctx context.Context, dryRun bool) (*StorageGC, error) {
	resp, err := r.storageCore().CollectGarbage(ctx, &gitalypb.CollectGarbageRequest{
		Repository: r.repository(),
		DryRun:     dryRun,
	})
	if err != nil {
		return nil, err
	}
	return &StorageGC{
		Boundary:           resp.GetBoundary(),
		CoveredDeleted:     resp.GetCoveredDeleted(),
		CheckpointsDropped: resp.GetCheckpointsDropped(),
		OrphansDeleted:     resp.GetOrphansDeleted(),
		OrphansObserved:    resp.GetOrphansObserved(),
		OrphansInGrace:     resp.GetOrphansInGrace(),
		PacksDeleted:       resp.GetPacksDeleted(),
		BytesReclaimed:     resp.GetBytesReclaimed(),
		DryRun:             resp.GetDryRun(),
	}, nil
}

// StorageGCRemoved deletes every object of the repositories of the storage
// that were removed at least gc_grace ago (with dryRun, only reports them),
// after it reconciled the bucket (a repository ID with objects and neither a
// row nor a tombstone gets a tombstone). An
// empty storage is the client's storage (WithStorage). It fails with
// FailedPrecondition when the storage core is off.
func (c *Client) StorageGCRemoved(ctx context.Context, storage string, dryRun bool) (*StorageGCRemoved, error) {
	if storage == "" {
		storage = c.storage
	}
	resp, err := gitalypb.NewStorageCoreServiceClient(c.conn).CollectRemovedRepositories(ctx, &gitalypb.CollectRemovedRepositoriesRequest{
		StorageName: storage,
		DryRun:      dryRun,
	})
	if err != nil {
		return nil, err
	}
	return &StorageGCRemoved{
		Repositories:        resp.GetRepositories(),
		PacksDeleted:        resp.GetPacksDeleted(),
		BytesReclaimed:      resp.GetBytesReclaimed(),
		DryRun:              resp.GetDryRun(),
		LeaksFound:          resp.GetLeaksFound(),
		LeaksTombstoned:     resp.GetLeaksTombstoned(),
		IDsAheadOfAllocator: resp.GetIdsAheadOfAllocator(),
	}, nil
}

// storageCore returns the storage core service client on the connection of the client.
func (r *Repo) storageCore() gitalypb.StorageCoreServiceClient {
	return gitalypb.NewStorageCoreServiceClient(r.client.conn)
}
