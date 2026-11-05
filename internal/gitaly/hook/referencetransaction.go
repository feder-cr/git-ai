package hook

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/mvcc"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/policyeval"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/graft/refchange"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/transaction/voting"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// ReferenceTransactionHook captures all reference updates taking place in the
// repository.
//
// When using praefect, this allows us to vote on reference updates and ensure
// that all nodes are in sync.
//
// When using WAL, this allows us to capture all the reference updates that take
// place in a transaction and add it to the transaction.
//
// We only capture regular reference updates, and updates to the default branch.
// Any other reference updates are ignored.
func (m *GitLabHookManager) ReferenceTransactionHook(ctx context.Context, state ReferenceTransactionState, env []string, stdin io.Reader) error {
	payload, err := gitcmd.HooksPayloadFromEnv(env)
	if err != nil {
		return fmt.Errorf("extracting hooks payload: %w", err)
	}

	objectHash, err := git.ObjectHashByFormat(payload.ObjectFormat)
	if err != nil {
		return fmt.Errorf("looking up object hash: %w", err)
	}

	changes, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin from request: %w", err)
	}

	// The storage core ties the admission of a transaction to its git process by the invocation,
	// which HooksPayloadFromEnv does not restore.
	payload.Changelog.Invocation = changesetInvocation(env)

	if state == ReferenceTransactionPreparing && m.storageVoter != nil {
		m.storageVoter.TransactionStarts(ctx, payload)
	}

	// Every reference update that git performs with this hook passes here before git writes it,
	// whatever asked for it (a fetch, for example, takes its names from the remote). A failed hook
	// makes git abort the whole transaction. The prepared phase is the one that always reaches the
	// server: gitaly-hooks forwards the preparing phase only when the storage core wants it, and
	// then this refuses before the core builds the pack of the transaction.
	if state == ReferenceTransactionPreparing || state == ReferenceTransactionPrepared {
		if err := ValidateReferenceNameLengths(payload.Repo, objectHash, changes); err != nil {
			return structerr.NewInvalidArgument("%w", err)
		}
	}

	m.trackPendingTransaction(state, env, changes)

	var phase voting.Phase
	switch state {
	// We're voting in preparing state to tell Praefect that the reference changes are ready,
	// although they are not locked yet. At this stage, we can serialize write transactions by
	// acquiring a lock for the transaction and releasing it only when the transaction is
	// committed or aborted. Other transactions must wait for the lock.
	case ReferenceTransactionPreparing:
		phase = voting.Preparing
	// We're voting in prepared state to tell Praefect we've locked the reference changes and
	// queued them for the transaction. We can abort the transaction if needed.
	case ReferenceTransactionPrepared:
		phase = voting.Prepared

	// We're also voting in committed state to tell Praefect we've actually persisted the
	// changes. This is necessary as some RPCs fail return errors in the response body rather
	// than as an error code. Praefect can't tell if these RPCs have failed. Voting on committed
	// ensure Praefect sees either a missing vote or that the RPC did commit the changes.
	case ReferenceTransactionCommitted:
		phase = voting.Committed

	// An aborted transaction gives up its place in the order of the changeset log, so that the
	// transactions prepared after it do not wait for it.
	case ReferenceTransactionAborted:
		m.abortChangesets(payload, env, changes)
		if m.storageVoter != nil {
			m.storageVoter.Aborted(ctx, payload, changes)
		}
		return nil

	default:
		return nil
	}

	// When deleting references, git has to delete them both in the packed-refs backend as well
	// as any loose refs -- if only the loose ref was deleted, it would potentially unshadow the
	// value contained in the packed-refs file and vice versa. As a result, git will create two
	// transactions when any ref exists in both backends: one session to force-delete all
	// existing refs in the packed-refs backend, and then one transaction to update all loose
	// refs. This is problematic for us, as our voting logic now requires all nodes to have the
	// same packed state, which we do not and cannot guarantee.
	//
	// We're lucky though and can fix this quite easily: git only needs to cope with unshadowing
	// refs when deleting loose refs, so it will only ever _delete_ refs from the packed-refs
	// backend and never _update_ any refs. And if such a force-deletion happens, the same
	// deletion will also get queued to the loose backend no matter whether the loose ref exists
	// or not given that it must be locked during the whole transaction. As such, we can easily
	// recognize those packed-refs cleanups: all queued ref updates are force deletions.
	//
	// The workaround is thus clear: we simply do not cast a vote on any reference transaction
	// which consists only of force-deletions -- the vote will instead only happen on the loose
	// backend transaction, which contains the full record of all refs which are to be updated.
	// The changeset log records the updates once they are committed. Like the
	// vote, it skips the packed-refs cleanup and records its deletions with the
	// loose backend transaction (see pendingTransactions). Recording never fails
	// the update, so its errors are only logged.
	if state == ReferenceTransactionCommitted {
		// In authoritative storage mode the changelog is written by the storage core, in the
		// same database transaction as the reference change (see doc/graft/STORAGE.md).
		if !m.storageAuthoritative() {
			m.recordChangesets(ctx, objectHash, payload, env, changes)
		}
		if m.storageVoter != nil {
			m.storageVoter.Committed(ctx, payload, changes)
		}
		m.observeCommitted(ctx, payload, changes)
	}

	// The standalone access policy is enforced here, while the references are
	// locked but not yet updated, so that it holds for every reference update
	// that Gitaly performs and not only for those that run the pre-receive hook.
	// It runs before the vote and before the transaction takes its place in the
	// order of the changeset log, as a denial aborts the transaction.
	if state == ReferenceTransactionPrepared {
		if err := m.enforceGraftPolicy(ctx, env, changes); err != nil {
			return err
		}

		// The storage core commits the transaction while git holds the locks, after the
		// policy allowed it. In authoritative mode this is the commit point: an error
		// aborts the transaction (see doc/graft/PLAN6.md, "Write path").
		if err := m.storagePrepared(ctx, payload, env, changes); err != nil {
			return err
		}

		// The file changelog records the identities of a land only after git committed it, so
		// they are recorded as intents now, before git may commit (doc/graft/PLAN11.md, 4.2).
		// An error aborts the land, which would otherwise be committed without its identity.
		if err := m.recordLandIntents(ctx, payload, env, changes); err != nil {
			return fmt.Errorf("recording the land intents: %w", err)
		}
	}

	if !isForceDeletionsOnly(objectHash, bytes.NewReader(changes)) {
		hash := sha1.Sum(changes)

		if err := m.voteOnTransaction(ctx, hash, phase, payload); err != nil {
			return fmt.Errorf("error voting on transaction: %w", err)
		}
	}

	// Before git locks the references, the storage core builds and uploads the pack of the
	// transaction, so that only the database commit runs under the locks in the Prepared phase
	// (see doc/graft/PLAN6.md, "LOCK AND HISTORY RULE"). It runs after the vote, which it does
	// not delay, and it never fails the transaction.
	if state == ReferenceTransactionPreparing {
		m.storagePreparing(ctx, payload, env, changes)
	}

	// The prepared transaction holds the locks of its references, so it takes its place in
	// the order of the changeset log now. It does so only once the vote succeeded, because a
	// failed hook aborts the transaction.
	// In authoritative storage mode the file changelog is not written, so no order is kept for it.
	// The mode is the storage core's configuration, fixed for the life of the process, so the
	// prepared and the committed phase of a transaction decide alike.
	if state == ReferenceTransactionPrepared && !m.storageAuthoritative() {
		m.prepareChangesets(payload, env, changes)
	}

	return nil
}

