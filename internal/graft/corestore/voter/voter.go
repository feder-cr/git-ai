// Package voter is the write path of the storage core: it commits every
// reference transaction to the database from the Prepared phase of the
// reference-transaction hook, after uploading its pack. Owned by WP54
// (doc/graft/PLAN6.md, "Write path", binding order, and "LOCK AND HISTORY
// RULE").
//
// The hook manager (internal/gitaly/hook) calls Preparing, Prepared, Committed
// and Aborted for every reference transaction. Preparing runs before git takes
// the reference locks: it builds and uploads the pack of the transaction, so
// that only the expectations (one for-each-ref) and the database commit run
// while git holds the repository-wide reftable lock. Preparing never fails the
// transaction: when it did not prepare the pack, Prepared builds and uploads it
// itself. Authoritative mode serves reftable repositories only (REFTABLE
// RULE); shadow mode also processes files-backend repositories and skips MVCC
// ones.
package voter

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/graft/refchange"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
)

// CrashEnv names the test-only environment variable that makes the voter
// exit the process (exit code 75) at a fault point, for the crash-injection
// tests of WP61: "after_upload" (pack uploaded, changeset not committed) or
// "after_commit" (changeset committed, git not yet committed). It is read
// once by New.
const CrashEnv = "GRAFT_STORAGE_TEST_CRASH"

// FaultHook is called at the fault points of the write path with "after_upload"
// (the pack is uploaded, the changeset is not committed) or "after_commit"
// (the changeset is committed, git has not committed the transaction yet). A
// non-nil error is returned from Prepared as if the process had died at that
// point: in authoritative mode the git transaction is aborted. It is nil in
// production.
var FaultHook func(point string) error

const (
	faultAfterUpload = "after_upload"
	faultAfterCommit = "after_commit"

	// crashExitCode is the exit code of a process that died at a fault point
	// selected with CrashEnv.
	crashExitCode = 75

	// pendingTTL and maxPendingEntries bound the transactions that were
	// committed in Prepared and whose Committed or Aborted phase is awaited, and
	// the packs prepared in Preparing whose Prepared phase is awaited.
	pendingTTL        = 15 * time.Minute
	maxPendingEntries = 4096

	resultCommitted = "committed"
	resultNoOp      = "noop"
	resultConflict  = "conflict"
	resultSkipped   = "skipped"
	resultError     = "error"
)

// Voter commits reference transactions to the storage core.
type Voter struct {
	core        *corestore.Core
	logger      log.Logger
	repoFactory localrepo.Factory
	locator     storage.Locator
	applier     corestore.LocalApplier

	// crashAt is the fault point at which the process exits, from CrashEnv.
	crashAt string
	// pending holds the transactions committed in Prepared, until their
	// Committed or Aborted phase.
	pending *pendingSet[committedEntry]
	// packs holds the outcome of the Preparing phase of a transaction, until
	// its Prepared or Aborted phase, keyed by transactionKey.
	packs   *pendingSet[preparedPack]
	metrics *metrics
	// beforeAcquire is called right before a transaction with an invocation takes
	// its admission; tests set it to end the process at that point.
	beforeAcquire func()
}

// committedEntry is a transaction committed to the database in Prepared.
type committedEntry struct {
	repoID int64
	seq    uint64
	// invocation is the process of the transaction, empty when it has none.
	invocation string
}

// preparedPack is the outcome of the Preparing phase of a transaction.
type preparedPack struct {
	// ok tells that pack was built and uploaded for the repository repoID.
	// When it is false, Preparing failed and Prepared does the work itself.
	ok     bool
	repoID int64
	// pack is the uploaded pack, with an empty key when the transaction brings
	// no new objects.
	pack corestore.PackUpload
	// spent is the time Preparing took. In shadow mode it counts against the
	// shadow timeout, which is the budget of the whole transaction.
	spent time.Duration
}

// usable reports whether Prepared may commit the pack of the Preparing phase:
// its write deadline has not passed (doc/graft/PLAN7.md, PACK OWNERSHIP RULE
// 4). A pack whose deadline passed may be an orphan that garbage collection
// deletes, so Prepared uploads a new one instead.
func (p preparedPack) usable(now time.Time) bool {
	return p.pack.Key == "" || now.Before(p.pack.Deadline)
}

// New returns the voter of core. applier is told about local commits (it may
// be nil in shadow mode).
func New(core *corestore.Core, logger log.Logger, repoFactory localrepo.Factory, locator storage.Locator, applier corestore.LocalApplier) *Voter {
	return &Voter{
		core:        core,
		logger:      logger,
		repoFactory: repoFactory,
		locator:     locator,
		applier:     applier,
		crashAt:     os.Getenv(CrashEnv),
		pending:     newPendingSet[committedEntry](),
		packs:       newPendingSet[preparedPack](),
		metrics:     newMetrics(),
	}
}

// Authoritative reports whether the database is the source of truth, in
// which case the hook manager does not write the file changelog.
func (v *Voter) Authoritative() bool { return v != nil && v.core.Authoritative() }

// Preparing builds the pack of the transaction described by the hook input
// (changes) and uploads it, before git takes the reference locks
// (doc/graft/PLAN6.md, LOCK AND HISTORY RULE). The outcome is remembered for
// the Prepared phase of the same input in the same repository, which then
// only reads the expectations and commits to the database.
//
// It never fails the transaction, in either mode: on any failure, or when the
// transaction is not one the storage core records, nothing is prepared and
// Prepared does all the work itself. In shadow mode it runs under the shadow
// timeout, which is the budget of the Preparing and Prepared phases together.
//
// The pack holds the objects reachable from the new tips and not from the
// references at the time of Preparing: the local references in authoritative
// mode, where every object reachable from a local reference is in an earlier
// pack of the repository (invariant I1), and the values the database records
// in shadow mode, where writes that the storage core did not record break I1
// (see buildPackAgainst). Either way every excluded object is in an earlier
// pack, so a reference that changes before Prepared only makes the pack hold
// more objects than needed, never fewer. A pack that is never committed (the
// transaction is aborted) is an orphan in the bucket, like the pack of any
// failed write.
func (v *Voter) Preparing(ctx context.Context, payload gitcmd.HooksPayload, changes []byte) {
	if v == nil || !v.core.Enabled() || payload.Repo == nil {
		return
	}

	// Deferred first, so that it runs last: the admission is waited for after
	// the pack work, when git is about to take the reference lock.
	defer v.admit(ctx, payload, changes)

	start := time.Now()
	defer func() { v.metrics.preparing.Observe(time.Since(start).Seconds()) }()

	key := transactionKey(payload, changes)
	// remember records the outcome, so that Prepared uses the pack or, in shadow
	// mode, knows how much of the shadow timeout is left.
	remember := func(p preparedPack) {
		p.spent = time.Since(start)
		v.packs.add(key, p, time.Now())
	}

	defer func() {
		if r := recover(); r != nil {
			v.logger.WithField("panic", fmt.Sprint(r)).WarnContext(ctx, "storage core: preparing the pack panicked, the prepared phase builds it")
			remember(preparedPack{})
		}
	}()

	if v.core.Shadow() && v.core.ShadowTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, v.core.ShadowTimeout)
		defer cancel()
	}

	prepared, err := v.preparePack(ctx, payload, changes)
	if err != nil {
		v.logger.WithError(err).WarnContext(ctx, "storage core: preparing the pack failed, the prepared phase builds it")
		remember(preparedPack{})
		return
	}
	if prepared != nil {
		remember(*prepared)
	}
}

