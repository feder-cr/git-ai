package updateref

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/transaction/txinfo"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// UpdaterWithHooks updates a ref with Git hooks.
type UpdaterWithHooks struct {
	cfg           config.Cfg
	logger        log.Logger
	locator       storage.Locator
	hookManager   hook.Manager
	gitCmdFactory gitcmd.CommandFactory
	catfileCache  catfile.Cache
}

// CustomHookError contains an error message when executing a custom hook.
type CustomHookError struct {
	err      error
	hookType gitcmd.Hook
	stdout   string
	stderr   string
}

// Error returns an error message.
func (e CustomHookError) Error() string {
	// If custom hooks write the "GitLab: " or "GL-HOOK-ERR: " prefix to either their stderr or
	// their stdout, then this prefix is taken as a hint by Rails to print the error as-is in
	// the web interface. We must thus make sure to not modify these custom hook error messages
	// at all. Ideally, this logic would be handled by the hook package, which would return an
	// error struct containing all necessary information. But the hook package does not manage
	// neither stderr nor stdout itself, and these may be directly connected to the user on a
	// clone. Given that we do use byte buffers here though, we do have enough information in
	// the updateref package to handle these custom hook errors.
	//
	// Eventually, we should find a solution which allows us to bubble up the error in hook
	// package such that we can also make proper use of structured errors for custom hooks.
	if len(strings.TrimSpace(e.stderr)) > 0 {
		return e.stderr
	}
	if len(strings.TrimSpace(e.stdout)) > 0 {
		return e.stdout
	}

	return e.err.Error()
}

// Proto returns the Protobuf representation of this error.
func (e CustomHookError) Proto() *gitalypb.CustomHookError {
	hookType := gitalypb.CustomHookError_HOOK_TYPE_UNSPECIFIED
	switch e.hookType {
	case gitcmd.PreReceiveHook:
		hookType = gitalypb.CustomHookError_HOOK_TYPE_PRERECEIVE
	case gitcmd.UpdateHook:
		hookType = gitalypb.CustomHookError_HOOK_TYPE_UPDATE
	case gitcmd.PostReceiveHook:
		hookType = gitalypb.CustomHookError_HOOK_TYPE_POSTRECEIVE
	}

	return &gitalypb.CustomHookError{
		HookType: hookType,
		Stdout:   []byte(e.stdout),
		Stderr:   []byte(e.stderr),
	}
}

// Unwrap will return the embedded error.
func (e CustomHookError) Unwrap() error {
	return e.err
}

// wrapHookError wraps errors returned by the hook manager into either a CustomHookError if it
// returned a `hook.CustomHookError`, or alternatively return the error with stderr or stdout
// appended to the message.
func wrapHookError(err error, hookType gitcmd.Hook, stdout, stderr string) error {
	var customHookErr hook.CustomHookError
	if errors.As(err, &customHookErr) {
		return CustomHookError{
			err:      err,
			hookType: hookType,
			stdout:   stdout,
			stderr:   stderr,
		}
	}

	if len(strings.TrimSpace(stderr)) > 0 {
		return fmt.Errorf("%w, stderr: %q", err, stderr)
	}
	if len(strings.TrimSpace(stdout)) > 0 {
		return fmt.Errorf("%w, stdout: %q", err, stdout)
	}

	return err
}

// Error reports an error in git update-ref
type Error struct {
	// Reference is the name of the reference that would have been updated.
	Reference git.ReferenceName
	// OldOID and NewOID are the expected object IDs previous to and after the update if it
	// would have succeeded.
	OldOID, NewOID git.ObjectID
	// Cause is an actual cause of the error.
	Cause error
}

// Unwrap returns Cause.
func (e Error) Unwrap() error {
	return e.Cause
}

func (e Error) Error() string {
	return fmt.Sprintf("reference update: %v", e.Cause.Error())
}

// Proto returns the Protobuf representation of this error.
func (e Error) Proto() *gitalypb.ReferenceUpdateError {
	return &gitalypb.ReferenceUpdateError{
		ReferenceName: []byte(e.Reference),
		OldOid:        string(e.OldOID),
		NewOid:        string(e.NewOID),
	}
}