// ValidateReferenceNameLengths refuses a transaction of repo that writes a reference name, or points a
// symbolic reference at a name, over git.MaxReferenceNameLength. A deletion is not a write of its name,
// so that an existing long reference can be cleaned up. A fetch into an object pool has its allowance
// (git.ObjectPoolReferenceNameAllowance). A line that does not parse is left to the checks that parse
// the input for what they do with it.
func ValidateReferenceNameLengths(repo *gitalypb.Repository, objectHash git.ObjectHash, changes []byte) error {
	inObjectPool := storage.IsPoolRepository(repo)
	for _, line := range strings.Split(string(changes), "\n") {
		_, newValue, name, ok := refchange.ParseHookLine(line)
		if !ok || objectHash.IsZeroOID(git.ObjectID(newValue)) {
			continue
		}

		if err := git.ValidateRepositoryReferenceNameLength(name, inObjectPool); err != nil {
			return err
		}
		if target, symbolic := strings.CutPrefix(newValue, symbolicHookValuePrefix); symbolic {
			if err := git.ValidateReferenceNameLength(target); err != nil {
				return fmt.Errorf("symbolic reference target: %w", err)
			}
		}
	}

	return nil
}

// symbolicHookValuePrefix starts the value of a symbolic reference in the input of the
// reference-transaction hook.
const symbolicHookValuePrefix = "ref:"

