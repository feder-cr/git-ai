package localrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// ErrMismatchingState is similar to updateref.MismatchingStateError. It's declared separately to avoid
// an import cycle and to allow us to compare error types in the FetchSourceBranch RPC.
var ErrMismatchingState = errors.New("reference does not point to expected object")

// HasRevision checks if a revision in the repository exists. This will not
// verify whether the target object exists. To do so, you can peel the revision
// to a given object type, e.g. by passing `refs/heads/master^{commit}`.
func (repo *Repo) HasRevision(ctx context.Context, revision git.Revision) (bool, error) {
	if _, err := repo.ResolveRevision(ctx, revision); err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ResolveRevision resolves the given revision to its object ID. This will not
// verify whether the target object exists. To do so, you can peel the
// reference to a given object type, e.g. by passing
// `refs/heads/master^{commit}`. Returns an ErrReferenceNotFound error in case
// the revision does not exist.
func (repo *Repo) ResolveRevision(ctx context.Context, revision git.Revision) (git.ObjectID, error) {
	if revision.String() == "" {
		return "", errors.New("repository cannot contain empty reference name")
	}

	var stdout bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "rev-parse",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--verify"}},
			Args:  []string{revision.String()},
		},
		gitcmd.WithStderr(io.Discard),
		gitcmd.WithStdout(&stdout),
	); err != nil {
		if _, ok := command.ExitStatus(err); ok {
			return "", git.ErrReferenceNotFound
		}
		return "", err
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", fmt.Errorf("detecting object hash: %w", err)
	}

	hex := strings.TrimSpace(stdout.String())
	oid, err := objectHash.FromHex(hex)
	if err != nil {
		return "", fmt.Errorf("unsupported object hash %q: %w", hex, err)
	}

	return oid, nil
}

// RequestObjectReader returns the object reader of a request that resolves revisions and reads
// objects through one `git cat-file --batch-command` process: one process for the whole request,
// where ResolveRevision starts one per revision (PLAN9, WP96). The returned function must be
// called on every path once the request is done with the reader.
func (repo *Repo) RequestObjectReader(ctx context.Context) (catfile.ObjectReader, func(), error) {
	contentReader, cancel, err := repo.catfileCache.ObjectReader(ctx, repo)
	if err != nil {
		return nil, nil, fmt.Errorf("creating object reader: %w", err)
	}

	reader, ok := contentReader.(catfile.ObjectReader)
	if !ok {
		cancel()
		return nil, nil, fmt.Errorf("object reader %T cannot read object info", contentReader)
	}

	return reader, cancel, nil
}

// ResolveRevisionWithReader is ResolveRevision through reader, the request's object reader: the
// same object ID, or git.ErrReferenceNotFound when the revision does not resolve. Unlike
// ResolveRevision it also requires the object to exist, which a revision peeled to an object type
// (`<rev>^{commit}`) requires anyway: pass only such revisions where ResolveRevision is replaced.
func (repo *Repo) ResolveRevisionWithReader(ctx context.Context, reader catfile.ObjectInfoReader, revision git.Revision) (git.ObjectID, error) {
	if revision.String() == "" {
		return "", errors.New("repository cannot contain empty reference name")
	}

	info, err := reader.Info(ctx, revision)
	if err != nil {
		if errors.As(err, &catfile.NotFoundError{}) {
			return "", git.ErrReferenceNotFound
		}
		return "", err
	}

	return info.Oid, nil
}

