package storagecore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	reviewstore "github.com/feder-cr/git-ai/internal/graft/review/store"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// importOperation is the operation of the changeset that imports a repository.
const importOperation = "import"

// resumeHint ends the error of an import that failed after the registration:
// nothing is rolled back, and the next import resumes the registration.
const resumeHint = "the import is not complete; run ImportRepository again to resume it"

// ImportRepository implements gitalypb.StorageCoreServiceServer, following the
// IMPORT RULE of doc/graft/PLAN6.md.
//
// The pack key contains the repository ID, which only RegisterRepository
// allocates, and a changeset must never reference a pack that is not uploaded
// (RULE 3), so the import is not one transaction. A tracked repository with
// head sequence 0 is an import that is not complete (no other path registers a
// repository without changeset 1):
//
//  1. An untracked repository is registered with its changelog history and its
//     reviews, and no changeset. A registration with head sequence 0 is resumed
//     under its ID (its history and reviews are already recorded). A tracked
//     repository whose import is complete gets the history written since then
//     instead (importHistory), and the import ends there.
//  2. The references are read and the pack of every object they reach is
//     built from exactly those values (buildImportPack) and uploaded under the
//     repository ID.
//  3. Changeset 1 records every reference (each expected absent), with the pack.
//     A conflict means that a concurrent import of the same registration
//     committed first: AlreadyExists.
//  4. The local watermark becomes (ID, 1).
//
// A failure after step 1 rolls nothing back and deletes nothing: the
// registration stays at head sequence 0, which the syncer and the voter treat
// as an incomplete import (they never touch the local repository, the only copy
// of what is being imported, and refuse writes in authoritative mode), and the
// error tells the operator to run ImportRepository again, which resumes it.
func (s *server) ImportRepository(ctx context.Context, req *gitalypb.ImportRepositoryRequest) (*gitalypb.ImportRepositoryResponse, error) {
	core, err := s.enabledCore()
	if err != nil {
		return nil, err
	}

	repo := req.GetRepository()
	if err := s.validateRepository(ctx, repo); err != nil {
		return nil, err
	}
	// An import makes the local copy the database's truth. One whose references
	// may have lost writes is never imported (the gate does not see this RPC).
	path, err := s.deps.GetLocator().GetRepoPath(ctx, repo)
	if err != nil {
		return nil, err
	}
	if core.RefDurability.Untrusted(path) {
		return nil, structerr.NewUnavailable("storage core: the local references of the repository may have lost writes; it cannot be imported until it is rebuilt")
	}

	local := s.localRepo(repo)
	backend, err := local.ReferenceBackend(ctx)
	if err != nil {
		return nil, structerr.NewInternal("detect reference backend: %w", err)
	}
	if backend.Name == git.ReferenceBackendMVCC.Name {
		return nil, structerr.NewFailedPrecondition("a repository with the MVCC reference backend cannot be imported into the storage core")
	}

	repoPath, err := local.Path(ctx)
	if err != nil {
		return nil, err
	}
	objectHash, err := local.ObjectHash(ctx)
	if err != nil {
		return nil, structerr.NewInternal("detect object format: %w", err)
	}

	key := corestore.KeyOf(repo)

	// Only one import of a repository runs at a time in this process, so that
	// concurrent requests do not all build and upload the same pack. Imports in
	// other processes are safe without it (step 3).
	release, ok := acquireImport(core, key)
	if !ok {
		return nil, structerr.NewAlreadyExists("an import of the repository into the storage core is already in progress")
	}
	defer release()

	// A repository with a name over the limit cannot be imported, so it is refused before anything
	// is registered or uploaded: an incomplete registration holds back the writes that would clean
	// it up. A complete import only brings history, and leaves the references alone.
	if existing, err := core.Meta.GetRepository(ctx, key); err != nil || existing.HeadSeq == 0 {
		refs, err := gitops.ReadRefs(ctx, local)
		if err != nil {
			return nil, structerr.NewInternal("read references: %w", err)
		}
		if err := refuseLongReferenceNames(refs); err != nil {
			return nil, err
		}
	}

	// Step 1.
	reg, err := s.registerImport(ctx, core, key, repoPath, objectHash.Format)
	if err != nil {
		return nil, err
	}
	logger := s.deps.GetLogger().WithFields(log.Fields{
		"repository":    key.String(),
		"repository_id": reg.repo.ID,
	})
	if reg.complete {
		// The history is only taken from the local copy that was imported into
		// this registration (or caught up from it): its watermark names the
		// repository ID. Another local repository at the same path, for example
		// one left behind when the repository was removed and created again on
		// another node, has history that does not belong to the registration.
		w, ok, err := gitops.ReadWatermark(repoPath)
		if err != nil {
			return nil, structerr.NewInternal("read watermark: %w", err)
		}
		if !ok || w.RepoID != reg.repo.ID {
			return nil, errAlreadyImported()
		}
		return s.importHistory(ctx, core, reg.repo, repoPath, logger)
	}
	if reg.resumed {
		logger.Info("storage core: resuming an incomplete import")
	}

	// Step 2. The pack is built from the object IDs of the references that
	// were read, not from the references as they are when it is built: in
	// shadow mode writes run during the import, and the pack must hold every
	// object reachable from the values that changeset 1 records.
	tmpDir, err := s.deps.GetLocator().TempDir(repo.GetStorageName())
	if err != nil {
		return nil, structerr.NewInternal("temporary directory: %w; %s", err, resumeHint)
	}
	if err := os.MkdirAll(tmpDir, mode.Directory); err != nil {
		return nil, structerr.NewInternal("create temporary directory: %w; %s", err, resumeHint)
	}
	refs, err := gitops.ReadRefs(ctx, local)
	if err != nil {
		return nil, structerr.NewInternal("read references: %w; %s", err, resumeHint)
	}
	if err := refuseLongReferenceNames(refs); err != nil {
		return nil, err
	}
	pack, err := buildImportPack(ctx, local, refs, tmpDir)
	if err != nil {
		return nil, structerr.NewInternal("build pack: %w; %s", err, resumeHint)
	}
	defer func() {
		if err := pack.Remove(); err != nil {
			logger.WithError(err).Warn("storage core: removing the temporary import pack")
		}
	}()

	// The write deadline of the pack starts at a read of the record taken right
	// before the upload, so that the build above, however long, does not consume
	// it (doc/graft/PLAN7.md, PACK OWNERSHIP RULE 4).
	var upload corestore.PackUpload
	if !pack.Empty() {
		readAt := time.Now()
		record, err := core.Meta.GetRepository(ctx, key)
		if err != nil {
			if errors.Is(err, corestore.ErrRepositoryNotFound) {
				return nil, errRegistrationRemoved()
			}
			return nil, structerr.NewUnavailable("storage core: %w; %s", err, resumeHint)
		}
		if record.ID != reg.repo.ID {
			return nil, errRegistrationRemoved()
		}

		f, err := pack.Open()
		if err != nil {
			return nil, structerr.NewInternal("open pack: %w; %s", err, resumeHint)
		}
		upload, err = core.UploadPack(ctx, record, readAt, pack.SHA256, pack.Size, f)
		_ = f.Close()
		if err != nil {
			return nil, structerr.NewUnavailable("storage core: upload pack: %w; %s", err, resumeHint)
		}
	}

	// Step 3.
	updates := make([]corestore.RefUpdate, 0, len(refs))
	for _, name := range sortedNames(refs) {
		updates = append(updates, corestore.RefUpdate{
			Name:   name,
			Expect: corestore.ExpectValue,
			Old:    "",
			New:    refs[name],
		})
	}
	commitCtx, cancel, err := upload.CommitContext(ctx)
	if err != nil {
		return nil, structerr.NewUnavailable("%w; %s", err, resumeHint)
	}
	defer cancel()

	res, err := core.Meta.CommitChangeset(commitCtx, corestore.ChangesetInput{
		RepoID:       reg.repo.ID,
		Updates:      updates,
		Operation:    importOperation,
		Actor:        identity.Actor(ctx, importOperation),
		NodeID:       core.NodeID,
		PackKey:      upload.Key,
		PackBytes:    upload.Bytes,
		PackDeadline: upload.DBDeadline,
	})
	if err != nil {
		var conflict *corestore.ErrRefConflict
		var nameConflict *corestore.ErrRefNameConflict
		switch {
		case errors.As(err, &conflict):
			// Writes are refused (authoritative) or not recorded (shadow) while the
			// head sequence is 0, so only another import of the same registration
			// can have recorded references.
			return nil, errAlreadyImported()
		case errors.Is(err, corestore.ErrRepositoryNotFound):
			return nil, errRegistrationRemoved()
		case errors.As(err, &nameConflict):
			// Local git cannot normally hold such a pair, but a retry cannot fix one.
			return nil, structerr.NewFailedPrecondition("storage core: %w", err)
		default:
			return nil, structerr.NewUnavailable("storage core: commit import changeset: %w; %s", err, resumeHint)
		}
	}

	// Step 4. The local repository is the source of the import, so it holds
	// every object and reference of changeset 1. The import is complete in the
	// database: a failed write only costs a hydration, which replaces the local
	// repository with one built from the database.
	if err := gitops.WriteWatermark(repoPath, corestore.Watermark{RepoID: reg.repo.ID, Seq: res.Seq}); err != nil {
		logger.WithError(err).Error("storage core: writing the local watermark after the import failed; the next catch-up rebuilds the local repository")
	}

	// In shadow mode a write committed locally after the references were read
	// was not recorded (the voter skips an incomplete import and counts it in
	// graft_storage_untracked_writes_total). VerifyRepository reports its
	// reference until a later write records the reference; the objects of the
	// write still reach the bucket with that later write, because shadow packs
	// leave out only what the database values reach, not what the local
	// references reach. Authoritative mode refuses such writes.
	if after, err := gitops.ReadRefs(ctx, local); err == nil && !maps.Equal(refs, after) {
		logger.Warn("storage core: references changed during the import; VerifyRepository reports the difference")
	}

	return &gitalypb.ImportRepositoryResponse{
		Sequence:         res.Seq,
		References:       uint32(len(refs)),
		PackBytes:        upload.Bytes,
		ChangelogEntries: uint32(reg.changelogEntries),
		Reviews:          uint32(reg.reviews),
	}, nil
}