// preparePack builds and uploads the pack of a transaction in the Preparing
// phase. It returns nil without an error when there is nothing to prepare: the
// input brings no new objects, or the Prepared phase skips or rejects the
// transaction (it checks the same conditions again under the lock).
func (v *Voter) preparePack(ctx context.Context, payload gitcmd.HooksPayload, changes []byte) (*preparedPack, error) {
	hash, err := git.ObjectHashByFormat(payload.ObjectFormat)
	if err != nil {
		return nil, fmt.Errorf("object format: %w", err)
	}

	updates, err := parsePayloadInput(payload, changes, hash)
	if err != nil {
		// Prepared rejects or skips the input with the same error.
		return nil, nil
	}
	if len(newTips(updates)) == 0 {
		return nil, nil
	}

	repo := v.repoFactory.Build(payload.Repo)

	backend, err := repo.ReferenceBackend(ctx)
	if err != nil {
		return nil, err
	}
	if backend.Name == git.ReferenceBackendMVCC.Name ||
		(v.core.Authoritative() && backend.Name != git.ReferenceBackendReftables.Name) {
		return nil, nil
	}

	// The write deadline of the pack starts before this read of the record
	// (doc/graft/PLAN7.md, PACK OWNERSHIP RULE 4).
	readAt := time.Now()
	record, err := v.core.Meta.GetRepository(ctx, corestore.KeyOf(payload.Repo))
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.observeStep(updates, stepRecord, time.Since(readAt))
	if record.HeadSeq == 0 {
		return nil, nil
	}

	pack, err := v.buildAndUpload(ctx, repo, payload.Repo.GetStorageName(), record, readAt, updates, true)
	if err != nil {
		return nil, err
	}

	return &preparedPack{ok: true, repoID: record.ID, pack: pack}, nil
}

// Prepared commits the transaction described by the hook input (changes) to
// the database, following the binding order of doc/graft/PLAN6.md. meta is
// the changelog metadata of the transaction (operation, actor, members). It
// uses the pack that Preparing uploaded for the same input, if there is one.
//
// Authoritative mode: a returned error aborts the git transaction; a
// reference conflict is returned as a wrapped
// updateref.MismatchingStateError, a file/directory conflict with a reference of
// the database as a wrapped updateref.FileDirectoryConflictError, any storage
// failure as Unavailable.
// Shadow mode: it always returns nil.
func (v *Voter) Prepared(ctx context.Context, payload gitcmd.HooksPayload, changes []byte, meta changelog.Meta) error {
	if v == nil || !v.core.Enabled() {
		return nil
	}

	start := time.Now()
	defer func() { v.metrics.prepared.Observe(time.Since(start).Seconds()) }()

	// The outcome of the Preparing phase is consumed here whatever happens next,
	// so that it is never used for a later transaction.
	var prepared *preparedPack
	if payload.Repo != nil {
		if p, ok := v.packs.take(transactionKey(payload, changes), time.Now()); ok {
			prepared = &p
		}
	}

	if v.core.Authoritative() {
		return v.authoritativePrepare(ctx, payload, changes, meta, prepared)
	}

	v.shadowPrepare(ctx, payload, changes, meta, prepared)
	return nil
}

// authoritativePrepare runs the write path in authoritative mode. A panic
// aborts the git transaction instead of taking the server down.
func (v *Voter) authoritativePrepare(ctx context.Context, payload gitcmd.HooksPayload, changes []byte, meta changelog.Meta, prepared *preparedPack) (returnedErr error) {
	defer func() {
		if r := recover(); r != nil {
			v.logger.WithField("panic", fmt.Sprint(r)).ErrorContext(ctx, "storage core: the write path panicked, the git transaction is aborted")
			returnedErr = v.unavailable(fmt.Errorf("panic: %v", r))
		}
	}()

	return v.prepare(ctx, payload, changes, meta, prepared, false)
}

// shadowPrepare runs the write path in shadow mode: under what is left of the
// shadow timeout after the Preparing phase, recovering from panics, and never
// returning an error.
func (v *Voter) shadowPrepare(ctx context.Context, payload gitcmd.HooksPayload, changes []byte, meta changelog.Meta, prepared *preparedPack) {
	defer func() {
		if r := recover(); r != nil {
			v.shadowFailed(ctx, fmt.Errorf("panic: %v", r))
		}
	}()

	if v.core.ShadowTimeout > 0 {
		budget := v.core.ShadowTimeout
		if prepared != nil {
			budget -= prepared.spent
		}

		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}

	if err := v.prepare(ctx, payload, changes, meta, prepared, true); err != nil {
		v.shadowFailed(ctx, err)
	}
}

// shadowFailed logs and counts a shadow error. The git write proceeds.
func (v *Voter) shadowFailed(ctx context.Context, err error) {
	v.metrics.shadowErrors.Inc()
	v.logger.WithError(err).ErrorContext(ctx, "storage core: shadow write failed, the git write proceeds")
}

// CreatedRefConflict returns a reference of the database that git cannot hold
// next to ref, which a push creates, and "" when there is none. It applies the
// database's own refusal (MetaStore.CommitChangeset, *corestore.ErrRefNameConflict)
// to one reference before git's transaction, from the update hook, so that
// receive-pack refuses that reference alone and the other references of a
// non-atomic push still land. It is not atomic with the commit, which checks
// again. The caller decides whether the client may see the name it returns.
//
// A repository the database does not track, or whose import is not complete,
// has nothing to conflict with. A failed read is returned as Unavailable.
//
// Shadow mode never refuses a git write: git's references are the truth there,
// and the database may be behind them. A conflict, or a failed read, is logged and
// counted as a shadow error, and "" is returned. The commit of the changeset
// then refuses to record the name (forced or not), so the database never holds a
// pair, and verification reports what it misses.
func (v *Voter) CreatedRefConflict(ctx context.Context, repo *gitalypb.Repository, ref string) (string, error) {
	if v == nil || !v.core.Enabled() || repo == nil {
		return "", nil
	}

	existing, err := v.createdRefConflict(ctx, repo, ref)
	if !v.core.Authoritative() {
		switch {
		case err != nil:
			v.shadowFailed(ctx, fmt.Errorf("check the name of a created reference: %w", err))
		case existing != "":
			v.metrics.shadowErrors.Inc()
			v.logger.WithFields(log.Fields{"reference": ref, "conflicts_with": existing}).WarnContext(ctx,
				"storage core: shadow write creates a reference git cannot hold next to one of the database, the git write proceeds")
		}
		return "", nil
	}
	if err != nil {
		return "", v.unavailable(err)
	}
	return existing, nil
}

func (v *Voter) createdRefConflict(ctx context.Context, repo *gitalypb.Repository, ref string) (string, error) {
	record, err := v.core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if record.HeadSeq == 0 {
		return "", nil
	}

	existing, found, err := v.core.Meta.ConflictingRef(ctx, record.ID, ref)
	if err != nil || !found {
		return "", err
	}
	return existing, nil
}