// ResolveBranchesExactly returns the commit of each of branches that exists under its exact
// reference name, read through reader, the request's object reader. A branch that does not exist,
// or that does not peel to a commit, is left out of the map.
//
// git resolves a name with its DWIM rules, so a missing refs/heads/x resolves through
// refs/refs/heads/x, refs/tags/refs/heads/x or refs/remotes/refs/heads/x when one of them exists.
// The exact name is the first name git tries, so a branch that reader cannot resolve does not
// exist exactly either, and costs nothing more. Each branch reader resolves is read by its exact
// name (ResolveReferenceExactly), and reader then peels its value.
func (repo *Repo) ResolveBranchesExactly(ctx context.Context, reader catfile.ObjectInfoReader, branches ...git.ReferenceName) (map[git.ReferenceName]git.ObjectID, error) {
	resolved := make(map[git.ReferenceName]git.ObjectID, len(branches))
	seen := make(map[git.ReferenceName]bool, len(branches))
	for _, branch := range branches {
		if seen[branch] {
			continue
		}
		seen[branch] = true

		if _, err := repo.ResolveRevisionWithReader(ctx, reader, branch.Revision()+"^{commit}"); err != nil {
			if errors.Is(err, git.ErrReferenceNotFound) {
				continue
			}
			return nil, fmt.Errorf("resolving %q: %w", branch, err)
		}

		value, err := repo.ResolveReferenceExactly(ctx, branch)
		if err != nil {
			if errors.Is(err, git.ErrReferenceNotFound) {
				continue
			}
			return nil, err
		}

		// The value of the reference itself, peeled to a commit as `<branch>^{commit}` is.
		commit, err := repo.ResolveRevisionWithReader(ctx, reader, value.Revision()+"^{commit}")
		if err != nil {
			if errors.Is(err, git.ErrReferenceNotFound) {
				continue
			}
			return nil, fmt.Errorf("peeling %q: %w", branch, err)
		}
		resolved[branch] = commit
	}

	return resolved, nil
}

// ResolveReferenceExactly returns the object ID that reference points to, read by its exact name
// with `git show-ref --verify`, or git.ErrReferenceNotFound when it does not exist (also when
// references below its name do, refs/heads/<x>/y, and when the name is not a valid reference name,
// such as one with glob characters or a revision like main~1). Unlike ResolveRevision, whose
// revision git resolves with its DWIM rules, a missing refs/heads/<x> never resolves through
// refs/tags/refs/heads/<x> or another reference. The value is not peeled: it is the value that a
// compare-and-swap of the reference expects, as `git rev-parse <reference>` returns it when the
// reference exists. A symbolic reference yields the object its target points to.
// ResolveBranchesExactly is the peeled form, for the callers that want the commit.
//
// show-ref --verify reads the one reference, where for-each-ref <reference> lists every loose
// reference of its directory (50,000 loose branches: 250 ms against 3 ms). A reference whose
// object is missing yields its value too, as rev-parse does: it can still be deleted.
func (repo *Repo) ResolveReferenceExactly(ctx context.Context, reference git.ReferenceName) (git.ObjectID, error) {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx, gitcmd.Command{
		Name:  "show-ref",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "--verify"}},
		Args:  []string{reference.String()},
	}, gitcmd.WithStdout(&stdout), gitcmd.WithStderr(&stderr)); err != nil {
		// show-ref --verify exits 128 with "not a valid ref" for a reference that does not
		// exist or whose name is not valid, and with "bad ref <reference> (<object ID>)" for one
		// whose object is missing; anything else is a failure.
		if code, ok := command.ExitStatus(err); ok && code == 128 {
			if strings.Contains(stderr.String(), "not a valid ref") {
				return "", git.ErrReferenceNotFound
			}
			if value, ok := badReferenceValue(stderr.String(), reference); ok {
				return value, nil
			}
		}
		return "", fmt.Errorf("show-ref %q: %w, stderr: %q", reference, err, stderr.String())
	}

	// The line is "<object ID> <reference>".
	value, name, ok := strings.Cut(strings.TrimSuffix(stdout.String(), "\n"), " ")
	if !ok || name != reference.String() {
		return "", fmt.Errorf("show-ref %q: unexpected output %q", reference, stdout.String())
	}

	return git.ObjectID(value), nil
}