// ordersChangesets reports whether the transaction with the given hook input takes a place in the
// order of the changeset log of its repository.
func (m *GitLabHookManager) ordersChangesets(payload gitcmd.HooksPayload, changes []byte) bool {
	return !m.cfg.Graft.Changelog.Disabled && payload.Repo != nil && len(changelog.ParseUpdates(changes)) > 0
}

// changesetInvocation returns the invocation of the git process that runs the transaction, which
// tells its ticket apart from those of the transactions of other processes (changesetTicketKey).
// It is empty when the payload carries none; the prepared and the committed phase of a
// transaction read the same environment, so they agree.
func changesetInvocation(env []string) string {
	meta, err := gitcmd.ChangelogMetaFromEnv(env)
	if err != nil {
		return ""
	}
	return meta.Invocation
}

// recordLandIntents records the land identities of a prepared transaction as intents, when the
// file changelog is the one that records them and the transaction updates the branch they landed
// on (see changelog.RecordLandIntents). In the authoritative storage mode the storage core records
// them with the reference change, in one database transaction.
func (m *GitLabHookManager) recordLandIntents(ctx context.Context, payload gitcmd.HooksPayload, env []string, changes []byte) error {
	if m.cfg.Graft.Changelog.Disabled || m.storageAuthoritative() || payload.Repo == nil {
		return nil
	}

	meta, err := gitcmd.ChangelogMetaFromEnv(env)
	if errors.Is(err, gitcmd.ErrPayloadNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading changelog metadata: %w", err)
	}
	if len(meta.Lands) == 0 {
		return nil
	}

	updatesBranch := false
	for _, update := range changelog.ParseUpdates(changes) {
		if update.Reference == string(meta.LandReference) {
			updatesBranch = true
		}
	}
	if !updatesBranch {
		return nil
	}

	repoPath, err := m.locator.GetRepoPath(ctx, payload.Repo)
	if err != nil {
		return fmt.Errorf("resolving repository path: %w", err)
	}

	start := time.Now()
	compact, err := changelog.RecordLandIntents(repoPath, string(meta.LandReference), meta.Invocation, meta.Lands)
	landIntentsSeconds.Observe(time.Since(start).Seconds())
	if err != nil {
		return err
	}

	// The compaction reads the changelog and rewrites the intents. It starts in the prepared
	// phase, in a goroutine that the hook does not wait for, so it runs while git still holds
	// the references' locks and after: it takes none of them, only the intents' lock file, which
	// the intents of other transactions of the repository wait for meanwhile. Its failure leaves
	// the intents as they are; it is counted and logged.
	if compact {
		run := func() {
			if err := changelog.CompactLandIntents(repoPath); err != nil {
				landIntentsCompactionFailuresTotal.Inc()
				m.logger.WithError(err).WithField("repository", repoPath).Error("graft: compacting the land intents failed")
			}
		}
		if m.startLandIntentsCompaction != nil {
			m.startLandIntentsCompaction(run)
		} else {
			go run()
		}
	}

	return nil
}

// prepareChangesets gives the prepared transaction its place in the order of the changeset log,
// see changesetTickets.
func (m *GitLabHookManager) prepareChangesets(payload gitcmd.HooksPayload, env []string, changes []byte) {
	if m.ordersChangesets(payload, changes) {
		takeChangesetTicket(changesetQueueKey(payload.Repo), changesetInvocation(env), changes)
	}
}

// abortChangesets gives up the place of the aborted transaction in the order of the changeset
// log, see changesetTickets.
func (m *GitLabHookManager) abortChangesets(payload gitcmd.HooksPayload, env []string, changes []byte) {
	if m.ordersChangesets(payload, changes) {
		releaseChangesetTicket(changesetQueueKey(payload.Repo), changesetInvocation(env), changes)
	}
}