// prepare is the binding write path of doc/graft/PLAN6.md. In shadow mode the
// changeset is forced: it is recorded even when a reference does not match.
// prepared is the outcome of the Preparing phase of the transaction, nil when
// there was none.
func (v *Voter) prepare(ctx context.Context, payload gitcmd.HooksPayload, changes []byte, meta changelog.Meta, prepared *preparedPack, shadow bool) (err error) {
	// result is the label of graft_storage_changesets_total. Every path that
	// does not set it, including a panic, counts as an error.
	result := ""
	defer func() {
		if result == "" {
			result = resultError
		}
		v.metrics.changesets.WithLabelValues(string(v.core.Mode), result).Inc()
	}()

	if payload.Repo == nil {
		return errors.New("storage core: the hooks payload has no repository")
	}

	hash, err := git.ObjectHashByFormat(payload.ObjectFormat)
	if err != nil {
		return fmt.Errorf("storage core: object format: %w", err)
	}

	updates, err := parsePayloadInput(payload, changes, hash)
	if err != nil {
		return fmt.Errorf("storage core: malformed reference transaction input: %w", err)
	}
	if len(updates) == 0 {
		result = resultSkipped
		return nil
	}

	repo := v.repoFactory.Build(payload.Repo)

	backend, err := repo.ReferenceBackend(ctx)
	if err != nil {
		return v.unavailable(err)
	}

	// The REFTABLE RULE of doc/graft/PLAN6.md. The files backend deletes a
	// packed reference with two transactions whose hook input can be identical,
	// so only reftable repositories are served in authoritative mode. Shadow
	// mode processes every transaction of the files backend: a forced deletion
	// is idempotent, and a checked deletion that follows the packed-refs half is
	// not counted as a divergence (see divergences).
	if !shadow && backend.Name != git.ReferenceBackendReftables.Name {
		return structerr.NewFailedPrecondition("storage core: authoritative storage mode requires a reftable repository").
			WithMetadata("reference_backend", backend.Name)
	}
	if backend.Name == git.ReferenceBackendMVCC.Name {
		// Shadow mode only: authoritative mode rejects the repository above.
		v.metrics.mvccSkipped.Inc()
		result = resultSkipped
		return nil
	}

	// The write deadline of a pack uploaded here starts before this read of the
	// record (doc/graft/PLAN7.md, PACK OWNERSHIP RULE 4).
	readAt := time.Now()
	record, err := v.core.Meta.GetRepository(ctx, corestore.KeyOf(payload.Repo))
	if err == nil {
		v.observePreparedStep(updates, stepRecord, time.Since(readAt))
	}
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		if shadow {
			v.metrics.untracked.Inc()
			result = resultSkipped
			return nil
		}
		return structerr.NewFailedPrecondition("storage core: repository is not imported into the storage core")
	}
	if errors.Is(err, corestore.ErrReviewRootInUse) {
		// Only non-forced (authoritative) changesets are refused.
		result = resultConflict
		return structerr.NewFailedPrecondition("%w", err)
	}
	if err != nil {
		return v.unavailable(err)
	}
	// The IMPORT RULE of doc/graft/PLAN6.md: head sequence 0 is an import that
	// is not complete (only an import registers a repository without changeset
	// 1). Its import changeset expects every reference to be absent, so a write
	// recorded now would make the import fail forever: the repository is
	// treated as untracked until the import is resumed and completed.
	if record.HeadSeq == 0 {
		if shadow {
			// The objects of the write are not lost to the bucket: later shadow
			// packs are built against the database values, not the local
			// references (see buildPackAgainst). The reference value itself is
			// missing from the database until a later write records it, which
			// VerifyRepository reports.
			v.metrics.untracked.Inc()
			v.logger.WithField("repo_id", record.ID).WarnContext(ctx,
				"storage core: shadow write to a repository whose import is not complete is not recorded")
			result = resultSkipped
			return nil
		}
		return structerr.NewFailedPrecondition("storage core: repository import is not complete; run ImportRepository again")
	}

	// The local_read step covers the read of the local values and the check of the
	// old values against them (rejectedByGit), in expectations.
	localRead := time.Now()
	refUpdates, err := v.expectations(ctx, repo, hash, updates, payload.IsHookRequested(gitcmd.PreReceiveHook))
	if err == nil {
		v.observePreparedStep(updates, stepLocalRead, time.Since(localRead))
	}
	if err != nil {
		var (
			exists   updateref.ReferenceAlreadyExistsError
			mismatch updateref.MismatchingStateError
		)
		if errors.As(err, &exists) || errors.As(err, &mismatch) {
			result = resultConflict
		}
		return err
	}

	// The pack prepared before git took the locks is used when it was uploaded
	// for this repository (not for a removed one that had the same path) and its
	// write deadline has not passed. Otherwise it is built and uploaded now, as
	// before the LOCK AND HISTORY RULE.
	var pack corestore.PackUpload
	if prepared != nil && prepared.ok && prepared.repoID == record.ID && prepared.usable(time.Now()) {
		pack = prepared.pack
	} else {
		built := time.Now()
		pack, err = v.buildAndUpload(ctx, repo, payload.Repo.GetStorageName(), record, readAt, updates, false)
		if err != nil {
			return v.unavailable(err)
		}
		// Like the Preparing steps, a transaction without new tips builds no pack.
		if len(newTips(updates)) > 0 {
			v.observePreparedStep(updates, stepPack, time.Since(built))
		}
	}

	in := corestore.ChangesetInput{
		RepoID:       record.ID,
		Updates:      refUpdates,
		Operation:    meta.Operation,
		Actor:        meta.Actor,
		NodeID:       v.core.NodeID,
		Force:        shadow,
		PackKey:      pack.Key,
		PackBytes:    pack.Bytes,
		PackDeadline: pack.DBDeadline,
	}

	if err := v.fault(faultAfterUpload); err != nil {
		return fmt.Errorf("storage core: %w", err)
	}

	if !shadow {
		metaCopy := meta
		in.Changelog = &metaCopy
	}

	// The changeset that references the pack commits before the pack's write
	// deadline, or not at all.
	commitCtx, cancel, err := pack.CommitContext(ctx)
	if err != nil {
		return v.unavailable(err)
	}
	defer cancel()

	committing := time.Now()
	res, err := v.core.Meta.CommitChangeset(commitCtx, in)
	if err == nil {
		v.observePreparedStep(updates, stepCommit, time.Since(committing))
	}

	var conflict *corestore.ErrRefConflict
	if errors.As(err, &conflict) {
		result = resultConflict
		return fmt.Errorf("storage core: %w", mismatchError(conflict, hash))
	}
	// git checked the name against the local references only, which can be behind
	// the database: this is the file/directory conflict git would have reported
	// had it seen the other node's reference.
	var nameConflict *corestore.ErrRefNameConflict
	if errors.As(err, &nameConflict) {
		result = resultConflict
		return fmt.Errorf("storage core: %w", updateref.FileDirectoryConflictError{
			ConflictingReferenceName: nameConflict.Name,
			ExistingReferenceName:    nameConflict.Existing,
		})
	}
	if errors.Is(err, corestore.ErrRepositoryNotFound) {
		if shadow {
			v.metrics.untracked.Inc()
			result = resultSkipped
			return nil
		}
		return structerr.NewFailedPrecondition("storage core: repository is not imported into the storage core")
	}
	if err != nil {
		return v.unavailable(err)
	}

	// In shadow mode a mismatch is a divergence between git and the database,
	// also when the database already holds the new values (NoOp).
	diverging := divergences(res.Conflicts, updates)
	if shadow && len(diverging) > 0 {
		v.metrics.divergences.Inc()
		v.logger.WithFields(log.Fields{
			"repo_id":   record.ID,
			"reference": diverging[0].Name,
			"expected":  string(diverging[0].Expected),
			"actual":    string(diverging[0].Actual),
			"conflicts": len(diverging),
		}).WarnContext(ctx, "storage core: shadow changeset diverges from the database")
	}

	if res.NoOp {
		// git still writes the values the database already holds.
		if err := v.observedDeferDurability(ctx, payload, changes, shadow, updates); err != nil {
			return v.unavailable(err)
		}
		result = resultNoOp
		return nil
	}

	if len(res.Changelog) > 0 {
		corestore.NotifyChangelog(record.ID)
	}

	// The entry is remembered before the fault point, so that the Aborted phase
	// of a transaction that fails after the database commit is recognized.
	v.pending.add(pendingKey(payload.Repo, changes), committedEntry{
		repoID:     record.ID,
		seq:        res.Seq,
		invocation: payload.Changelog.Invocation,
	}, time.Now())
	// A process that ended during this phase (git was killed) reports nothing more, and its end
	// did not find this entry.
	if invocation := payload.Changelog.Invocation; invocation != "" && !gitcmd.InvocationLive(invocation) {
		v.forgetCommitted(invocation)
	}

	// After the entry, so that a failure here counts as a local failure in the
	// Aborted phase: the database holds the changeset, catch-up repairs the
	// local repository.
	if err := v.observedDeferDurability(ctx, payload, changes, shadow, updates); err != nil {
		return v.unavailable(err)
	}

	if err := v.fault(faultAfterCommit); err != nil {
		return fmt.Errorf("storage core: %w", err)
	}

	if res.Forced && len(diverging) > 0 {
		// The changeset was recorded despite the mismatch (shadow mode).
		result = resultConflict
		return nil
	}

	result = resultCommitted
	return nil
}