// registration is the database registration an import works on.
type registration struct {
	repo corestore.Repository
	// resumed is true when an incomplete import is resumed.
	resumed bool
	// complete is true when the import of the repository is complete (head
	// sequence greater than 0).
	complete bool
	// changelogEntries and reviews are the numbers of changelog entries and
	// reviews recorded with the registration.
	changelogEntries, reviews int
}

// refuseLongReferenceNames refuses the import of refs when a name or a symbolic target is over the limit
// on reference names. It counts them and quotes the beginning of the first few, for the operator who
// has to delete them. The error is permanent: retrying the import does not help.
func refuseLongReferenceNames(refs map[string]corestore.Target) error {
	const (
		quoted      = 3
		quotedBytes = 64
	)

	var long []string
	for _, name := range sortedNames(refs) {
		target := refs[name]
		if git.ValidateReferenceNameLength(name) != nil || (target.IsSymbolic() && git.ValidateReferenceNameLength(target.SymbolicName()) != nil) {
			long = append(long, name)
		}
	}
	if len(long) == 0 {
		return nil
	}

	first := make([]string, 0, quoted)
	for _, name := range long[:min(len(long), quoted)] {
		first = append(first, fmt.Sprintf("%.*q...", quotedBytes, name))
	}

	return structerr.NewFailedPrecondition(
		"%d references have a name or symbolic target over the limit of %d bytes, starting with %s: delete them before importing (doc/graft/LIMITATIONS.md)",
		len(long), git.MaxReferenceNameLength, strings.Join(first, ", "))
}