// recordChangesets appends the committed reference updates to the changeset log
// of the repository. It must never fail the reference update, so it recovers
// from panics and only logs errors.
//
// The repository is verified like any other, so the log is only ever written
// into an existing repository. Committed reference updates always happen in an
// existing git directory, including the staging repositories of repoutil.Create.
//
// Trust model: the hook payload and the stdin come from the caller of the
// HookService.ReferenceTransactionHook RPC, as they do for every upstream hook.
// The operation, actor and updates recorded here are therefore only as
// trustworthy as that caller. Entries can describe updates that never happened
// when an authenticated client calls the RPC directly, so the log is an audit
// trail of the server's own operations, not a proof against forged calls. The
// RPC is meant for the internal socket that Git hooks use.
func (m *GitLabHookManager) recordChangesets(ctx context.Context, objectHash git.ObjectHash, payload gitcmd.HooksPayload, env []string, changes []byte) {
	if m.cfg.Graft.Changelog.Disabled {
		return
	}

	defer func() {
		if r := recover(); r != nil {
			m.logger.WithFields(log.Fields{"panic": fmt.Sprint(r)}).ErrorContext(ctx, "recording changesets panicked")
		}
	}()

	graftMeta, metaErr := gitcmd.ChangelogMetaFromEnv(env)

	// The packed-refs cleanup of a deletion is recorded with the loose backend
	// transaction that git commits right after it. Its prepared state is
	// forgotten even when it updates nothing that is recorded.
	packedCleanup := metaErr == nil && m.pendingTransactions.committed(objectHash, graftMeta.Invocation, changes)

	updates := changelog.ParseUpdates(changes)
	if len(updates) == 0 {
		return
	}

	// The updates are recorded in the order in which their transactions were prepared, which
	// is the order in which the references were updated. See changesetTickets. A transaction
	// that records nothing gives up its place at once, so that later transactions do not wait
	// for it: one whose repository path cannot be resolved, a packed-refs cleanup, one whose
	// metadata cannot be read, and one on the MVCC reference backend, whose updates are
	// recorded after its commit.
	queueKey := changesetQueueKey(payload.Repo)
	invocation := changesetInvocation(env)
	release := func() {
		releaseChangesetTicket(queueKey, invocation, changes)
	}

	repoPath, err := m.locator.GetRepoPath(ctx, payload.Repo)
	if err != nil {
		release()
		m.reportLostChangesets(ctx, updates, fmt.Errorf("resolving repository path: %w", err))
		return
	}

	if packedCleanup {
		release()
		return
	}

	if metaErr != nil {
		release()
		m.reportLostChangesets(ctx, updates, fmt.Errorf("reading changelog metadata: %w", metaErr))
		return
	}

	record := changelog.Record{Meta: changelogMetaOf(payload, graftMeta), Updates: updates}

	// On a repository that uses the MVCC reference backend, the updates are staged in the
	// snapshot of the request that performs them, and are only published when that request
	// commits its MVCC cache after its handler returned. The commit fails when a concurrent
	// request committed first, and the updates are then thrown away, so they are only
	// recorded once the commit succeeded, outside of the order of the tickets.
	if graftMeta.MVCCCommit != "" {
		release()
		cache := mvcc.CacheByCommitHandle(graftMeta.MVCCCommit)
		if cache == nil || !cache.AfterCommit(func() {
			if err := appendChangesetGroupRecovering(repoPath, []changelog.Record{record}); err != nil {
				m.reportLostChangesets(ctx, updates, err)
			}
		}) {
			// The cache committed or was cleaned up before the updates could be registered for
			// its commit, so whether they were published cannot be told: they are not recorded.
			m.reportLostChangesets(ctx, updates, errors.New("the MVCC cache that commits the updates is gone"))
		}
		return
	}

	// A transaction without a queued ticket, prepared before the changeset log was enabled or
	// whose ticket expired, is recorded at once (see recordChangeset).
	if err := recordChangeset(queueKey, repoPath, invocation, changes, record); err != nil {
		m.reportLostChangesets(ctx, updates, err)
	}
}

// reportLostChangesets reports committed reference updates that the changeset log does not
// record because writing them failed. Recording never fails the reference update, which git
// committed already, so the loss is made visible instead: it is counted
// (graft_changelog_records_lost_total) and logged with the references.
func (m *GitLabHookManager) reportLostChangesets(ctx context.Context, updates []changelog.Update, err error) {
	changelogRecordsLostTotal.Add(float64(len(updates)))

	references := make([]string, 0, len(updates))
	for _, update := range updates {
		references = append(references, update.Reference)
	}

	m.logger.WithError(err).WithField("references", references).ErrorContext(ctx,
		"recording changesets failed: the changeset log misses these committed reference updates")
}