// expectations returns the compare-and-swap condition of every update, as in
// the table of doc/graft/PLAN6.md (RULE 5). The local references are read in
// one git process: git holds their locks in the Prepared phase, so they still
// show the old values.
//
// oldValueAlwaysSent tells that the writer always gives git an old value:
// receive-pack and UpdaterWithHooks, the only writers that request the pre-receive
// hook. git checked every such old value against the local reference under the
// lock, which it still holds, so a local value that differs from it means git
// rejected the update: receive-pack without --atomic lets one update fail and still
// feeds it to this hook, without marking it (rejectedByGit). The database may hold
// that old value nonetheless (the local copy is behind it), so such an update must
// never be recorded, and a zero old value must never be taken for a forced update.
// The whole transaction is refused, because the voter does not know the other
// rejected updates of the input.
func (v *Voter) expectations(ctx context.Context, repo *localrepo.Repo, hash git.ObjectHash, updates []inputUpdate, oldValueAlwaysSent bool) ([]corestore.RefUpdate, error) {
	names := make([]string, len(updates))
	for i, u := range updates {
		names[i] = u.name
	}

	local, err := gitops.ReadRefValues(ctx, repo, names)
	if err != nil {
		return nil, v.unavailable(err)
	}

	// ReadRefValues leaves out broken references, among them a symbolic
	// reference whose target does not exist (a dangling refs/remotes/origin/HEAD,
	// for example). Such a reference exists, and git reports the zero value as
	// its old value. It can only be the old side of an update that makes it
	// symbolic or deletes it, so only those updates are checked, one
	// symbolic-ref run each. HEAD is read with symbolic-ref already.
	checked := make(map[string]bool)
	for _, u := range updates {
		if !u.oldZero || u.name == "HEAD" || checked[u.name] || (!u.newZero && !u.new.IsSymbolic()) {
			continue
		}
		if _, ok := local[u.name]; ok {
			continue
		}
		checked[u.name] = true

		symbolic, err := isSymbolicRef(ctx, repo, u.name)
		if err != nil {
			return nil, v.unavailable(err)
		}
		if symbolic {
			// The value is not needed: an existing local reference only makes the
			// update forced.
			local[u.name] = ""
		}
	}

	refUpdates := make([]corestore.RefUpdate, 0, len(updates))
	for _, u := range updates {
		if oldValueAlwaysSent {
			if err := rejectedByGit(u, local, hash); err != nil {
				return nil, fmt.Errorf("storage core: %w", err)
			}
		}

		ru := corestore.RefUpdate{Name: u.name, New: u.new}
		if u.oldZero {
			// Git reports the zero value both for "must not exist" and for a forced
			// update. The local reference tells them apart.
			if _, exists := local[u.name]; exists {
				ru.Expect = corestore.ExpectAny
			} else {
				ru.Expect = corestore.ExpectValue
			}
		} else {
			ru.Expect = corestore.ExpectValue
			ru.Old = u.old
		}
		refUpdates = append(refUpdates, ru)
	}

	return refUpdates, nil
}

// rejectedByGit returns the error of an update with an old value that git checked
// under the reference lock and that the local reference (local, read under that
// lock) does not have: git rejected it. A zero old value means the reference must
// not exist (updateref.ReferenceAlreadyExistsError when it does); any other old
// value must be the local value (updateref.MismatchingStateError otherwise, with
// the zero object ID for an absent reference). It returns nil for an update git
// accepted.
func rejectedByGit(u inputUpdate, local map[string]corestore.Target, hash git.ObjectHash) error {
	actual, exists := local[u.name]
	if u.oldZero {
		if exists {
			return updateref.ReferenceAlreadyExistsError{ReferenceName: u.name}
		}
		return nil
	}
	if exists && actual == u.old {
		return nil
	}

	zero := hash.ZeroOID.String()
	return updateref.MismatchingStateError{
		ReferenceName:    u.name,
		ExpectedObjectID: targetOrZero(u.old, zero),
		ActualObjectID:   targetOrZero(actual, zero),
	}
}

// isSymbolicRef reports whether name is a symbolic reference, also when its
// target does not exist. name is a validated reference name, so it is never
// read as an option.
func isSymbolicRef(ctx context.Context, repo *localrepo.Repo, name string) (bool, error) {
	var stderr bytes.Buffer
	err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "symbolic-ref",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--quiet"}},
			Args:  []string{name},
		},
		// symbolic-ref is a reference-updating command for git, but this call only reads.
		gitcmd.WithDisabledHooks(),
		gitcmd.WithStdout(io.Discard),
		gitcmd.WithStderr(&stderr),
	)
	if err == nil {
		return true, nil
	}

	// git-symbolic-ref(1) with --quiet exits with 1 when the reference is not
	// symbolic or does not exist.
	if code, ok := command.ExitStatus(err); ok && code == 1 {
		return false, nil
	}
	return false, structerr.New("symbolic-ref: %w", err).WithMetadata("stderr", stderr.String())
}

// newTips returns the new object IDs of the updates, the tips whose objects
// the pack of the transaction holds.
func newTips(updates []inputUpdate) []git.ObjectID {
	tips := make([]git.ObjectID, 0, len(updates))
	for _, u := range updates {
		if u.new != "" && !u.new.IsSymbolic() {
			tips = append(tips, git.ObjectID(u.new))
		}
	}
	return tips
}