// WrittenReference returns the reference that an update of reference writes: for a symbolic
// reference the end of its chain of targets, whether or not that exists, and otherwise reference
// itself, also when it does not exist. The name is read exactly, with no DWIM rule. A name that is
// not a valid reference is returned as it is: no update can write it, and git-update-ref(1) says why.
//
// A symbolic reference whose chain git does not follow to its end, a cycle or one of more hops than
// git-symbolic-ref(1) takes, fails with FailedPrecondition. Its name is not what an update of it
// writes, and no other name can be judged for it.
//
// An error says nothing of where the repository is on disk: it can reach the caller of an RPC.
func (repo *Repo) WrittenReference(ctx context.Context, reference git.ReferenceName) (git.ReferenceName, error) {
	if err := git.ValidateReference(reference.String()); err != nil {
		return reference, nil //nolint:nilerr // No update can write it, and git-update-ref(1) says why.
	}

	mayBeSymbolic, err := repo.mayBeSymbolic(ctx, reference)
	if err != nil {
		return "", err
	}
	if !mayBeSymbolic {
		return reference, nil
	}

	target, state, err := repo.symbolicTarget(ctx, reference, nil)
	if err != nil {
		return "", err
	}
	switch state {
	case referenceIsSymbolic:
		return target, nil
	case referenceIsPlain:
		return reference, nil
	}

	// git gave up on the name: one hop tells a chain it does not follow from a name it cannot read.
	_, state, err = repo.symbolicTarget(ctx, reference, []gitcmd.Option{gitcmd.Flag{Name: "--no-recurse"}})
	if err != nil {
		return "", err
	}
	if state == referenceIsSymbolic {
		return "", structerr.NewFailedPrecondition("%q is a cycle of symbolic references or a chain longer than git follows", reference)
	}
	return reference, nil
}

// referenceState is what git-symbolic-ref(1) says of a reference.
type referenceState int

const (
	// referenceIsPlain is a reference that is not symbolic or that does not exist.
	referenceIsPlain referenceState = iota
	// referenceIsSymbolic is a symbolic reference that git read to a target.
	referenceIsSymbolic
	// referenceUnread is a name that git gave up reading.
	referenceUnread
)

// symbolicTarget asks git-symbolic-ref(1) for the target of reference.
func (repo *Repo) symbolicTarget(ctx context.Context, reference git.ReferenceName, flags []gitcmd.Option) (git.ReferenceName, referenceState, error) {
	var stdout, stderr bytes.Buffer
	err := repo.ExecAndWait(ctx, gitcmd.Command{
		Name:  "symbolic-ref",
		Flags: append([]gitcmd.Option{gitcmd.Flag{Name: "--quiet"}}, flags...),
		Args:  []string{reference.String()},
	},
		// symbolic-ref is a reference-updating command for git, but this call only reads.
		gitcmd.WithDisabledHooks(),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	)
	if err == nil {
		return git.ReferenceName(strings.TrimSuffix(stdout.String(), "\n")), referenceIsSymbolic, nil
	}

	// With --quiet it exits 1 for a reference that is not symbolic or does not exist, and 128 with
	// "No such ref" for one it cannot read as a reference.
	code, exited := command.ExitStatus(err)
	switch {
	case exited && code == 1:
		return "", referenceIsPlain, nil
	case exited && code == 128 && strings.Contains(stderr.String(), "No such ref"):
		return "", referenceUnread, nil
	}

	repo.logger.WithError(err).WithField("stderr", stderr.String()).ErrorContext(ctx, "reading a symbolic reference")
	return "", referenceIsPlain, fmt.Errorf("reading symbolic reference %q failed", reference)
}