// observeCommitted hands the committed reference updates to the eager observer,
// see eager.Observer. It runs whether or not the changeset log records them, and
// the observer only schedules the work, so it never fails or blocks the update.
func (m *GitLabHookManager) observeCommitted(ctx context.Context, payload gitcmd.HooksPayload, changes []byte) {
	if m.observer == nil || payload.Repo == nil {
		return
	}

	updates := changelog.ParseUpdates(changes)
	if len(updates) == 0 {
		return
	}

	// The background work addresses the repository only. The payload may carry
	// the object directory of a quarantine, which is gone once the work runs.
	repo := &gitalypb.Repository{
		StorageName:  payload.Repo.GetStorageName(),
		RelativePath: payload.Repo.GetRelativePath(),
	}

	m.observer.ObserveCommitted(ctx, repo, updates)
}

// trackPendingTransaction keeps the transactions that the git process running
// the hook has prepared and not yet committed or aborted, so that recordChangesets
// recognizes the packed-refs cleanup of a deletion. Only transactions that git
// runs the hook for itself are tracked: their hooks payload carries the
// invocation of the git process.
func (m *GitLabHookManager) trackPendingTransaction(state ReferenceTransactionState, env []string, changes []byte) {
	// In authoritative storage mode recordChangesets does not run, so nothing would ever take a
	// tracked transaction out again.
	if m.cfg.Graft.Changelog.Disabled || m.pendingTransactions == nil || m.storageAuthoritative() {
		return
	}

	if state != ReferenceTransactionPrepared && state != ReferenceTransactionAborted {
		return
	}

	graftMeta, err := gitcmd.ChangelogMetaFromEnv(env)
	if err != nil || graftMeta.Invocation == "" {
		return
	}

	switch state {
	case ReferenceTransactionPrepared:
		m.pendingTransactions.prepared(graftMeta.Invocation, changes)
	case ReferenceTransactionAborted:
		m.pendingTransactions.aborted(graftMeta.Invocation, changes)
	}
}

// enforceGraftPolicy evaluates the standalone access policy ([[graft.policy]])
// for reference updates that a request authenticated with a per-agent token
// performs. It returns a PermissionDenied error when the policy denies one of
// the updated references, which makes Git abort the whole reference
// transaction.
//
// The pre-receive hook only runs for OperationService RPCs and pushes. Other
// RPCs, such as RepositoryService.WriteRef, RefService.DeleteRefs and
// RepositoryService.FetchRemote, update references with only this hook, so the
// policy has to be enforced here for an agent not to bypass it.
//
// The policy decides on the agent of the token, as the pre-receive hook does.
// Callers that did not authenticate with an agent token (the shared token,
// unauthenticated setups, Gitaly's own internal operations) are not restricted
// here: they are trusted, and their pushes and OperationService RPCs are still
// checked by the pre-receive hook. The policy governs the references under
// refs/ and HEAD, so that a symbolic update of the default branch is decided
// like any other update. Other pseudo-references are not governed.
//
// The reservation of refs/graft/ (GraftReservedRefPrefix) applies to agents in every mode; the
// rest of the policy applies only in standalone mode with a policy.
func (m *GitLabHookManager) enforceGraftPolicy(ctx context.Context, env []string, changes []byte) error {
	// One policy decides the whole transaction: the one in force now, while git holds the
	// references' locks (doc/graft/PLAN12.md, section 5).
	policy := m.policy.Current()
	decision, err := m.graftPolicyDecision(policy, env, changes)
	if err != nil {
		return err
	}
	if !decision.evaluated && decision.denial == "" {
		return nil
	}

	// A decision of the policy names the policy that took it, so that an operator can tell which
	// generation decided an update while policies are reloaded: a denial at INFO, as before, and
	// an allowed update at DEBUG, since agents land many times a second and the changelog already
	// records who landed what. A denial of the reservation of refs/graft/ is no decision of the
	// policy.
	fields := log.Fields{"actor": decision.actor}
	if decision.evaluated {
		fields["policy_generation"] = policy.Generation
		fields["policy_sha256"] = policy.SHA256
	}
	if decision.reference != "" {
		fields["reference"] = decision.reference
	}
	if decision.denial != "" {
		fields["reason"] = decision.denial
		m.logger.WithFields(fields).InfoContext(ctx, "agent reference update denied")

		return structerr.NewPermissionDenied("%s", decision.denial)
	}
	m.logger.WithFields(fields).DebugContext(ctx, "agent reference update allowed by the policy")

	return nil
}