// buildAndUpload builds the pack of the objects reachable from the new tips of
// updates and not from what is already in the bucket, and uploads it under a
// new owned key of repoID with the write deadline that starts at readAt, the
// time taken before the read of the repository record (Core.UploadPack). An
// empty pack is not uploaded.
//
// What is already in the bucket: in authoritative mode, everything any current
// local reference reaches (invariant I1, gitops.BuildPack); in shadow mode,
// everything that the values the database records reach (buildPackAgainst),
// because local references can hold values the storage core never recorded.
//
// preparing tells that the Preparing phase builds the pack: its steps are then
// observed (observeStep).
func (v *Voter) buildAndUpload(ctx context.Context, repo *localrepo.Repo, storageName string, record corestore.Repository, readAt time.Time, updates []inputUpdate, preparing bool) (corestore.PackUpload, error) {
	repoID := record.ID
	tips := newTips(updates)
	if len(tips) == 0 {
		return corestore.PackUpload{}, nil
	}

	tmpDir, err := v.locator.TempDir(storageName)
	if err != nil {
		return corestore.PackUpload{}, err
	}
	// The server creates the temporary directory when it starts, but it may have
	// been cleaned up since.
	if err := os.MkdirAll(tmpDir, mode.Directory); err != nil {
		return corestore.PackUpload{}, fmt.Errorf("creating the temporary directory: %w", err)
	}

	var pack gitops.Pack
	if v.core.Authoritative() {
		built := time.Now()
		pack, err = gitops.BuildPack(ctx, repo, tips, tmpDir)
		if err == nil && preparing {
			v.observeStep(updates, stepWalk, pack.Walk)
			if !pack.Empty() {
				v.observeStep(updates, stepPack, time.Since(built)-pack.Walk)
			}
		}
	} else {
		var known map[string]corestore.Target
		known, _, err = v.core.Meta.Refs(ctx, repoID)
		if err != nil {
			return corestore.PackUpload{}, fmt.Errorf("reading the database references: %w", err)
		}
		pack, err = buildPackAgainst(ctx, repo, tips, known, tmpDir)
	}
	if err != nil {
		return corestore.PackUpload{}, err
	}
	defer func() { _ = pack.Remove() }()

	if pack.Empty() {
		return corestore.PackUpload{}, nil
	}

	r, err := pack.Open()
	if err != nil {
		return corestore.PackUpload{}, err
	}
	defer func() { _ = r.Close() }()

	uploading := time.Now()
	upload, err := v.core.UploadPack(ctx, record, readAt, pack.SHA256, pack.Size, r)
	if err != nil {
		return corestore.PackUpload{}, err
	}
	if preparing {
		v.observeStep(updates, stepUpload, time.Since(uploading))
	}
	v.metrics.packBytes.Add(float64(pack.Size))

	return upload, nil
}

// The steps of the Preparing phase that graft_storage_preparing_step_seconds
// observes (doc/graft/STORAGE.md).
const (
	// stepRecord is the read of the repository record.
	stepRecord = "record"
	// stepWalk is the walk of the objects that the pack holds, until its object
	// list ended (gitops.Pack.Walk). It is observed for every transaction with new
	// tips, an empty walk included.
	stepWalk = "walk"
	// stepPack is the packing of the walked objects into the temporary file,
	// hashed while it is written. It is observed for non-empty packs only.
	stepPack = "pack"
	// stepUpload is the upload of the pack to the bucket.
	stepUpload = "upload"
)

// The kinds of transactions that graft_storage_preparing_step_seconds tells apart.
const (
	// kindCreate is a transaction that only creates references, such as a commit
	// onto a new branch.
	kindCreate = "create"
	// kindUpdate is a transaction that changes or deletes an existing reference,
	// such as a land onto its branch.
	kindUpdate = "update"
)

// transactionKind returns the kind of the transaction of updates.
func transactionKind(updates []inputUpdate) string {
	for _, u := range updates {
		if !u.oldZero {
			return kindUpdate
		}
	}
	return kindCreate
}

// observeStep observes d in step of graft_storage_preparing_step_seconds, for the
// kind of the transaction of updates. Only authoritative mode observes the steps:
// the shadow mode builds its packs another way (buildPackAgainst).
func (v *Voter) observeStep(updates []inputUpdate, step string, d time.Duration) {
	if !v.core.Authoritative() {
		return
	}
	v.metrics.preparingSteps.WithLabelValues(transactionKind(updates), step).Observe(d.Seconds())
}

// The steps of the Prepared phase, while git holds the reference locks, that
// graft_storage_prepared_step_seconds observes (doc/graft/STORAGE.md), besides
// stepRecord (the read of the repository record) and stepPack (here the pack
// built and uploaded when the Preparing phase did not prepare it).
const (
	// stepLocalRead is the read of the local references that the expected
	// values need (expectations).
	stepLocalRead = "local_read"
	// stepCommit is the database commit of the changeset (CommitChangeset).
	stepCommit = "commit"
	// stepDurability is the durable dirty marker of deferred reference
	// durability (deferDurability).
	stepDurability = "durability"
)

// observePreparedStep observes d in step of graft_storage_prepared_step_seconds,
// for the kind of the transaction of updates. Like observeStep, only
// authoritative mode observes the steps, and only steps that succeeded.
func (v *Voter) observePreparedStep(updates []inputUpdate, step string, d time.Duration) {
	if !v.core.Authoritative() {
		return
	}
	v.metrics.preparedSteps.WithLabelValues(transactionKind(updates), step).Observe(d.Seconds())
}

// observedDeferDurability is deferDurability, observed as stepDurability when
// the durability is deferred (otherwise deferDurability does nothing).
func (v *Voter) observedDeferDurability(ctx context.Context, payload gitcmd.HooksPayload, changes []byte, shadow bool, updates []inputUpdate) error {
	if shadow || !v.DeferredReferenceDurability() {
		return nil
	}
	start := time.Now()
	if err := v.deferDurability(ctx, payload, changes, shadow); err != nil {
		return err
	}
	v.observePreparedStep(updates, stepDurability, time.Since(start))
	return nil
}

// fault calls FaultHook and exits the process when CrashEnv selects the point.
func (v *Voter) fault(point string) error {
	if FaultHook != nil {
		if err := FaultHook(point); err != nil {
			return err
		}
	}

	if v.crashAt != "" && v.crashAt == point {
		os.Exit(crashExitCode)
	}

	return nil
}

// unavailable wraps a storage failure, see the Prepared contract. The code is
// Unavailable even when err carries a more specific gRPC code, which
// structerr would otherwise keep.
func (v *Voter) unavailable(err error) error {
	return structerr.NewUnavailable("storage core: %w", err).WithGRPCCode(codes.Unavailable)
}

// Committed tells the voter that git committed the transaction locally.
func (v *Voter) Committed(ctx context.Context, payload gitcmd.HooksPayload, changes []byte) {
	if v == nil || payload.Repo == nil {
		return
	}

	key := pendingKey(payload.Repo, changes)
	entry, ok := v.pending.take(key, time.Now())
	if !v.localRefs() && (!ok || v.applier == nil) {
		return
	}

	repoPath, err := v.repoPath(ctx, payload)
	if err != nil {
		v.logger.WithError(err).ErrorContext(ctx, "storage core: resolving the repository path after a commit")
		return
	}

	// git finished the transaction, its auto-compaction included, and released
	// the reference lock.
	v.core.RefAdmission.Release(repoPath, localrefs.Key(transactionKey(payload, changes)))

	if !ok || v.applier == nil {
		return
	}
	v.applier.NoteCommitted(ctx, repoPath, entry.repoID, entry.seq)
}

// repoPath returns the path of the repository of the transaction. It is built
// from the storage and the relative path only, so that a quarantine of the
// hooks payload does not end up in it.
func (v *Voter) repoPath(ctx context.Context, payload gitcmd.HooksPayload) (string, error) {
	return v.locator.GetRepoPath(ctx, &gitalypb.Repository{
		StorageName:  payload.Repo.GetStorageName(),
		RelativePath: payload.Repo.GetRelativePath(),
	}, storage.WithRepositoryVerificationSkipped())
}

// localRefs reports whether the local reference writes of the node are
// admitted or their durability deferred (authoritative mode).
func (v *Voter) localRefs() bool {
	return v.core.Authoritative() && (v.core.RefAdmission != nil || v.core.RefDurability.Enabled())
}

