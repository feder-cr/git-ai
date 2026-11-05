package updateref

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/structerr"
)

var (
	// errClosed is returned when accessing an updater that has already been closed.
	errClosed = errors.New("closed")
	// ErrPackedRefsLocked indicates an operation failed due to the 'packed-refs' being locked. This is
	// the case if either `packed-refs.new` or `packed-refs.lock` exists in the repository.
	ErrPackedRefsLocked = errors.New("packed-refs locked")
)

// MultipleUpdatesError indicates that a reference cannot have multiple updates
// within the same transaction.
type MultipleUpdatesError struct {
	// ReferenceName is the name of the reference that has multiple updates.
	ReferenceName string
}

func (e MultipleUpdatesError) Error() string {
	return "reference has been updated multiple times within a transaction"
}

// ErrorMetadata implements the `structerr.ErrorMetadater` interface and provides the name of the reference that
// has multiple updates.
func (e MultipleUpdatesError) ErrorMetadata() []structerr.MetadataItem {
	return []structerr.MetadataItem{
		{Key: "reference", Value: e.ReferenceName},
	}
}

// AlreadyLockedError indicates a reference cannot be locked because another
// process has already locked it.
type AlreadyLockedError struct {
	// ReferenceName is the name of the reference that is already locked.
	ReferenceName string
}

func (e AlreadyLockedError) Error() string {
	return "reference is already locked"
}

// ErrorMetadata implements the `structerr.ErrorMetadater` interface and provides the name of the reference that was
// locked already.
func (e AlreadyLockedError) ErrorMetadata() []structerr.MetadataItem {
	return []structerr.MetadataItem{
		{Key: "reference", Value: e.ReferenceName},
	}
}

// ReferenceAlreadyExistsError is returned when attempting to create a reference
// that already exists.
type ReferenceAlreadyExistsError struct {
	// ReferenceName is the name of the reference that already exists.
	ReferenceName string
}

func (e ReferenceAlreadyExistsError) Error() string {
	return "reference already exists"
}

// ErrorMetadata implements the `structerr.ErrorMetadater` interface and provides the name of the
// reference that already existed.
func (e ReferenceAlreadyExistsError) ErrorMetadata() []structerr.MetadataItem {
	return []structerr.MetadataItem{
		{Key: "reference", Value: e.ReferenceName},
	}
}

// InvalidReferenceFormatError indicates a reference name was invalid.
type InvalidReferenceFormatError struct {
	// ReferenceName is the invalid reference name.
	ReferenceName string
}

func (e InvalidReferenceFormatError) Error() string {
	return "invalid reference format"
}

// ErrorMetadata implements the `structerr.ErrorMetadater` interface and provides the name of the reference that was
// invalid.
func (e InvalidReferenceFormatError) ErrorMetadata() []structerr.MetadataItem {
	return []structerr.MetadataItem{
		{Key: "reference", Value: e.ReferenceName},
	}
}

// FileDirectoryConflictError is returned when an operation would causes a file-directory conflict
// in the reference store.
type FileDirectoryConflictError struct {
	// ConflictingReferenceName is the name of the reference that would have conflicted.
	ConflictingReferenceName string
	// ExistingReferenceName is the name of the already existing reference.
	ExistingReferenceName string
}

func (e FileDirectoryConflictError) Error() string {
	return "file directory conflict"
}

// ErrorMetadata implements the `structerr.ErrorMetadater` interface and provides the name of preexisting and
// conflicting reference names.
func (e FileDirectoryConflictError) ErrorMetadata() []structerr.MetadataItem {
	return []structerr.MetadataItem{
		{Key: "conflicting_reference", Value: e.ConflictingReferenceName},
		{Key: "existing_reference", Value: e.ExistingReferenceName},
	}
}

// InTransactionConflictError is returned when creating two F/D or D/F conflicting references
// in the same transaction. For example creation of 'refs/heads/parent' and creation of
// 'refs/heads/parent/child' is not allowed in the same transaction.
type InTransactionConflictError struct {
	// FirstReferenceName is the name of the first reference that was created.
	FirstReferenceName string
	// SecondReferenceName is the name of the second reference that was created.
	SecondReferenceName string
}

func (e InTransactionConflictError) Error() string {
	return "conflicting reference updates in the same transaction"
}

// ErrorMetadata implements the `structerr.ErrorMetadater` interface and provides the name of the first and second
// conflicting reference names.
func (e InTransactionConflictError) ErrorMetadata() []structerr.MetadataItem {
	return []structerr.MetadataItem{
		{Key: "first_reference", Value: e.FirstReferenceName},
		{Key: "second_reference", Value: e.SecondReferenceName},
	}
}