// graftDecision is the decision of graftPolicyDecision.
type graftDecision struct {
	// denial is the message of the denial of the transaction, or empty when the policy allows it
	// or does not apply to it.
	denial string
	// evaluated tells whether the rules of the policy decided, rather than the reservation of
	// refs/graft/ or the skips.
	evaluated bool
	// reference is the reference the reservation refused, for the log: the denial does not name it.
	reference string
	// actor is the agent of the update, when the metadata names one.
	actor string
}

// PolicyChangedError refuses the combined update of a land batch whose members were admitted under
// another generation of the access policy than the one in force (doc/graft/PLAN12.md, section 5).
// The batch then lands each request alone, under the policy in force.
type PolicyChangedError struct {
	// Admitted is the generation the batch admitted its members under (0 when the batch carried
	// none), InForce the generation in force.
	Admitted, InForce uint64
}

func (e PolicyChangedError) Error() string {
	return fmt.Sprintf("the access policy changed during the land batch: its members were admitted under generation %d, generation %d is in force", e.Admitted, e.InForce)
}

// graftPolicyDecision is the decision of enforceGraftPolicy with policy.
func (m *GitLabHookManager) graftPolicyDecision(policy *policystore.Snapshot, env []string, changes []byte) (graftDecision, error) {
	policyApplies := m.cfg.Graft.Standalone && len(policy.Rules) > 0

	meta, err := gitcmd.ChangelogMetaFromEnv(env)
	if err != nil {
		// The metadata names the agent of the update. Without agents and without a policy to
		// apply there is nothing to decide; otherwise the update cannot be judged.
		if !policyApplies && len(m.cfg.Graft.Agents) == 0 {
			return graftDecision{}, nil
		}
		return graftDecision{}, fmt.Errorf("reading graft metadata: %w", err)
	}

	// A batched land has already checked the policy for every member in process,
	// before the member entered the running tip of the batch (see
	// executeLandBatch in the operations service), so the combined update is not
	// checked again. The members are taken from the context that builds the hooks
	// payload, so a client of an OperationService RPC cannot set them: the
	// environment of the git process that runs this hook is built by Gitaly, and
	// agents cannot set git configuration (see the trust model in recordChangesets).
	// A direct caller of HookService.ReferenceTransactionHook supplies the payload,
	// but it could already skip this check by setting agent to false, and such a
	// call updates no references. The skip therefore gives it no new reach.
	// Only the batch operation carries members, so any other update is still
	// checked here.
	batch := meta.Operation == changelog.OperationLandBatch && len(meta.Members) > 0
	agent := meta.Agent && meta.Actor != ""

	// refs/graft/ is the server's, whatever the policy says and whether or not the server runs
	// in standalone mode: it is not a rule of the policy but an invariant of the server, for
	// every agent. A batch runs without an identity but lands the changes of its members, so it
	// is held to the reservation too, before the batch skip below: a batch only ever updates
	// its branch, and this keeps it so whatever builds it.
	if batch || agent {
		if message, reference := graftReservedDenial(changes); message != "" {
			return graftDecision{denial: message, reference: reference, actor: meta.Actor}, nil
		}
	}

	// The members of a batch were admitted under one generation of the policy. If another one is
	// in force, a member it denies would be landed: the batch is refused, and its requests land
	// alone, each judged by the policy in force. Outside standalone mode no member was admitted
	// by a policy, so there is nothing to compare.
	if batch && m.cfg.Graft.Standalone && meta.PolicyGeneration != policy.Generation {
		return graftDecision{}, PolicyChangedError{Admitted: meta.PolicyGeneration, InForce: policy.Generation}
	}

	if batch || !agent || !policyApplies {
		return graftDecision{}, nil
	}

	message, err := policyeval.Denial(policy.Rules, meta.Actor, changes)
	if err != nil {
		return graftDecision{}, fmt.Errorf("evaluating graft policy: %w", err)
	}

	return graftDecision{denial: message, evaluated: true, actor: meta.Actor}, nil
}