// DeferredReferenceDurability reports whether the reference writes of the
// transactions this voter commits may run without fsync: authoritative mode,
// and this process owns the deferred durability of the node (doc/graft/PLAN7.md,
// LOCAL REFERENCE WRITES RULE). It goes from false to true at most once in a
// process.
func (v *Voter) DeferredReferenceDurability() bool {
	return v != nil && v.core.Authoritative() && v.core.RefDurability.Deferring()
}

// DeferReferenceWrites decides whether the git process that the caller is about
// to start for repo (UpdateWithHooks' update-ref) writes its references without
// fsync. When ok, the process counts as live for the repository's deferred
// durability, and the caller calls end once it has exited: until then it may
// write a table or compact the stack, whichever hooks git runs or skips.
func (v *Voter) DeferReferenceWrites(ctx context.Context, repo *gitalypb.Repository) (end func(), ok bool) {
	if !v.DeferredReferenceDurability() {
		return nil, false
	}

	repoPath, err := v.locator.GetRepoPath(ctx, &gitalypb.Repository{
		StorageName:  repo.GetStorageName(),
		RelativePath: repo.GetRelativePath(),
	}, storage.WithRepositoryVerificationSkipped())
	if err != nil {
		v.logger.WithError(err).WarnContext(ctx, "storage core: resolving the repository path of a reference write; it is made durable by git")
		return nil, false
	}
	return v.core.RefDurability.Begin(repoPath), true
}

// admit waits for the admission of the transaction to the reference lock of
// its repository (authoritative mode). It never fails the transaction: without
// an admission git waits for its lock as before.
//
// Committed or Aborted releases the admission, but git reports neither for a
// transaction whose prepare fails in git update-ref (a stale old value, a
// reference that is gone, a lock that stayed busy), which dies without aborting
// it, for an atomic push that fails the same way in git receive-pack, which reports
// the failure and exits normally, nor for a killed process. So the admission belongs
// to the process of the transaction, known by its invocation: it ends with the
// process at the latest, and it stands for no other process, also one with the same
// hook input.
//
// A process that ended while its hook was still on its way (git was killed) holds
// nothing: the hook leaves the queue, or is not admitted at all, and the writers
// behind it go on. A hook call without an invocation (a direct caller of the hook
// RPC) has no process to watch: its admission ends at the hold bound.
func (v *Voter) admit(ctx context.Context, payload gitcmd.HooksPayload, changes []byte) {
	if !v.core.Authoritative() || v.core.RefAdmission == nil {
		return
	}

	repoPath, err := v.repoPath(ctx, payload)
	if err != nil {
		v.logger.WithError(err).WarnContext(ctx, "storage core: resolving the repository path for the admission, the transaction goes on without it")
		return
	}

	key := localrefs.Key(transactionKey(payload, changes))
	invocation := payload.Changelog.Invocation
	if invocation == "" {
		v.core.RefAdmission.Acquire(ctx, repoPath, key, localrefs.WriterHook)
		return
	}

	// A process that ends while its writer waits takes the writer out of the queue.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !gitcmd.OnInvocationExit(invocation, func() {
		cancel()
		v.forgetTransaction(key, invocation)
		v.releaseUnreported(repoPath, key, invocation)
	}) {
		v.forgetTransaction(key, invocation)
		return
	}

	if v.beforeAcquire != nil {
		v.beforeAcquire()
	}

	// An end of the process that came before the writer held the admission released
	// nothing. Such an end has made the invocation not live, and a later one finds
	// the admission.
	if v.core.RefAdmission.Acquire(ctx, repoPath, key, localrefs.WriterHook) && !gitcmd.InvocationLive(invocation) {
		v.releaseUnreported(repoPath, key, invocation)
	}
}

// TransactionStarts is called first thing in the Preparing phase of a transaction,
// before anything that can skip the rest of that phase (a vote, the access policy).
// In its process the transaction replaces the earlier one (see transactionKey), of
// which git reported no end if anything of it is left (git fetch --prune goes on
// after a prune whose prepare failed): its admission is released, so that writers
// do not wait on it during the work of this transaction, nor this one behind itself;
// and its pack is dropped, as it does not hold the objects of this transaction.
func (v *Voter) TransactionStarts(ctx context.Context, payload gitcmd.HooksPayload) {
	if v == nil || !v.core.Enabled() || payload.Repo == nil || payload.Changelog.Invocation == "" {
		return
	}

	key := transactionKey(payload, nil)
	v.packs.take(key, time.Now())

	if !v.core.Authoritative() || v.core.RefAdmission == nil {
		return
	}

	repoPath, err := v.repoPath(ctx, payload)
	if err != nil {
		v.logger.WithError(err).WarnContext(ctx, "storage core: resolving the repository path at the start of a transaction, an earlier admission ends with its process")
		return
	}
	v.releaseUnreported(repoPath, localrefs.Key(key), payload.Changelog.Invocation)
}

// forgetTransaction drops what the voter keeps between the phases of a transaction whose process
// ended: no later phase comes for it. An uploaded pack stays in the bucket as an orphan, like the pack
// of any failed write.
func (v *Voter) forgetTransaction(key localrefs.Key, invocation string) {
	v.packs.take(key, time.Now())
	v.forgetCommitted(invocation)
}

// forgetCommitted drops the transactions of a process that ended which the database committed and of
// which git reported no end. git may or may not have committed them: the catch-up repairs the local
// repository if it did not. An entry of the same hook input from another process is that process's.
func (v *Voter) forgetCommitted(invocation string) {
	for _, entry := range v.pending.removeIf(func(e committedEntry) bool { return e.invocation == invocation }) {
		v.logger.WithFields(log.Fields{
			"repo_id":    entry.repoID,
			"seq":        entry.seq,
			"invocation": invocation,
		}).Warn("storage core: git ended without reporting a transaction that the database committed, the next catch-up repairs the local repository if it is behind")
	}
}

// releaseUnreported releases the admission of a transaction that ended without a
// report from git, if it still holds it, and says so.
func (v *Voter) releaseUnreported(repoPath string, key localrefs.Key, invocation string) {
	if v.core.RefAdmission.ReleaseUnreported(repoPath, key) {
		v.logger.WithFields(log.Fields{
			"repository": repoPath,
			"invocation": invocation,
		}).Info("storage core: git ended a reference transaction without reporting Committed or Aborted, its admission is released")
	}
}

// transactionKey identifies a transaction between the phases of the hook, for what
// belongs to one transaction of one process (its admission and the pack of its
// Preparing phase): one key per process of the repository, from the invocation.
// All the phases of a transaction carry its invocation, also when they name other
// references than Preparing did (git reports a symbolic reference by its target in
// Committed). This rests on a process running one transaction at a time, which holds
// on reftable only: the files backend nests a packed-refs transaction when it deletes
// a packed reference. Authoritative mode serves reftable repositories only (the
// REFTABLE RULE in prepare); shadow mode, which keeps packs but no admission, loses
// at most the pack of the outer transaction, which Prepared then builds itself.
//
// Without an invocation it is the pending key, which all the transactions with the
// same hook input share.
func transactionKey(payload gitcmd.HooksPayload, changes []byte) [32]byte {
	if payload.Changelog.Invocation == "" {
		return pendingKey(payload.Repo, changes)
	}
	return pendingKey(payload.Repo, []byte("invocation\x00"+payload.Changelog.Invocation))
}