// deferredDurability is implemented by a hook manager whose storage core writes the references
// of the transactions it commits without fsync (hook.GitLabHookManager).
type deferredDurability interface {
	DeferReferenceWrites(ctx context.Context, repo *gitalypb.Repository) (end func(), ok bool)
}

// deferReferenceWrites decides whether the update-ref process about to start for repo with
// hookManager writes its references without fsync: only when its storage voter says so. When
// ok, end is called once the process has exited.
func deferReferenceWrites(ctx context.Context, hookManager hook.Manager, repo *gitalypb.Repository) (end func(), ok bool) {
	d, isDeferring := hookManager.(deferredDurability)
	if !isDeferring {
		return nil, false
	}
	return d.DeferReferenceWrites(ctx, repo)
}

// NewUpdaterWithHooks creates a new instance of a struct that will update a Git reference.
func NewUpdaterWithHooks(
	cfg config.Cfg,
	logger log.Logger,
	locator storage.Locator,
	hookManager hook.Manager,
	gitCmdFactory gitcmd.CommandFactory,
	catfileCache catfile.Cache,
) *UpdaterWithHooks {
	return &UpdaterWithHooks{
		cfg:           cfg,
		logger:        logger,
		locator:       locator,
		hookManager:   hookManager,
		gitCmdFactory: gitCmdFactory,
		catfileCache:  catfileCache,
	}
}