// mayBeSymbolic tells whether reference has to be asked of git to know if it is symbolic. Every write
// of every RPC asks, nearly always of a plain branch, so the files backend answers without a process:
// git never packs a symbolic reference, so one is a loose file that starts with "ref:", or a symbolic
// link, which git reads as one when it points to a reference. The other backends are read by git only.
// reference is a valid reference name, so it stays inside the repository as a path.
func (repo *Repo) mayBeSymbolic(ctx context.Context, reference git.ReferenceName) (bool, error) {
	backend, err := repo.ReferenceBackend(ctx)
	if err != nil {
		repo.logger.WithError(err).ErrorContext(ctx, "detecting the reference backend")
		return false, errors.New("detecting the reference backend failed")
	}
	if backend.Name != git.ReferenceBackendFiles.Name {
		return true, nil
	}

	repoPath, err := repo.Path(ctx)
	if err != nil {
		repo.logger.WithError(err).ErrorContext(ctx, "getting the repository path")
		return false, errors.New("getting the repository path failed")
	}
	path := filepath.Join(repoPath, reference.String())

	info, err := os.Lstat(path)
	if err != nil {
		return false, looseReferenceError(reference, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return true, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return false, looseReferenceError(reference, err)
	}
	return bytes.HasPrefix(content, []byte("ref:")), nil
}

// looseReferenceError returns what a failed read of the loose file of reference means: nothing, for
// a reference that is packed or missing, a directory of references, or a name too long for the file
// system, which git refuses when it writes it. Any other failure is returned with the cause alone,
// as the path of a *fs.PathError would tell the caller where the repository is.
func looseReferenceError(reference git.ReferenceName, err error) error {
	for _, notALooseFile := range []error{os.ErrNotExist, syscall.ENOTDIR, syscall.EISDIR, syscall.ENAMETOOLONG} {
		if errors.Is(err, notALooseFile) {
			return nil
		}
	}

	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return fmt.Errorf("reading the loose reference %q: %w", reference, err)
}

// ReflogExists reports whether the reference named reference has a reflog, with `git reflog
// exists`, which reads that one reference's log: no DWIM rule.
func (repo *Repo) ReflogExists(ctx context.Context, reference git.ReferenceName) (bool, error) {
	var stderr bytes.Buffer
	err := repo.ExecAndWait(ctx, gitcmd.Command{
		Name:   "reflog",
		Action: "exists",
		Args:   []string{reference.String()},
	}, gitcmd.WithStderr(&stderr))
	if err == nil {
		return true, nil
	}
	// reflog exists exits 1 for a reference without a reflog.
	if code, ok := command.ExitStatus(err); ok && code == 1 {
		return false, nil
	}
	return false, fmt.Errorf("reflog exists %q: %w, stderr: %q", reference, err, stderr.String())
}

// badReferenceValue returns the value that show-ref --verify reports for reference in stderr when
// the object of the reference is missing: "fatal: git show-ref: bad ref <reference> (<object ID>)".
func badReferenceValue(stderr string, reference git.ReferenceName) (git.ObjectID, bool) {
	_, rest, ok := strings.Cut(stderr, "bad ref "+reference.String()+" (")
	if !ok {
		return "", false
	}
	value, _, ok := strings.Cut(rest, ")")
	if !ok || value == "" {
		return "", false
	}
	return git.ObjectID(value), true
}

// GetReference looks up and returns the given reference. Returns a
// ReferenceNotFound error if the reference was not found.
func (repo *Repo) GetReference(ctx context.Context, reference git.ReferenceName) (git.Reference, error) {
	refs, err := gitcmd.GetReferences(ctx, repo, gitcmd.GetReferencesConfig{
		Patterns: []string{reference.String()},
		Limit:    1,
	})
	if err != nil {
		return git.Reference{}, err
	}

	if len(refs) == 0 {
		return git.Reference{}, git.ErrReferenceNotFound
	}
	if refs[0].Name != reference {
		return git.Reference{}, fmt.Errorf("%w: conflicts with %q", git.ErrReferenceAmbiguous, refs[0].Name)
	}

	return refs[0], nil
}

// HasBranches determines whether there is at least one branch in the
// repository.
func (repo *Repo) HasBranches(ctx context.Context) (bool, error) {
	refs, err := gitcmd.GetReferences(ctx, repo, gitcmd.GetReferencesConfig{
		Patterns: []string{"refs/heads/"},
		Limit:    1,
	})
	return len(refs) > 0, err
}

// GetReferences returns references matching any of the given patterns. If no patterns are given,
// all references are returned.
func (repo *Repo) GetReferences(ctx context.Context, patterns ...string) ([]git.Reference, error) {
	return gitcmd.GetReferences(ctx, repo, gitcmd.GetReferencesConfig{
		Patterns: patterns,
	})
}

// GetBranches returns all branches.
func (repo *Repo) GetBranches(ctx context.Context) ([]git.Reference, error) {
	return repo.GetReferences(ctx, "refs/heads/")
}

// UpdateRef updates reference from oldValue to newValue. If oldValue is a
// non-empty string, the update will fail it the reference is not currently at
// that revision. If newValue is the ZeroOID, the reference will be deleted.
// If oldValue is the ZeroOID, the reference will created.
func (repo *Repo) UpdateRef(ctx context.Context, reference git.ReferenceName, newValue, oldValue git.ObjectID) error {
	updater, err := updateref.New(ctx, repo)
	if err != nil {
		return fmt.Errorf("creating updateref: %w", err)
	}

	if err := updater.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	if err := updater.Update(reference, newValue, oldValue); err != nil {
		return fmt.Errorf("update: %w", err)
	}

	if err := updater.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	if err := updater.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	return nil
}

// SetDefaultBranch sets the repository's HEAD to point to the given reference.
// It will not verify the reference actually exists.
func (repo *Repo) SetDefaultBranch(ctx context.Context, txManager transaction.Manager, reference git.ReferenceName) error {
	if err := git.ValidateReference(reference.String()); err != nil {
		return fmt.Errorf("%q is a malformed refname", reference)
	}

	return repo.setDefaultBranchWithUpdateRef(ctx, reference)
}

// setDefaultBranchWithUpdateRef uses 'symref-update' command to update HEAD.
func (repo *Repo) setDefaultBranchWithUpdateRef(
	ctx context.Context,
	reference git.ReferenceName,
) (err error) {
	updater, err := updateref.New(ctx, repo, updateref.WithNoDeref())
	if err != nil {
		return fmt.Errorf("creating updateref: %w", err)
	}

	defer func() {
		if cErr := updater.Close(); err == nil && cErr != nil {
			err = fmt.Errorf("close: %w", cErr)
		}
	}()

	if err = updater.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	if err = updater.UpdateSymbolicReference("HEAD", reference); err != nil {
		return fmt.Errorf("update: %w", err)
	}

	if err := updater.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	return nil
}

type getRemoteReferenceConfig struct {
	patterns   []string
	config     []gitcmd.ConfigPair
	sshCommand string
}

// GetRemoteReferencesOption is an option which can be passed to GetRemoteReferences.
type GetRemoteReferencesOption func(*getRemoteReferenceConfig)

// WithPatterns sets up a set of patterns which are then used to filter the list of returned
// references.
func WithPatterns(patterns ...string) GetRemoteReferencesOption {
	return func(cfg *getRemoteReferenceConfig) {
		cfg.patterns = patterns
	}
}

// WithSSHCommand sets the SSH invocation to use when communicating with the remote.
func WithSSHCommand(cmd string) GetRemoteReferencesOption {
	return func(cfg *getRemoteReferenceConfig) {
		cfg.sshCommand = cmd
	}
}

// WithConfig sets up Git configuration for the git-ls-remote(1) invocation. The config pairs are
// set up via `WithConfigEnv()` and are thus not exposed via the command line.
func WithConfig(config ...gitcmd.ConfigPair) GetRemoteReferencesOption {
	return func(cfg *getRemoteReferenceConfig) {
		cfg.config = config
	}
}

// GetRemoteReferences lists references of the remote. Peeled tags are not returned.
func (repo *Repo) GetRemoteReferences(ctx context.Context, remote string, opts ...GetRemoteReferencesOption) ([]git.Reference, error) {
	var cfg getRemoteReferenceConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	var env []string
	if cfg.sshCommand != "" {
		env = append(env, envGitSSHCommand(cfg.sshCommand))
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "ls-remote",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "--refs"},
				gitcmd.Flag{Name: "--symref"},
			},
			Args: append([]string{remote}, cfg.patterns...),
		},
		gitcmd.WithStdout(stdout),
		gitcmd.WithStderr(stderr),
		gitcmd.WithEnv(env...),
		gitcmd.WithConfigEnv(cfg.config...),
	); err != nil {
		return nil, fmt.Errorf("create git ls-remote: %w, stderr: %q", err, stderr)
	}

	var refs []git.Reference
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		split := strings.SplitN(scanner.Text(), "\t", 2)
		if len(split) != 2 {
			return nil, fmt.Errorf("invalid ls-remote output line: %q", scanner.Text())
		}

		// Symbolic references are outputted as:
		//	ref: refs/heads/master	refs/heads/symbolic-ref
		//	0c9cf732b5774fa948348bbd6f273009bd66e04c	refs/heads/symbolic-ref
		if strings.HasPrefix(split[0], "ref: ") {
			symRef := split[1]
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return nil, fmt.Errorf("scan dereferenced symbolic ref: %w", err)
				}

				return nil, fmt.Errorf("missing dereferenced symbolic ref line for %q", symRef)
			}

			split = strings.SplitN(scanner.Text(), "\t", 2)
			if len(split) != 2 {
				return nil, fmt.Errorf("invalid dereferenced symbolic ref line: %q", scanner.Text())
			}

			if split[1] != symRef {
				return nil, fmt.Errorf("expected dereferenced symbolic ref %q but got reference %q", symRef, split[1])
			}

			refs = append(refs, git.NewSymbolicReference(git.ReferenceName(symRef), git.ReferenceName(split[0])))
			continue
		}

		refs = append(refs, git.NewReference(git.ReferenceName(split[1]), git.ObjectID(split[0])))
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	return refs, nil
}