// deferDurability lets git write the references of a transaction that the
// database committed without fsync (authoritative mode, and this process owns
// the deferred durability): the repository gets its durable dirty marker
// first. A git process that writes without fsync counts as live from before it
// started until it exited (DeferReferenceWrites), so the durable step keeps the
// marker meanwhile. It runs in the Prepared phase, under git's reference lock
// and before git writes anything. A transaction whose git process writes with
// fsync (a push) gets a marker too, which only delays its removal.
func (v *Voter) deferDurability(ctx context.Context, payload gitcmd.HooksPayload, changes []byte, shadow bool) error {
	if shadow || !v.DeferredReferenceDurability() {
		return nil
	}

	repoPath, err := v.repoPath(ctx, payload)
	if err != nil {
		return err
	}
	repoKey := corestore.KeyOf(payload.Repo)
	return v.core.RefDurability.MarkDirty(ctx, localrefs.RepoRef{Storage: repoKey.Storage, RelativePath: repoKey.RelativePath}, repoPath)
}

// Aborted tells the voter that git aborted the transaction.
//
// The pending entries are keyed by the repository and the hook input only (the
// hook passes no identity of the transaction), so two concurrent transactions
// with the same input share one: two creations of the same branch at the same
// commit, for example. The second one waits for the reference lock of the
// first, which commits to the database and then in git, and then fails in git
// without reaching Prepared. Its Aborted phase must not take the entry of the
// first: when the local repository already holds every new value of the input,
// a transaction with that input committed in git, and the entry is left for
// its Committed phase. Only otherwise did git fail to commit a transaction that
// the database committed.
func (v *Voter) Aborted(ctx context.Context, payload gitcmd.HooksPayload, changes []byte) {
	if v == nil || payload.Repo == nil {
		return
	}

	key := pendingKey(payload.Repo, changes)

	if v.localRefs() {
		if repoPath, err := v.repoPath(ctx, payload); err != nil {
			v.logger.WithError(err).WarnContext(ctx, "storage core: resolving the repository path after an abort, the admission ends at its bound")
		} else {
			v.core.RefAdmission.Release(repoPath, localrefs.Key(transactionKey(payload, changes)))
		}
	}

	// A transaction aborted before its Prepared phase (git did not get the
	// locks) leaves the pack of its Preparing phase unused: an orphan in the
	// bucket, like the pack of any failed write.
	v.packs.take(transactionKey(payload, changes), time.Now())

	if !v.pending.has(key, time.Now()) {
		return
	}
	if v.committedLocally(ctx, payload, changes) {
		return
	}

	entry, ok := v.pending.take(key, time.Now())
	if !ok {
		// The Committed phase of the transaction took it meanwhile.
		return
	}

	// The database holds the changeset, the local repository does not. The next
	// catch-up repairs it.
	v.metrics.localFailures.Inc()
	v.logger.WithFields(log.Fields{
		"repo_id": entry.repoID,
		"seq":     entry.seq,
	}).ErrorContext(ctx, "storage core: git aborted a transaction that the database committed, the next catch-up repairs the local repository")
}

// committedLocally reports whether the local repository holds every new value
// of the hook input (absent for a deletion), so that a transaction with this
// input committed in git. It reports false when it cannot tell (the input does
// not parse, or the references cannot be read): the abort is then counted as a
// local commit failure, as it would be without the check.
func (v *Voter) committedLocally(ctx context.Context, payload gitcmd.HooksPayload, changes []byte) bool {
	hash, err := git.ObjectHashByFormat(payload.ObjectFormat)
	if err != nil {
		return false
	}
	updates, err := parsePayloadInput(payload, changes, hash)
	if err != nil || len(updates) == 0 {
		return false
	}

	names := make([]string, len(updates))
	for i, u := range updates {
		names[i] = u.name
	}

	local, err := gitops.ReadRefValues(ctx, v.repoFactory.Build(payload.Repo), names)
	if err != nil {
		v.logger.WithError(err).WarnContext(ctx, "storage core: reading the references of an aborted transaction")
		return false
	}

	for _, u := range updates {
		if local[u.name] != u.new {
			return false
		}
	}
	return true
}

// Describe implements prometheus.Collector (the graft_storage_* metrics of
// the write path).
func (v *Voter) Describe(descs chan<- *prometheus.Desc) {
	if v == nil {
		return
	}
	for _, c := range v.metrics.collectors() {
		c.Describe(descs)
	}
}

// Collect implements prometheus.Collector.
func (v *Voter) Collect(metrics chan<- prometheus.Metric) {
	if v == nil {
		return
	}
	for _, c := range v.metrics.collectors() {
		c.Collect(metrics)
	}
}

// parsePayloadInput parses the hook input of the transaction of payload's repository.
func parsePayloadInput(payload gitcmd.HooksPayload, changes []byte, hash git.ObjectHash) ([]inputUpdate, error) {
	return parseInput(changes, hash, storage.IsPoolRepository(payload.Repo))
}

// parseInput parses the hook input "<old> SP <new> SP <ref>" lines. Empty lines
// are ignored. Values are object IDs of the hash or symbolic "ref:<target>"
// values.
func parseInput(changes []byte, hash git.ObjectHash, inObjectPool bool) ([]inputUpdate, error) {
	zero := string(hash.ZeroOID)

	var updates []inputUpdate
	for _, line := range strings.Split(string(changes), "\n") {
		if line == "" {
			continue
		}

		oldValue, newValue, name, ok := refchange.ParseHookLine(line)
		if !ok {
			return nil, fmt.Errorf("line %q is not <old> <new> <ref>", line)
		}

		// The catch-up of every node rejects a name that git's rules reject, so
		// such a name must never reach the database.
		if err := gitops.ValidateName(name); err != nil {
			return nil, fmt.Errorf("reference name %q: %w", name, err)
		}

		u := inputUpdate{name: name, oldZero: oldValue == zero, newZero: newValue == zero}
		if !u.oldZero {
			if err := gitops.ValidateTarget(corestore.Target(oldValue), hash); err != nil {
				return nil, fmt.Errorf("old value of %q: %w", name, err)
			}
			u.old = corestore.Target(oldValue)
		}
		if !u.newZero {
			if err := gitops.ValidateTarget(corestore.Target(newValue), hash); err != nil {
				return nil, fmt.Errorf("new value of %q: %w", name, err)
			}
			u.new = corestore.Target(newValue)

			// A deletion is exempt, so that an existing long reference can be cleaned up.
			if err := git.ValidateRepositoryReferenceNameLength(name, inObjectPool); err != nil {
				return nil, err
			}
			if u.new.IsSymbolic() {
				if err := git.ValidateReferenceNameLength(u.new.SymbolicName()); err != nil {
					return nil, fmt.Errorf("symbolic reference target: %w", err)
				}
			}
		}

		updates = append(updates, u)
	}

	return updates, nil
}

// inputUpdate is one line of the hook input.
type inputUpdate struct {
	name string
	// old and new are the values, empty when they are the zero value.
	old, new corestore.Target
	// oldZero and newZero tell that the value of the line is the zero OID.
	oldZero, newZero bool
}

// divergences returns the conflicts that are divergences between git and the
// database. A conflict of a deletion whose reference is already absent is not
// one: on the files backend the packed-refs half of a deletion deletes the
// reference first, so the checked deletion of the loose half that follows
// finds it absent (doc/graft/PLAN6.md, REFTABLE RULE).
func divergences(conflicts []corestore.RefConflict, updates []inputUpdate) []corestore.RefConflict {
	if len(conflicts) == 0 {
		return nil
	}

	deleted := make(map[string]bool, len(updates))
	for _, u := range updates {
		deleted[u.name] = u.newZero
	}

	var diverging []corestore.RefConflict
	for _, c := range conflicts {
		if c.Actual.Absent() && deleted[c.Name] {
			continue
		}
		diverging = append(diverging, c)
	}
	return diverging
}