// registerImport is step 1 of the import: it registers an untracked
// repository with its changelog history and reviews, or returns the
// registration of an incomplete import (head sequence 0) to resume it, or the
// registration of a complete import (head sequence greater than 0).
func (s *server) registerImport(ctx context.Context, core *corestore.Core, key corestore.RepoKey, repoPath, format string) (registration, error) {
	existing, err := core.Meta.GetRepository(ctx, key)
	switch {
	case err == nil:
		return resumeImport(ctx, core, existing, format)
	case !errors.Is(err, corestore.ErrRepositoryNotFound):
		return registration{}, structerr.NewUnavailable("storage core: %w", err)
	}

	history, err := scanChangelog(repoPath)
	if err != nil {
		return registration{}, structerr.NewInternal("read changelog: %w", err)
	}
	reviews, err := reviewstore.Open(repoPath).List("", true, 0)
	if err != nil {
		return registration{}, structerr.NewInternal("read reviews: %w", err)
	}

	registered, _, err := core.Meta.RegisterRepository(ctx, key, format, corestore.InitialState{
		Changelog: history,
		Reviews:   reviews,
	})
	if errors.Is(err, corestore.ErrRepositoryExists) {
		// Registered concurrently (another node): resume it if it is an
		// incomplete import too.
		existing, err := core.Meta.GetRepository(ctx, key)
		if err != nil {
			return registration{}, structerr.NewUnavailable("storage core: %w", err)
		}
		return resumeImport(ctx, core, existing, format)
	}
	if err != nil {
		return registration{}, structerr.NewUnavailable("storage core: register repository: %w", err)
	}
	return registration{repo: registered, changelogEntries: len(history), reviews: len(reviews)}, nil
}