// NonExistentObjectError is returned when attempting to point a reference to an object that does not
// exist in the object database.
type NonExistentObjectError struct {
	// ReferenceName is the name of the reference that was being updated.
	ReferenceName string
	// ObjectID is the object ID of the non-existent object.
	ObjectID string
}

func (e NonExistentObjectError) Error() string {
	return "target object missing"
}

// ErrorMetadata implements the `structerr.ErrorMetadater` interface and provides the missing object as well as the
// reference that should have been updated to point to it.
func (e NonExistentObjectError) ErrorMetadata() []structerr.MetadataItem {
	return []structerr.MetadataItem{
		{Key: "reference", Value: e.ReferenceName},
		{Key: "missing_object", Value: e.ObjectID},
	}
}

// NonCommitObjectError is returned when attempting to point a branch to an object that is not an object.
type NonCommitObjectError struct {
	// ReferenceName is the name of the branch that was being updated.
	ReferenceName string
	// ObjectID is the object ID of the non-commit object.
	ObjectID string
}

func (e NonCommitObjectError) Error() string {
	return "target object not a commit"
}

// ErrorMetadata implements the `structerr.ErrorMetadater` interface and provides the object that is not a commit as
// well as the reference that should have been updated to point to it.
func (e NonCommitObjectError) ErrorMetadata() []structerr.MetadataItem {
	return []structerr.MetadataItem{
		{Key: "reference", Value: e.ReferenceName},
		{Key: "non_commit_object", Value: e.ObjectID},
	}
}

// MismatchingStateError is returned when attempting to update a reference where the expected object ID does not match
// the actual object ID that the reference currently points to.
type MismatchingStateError struct {
	// ReferenceName is the name of the reference that was being updated.
	ReferenceName string
	// ExpectedObjectID is the expected object ID as specified by the caller.
	ExpectedObjectID string
	// ActualObjectID is the actual object ID that the reference was pointing to.
	ActualObjectID string
}

func (e MismatchingStateError) Error() string {
	return "reference does not point to expected object"
}

// ErrorMetadata implements the `structerr.ErrorMetadater` interface and provides error metadata about the expected and
// actual object ID of the failed reference update.
func (e MismatchingStateError) ErrorMetadata() []structerr.MetadataItem {
	return []structerr.MetadataItem{
		{Key: "reference", Value: e.ReferenceName},
		{Key: "expected_object_id", Value: e.ExpectedObjectID},
		{Key: "actual_object_id", Value: e.ActualObjectID},
	}
}

// state represents a possible state the updater can be in.
type state string

const (
	// stateIdle means the updater is ready for a new transaction to start.
	stateIdle state = "idle"
	// stateStarted means the updater has an open transaction and accepts
	// new reference changes.
	stateStarted state = "started"
	// statePrepared means the updater has prepared a transaction and no longer
	// accepts reference changes until the current transaction is committed and
	// a new one started.
	statePrepared state = "prepared"
)

// invalidStateTransitionError is returned when the updater is used incorrectly.
type invalidStateTransitionError struct {
	// expected is the state the updater was expected to be in.
	expected state
	// actual is the state the updater was actually in.
	actual state
}

// Error returns the formatted error string.
func (err invalidStateTransitionError) Error() string {
	return fmt.Sprintf("expected state %q but it was %q", err.expected, err.actual)
}

// Updater wraps a `git update-ref --stdin` process, presenting an interface
// that allows references to be easily updated in bulk. It is not suitable for
// concurrent use.
//
// Correct usage of the Updater is as follows:
//  1. Transaction must be started before anything else.
//  2. Transaction can't be started if there is an active transaction.
//  3. Updates can be staged only when there is an unprepared transaction.
//  4. Prepare can be called only with an unprepared transaction.
//  5. Commit can be called only with an active transaction. The transaction
//     can be committed unprepared or prepared.
//  7. Close can be called at any time. The active transaction is aborted.
//  8. Any sort of error causes the updater to close.
type Updater struct {
	repo             gitcmd.RepositoryExecutor
	cmd              *command.Command
	closeErr         error
	stdout           *bufio.Reader
	stderr           *bytes.Buffer
	objectHash       git.ObjectHash
	referenceBackend git.ReferenceBackend
	ctx              context.Context

	// copiesExistingReferences is set by WithExistingReferences.
	copiesExistingReferences bool

	// state tracks the current state of the updater to ensure correct calling semantics.
	state state
}

// UpdaterOpt is a type representing options for the Updater.
type UpdaterOpt func(*updaterConfig)