// CheckAgentPolicy evaluates the standalone access policy for the reference
// updates in changes, in the "<old-oid> <new-oid> <ref>" per-line format, that
// the RPC of ctx performs without running the pre-receive hook, such as
// OperationService.UserMergeToRef. The reference-transaction hook enforces the
// policy on these updates too, but Git reports only that it failed, so the RPC
// checks it first to return the policy message with PermissionDenied.
//
// Like enforceGraftPolicy, it decides only for a caller that authenticated with
// a per-agent token: the reservation of refs/graft/ in every mode, the rest of
// the policy only in standalone mode with a policy. The hook still runs
// afterwards.
func CheckAgentPolicy(ctx context.Context, graft config.Graft, changes string) error {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Source != identity.SourceAgent {
		return nil
	}
	if err := CheckAgentReservation(ctx, changes); err != nil {
		return err
	}
	if !graft.Standalone || len(graft.Policy) == 0 {
		return nil
	}

	message, err := policyeval.Denial(graft.Policy, id.Name, []byte(changes))
	if err != nil {
		return structerr.NewInternal("evaluating graft policy: %w", err)
	}

	if message != "" {
		return structerr.NewPermissionDenied("%s", message)
	}

	return nil
}

// CheckAgentReservation refuses the reference updates in changes, in the "<old-oid> <new-oid> <ref>"
// per-line format, that the reservations forbid a caller that authenticated with a per-agent token,
// in every mode, as the hook does. UpdaterWithHooks asks before it runs any hook: what a hook reports,
// such as the changes of an access check, names the target of a symbolic branch, which an agent may
// not be able to see.
func CheckAgentReservation(ctx context.Context, changes string) error {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Source != identity.SourceAgent {
		return nil
	}
	if message, _ := graftReservedDenial([]byte(changes)); message != "" {
		return structerr.NewPermissionDenied("%s", message)
	}
	return nil
}

// GraftReservedRefPrefix is the namespace of the references that only the server writes: the
// proposals of a land (refs/graft/proposals/) and graft-verify's green references, which it
// writes with the shared token. An agent never creates, updates or deletes a reference under it,
// whatever the policy says and in every mode. The layers that refuse it, in the order a write
// meets them: the request filter of the agent interceptor (a request that names it), git's
// receive.hideRefs on a push, CheckAgentPolicy in the RPCs that skip the pre-receive hook,
// CheckAgentReservation in those that run the hooks themselves, and last the reference-transaction
// hook (graftPolicyDecision), which is told the references actually updated, by git or by
// UpdaterWithHooks, the target of a symbolic reference included. See doc/graft/AGENT-SECURITY.md.
const GraftReservedRefPrefix = "refs/graft/"

// graftDecoyRefPrefixes are the namespaces whose references git's DWIM rules take for a full name:
// refs/tags/refs/heads/x answers a revision lookup of refs/heads/x while that does not exist. An agent
// neither creates nor updates a reference under them, in every mode. It may delete one, so that one
// from before this rule can be cleaned up. The server reads its own references by their exact names,
// but a consumer that reads a revision may not.
var graftDecoyRefPrefixes = []string{"refs/refs/", "refs/tags/refs/", "refs/heads/refs/", "refs/remotes/refs/"}

// graftReservedDenial returns the denial of changes that update a reference an agent may not update,
// and that reference, or "". The denial does not name the reference: an agent that wrote through a
// symbolic branch named something else, and the target may be hidden from it.
func graftReservedDenial(changes []byte) (message, reference string) {
	for _, line := range strings.Split(string(changes), "\n") {
		_, newValue, name, ok := refchange.ParseHookLine(line)
		if !ok {
			continue
		}
		if strings.HasPrefix(name, GraftReservedRefPrefix) {
			return fmt.Sprintf("a reference under %s is reserved for the server: agents may not update it", GraftReservedRefPrefix), name
		}
		if strings.Trim(newValue, "0") == "" {
			continue
		}
		for _, prefix := range graftDecoyRefPrefixes {
			if strings.HasPrefix(name, prefix) {
				return fmt.Sprintf("a reference under %s can answer for another reference: agents may not create or update it", prefix), name
			}
		}
	}
	return "", ""
}

// isForceDeletionsOnly determines whether the given changes only consist of force-deletions.
func isForceDeletionsOnly(objectHash git.ObjectHash, changes io.Reader) bool {
	// forceDeletionPrefix is the prefix of a queued reference transaction which deletes a
	// reference without checking its current value.
	forceDeletionPrefix := fmt.Sprintf("%[1]s %[1]s ", objectHash.ZeroOID)

	scanner := bufio.NewScanner(changes)

	for scanner.Scan() {
		line := scanner.Bytes()

		if bytes.HasPrefix(line, []byte(forceDeletionPrefix)) {
			continue
		}

		return false
	}

	return true
}