// resumeImport returns the registration of the tracked repository r to resume
// its import, or marked complete when its import is complete.
func resumeImport(ctx context.Context, core *corestore.Core, r corestore.Repository, format string) (registration, error) {
	if r.HeadSeq != 0 {
		return registration{repo: r, complete: true}, nil
	}
	if r.ObjectFormat != format {
		return registration{}, structerr.NewFailedPrecondition("the incomplete import registered object format %q, the repository uses %q",
			r.ObjectFormat, format)
	}

	entries, err := core.Meta.ScanChangelog(ctx, r.ID, 0, 0)
	if err != nil {
		return registration{}, structerr.NewUnavailable("storage core: read imported changelog: %w; %s", err, resumeHint)
	}
	reviews, err := core.Meta.ListReviews(ctx, r.ID, "", true, 0)
	if err != nil {
		return registration{}, structerr.NewUnavailable("storage core: read imported reviews: %w; %s", err, resumeHint)
	}
	return registration{repo: r, resumed: true, changelogEntries: len(entries), reviews: len(reviews)}, nil
}

// importHistory brings the history that was written into the files of the
// repository after its import into the database (doc/graft/PLAN6.md, LOCK AND
// HISTORY RULE, part B): in shadow mode the voter does not append the database
// changelog and reviews live in files. Every file changelog entry with a
// sequence greater than the database's log sequence is appended with its
// sequence, and every file review that the database does not have, or that was
// changed in the file since an earlier import, is written
// (corestore.ReviewNeedsImport). References are not touched. It is idempotent; operators run it once
// while switching to authoritative mode, with writers stopped.
func (s *server) importHistory(ctx context.Context, core *corestore.Core, r corestore.Repository, repoPath string, logger log.Logger) (*gitalypb.ImportRepositoryResponse, error) {
	var entries []changelog.Entry
	if err := changelog.Scan(repoPath, r.LogSeq, func(e changelog.Entry) (bool, error) {
		entries = append(entries, e)
		return true, nil
	}); err != nil {
		return nil, structerr.NewInternal("read changelog: %w", err)
	}

	reviews, err := reviewstore.Open(repoPath).List("", true, 0)
	if err != nil {
		return nil, structerr.NewInternal("read reviews: %w", err)
	}

	importedEntries, importedReviews, err := core.Meta.ImportHistory(ctx, r.ID, entries, reviews)
	switch {
	case errors.Is(err, corestore.ErrRepositoryNotFound):
		return nil, structerr.NewFailedPrecondition("storage core: the repository was removed from the storage core concurrently; run ImportRepository again")
	case err != nil:
		return nil, structerr.NewUnavailable("storage core: import history: %w", err)
	}

	if importedEntries > 0 || importedReviews > 0 {
		logger.WithFields(log.Fields{
			"changelog_entries": importedEntries,
			"reviews":           importedReviews,
		}).Info("storage core: imported the history written since the import")
	}

	// The response reports the current head: the references are not touched.
	current, err := core.Meta.GetRepository(ctx, r.Key)
	if err != nil {
		return nil, structerr.NewUnavailable("storage core: %w", err)
	}

	return &gitalypb.ImportRepositoryResponse{
		Sequence:         current.HeadSeq,
		ChangelogEntries: uint32(importedEntries),
		Reviews:          uint32(importedReviews),
	}, nil
}

// errAlreadyImported is the error of an import of a tracked repository.
func errAlreadyImported() error {
	return structerr.NewAlreadyExists("repository is already imported into the storage core")
}

// importKey identifies an import: the same repository key in the same storage
// core (database).
type importKey struct {
	core *corestore.Core
	key  corestore.RepoKey
}

var (
	importsMu sync.Mutex
	// imports are the imports in progress in this process. It is shared by
	// every StorageCoreService instance (Gitaly registers the services once per
	// listening socket).
	imports = map[importKey]struct{}{}
)

// acquireImport marks the import of key into core as in progress. It returns
// false when it already is; otherwise release ends it.
func acquireImport(core *corestore.Core, key corestore.RepoKey) (release func(), ok bool) {
	k := importKey{core: core, key: key}

	importsMu.Lock()
	defer importsMu.Unlock()
	if _, busy := imports[k]; busy {
		return nil, false
	}
	imports[k] = struct{}{}
	return func() {
		importsMu.Lock()
		defer importsMu.Unlock()
		delete(imports, k)
	}, true
}

// errRegistrationRemoved is the error of an import whose registration was
// removed while it ran. Not Aborted: Gitaly replaces the message of that code
// with a generic one, and this one tells the operator what to do.
func errRegistrationRemoved() error {
	return structerr.NewFailedPrecondition("storage core: the registration of the import was removed concurrently; run ImportRepository again")
}

// scanChangelog returns every entry of the file changelog of the repository,
// with its sequences as they are, so that consumers' cursors stay valid.
func scanChangelog(repoPath string) ([]changelog.Entry, error) {
	var entries []changelog.Entry
	err := changelog.Scan(repoPath, 0, func(e changelog.Entry) (bool, error) {
		entries = append(entries, e)
		return true, nil
	})
	return entries, err
}