type updaterConfig struct {
	disableTransactions bool
	noDeref             bool
	deferredDurability  bool
	existingReferences  bool
	refBackend          *git.ReferenceBackend
	refURI              string
}

// WithExistingReferences lifts the limit on the length of reference names, for a caller that copies
// references that already exist from one store of the repository to another, rather than taking a new
// name from anyone: a rebuild from the storage core's database, a migration of the reference backend.
// Refusing there would make a repository with an existing long reference impossible to rebuild,
// migrate or clean up.
func WithExistingReferences() UpdaterOpt {
	return func(cfg *updaterConfig) {
		cfg.existingReferences = true
	}
}

// WithDeferredReferenceDurability makes git write the references without fsync. Only the
// storage core's authoritative mode may set it: its database commit is the durable point of
// the transaction, and it makes the local references durable itself (doc/graft/PLAN7.md,
// LOCAL REFERENCE WRITES RULE). Objects stay durable as for every other write.
func WithDeferredReferenceDurability() UpdaterOpt {
	return func(cfg *updaterConfig) {
		cfg.deferredDurability = true
	}
}

// WithDisabledTransactions disables hooks such that no reference-transactions
// are used for the updater.
func WithDisabledTransactions() UpdaterOpt {
	return func(cfg *updaterConfig) {
		cfg.disableTransactions = true
	}
}

// WithRefURI directs all reference operations to an alternate reference store
// of the given backend at path, overriding the repository's own via the
// GIT_REFERENCE_BACKEND environment variable.
func WithRefURI(backend git.ReferenceBackend, path string) UpdaterOpt {
	return func(cfg *updaterConfig) {
		cfg.refBackend = &backend
		cfg.refURI = git.CreateRefURI(backend, path)
	}
}

// WithNoDeref disables de-reference while updating ref. If this option is turned on,
// <ref> itself is overwritten, rather than the result of following the symbolic ref.
func WithNoDeref() UpdaterOpt {
	return func(cfg *updaterConfig) {
		cfg.noDeref = true
	}
}

// New returns a new bulk updater, wrapping a `git update-ref` process. Call the
// various methods to enqueue updates, then call Commit() to attempt to apply all
// the updates at once.
//
// It is important that ctx gets canceled somewhere. If it doesn't, the process
// spawned by New() may never terminate.
func New(ctx context.Context, repo gitcmd.RepositoryExecutor, opts ...UpdaterOpt) (*Updater, error) {
	var cfg updaterConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	txOption := gitcmd.WithRefTxHook(objectHash, repo)
	if cfg.disableTransactions {
		txOption = gitcmd.WithDisabledHooks()
	}

	cmdFlags := []gitcmd.Option{gitcmd.Flag{Name: "-z"}, gitcmd.Flag{Name: "--stdin"}}
	if cfg.noDeref {
		cmdFlags = append(cmdFlags, gitcmd.Flag{Name: "--no-deref"})
	}

	var stderr bytes.Buffer
	cmdOpts := []gitcmd.CmdOpt{
		txOption,
		gitcmd.WithSetupStdin(),
		gitcmd.WithSetupStdout(),
		gitcmd.WithStderr(&stderr),
	}
	if cfg.refURI != "" {
		cmdOpts = append(cmdOpts, gitcmd.WithEnv("GIT_REFERENCE_BACKEND="+cfg.refURI))
	}
	if cfg.deferredDurability {
		cmdOpts = append(cmdOpts, gitcmd.WithConfig(gitcmd.ConfigPair{Key: "core.fsync", Value: gitcmd.FsyncComponentsWithoutReferences}))
	}

	cmd, err := repo.Exec(
		ctx,
		gitcmd.Command{
			Name:  "update-ref",
			Flags: cmdFlags,
		},
		cmdOpts...,
	)
	if err != nil {
		return nil, err
	}

	// Errors are parsed with the backend git actually writes to, as error
	// messages differ between backends.
	var referenceBackend git.ReferenceBackend
	if cfg.refBackend != nil {
		referenceBackend = *cfg.refBackend
	} else if referenceBackend, err = repo.ReferenceBackend(ctx); err != nil {
		return nil, fmt.Errorf("detecting reference backend: %w", err)
	}

	return &Updater{
		repo:             repo,
		cmd:              cmd,
		stderr:           &stderr,
		stdout:           bufio.NewReader(cmd),
		objectHash:       objectHash,
		referenceBackend: referenceBackend,
		state:            stateIdle,
		ctx:              ctx,

		copiesExistingReferences: cfg.existingReferences,
	}, nil
}