// GetDefaultBranch determines the default branch name
func (repo *Repo) GetDefaultBranch(ctx context.Context) (git.ReferenceName, error) {
	headReference, err := repo.HeadReference(ctx)
	if err != nil {
		return "", err
	}

	// Ideally we would only use HEAD to determine the default branch, but
	// gitlab-rails depends on the branch being determined like this. Each
	// candidate is read by its exact name: with its DWIM rules, git would take a
	// missing refs/heads/main for one of refs/refs/heads/main,
	// refs/tags/refs/heads/main, refs/heads/refs/heads/main or
	// refs/remotes/refs/heads/main. A candidate counts when it has a value: a
	// branch whose object is missing does, a dangling symbolic reference does
	// not.
	for _, headCandidate := range []git.ReferenceName{
		headReference, git.DefaultRef, git.LegacyDefaultRef,
	} {
		if _, err := repo.ResolveReferenceExactly(ctx, headCandidate); err != nil {
			if errors.Is(err, git.ErrReferenceNotFound) {
				continue
			}
			return "", err
		}
		return headCandidate, nil
	}

	// If all else fails, return the first branch name
	branches, err := gitcmd.GetReferences(ctx, repo, gitcmd.GetReferencesConfig{
		Patterns: []string{"refs/heads/"},
		Limit:    1,
	})
	if err != nil || len(branches) == 0 {
		return "", err
	}

	return branches[0].Name, nil
}