// UpdateReference updates a branch with a given commit ID using the Git hooks. If a quarantine
// directory is given, then the pre-receive, update and reference-transaction hook will be invoked
// with the quarantined repository as returned by the quarantine structure. If these hooks succeed,
// quarantined objects will be migrated and all subsequent hooks are executed via the unquarantined
// repository. The hooks see the reference that is written: the target of a symbolic reference, not
// its name. An Error names the reference that was asked for.
func (u *UpdaterWithHooks) UpdateReference(
	ctx context.Context,
	repoProto *gitalypb.Repository,
	user *gitalypb.User,
	quarantineDir *quarantine.Dir,
	reference git.ReferenceName,
	newrev, oldrev git.ObjectID,
	pushOptions ...string,
) error {
	var transaction *txinfo.Transaction
	if tx, err := txinfo.TransactionFromContext(ctx); err == nil {
		transaction = &tx
	} else if !errors.Is(err, txinfo.ErrTransactionNotFound) {
		return fmt.Errorf("getting transaction: %w", err)
	}

	repo := u.localrepo(repoProto)

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	if reference == "" {
		return fmt.Errorf("reference cannot be empty")
	}
	if err := objectHash.ValidateHex(oldrev.String()); err != nil {
		return fmt.Errorf("validating old value: %w", err)
	}
	if err := objectHash.ValidateHex(newrev.String()); err != nil {
		return fmt.Errorf("validating new value: %w", err)
	}

	// A symbolic reference is written through to its target. git tells the reference-transaction
	// hook which reference it writes, but here that hook is driven by this function, so the name
	// the hooks judge has to be the one written. The target is read once, and every hook and the
	// update itself use it, with no dereference of their own: whatever the reference points to by
	// then, what the hooks allowed is what is written.
	written, err := repo.WrittenReference(ctx, reference)
	if err != nil {
		return fmt.Errorf("reading the reference to write: %w", err)
	}

	changes := fmt.Sprintf("%s %s %s\n", oldrev, newrev, written)

	if err := hook.CheckAgentReservation(ctx, changes); err != nil {
		return err
	}

	receiveHooksPayload := gitcmd.UserDetails{
		UserID:   user.GetGlId(),
		Username: user.GetGlUsername(),
		Protocol: "web",
	}

	// In case there's no quarantine directory, we simply take the normal unquarantined
	// repository as input for the hooks payload. Otherwise, we'll take the quarantined
	// repository, which carries information about the quarantined object directory. This is
	// then subsequently passed to Rails, which can use the quarantine directory to more
	// efficiently query which objects are new.
	quarantinedRepo := repoProto
	if quarantineDir != nil {
		quarantinedRepo = quarantineDir.QuarantinedRepo()
	}

	// This function drives the reference-transaction hook of one transaction itself. The
	// changeset log tells it apart from every other transaction, also from one with the same
	// updates, by a value unique to this call, as it does for a git process that runs the hook.
	var invocation string
	if gitcmd.InvocationRequired(u.cfg) {
		if invocation, err = gitcmd.NewChangelogInvocation(); err != nil {
			return err
		}
	}
	// This call owns the transaction as a git process that runs the hook owns its own. It ends
	// after every other deferred step of this function, however the function ends, and tells
	// the storage core that no phase of the transaction comes any more.
	defer gitcmd.BeginInvocation(invocation)()
	newHooksPayload := func(repo *gitalypb.Repository) (string, error) {
		payload := gitcmd.NewHooksPayload(ctx, u.cfg, repo, objectHash, transaction, &receiveHooksPayload, gitcmd.ReceivePackHooks, featureflag.FromContext(ctx))
		payload.Changelog.Invocation = invocation
		return payload.Env()
	}

	hooksPayload, err := newHooksPayload(quarantinedRepo)
	if err != nil {
		return fmt.Errorf("constructing hooks payload: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := u.hookManager.PreReceiveHook(ctx, quarantinedRepo, pushOptions, []string{hooksPayload}, strings.NewReader(changes), &stdout, &stderr); err != nil {
		return fmt.Errorf("running pre-receive hooks: %w", wrapHookError(err, gitcmd.PreReceiveHook, stdout.String(), stderr.String()))
	}

	// Now that Rails has told us that the change is okay via the pre-receive hook, we can
	// migrate any potentially quarantined objects into the main repository. This must happen
	// before we start updating the refs because git-update-ref(1) will verify that it got all
	// referenced objects available.
	if quarantineDir != nil {
		if err := quarantineDir.Migrate(ctx); err != nil {
			return fmt.Errorf("migrating quarantined objects: %w", err)
		}

		// We only need to update the hooks payload to the unquarantined repo in case we
		// had a quarantine environment. Otherwise, the initial hooks payload is for the
		// real repository anyway.
		hooksPayload, err = newHooksPayload(repoProto)
		if err != nil {
			return fmt.Errorf("constructing quarantined hooks payload: %w", err)
		}
	}

	if err := u.hookManager.UpdateHook(ctx, quarantinedRepo, written.String(), oldrev.String(), newrev.String(), []string{hooksPayload}, &stdout, &stderr); err != nil {
		// The storage core refused the name in the update hook, before the transaction: the
		// same refusal as in the prepared phase below.
		var fileDirectoryErr updateref.FileDirectoryConflictError
		if errors.As(err, &fileDirectoryErr) {
			return Error{
				Reference: reference,
				OldOID:    oldrev,
				NewOID:    newrev,
				Cause:     err,
			}
		}
		return fmt.Errorf("running update hooks: %w", wrapHookError(err, gitcmd.UpdateHook, stdout.String(), stderr.String()))
	}

	// We are already manually invoking the reference-transaction hook, so there is no need to
	// set up hooks again here. One could argue that it would be easier to just have git handle
	// execution of the reference-transaction hook. But unfortunately, it has proven to be
	// problematic: if we queue a deletion, and the reference to be deleted exists both as
	// packed-ref and as loose ref, then we would see two transactions: first a transaction
	// deleting the packed-ref which would otherwise get unshadowed by deleting the loose ref,
	// and only then do we see the deletion of the loose ref. So this depends on how well a repo
	// is packed, which is obviously a bad thing as Gitaly nodes may be differently packed. We
	// thus continue to manually drive the reference-transaction hook here, which doesn't have
	// this problem.
	updaterOpts := []updateref.UpdaterOpt{updateref.WithDisabledTransactions(), updateref.WithNoDeref()}
	// The storage core's authoritative mode makes the references durable itself: its database
	// commit in the prepared phase is the durable point of the transaction. It counts the
	// update-ref process as writing without fsync until the process has exited (end runs after
	// updater.Close, which waits for it).
	if end, ok := deferReferenceWrites(ctx, u.hookManager, repoProto); ok {
		defer end()
		updaterOpts = append(updaterOpts, updateref.WithDeferredReferenceDurability())
	}
	updater, err := updateref.New(ctx, repo, updaterOpts...)
	if err != nil {
		return fmt.Errorf("creating updater: %w", err)
	}

	// We need to explicitly cancel the update here such that we release the lock when this
	// function exits if there is any error between locking and committing.
	defer func() { _ = updater.Close() }()

	if err := updater.Start(); err != nil {
		return fmt.Errorf("start reference transaction: %w", err)
	}

	if err := updater.Update(written, newrev, oldrev); err != nil {
		return fmt.Errorf("queueing ref update: %w", err)
	}

	if err := u.hookManager.ReferenceTransactionHook(ctx, hook.ReferenceTransactionPreparing, []string{hooksPayload}, strings.NewReader(changes)); err != nil {
		return fmt.Errorf("executing pre-locked reference-transaction hook: %w", err)
	}

	// abortTransaction reports the abort of the transaction to the reference-transaction hook,
	// so that the hook releases what it set up for the transaction in an earlier phase. Like
	// Git does for a transaction that it aborts, the error is only logged: the caller already
	// has an error to return.
	abortTransaction := func() {
		if abortErr := u.hookManager.ReferenceTransactionHook(ctx, hook.ReferenceTransactionAborted, []string{hooksPayload}, strings.NewReader(changes)); abortErr != nil {
			u.logger.WithError(abortErr).ErrorContext(ctx, "executing aborting reference-transaction hook")
		}
	}

	// We need to lock the reference before executing the reference-transaction hook such that
	// there cannot be any concurrent modification.
	if err := updater.Prepare(); err != nil {
		abortTransaction()

		return Error{
			Reference: reference,
			OldOID:    oldrev,
			NewOID:    newrev,
			Cause:     err,
		}
	}

	if err := u.hookManager.ReferenceTransactionHook(ctx, hook.ReferenceTransactionPrepared, []string{hooksPayload}, strings.NewReader(changes)); err != nil {
		abortTransaction()

		// The storage core refused the name: the same refusal as git's file/directory
		// conflict, which git could not report because the other reference is not in
		// this node's local copy yet. Callers treat both the same way.
		var fileDirectoryErr updateref.FileDirectoryConflictError
		if errors.As(err, &fileDirectoryErr) {
			return Error{
				Reference: reference,
				OldOID:    oldrev,
				NewOID:    newrev,
				Cause:     err,
			}
		}

		return fmt.Errorf("executing preparatory reference-transaction hook: %w", err)
	}

	if err := updater.Commit(); err != nil {
		// The transaction was prepared but not committed. Like Git does for a transaction
		// that it aborts after it was prepared, report the abort so that the hook releases
		// what it set up for the transaction in the prepared phase.
		abortTransaction()

		return Error{
			Reference: reference,
			OldOID:    oldrev,
			NewOID:    newrev,
			Cause:     err,
		}
	}

	if err := u.hookManager.ReferenceTransactionHook(ctx, hook.ReferenceTransactionCommitted, []string{hooksPayload}, strings.NewReader(changes)); err != nil {
		return fmt.Errorf("executing committing reference-transaction hook: %w", err)
	}

	if err := u.hookManager.PostReceiveHook(ctx, repoProto, pushOptions, []string{hooksPayload}, strings.NewReader(changes), &stdout, &stderr); err != nil {
		// CustomHook errors are returned in case a custom hook has returned an error code.
		// The post-receive hook has special semantics though. Quoting githooks(5):
		//
		//    This hook does not affect the outcome of git receive-pack, as it is called
		//    after the real work is done.
		//
		// This means that even if the hook returns an error, then that error should not
		// impact whatever git-receive-pack(1) has been doing. And given that we emulate
		// behaviour of this command here, we need to behave the same.
		var customHookErr hook.CustomHookError
		if errors.As(err, &customHookErr) {
			// Only log the error when we've got a custom-hook error, but otherwise
			// ignore it and continue with whatever we have been doing.
			u.logger.WithError(err).ErrorContext(ctx, "custom post-receive hook returned an error")
		} else {
			return fmt.Errorf("running post-receive hooks: %w", wrapHookError(err, gitcmd.PostReceiveHook, stdout.String(), stderr.String()))
		}
	}

	return nil
}

func (u *UpdaterWithHooks) localrepo(repo storage.Repository) *localrepo.Repo {
	return localrepo.New(u.logger, u.locator, u.gitCmdFactory, u.catfileCache, repo)
}