// expectState returns an error and closes the updater if it is not in the expected state.
func (u *Updater) expectState(expected state) error {
	if u.closeErr != nil {
		return u.closeErr
	}

	if err := u.checkState(expected); err != nil {
		return u.closeWithError(err)
	}

	return nil
}

// checkState returns an error if the updater is not in the expected state.
func (u *Updater) checkState(expected state) error {
	if u.state != expected {
		return invalidStateTransitionError{expected: expected, actual: u.state}
	}

	return nil
}

// Start begins a new reference transaction. The reference changes are not performed until Commit
// is explicitly called.
func (u *Updater) Start() error {
	if err := u.expectState(stateIdle); err != nil {
		return err
	}

	u.state = stateStarted

	return u.setState("start")
}

// Update commands the reference to be updated to point at the object ID specified in newOID. If
// newOID is the zero OID, then the branch will be deleted. If oldOID is a non-empty string, then
// the reference will only be updated if its current value matches the old value. If the old value
// is the zero OID, then the branch must not exist.
//
// A reference transaction must be started before calling Update.
func (u *Updater) Update(reference git.ReferenceName, newOID, oldOID git.ObjectID) error {
	if err := u.expectState(stateStarted); err != nil {
		return err
	}

	// A deletion is exempt: it is how an existing long reference is cleaned up.
	if !u.objectHash.IsZeroOID(newOID) {
		if err := u.validateNameLength(reference); err != nil {
			return err
		}
	}

	return u.write("update %s\x00%s\x00%s\x00", reference.String(), newOID, oldOID)
}

// UpdateSymbolicReference is used to do a symbolic reference update. We can potentially provide the oldTarget
// or the oldOID.
func (u *Updater) UpdateSymbolicReference(reference, newTarget git.ReferenceName) error {
	if err := u.expectState(stateStarted); err != nil {
		return err
	}

	if err := u.validateNameLength(reference); err != nil {
		return err
	}
	if err := u.validateNameLength(newTarget); err != nil {
		return err
	}

	return u.write("symref-update %s\x00%s\x00\x00\x00", reference.String(), newTarget)
}

// Create commands the reference to be created with the given object ID. The ref must not exist.
//
// A reference transaction must be started before calling Create.
func (u *Updater) Create(reference git.ReferenceName, oid git.ObjectID) error {
	return u.Update(reference, oid, u.objectHash.ZeroOID)
}

// Delete commands the reference to be removed from the repository. This command will ignore any old
// state of the reference and just force-remove it.
//
// A reference transaction must be started before calling Delete.
func (u *Updater) Delete(reference git.ReferenceName) error {
	return u.Update(reference, u.objectHash.ZeroOID, "")
}

// Prepare prepares the reference transaction by locking all references and determining their
// current values. The updates are not yet committed and will be rolled back in case there is no
// call to `Commit()`. This call is optional.
func (u *Updater) Prepare() error {
	if err := u.expectState(stateStarted); err != nil {
		return err
	}

	u.state = statePrepared

	return u.setState("prepare")
}

// Commit applies the commands specified in other calls to the Updater. Commit finishes the
// reference transaction and another one must be started before further changes can be staged.
func (u *Updater) Commit() error {
	// Commit can be called without preparing the transactions.
	if err := u.checkState(statePrepared); err != nil {
		if err := u.expectState(stateStarted); err != nil {
			return err
		}
	}

	u.state = stateIdle

	if err := u.setState("commit"); err != nil {
		return err
	}

	return nil
}

// Close closes the updater and aborts a possible open transaction. No changes will be written
// to disk, all lockfiles will be cleaned up and the process will exit.
func (u *Updater) Close() error {
	return u.closeWithError(nil)
}

// closeWithError closes the updater with the given error. The passed in error is only used
// if the updater closes successfully. This is used to close the Updater with errors raised
// by our logic when the command itself hasn't errored. All subsequent method calls return
// the error returned from first closeWithError call.
func (u *Updater) closeWithError(closeErr error) error {
	if u.closeErr != nil {
		return u.closeErr
	}

	if err := u.cmd.Wait(); err != nil {
		err = structerr.New("%w", err).WithMetadataItems(
			structerr.MetadataItem{Key: "stderr", Value: u.stderr.String()},
			structerr.MetadataItem{Key: "close_error", Value: closeErr},
		)
		if parsedErr := u.parseStderr(); parsedErr != nil {
			// If stderr contained a specific error, return it instead.
			err = parsedErr
		}

		u.closeErr = err
		return err
	}

	if u.ctx.Err() != nil {
		u.closeErr = u.ctx.Err()
		return u.closeErr
	}

	if closeErr != nil {
		u.closeErr = closeErr
		return closeErr
	}

	u.closeErr = errClosed
	return nil
}