// HeadReference returns the current value of HEAD.
func (repo *Repo) HeadReference(ctx context.Context) (git.ReferenceName, error) {
	symref, err := gitcmd.GetSymbolicRef(ctx, repo, "HEAD")
	if err != nil {
		return "", err
	}

	return git.ReferenceName(symref.Target), nil
}

// GuessHead tries to guess what branch HEAD would be pointed at. If no
// reference is found git.ErrReferenceNotFound is returned.
//
// This function should be roughly equivalent to the corresponding function in
// git:
// https://github.com/git/git/blob/2a97289ad8b103625d3a1a12f66c27f50df822ce/remote.c#L2198
func (repo *Repo) GuessHead(ctx context.Context, head git.Reference) (git.ReferenceName, error) {
	if head.IsSymbolic {
		return git.ReferenceName(head.Target), nil
	}

	// Try current and historic default branches first. Ideally we might look
	// up the git config `init.defaultBranch` but we do not allow this
	// configuration to be set by the user. It is always set to
	// `git.DefaultRef`.
	for _, name := range []git.ReferenceName{git.DefaultRef, git.LegacyDefaultRef} {
		ref, err := repo.GetReference(ctx, name)
		switch {
		case errors.Is(err, git.ErrReferenceNotFound):
			continue
		case err != nil:
			return "", fmt.Errorf("guess head: default: %w", err)
		case head.Target == ref.Target:
			return ref.Name, nil
		}
	}

	refs, err := repo.GetBranches(ctx)
	if err != nil {
		return "", fmt.Errorf("guess head: %w", err)
	}
	for _, ref := range refs {
		if ref.Target != head.Target {
			continue
		}
		return ref.Name, nil
	}

	return "", fmt.Errorf("guess head: %w", git.ErrReferenceNotFound)
}