// mismatchError is the error of the first conflict, as a
// updateref.MismatchingStateError. Absent values are the zero OID.
func mismatchError(conflict *corestore.ErrRefConflict, hash git.ObjectHash) updateref.MismatchingStateError {
	zero := string(hash.ZeroOID)
	if len(conflict.Conflicts) == 0 {
		return updateref.MismatchingStateError{ExpectedObjectID: zero, ActualObjectID: zero}
	}

	c := conflict.Conflicts[0]
	return updateref.MismatchingStateError{
		ReferenceName:    c.Name,
		ExpectedObjectID: targetOrZero(c.Expected, zero),
		ActualObjectID:   targetOrZero(c.Actual, zero),
	}
}

func targetOrZero(t corestore.Target, zero string) string {
	if t.Absent() {
		return zero
	}
	return string(t)
}

// pendingKey identifies a transaction between two phases of the hook: the
// SHA-256 of the repository's storage name and normalized relative path (its
// database key, so that the same relative path in two storages is two keys)
// and the hook input.
func pendingKey(repo storage.Repository, changes []byte) [32]byte {
	repoKey := corestore.KeyOf(repo)

	h := sha256.New()
	h.Write([]byte(repoKey.Storage))
	h.Write([]byte{0})
	h.Write([]byte(repoKey.RelativePath))
	h.Write([]byte{0})
	h.Write(changes)

	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key
}

// pendingEntry is a value of a pendingSet with its key and the time it was
// added.
type pendingEntry[T any] struct {
	key   [32]byte
	value T
	added time.Time
}

// pendingSet is a process-local set of transactions between two phases of
// the hook, keyed by pendingKey. Entries expire after pendingTTL, and the
// oldest is evicted beyond maxPendingEntries.
type pendingSet[T any] struct {
	mu    sync.Mutex
	items map[[32]byte]*list.Element
	// order holds the *pendingEntry[T] values, oldest first.
	order *list.List
}

func newPendingSet[T any]() *pendingSet[T] {
	return &pendingSet[T]{items: map[[32]byte]*list.Element{}, order: list.New()}
}

// add remembers value under key, replacing an entry of the same key.
func (p *pendingSet[T]) add(key [32]byte, value T, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if el, ok := p.items[key]; ok {
		p.removeLocked(el)
	}

	for el := p.order.Front(); el != nil; el = p.order.Front() {
		if now.Sub(el.Value.(*pendingEntry[T]).added) < pendingTTL {
			break
		}
		p.removeLocked(el)
	}

	p.items[key] = p.order.PushBack(&pendingEntry[T]{key: key, value: value, added: now})

	for p.order.Len() > maxPendingEntries {
		p.removeLocked(p.order.Front())
	}
}

// take removes and returns the value of key, if it is still there and has not
// expired.
func (p *pendingSet[T]) take(key [32]byte, now time.Time) (T, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var zero T
	el, ok := p.items[key]
	if !ok {
		return zero, false
	}

	e := el.Value.(*pendingEntry[T])
	p.removeLocked(el)
	if now.Sub(e.added) >= pendingTTL {
		return zero, false
	}
	return e.value, true
}

// has reports whether key has an entry that has not expired, without removing
// it.
func (p *pendingSet[T]) has(key [32]byte, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	el, ok := p.items[key]
	return ok && now.Sub(el.Value.(*pendingEntry[T]).added) < pendingTTL
}

// removeIf removes the entries whose value matches, and returns their values.
func (p *pendingSet[T]) removeIf(match func(T) bool) []T {
	p.mu.Lock()
	defer p.mu.Unlock()

	var removed []T
	for el := p.order.Front(); el != nil; {
		next := el.Next()
		if value := el.Value.(*pendingEntry[T]).value; match(value) {
			p.removeLocked(el)
			removed = append(removed, value)
		}
		el = next
	}
	return removed
}

// len returns the number of entries, expired ones included.
func (p *pendingSet[T]) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.order.Len()
}

func (p *pendingSet[T]) removeLocked(el *list.Element) {
	e := p.order.Remove(el).(*pendingEntry[T])
	delete(p.items, e.key)
}

// metrics are the graft_storage_* metrics of the write path. The collectors are
// owned by the voter; registering the voter registers them.
type metrics struct {
	changesets    *prometheus.CounterVec
	divergences   prometheus.Counter
	shadowErrors  prometheus.Counter
	untracked     prometheus.Counter
	mvccSkipped   prometheus.Counter
	localFailures prometheus.Counter
	packBytes     prometheus.Counter
	preparing     prometheus.Histogram
	// preparingSteps splits the Preparing phase of authoritative writes into its
	// steps (observeStep).
	preparingSteps *prometheus.HistogramVec
	prepared       prometheus.Histogram
	// preparedSteps splits the Prepared phase of authoritative writes into its
	// steps (observePreparedStep).
	preparedSteps *prometheus.HistogramVec
}

func newMetrics() *metrics {
	return &metrics{
		changesets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_storage_changesets_total",
			Help: "Reference transactions handled by the storage core write path, by mode and result.",
		}, []string{"mode", "result"}),
		divergences: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_shadow_divergences_total",
			Help: "Shadow mode changesets recorded despite a reference mismatch.",
		}),
		shadowErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_shadow_errors_total",
			Help: "Shadow mode write path failures; the git write proceeded.",
		}),
		untracked: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_untracked_writes_total",
			Help: "Shadow mode reference transactions of repositories not imported into the storage core.",
		}),
		mvccSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_mvcc_skipped_total",
			Help: "Reference transactions of MVCC repositories seen by the storage core write path.",
		}),
		localFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_local_commit_failures_total",
			Help: "Transactions committed to the database that git then aborted.",
		}),
		packBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_pack_bytes_total",
			Help: "Bytes of changeset packs uploaded to object storage.",
		}),
		preparing: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "graft_storage_preparing_seconds",
			Help:    "Time spent in the Preparing phase of the storage core write path (pack build and upload, before git takes the reference locks).",
			Buckets: prometheus.DefBuckets,
		}),
		preparingSteps: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "graft_storage_preparing_step_seconds",
			Help: "Time of each step of the Preparing phase of authoritative writes: record (the read of the " +
				"repository record), walk (the objects of the pack), pack (writing and hashing a non-empty pack) " +
				"and upload (to the bucket); kind is create for a transaction that only creates references, " +
				"update otherwise.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"kind", "step"}),
		prepared: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "graft_storage_prepared_seconds",
			Help:    "Time spent in the Prepared phase of the storage core write path, while git holds the reference locks.",
			Buckets: prometheus.DefBuckets,
		}),
		preparedSteps: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "graft_storage_prepared_step_seconds",
			Help: "Time of each step of the Prepared phase of authoritative writes, while git holds the reference " +
				"locks: record (the read of the repository record), local_read (the local references the expected " +
				"values need), pack (walk, pack and upload of the pack of a transaction with new tips, when the " +
				"Preparing phase did not prepare it), commit (the database commit of the changeset) and durability " +
				"(the dirty marker of deferred reference durability), each observed when it succeeded; kind is " +
				"create for a transaction that only creates references, update otherwise.",
			Buckets: []float64{.0005, .001, .002, .003, .005, .0075, .01, .015, .02, .03, .05, .075, .1, .25, .5, 1, 2.5},
		}, []string{"kind", "step"}),
	}
}

func (m *metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.changesets,
		m.divergences,
		m.shadowErrors,
		m.untracked,
		m.mvccSkipped,
		m.localFailures,
		m.packBytes,
		m.preparing,
		m.preparingSteps,
		m.prepared,
		m.preparedSteps,
	}
}