// validateNameLength refuses a name over the limit before git-update-ref(1) sees it. Like any error
// of the updater it closes the updater, which aborts the transaction and everything queued in it.
func (u *Updater) validateNameLength(name git.ReferenceName) error {
	if u.copiesExistingReferences {
		return nil
	}

	if err := git.ValidateReferenceNameLength(name.String()); err != nil {
		return u.closeWithError(structerr.NewInvalidArgument("%w", err))
	}

	return nil
}

func (u *Updater) write(format string, args ...interface{}) error {
	if _, err := fmt.Fprintf(u.cmd, format, args...); err != nil {
		return u.closeWithError(err)
	}

	return nil
}

func (u *Updater) setState(state string) error {
	if err := u.write("%s\x00", state); err != nil {
		return err
	}

	// For each state-changing command, git-update-ref(1) will report successful execution via
	// "<command>: ok" lines printed to its stdout. Ideally, we should thus verify here whether
	// the command was successfully executed by checking for exactly this line, otherwise we
	// cannot be sure whether the command has correctly been processed by Git or if an error was
	// raised.
	line, err := u.stdout.ReadString('\n')
	if err != nil {
		return u.closeWithError(fmt.Errorf("state update to %q failed: %w", state, err))
	}

	if line != fmt.Sprintf("%s: ok\n", state) {
		return u.closeWithError(fmt.Errorf("state update to %q not successful: expected ok, got %q", state, line))
	}

	return nil
}

func (u *Updater) parseStderr() error {
	stderr := u.stderr.Bytes()

	matches := u.referenceBackend.RefLockedRegex.FindSubmatch(stderr)
	// Reftable locks are on an entire table instead of per reference, so
	// git doesn't output the name of the individual ref. The MVCC backend is derived from
	// reftables and reports its locks the same way. The backends are compared by name because
	// the MVCC backend shares the reftable regexes but is not equal to ReferenceBackendReftables.
	isTableBackend := u.referenceBackend.Name == git.ReferenceBackendReftables.Name ||
		u.referenceBackend.Name == git.ReferenceBackendMVCC.Name
	if isTableBackend && len(matches) == 2 {
		return AlreadyLockedError{}
	} else if len(matches) > 2 {
		return AlreadyLockedError{ReferenceName: string(matches[2])}
	}

	matches = u.referenceBackend.PackedRefsLockedRegex.FindSubmatch(stderr)
	if len(matches) > 1 {
		return ErrPackedRefsLocked
	}

	matches = u.referenceBackend.RefInvalidFormatRegex.FindSubmatch(stderr)
	if len(matches) > 1 {
		return InvalidReferenceFormatError{ReferenceName: string(matches[1])}
	}

	matches = u.referenceBackend.ReferenceExistsConflictRegex.FindSubmatch(stderr)
	if len(matches) > 1 {
		return FileDirectoryConflictError{
			ExistingReferenceName:    string(matches[1]),
			ConflictingReferenceName: string(matches[2]),
		}
	}

	matches = u.referenceBackend.InTransactionConflictRegex.FindSubmatch(stderr)
	if len(matches) > 1 {
		return InTransactionConflictError{
			FirstReferenceName:  string(matches[1]),
			SecondReferenceName: string(matches[2]),
		}
	}

	matches = u.referenceBackend.NonExistentObjectRegex.FindSubmatch(stderr)
	if len(matches) > 1 {
		return NonExistentObjectError{
			ReferenceName: string(matches[1]),
			ObjectID:      string(matches[2]),
		}
	}

	matches = u.referenceBackend.NonCommitObjectRegex.FindSubmatch(stderr)
	if len(matches) > 1 {
		return NonCommitObjectError{
			ReferenceName: string(matches[2]),
			ObjectID:      string(matches[1]),
		}
	}

	matches = u.referenceBackend.MismatchingStateRegex.FindSubmatch(stderr)
	if len(matches) > 2 {
		return MismatchingStateError{
			ReferenceName:    string(matches[1]),
			ExpectedObjectID: string(matches[3]),
			ActualObjectID:   string(matches[2]),
		}
	}

	matches = u.referenceBackend.ReferenceAlreadyExistsRegex.FindSubmatch(stderr)
	if len(matches) > 1 {
		return ReferenceAlreadyExistsError{
			ReferenceName: string(matches[1]),
		}
	}

	matches = u.referenceBackend.MultipleUpdatesRegex.FindSubmatch(stderr)
	if len(matches) > 1 {
		return MultipleUpdatesError{
			ReferenceName: string(matches[1]),
		}
	}

	return nil
}
