package hook

import (
	"context"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// StorageVoter is the write path of the Graft storage core
// (internal/graft/corestore/voter), called from the reference-transaction
// hook. See doc/graft/PLAN6.md, "Write path".
type StorageVoter interface {
	// TransactionStarts is called first thing in the Preparing phase of a transaction,
	// before anything that can skip the rest of the phase. A transaction of the same
	// process that is still open then has ended, without a report from git.
	TransactionStarts(ctx context.Context, payload gitcmd.HooksPayload)
	// Preparing does the slow work of the transaction (building and uploading
	// its pack) before git takes the reference locks. It never fails the
	// transaction: when it did not complete, Prepared does that work itself.
	Preparing(ctx context.Context, payload gitcmd.HooksPayload, changes []byte)
	// Prepared commits the transaction to the storage core while git holds
	// the reference locks. An error aborts the transaction.
	Prepared(ctx context.Context, payload gitcmd.HooksPayload, changes []byte, meta changelog.Meta) error
	// Committed is called after git committed the transaction.
	Committed(ctx context.Context, payload gitcmd.HooksPayload, changes []byte)
	// Aborted is called after git aborted the transaction.
	Aborted(ctx context.Context, payload gitcmd.HooksPayload, changes []byte)
	// Authoritative reports whether the storage core's database replaces the
	// file changelog.
	Authoritative() bool
	// DeferReferenceWrites decides whether the git process the caller is about
	// to start for repo writes its references without fsync: the database
	// commit is their durable point, and the storage core makes the local
	// references durable later (doc/graft/PLAN7.md, LOCAL REFERENCE WRITES RULE).
	// When ok, the caller calls end once that process has exited.
	DeferReferenceWrites(ctx context.Context, repo *gitalypb.Repository) (end func(), ok bool)
}

// DeferReferenceWrites decides whether the git process that the manager's
// caller (UpdateWithHooks) is about to start writes its references without
// fsync; when ok, end is called once that process has exited. It is false
// without a storage voter, so only the storage core's authoritative mode ever
// turns the fsync of references off.
func (m *GitLabHookManager) DeferReferenceWrites(ctx context.Context, repo *gitalypb.Repository) (end func(), ok bool) {
	if m.storageVoter == nil {
		return nil, false
	}
	return m.storageVoter.DeferReferenceWrites(ctx, repo)
}

// WithStorageVoter makes the manager commit every reference transaction to
// the storage core.
func WithStorageVoter(voter StorageVoter) ManagerOption {
	return func(m *GitLabHookManager) {
		m.storageVoter = voter
	}
}

// storageAuthoritative reports whether the storage core's database is the
// source of truth, so that the file changelog is not written.
func (m *GitLabHookManager) storageAuthoritative() bool {
	return m.storageVoter != nil && m.storageVoter.Authoritative()
}

// storagePreparing hands a transaction to the storage core before git locks
// its references (doc/graft/PLAN6.md, LOCK AND HISTORY RULE). It never fails:
// the Preparing phase only moves work out of the locked Prepared phase. A
// transaction that the standalone access policy denies is aborted in the
// Prepared phase, so nothing is uploaded for it.
func (m *GitLabHookManager) storagePreparing(ctx context.Context, payload gitcmd.HooksPayload, env []string, changes []byte) {
	if m.storageVoter == nil {
		return
	}

	if decision, err := m.graftPolicyDecision(m.policy.Current(), env, changes); err != nil || decision.denial != "" {
		return
	}

	m.storageVoter.Preparing(ctx, payload, changes)
}

// storagePrepared hands a prepared transaction to the storage core.
func (m *GitLabHookManager) storagePrepared(ctx context.Context, payload gitcmd.HooksPayload, env []string, changes []byte) error {
	if m.storageVoter == nil {
		return nil
	}

	meta := changelog.Meta{Operation: changelog.UnknownOperation}
	if graftMeta, err := gitcmd.ChangelogMetaFromEnv(env); err == nil {
		meta = changelogMetaOf(payload, graftMeta)
	} else if payload.UserDetails != nil {
		meta.Actor = payload.UserDetails.UserID
	}

	return m.storageVoter.Prepared(ctx, payload, changes, meta)
}

// changelogMetaOf is the changelog metadata of a transaction, shared by the
// file changelog and the storage core.
func changelogMetaOf(payload gitcmd.HooksPayload, graftMeta gitcmd.ChangelogMeta) changelog.Meta {
	meta := changelog.Meta{
		Operation:        graftMeta.Operation,
		Actor:            graftMeta.Actor,
		Rewritten:        graftMeta.Rewritten,
		Members:          graftMeta.Members,
		MembersTruncated: graftMeta.MembersTruncated,
		LandReference:    string(graftMeta.LandReference),
		Lands:            graftMeta.Lands,
	}
	if len(graftMeta.Lands) > 0 {
		meta.LandInvocation = graftMeta.Invocation
	}
	if meta.Operation == "" {
		meta.Operation = changelog.UnknownOperation
	}
	if meta.Actor == "" && payload.UserDetails != nil {
		meta.Actor = payload.UserDetails.UserID
	}
	return meta
}
