// Package gitops holds the git operations of the storage core: building and
// indexing changeset packs, reading and applying references, creating the
// bare repository of a hydration and the local watermark. Owned by WP53
// (doc/graft/PLAN6.md).
//
// Every function works on a *localrepo.Repo and runs git through it, so the
// repository's quarantine (hooks payload) and the configured git binary are
// used. Nothing here runs hooks: references are applied with
// updateref.WithDisabledTransactions.
package gitops

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// errEmptyPack is returned when the file of an empty pack is requested.
var errEmptyPack = errors.New("gitops: empty pack has no file")

// forEachRefFormat is the for-each-ref format: refname, object ID and symbolic
// target, separated by NUL bytes.
const forEachRefFormat = "--format=%(refname)%00%(objectname)%00%(symref)"

// Pack is a pack written to a temporary file.
type Pack struct {
	// Path is the temporary file, empty when the pack has no objects.
	Path string
	// SHA256 is the hash of the file's bytes, Size its length.
	SHA256 [32]byte
	Size   int64
	// Objects is the number of objects in the pack.
	Objects int
	// Walk is the time from the start of the walk until its object list ended:
	// until pack-objects read the end of the list, or, for an empty list, until
	// rev-list exited. The rest of the build is the packing of the objects.
	Walk time.Duration
}

// Empty reports whether the pack has no objects (and no file).
func (p Pack) Empty() bool { return p.Path == "" }

// Open opens the pack file for reading.
func (p Pack) Open() (io.ReadCloser, error) {
	if p.Empty() {
		return nil, errEmptyPack
	}

	f, err := os.Open(p.Path)
	if err != nil {
		return nil, err
	}

	return f, nil
}

// Remove deletes the temporary file; it is safe on an empty pack.
func (p Pack) Remove() error {
	if p.Empty() {
		return nil
	}

	if err := os.Remove(p.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// BuildPack writes a self-contained (non-thin) pack of the objects reachable
// from tips that are not reachable from any current reference of the
// repository. The tips are fed to `git rev-list --objects --stdin --not --all`
// on stdin, so their number is not limited by the command line (a push can
// create tens of thousands of tags). The object list is piped into
// `git pack-objects --stdout`, written to a new file in tmpDir while hashing.
// tips may be empty; zero OIDs are ignored. An empty result returns an empty
// Pack and no file.
func BuildPack(ctx context.Context, repo *localrepo.Repo, tips []git.ObjectID, tmpDir string) (Pack, error) {
	// Tips read from stdin are interesting even though --not is given on the
	// command line: git only applies --not to the revisions it parses from
	// the arguments.
	stdin, positive, err := tipsInput(ctx, repo, tips)
	if err != nil {
		return Pack{}, err
	}
	if positive == 0 {
		return Pack{}, nil
	}

	// The --not flag makes --all excluded; the tips on stdin are included.
	return buildPack(ctx, repo, gitcmd.Command{
		Name: "rev-list",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "--objects"},
			gitcmd.Flag{Name: "--stdin"},
			gitcmd.Flag{Name: "--not"},
			gitcmd.Flag{Name: "--all"},
		},
	}, &stdin, tmpDir)
}

// BuildPackFrom writes a self-contained (non-thin) pack of every object
// reachable from tips, whatever the references of the repository: the
// consolidated pack of a checkpoint (doc/graft/PLAN7.md). The tips are fed to
// `git rev-list --objects --stdin`, piped into `git pack-objects --stdout` and
// written to a new file in tmpDir while hashing. Zero tips are ignored; without
// any tip the result is an empty Pack and no file. A tip that is not in the
// repository is an error, never a partial pack.
func BuildPackFrom(ctx context.Context, repo *localrepo.Repo, tips []git.ObjectID, tmpDir string) (Pack, error) {
	stdin, positive, err := tipsInput(ctx, repo, tips)
	if err != nil {
		return Pack{}, err
	}
	if positive == 0 {
		return Pack{}, nil
	}

	return buildPack(ctx, repo, gitcmd.Command{
		Name: "rev-list",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "--objects"},
			gitcmd.Flag{Name: "--stdin"},
		},
	}, &stdin, tmpDir)
}

// tipsInput returns the standard input of a rev-list that walks from tips: one
// validated object ID per line, zero OIDs left out, and how many there are.
func tipsInput(ctx context.Context, repo *localrepo.Repo, tips []git.ObjectID) (bytes.Buffer, int, error) {
	var stdin bytes.Buffer

	hash, err := repo.ObjectHash(ctx)
	if err != nil {
		return stdin, 0, fmt.Errorf("object hash: %w", err)
	}

	positive := 0
	for _, tip := range tips {
		if hash.IsZeroOID(tip) {
			continue
		}
		if err := hash.ValidateHex(tip.String()); err != nil {
			return stdin, 0, fmt.Errorf("tip: %w", err)
		}
		stdin.WriteString(tip.String())
		stdin.WriteByte('\n')
		positive++
	}
	return stdin, positive, nil
}

// MissingObjects returns the objects among oids that are not in the
// repository, in the order given, with one `git cat-file --batch-check` run.
// Every object ID is validated against the repository's hash first.
func MissingObjects(ctx context.Context, repo *localrepo.Repo, oids []git.ObjectID) ([]git.ObjectID, error) {
	if len(oids) == 0 {
		return nil, nil
	}

	hash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("object hash: %w", err)
	}

	var stdin bytes.Buffer
	for _, oid := range oids {
		if err := hash.ValidateHex(oid.String()); err != nil {
			return nil, fmt.Errorf("object %q: %w", oid, err)
		}
		stdin.WriteString(oid.String())
		stdin.WriteByte('\n')
	}

	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "cat-file",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--batch-check=%(objectname) %(objecttype)"}},
		},
		gitcmd.WithStdin(&stdin),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, structerr.New("cat-file: %w", err).WithMetadata("stderr", stderr.String())
	}

	// One output line per input line: "<oid> <type>", or "<oid> missing".
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != len(oids) {
		return nil, fmt.Errorf("cat-file: %d answers for %d objects", len(lines), len(oids))
	}

	var missing []git.ObjectID
	for i, line := range lines {
		name, kind, ok := strings.Cut(line, " ")
		if !ok || name != oids[i].String() {
			return nil, fmt.Errorf("cat-file: unexpected answer %q for %s", line, oids[i])
		}
		if kind == "missing" {
			missing = append(missing, oids[i])
		}
	}
	return missing, nil
}

// BuildFullPack writes a pack of every object reachable from any reference
// (including HEAD), for the import of a repository.
func BuildFullPack(ctx context.Context, repo *localrepo.Repo, tmpDir string) (Pack, error) {
	return buildPack(ctx, repo, gitcmd.Command{
		Name: "rev-list",
		Flags: []gitcmd.Option{
			gitcmd.Flag{Name: "--objects"},
			gitcmd.Flag{Name: "--all"},
		},
	}, nil, tmpDir)
}

// buildPack pipes the object list of revList into `git pack-objects --stdout`
// and writes the pack to a new file in tmpDir while hashing it. stdin, when
// not nil, is the standard input of revList. An empty object list returns an
// empty Pack without creating a file.
func buildPack(ctx context.Context, repo *localrepo.Repo, revList gitcmd.Command, stdin io.Reader, tmpDir string) (_ Pack, returnedErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	start := time.Now()

	var revStderr bytes.Buffer
	revOpts := []gitcmd.CmdOpt{gitcmd.WithSetupStdout(), gitcmd.WithStderr(&revStderr)}
	if stdin != nil {
		revOpts = append(revOpts, gitcmd.WithStdin(stdin))
	}
	revCmd, err := repo.Exec(ctx, revList, revOpts...)
	if err != nil {
		return Pack{}, fmt.Errorf("rev-list: %w", err)
	}

	objectList := bufio.NewReader(revCmd)
	if _, err := objectList.Peek(1); err != nil {
		if !errors.Is(err, io.EOF) {
			cancel()
			_ = revCmd.Wait()
			return Pack{}, fmt.Errorf("read object list: %w", err)
		}

		// No objects: the reference transaction brought nothing new.
		if err := revCmd.Wait(); err != nil {
			return Pack{}, structerr.New("rev-list: %w", err).WithMetadata("stderr", revStderr.String())
		}
		return Pack{Walk: time.Since(start)}, nil
	}
	listEnd := &endReader{r: objectList}

	file, err := os.CreateTemp(tmpDir, "graft-pack-*")
	if err != nil {
		cancel()
		_ = revCmd.Wait()
		return Pack{}, fmt.Errorf("create pack file: %w", err)
	}

	defer func() {
		if returnedErr != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()

	hasher := sha256.New()
	var packStderr bytes.Buffer
	packCmd, err := repo.Exec(ctx,
		gitcmd.Command{
			Name: "pack-objects",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "-q"},
				gitcmd.Flag{Name: "--stdout"},
			},
		},
		gitcmd.WithStdin(listEnd),
		gitcmd.WithStdout(io.MultiWriter(file, hasher)),
		gitcmd.WithStderr(&packStderr),
	)
	if err != nil {
		cancel()
		_ = revCmd.Wait()
		return Pack{}, fmt.Errorf("pack-objects: %w", err)
	}

	packErr := packCmd.Wait()
	if packErr != nil {
		// pack-objects stopped reading: rev-list must not wait for a reader.
		cancel()
	}
	revErr := revCmd.Wait()

	if packErr != nil {
		return Pack{}, structerr.New("pack-objects: %w", packErr).WithMetadata("stderr", packStderr.String())
	}
	if revErr != nil {
		return Pack{}, structerr.New("rev-list: %w", revErr).WithMetadata("stderr", revStderr.String())
	}

	info, err := file.Stat()
	if err != nil {
		return Pack{}, fmt.Errorf("stat pack file: %w", err)
	}

	var header [12]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return Pack{}, fmt.Errorf("read pack header: %w", err)
	}
	if string(header[:4]) != "PACK" {
		return Pack{}, errors.New("pack-objects wrote no pack header")
	}

	if err := file.Close(); err != nil {
		return Pack{}, fmt.Errorf("close pack file: %w", err)
	}

	var sum [32]byte
	copy(sum[:], hasher.Sum(nil))

	return Pack{
		Path:    file.Name(),
		SHA256:  sum,
		Size:    info.Size(),
		Objects: int(binary.BigEndian.Uint32(header[8:12])),
		Walk:    listEnd.endedAfter(start),
	}, nil
}

// endReader is a reader that records when the reader it wraps ended. It is read
// by the goroutine that feeds the standard input of a command, and asked once the
// command has exited.
type endReader struct {
	r io.Reader
	// end is the time the reader ended, in Unix nanoseconds; 0 until then.
	end atomic.Int64
}

func (e *endReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		e.end.CompareAndSwap(0, time.Now().UnixNano())
	}
	return n, err
}

// endedAfter returns the time from start until the reader ended. A command that
// exited without reading its input to the end (pack-objects reads all of it
// before it writes) has no end, and then the whole time since start counts.
func (e *endReader) endedAfter(start time.Time) time.Duration {
	end := e.end.Load()
	if end == 0 {
		return time.Since(start)
	}
	return time.Unix(0, end).Sub(start)
}

// IndexPack adds the pack read from r to the repository with
// `git index-pack --stdin` (which verifies every object), so that the objects
// are in objects/pack of the repository afterwards. An empty reader is an
// error.
func IndexPack(ctx context.Context, repo *localrepo.Repo, r io.Reader) error {
	var stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "index-pack",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--stdin"}},
		},
		gitcmd.WithStdin(r),
		gitcmd.WithStdout(io.Discard),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return structerr.New("index pack: %w", err).WithMetadata("stderr", stderr.String())
	}

	return nil
}

// ReadRefs returns every reference of the repository, including HEAD, as
// Targets: object IDs in hex, symbolic references as "ref:<name>" with their
// immediate target (a chain HEAD -> refs/heads/a -> refs/heads/b reports HEAD as
// "ref:refs/heads/a"). A HEAD whose target does not exist is reported with its
// target. git's reference listing leaves out the other symbolic references whose
// target does not exist, and there is no listing that includes them, so ReadRefs
// cannot report those; ReadRefValues, which is given the names, does.
func ReadRefs(ctx context.Context, repo *localrepo.Repo) (map[string]corestore.Target, error) {
	refs, err := readForEachRef(ctx, repo)
	if err != nil {
		return nil, err
	}

	head, err := readHead(ctx, repo)
	if err != nil {
		return nil, err
	}
	if !head.Absent() {
		refs["HEAD"] = head
	}

	return refs, nil
}

// refValuesChunk is the number of names passed to one for-each-ref run of
// ReadRefValues, which keeps the command line far below ARG_MAX.
const refValuesChunk = 500

// ReadRefValues returns the current values of the named references; absent
// references are omitted. Names may include HEAD. HEAD is read with symbolic-ref
// (and rev-parse when it is detached). The other names are looked up with
// for-each-ref restricted to them, one run per 500 names, so the references of
// the rest of the repository are not read. A name that cannot exist as a
// reference (not under refs/, or not a valid reference name) is skipped.
//
// Symbolic references are reported with their immediate target, like ReadRefs,
// also when that target does not exist (a dangling symbolic reference, which
// for-each-ref leaves out): the names for-each-ref did not report are looked up
// with one cat-file run, which warns about each dangling symbolic reference,
// and only those are read with symbolic-ref.
func ReadRefValues(ctx context.Context, repo *localrepo.Repo, names []string) (map[string]corestore.Target, error) {
	result := make(map[string]corestore.Target, len(names))

	var wantRefs []string
	wantHead := false
	for _, name := range names {
		switch {
		case name == "HEAD":
			wantHead = true
		case strings.HasPrefix(name, "refs/") && git.ValidateReference(name) == nil:
			// Only validated names reach the command line, so none is read as an option.
			wantRefs = append(wantRefs, name)
		}
	}

	if wantHead {
		head, err := readHead(ctx, repo)
		if err != nil {
			return nil, err
		}
		if !head.Absent() {
			result["HEAD"] = head
		}
	}

	for start := 0; start < len(wantRefs); start += refValuesChunk {
		end := start + refValuesChunk
		if end > len(wantRefs) {
			end = len(wantRefs)
		}
		chunk := wantRefs[start:end]

		// A pattern also matches the references below it: only the exact names are kept.
		all, err := readForEachRef(ctx, repo, chunk...)
		if err != nil {
			return nil, err
		}

		for _, name := range chunk {
			if target, ok := all[name]; ok {
				result[name] = target
			}
		}
	}

	var missing []string
	for _, name := range wantRefs {
		if _, ok := result[name]; !ok {
			missing = append(missing, name)
		}
	}
	dangling, err := readDangling(ctx, repo, missing)
	if err != nil {
		return nil, err
	}
	for name, target := range dangling {
		result[name] = target
	}

	return result, nil
}

// LocalRef is a reference of the local repository and its value.
type LocalRef struct {
	Name   string
	Target corestore.Target
}

// LocalNameConflicts returns, for each name of names that has one, a local reference that git
// cannot hold next to a reference named name: a parent directory of name that is a reference, or
// else the first reference below name. Names that cannot exist as references are skipped. It
// costs one ReadRefValues of the parent names and one for-each-ref per 500 names; it does not read
// the other references of the repository.
func LocalNameConflicts(ctx context.Context, repo *localrepo.Repo, names []string) (map[string]LocalRef, error) {
	var valid, parents []string
	seen := map[string]bool{}
	for _, name := range names {
		if !strings.HasPrefix(name, "refs/") || git.ValidateReference(name) != nil {
			continue
		}
		valid = append(valid, name)
		for _, parent := range corestore.ParentRefNames(name) {
			if !seen[parent] {
				seen[parent] = true
				parents = append(parents, parent)
			}
		}
	}
	if len(valid) == 0 {
		return nil, nil
	}

	parentValues, err := ReadRefValues(ctx, repo, parents)
	if err != nil {
		return nil, err
	}

	// A pattern matches the name and the references below it.
	below := map[string]corestore.Target{}
	for start := 0; start < len(valid); start += refValuesChunk {
		chunk := valid[start:min(start+refValuesChunk, len(valid))]
		refs, err := readForEachRef(ctx, repo, chunk...)
		if err != nil {
			return nil, err
		}
		for ref, target := range refs {
			below[ref] = target
		}
	}

	result := map[string]LocalRef{}
	for _, name := range valid {
		for _, parent := range corestore.ParentRefNames(name) {
			if target, ok := parentValues[parent]; ok {
				result[name] = LocalRef{Name: parent, Target: target}
				break
			}
		}
		if _, found := result[name]; found {
			continue
		}
		first := ""
		for ref := range below {
			if strings.HasPrefix(ref, name+"/") && (first == "" || ref < first) {
				first = ref
			}
		}
		if first != "" {
			result[name] = LocalRef{Name: first, Target: below[first]}
		}
	}
	return result, nil
}

// danglingWarning is the warning git prints when it looks up a name that is a
// symbolic reference whose target does not exist. Gitaly runs git with an
// English locale.
const danglingWarning = "warning: ignoring dangling symref "

// readDangling returns the names among names (validated reference names that
// for-each-ref did not report) that are dangling symbolic references, with
// their immediate targets. One cat-file run looks them all up: it reports every
// name as missing, and warns about each dangling symbolic reference. Only the
// warned names are read with symbolic-ref, which also confirms them.
func readDangling(ctx context.Context, repo *localrepo.Repo, names []string) (map[string]corestore.Target, error) {
	if len(names) == 0 {
		return nil, nil
	}

	wanted := make(map[string]bool, len(names))
	var stdin bytes.Buffer
	for _, name := range names {
		wanted[name] = true
		// A valid reference name has no whitespace, so one name per line is safe.
		stdin.WriteString(name)
		stdin.WriteByte('\n')
	}

	var stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "cat-file",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--batch-check=%(objectname)"}},
		},
		gitcmd.WithStdin(&stdin),
		gitcmd.WithStdout(io.Discard),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, structerr.New("cat-file: %w", err).WithMetadata("stderr", stderr.String())
	}

	result := map[string]corestore.Target{}
	for _, line := range strings.Split(stderr.String(), "\n") {
		name, ok := strings.CutPrefix(line, danglingWarning)
		if !ok || !wanted[name] {
			continue
		}
		if _, done := result[name]; done {
			continue
		}

		target, symbolic, err := readSymbolicRef(ctx, repo, name)
		if err != nil {
			return nil, err
		}
		if symbolic {
			result[name] = corestore.SymbolicTarget(target)
		}
	}

	return result, nil
}

// readSymbolicRef returns the immediate target of the symbolic reference name
// (a validated reference name, never read as an option). symbolic is false when
// name is not a symbolic reference (or does not exist).
func readSymbolicRef(ctx context.Context, repo *localrepo.Repo, name string) (target string, symbolic bool, _ error) {
	var stdout, stderr bytes.Buffer
	err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "symbolic-ref",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--quiet"}, gitcmd.Flag{Name: "--no-recurse"}},
			Args:  []string{name},
		},
		// symbolic-ref is a reference-updating command for git, but this call only reads.
		gitcmd.WithDisabledHooks(),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	)
	if err != nil {
		// git-symbolic-ref(1) with --quiet exits with 1 when name is not a
		// symbolic reference.
		if code, ok := command.ExitStatus(err); ok && code == 1 {
			return "", false, nil
		}
		return "", false, structerr.New("symbolic-ref: %w", err).WithMetadata("stderr", stderr.String())
	}

	target, ok := strings.CutSuffix(stdout.String(), "\n")
	if !ok || target == "" {
		return "", false, fmt.Errorf("symbolic-ref: unexpected output for %q", name)
	}
	return target, true, nil
}

// readForEachRef lists the references with one for-each-ref run: every
// reference under refs/, or only those matching the patterns when given.
// for-each-ref's %(symref) is the end of a chain of symbolic references, so
// each symbolic reference is read again with symbolic-ref for its immediate
// target (symbolic references are few: HEAD-like references of remotes).
func readForEachRef(ctx context.Context, repo *localrepo.Repo, patterns ...string) (map[string]corestore.Target, error) {
	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "for-each-ref",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: forEachRefFormat}},
			Args:  patterns,
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return nil, structerr.New("for-each-ref: %w", err).WithMetadata("stderr", stderr.String())
	}

	refs := map[string]corestore.Target{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line == "" {
			continue
		}

		fields := strings.Split(line, "\x00")
		if len(fields) != 3 {
			return nil, errors.New("unexpected reference format")
		}

		if fields[2] == "" {
			refs[fields[0]] = corestore.Target(fields[1])
			continue
		}

		// A valid name only: for-each-ref does not list a reference with an
		// invalid name, but the name is passed as an argument.
		if err := git.ValidateReference(fields[0]); err != nil {
			return nil, fmt.Errorf("symbolic reference %q: %w", fields[0], err)
		}
		target, symbolic, err := readSymbolicRef(ctx, repo, fields[0])
		if err != nil {
			return nil, err
		}
		if !symbolic {
			// It stopped being a symbolic reference after for-each-ref read it:
			// report what for-each-ref read.
			target = fields[2]
		}
		refs[fields[0]] = corestore.SymbolicTarget(target)
	}

	return refs, nil
}

// readHead returns HEAD: its immediate symbolic target (also when that target
// does not exist), or its object ID when detached. The result is absent when
// HEAD points to nothing.
func readHead(ctx context.Context, repo *localrepo.Repo) (corestore.Target, error) {
	name, symbolic, err := readSymbolicRef(ctx, repo, "HEAD")
	if err != nil {
		return "", err
	}
	if symbolic {
		return corestore.SymbolicTarget(name), nil
	}

	var stdout, stderr bytes.Buffer
	err = repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "rev-parse",
			Flags: []gitcmd.Option{gitcmd.Flag{Name: "--verify"}, gitcmd.Flag{Name: "--quiet"}},
			Args:  []string{"HEAD"},
		},
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	)
	if code, ok := command.ExitStatus(err); ok && code == 1 {
		return "", nil
	}
	if err != nil {
		return "", structerr.New("rev-parse HEAD: %w", err).WithMetadata("stderr", stderr.String())
	}

	oid, ok := strings.CutSuffix(stdout.String(), "\n")
	if !ok {
		return "", errors.New("rev-parse: unexpected output for HEAD")
	}

	hash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", fmt.Errorf("object hash: %w", err)
	}
	if err := hash.ValidateHex(oid); err != nil {
		return "", fmt.Errorf("HEAD: %w", err)
	}

	return corestore.Target(oid), nil
}

// ApplyRefs sets every named reference to its Target unconditionally (empty
// Target = delete) without hooks; symbolic targets use symref-update. It
// validates every name (ValidateName) and target (ValidateTarget) before
// running git and changes nothing when one is invalid. A reference lock held
// by a concurrent writer is returned as an error that IsLockError recognizes.
//
// The names other than HEAD are applied in one update-ref transaction. HEAD is
// applied alone afterwards: git refuses a transaction that updates HEAD together
// with the reference it points to, and a repository's HEAD usually points to
// one of the names being applied.
func ApplyRefs(ctx context.Context, repo *localrepo.Repo, refs map[string]corestore.Target) error {
	if len(refs) == 0 {
		return nil
	}

	hash, err := repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("object hash: %w", err)
	}

	names := make([]string, 0, len(refs))
	for name, target := range refs {
		if err := ValidateName(name); err != nil {
			return fmt.Errorf("reference %q: %w", name, err)
		}
		if err := ValidateTarget(target, hash); err != nil {
			return fmt.Errorf("reference %q: %w", name, err)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var others []string
	updatesHead := false
	for _, name := range names {
		if name == "HEAD" {
			updatesHead = true
			continue
		}
		others = append(others, name)
	}

	if len(others) > 0 {
		if err := applyNames(ctx, repo, others, refs); err != nil {
			return err
		}
	}

	if updatesHead {
		return applyTransaction(ctx, repo, []string{"HEAD"}, refs)
	}

	return nil
}

// applyNames applies the named references, which must not include HEAD, in one
// transaction. A deletion and a creation of conflicting names (refs/a and
// refs/a/b) cannot always share a transaction: then the deletions are applied
// first, in their own transaction, and the creations after them.
func applyNames(ctx context.Context, repo *localrepo.Repo, names []string, refs map[string]corestore.Target) error {
	err := applyTransaction(ctx, repo, names, refs)
	if err == nil || !isFileDirectoryConflict(err) {
		return err
	}

	var deletions, creations []string
	for _, name := range names {
		if refs[name].Absent() {
			deletions = append(deletions, name)
		} else {
			creations = append(creations, name)
		}
	}
	if len(deletions) == 0 || len(creations) == 0 {
		return err
	}

	if err := applyTransaction(ctx, repo, deletions, refs); err != nil {
		return fmt.Errorf("apply deletions: %w", err)
	}

	return applyTransaction(ctx, repo, creations, refs)
}

// applyTransaction applies the named references of refs in one update-ref
// transaction without hooks.
func applyTransaction(ctx context.Context, repo *localrepo.Repo, names []string, refs map[string]corestore.Target) error {
	// The references are set as they are stored in the database: a detached HEAD
	// must not write through a symbolic HEAD, so no dereferencing.
	updater, err := updateref.New(ctx, repo, updateref.WithDisabledTransactions(), updateref.WithNoDeref(), updateref.WithExistingReferences())
	if err != nil {
		return fmt.Errorf("new updater: %w", err)
	}

	if err := updater.Start(); err != nil {
		_ = updater.Close()
		return err
	}

	for _, name := range names {
		target := refs[name]

		var opErr error
		switch {
		case target.Absent():
			opErr = updater.Delete(git.ReferenceName(name))
		case target.IsSymbolic():
			opErr = updater.UpdateSymbolicReference(git.ReferenceName(name), git.ReferenceName(target.SymbolicName()))
		default:
			opErr = updater.Update(git.ReferenceName(name), git.ObjectID(target), "")
		}
		if opErr != nil {
			_ = updater.Close()
			return opErr
		}
	}

	if err := updater.Commit(); err != nil {
		_ = updater.Close()
		return err
	}

	return updater.Close()
}

// isFileDirectoryConflict reports whether err is a file/directory conflict
// between reference names, within one transaction or with an existing name.
func isFileDirectoryConflict(err error) bool {
	var fdErr updateref.FileDirectoryConflictError
	var inTxErr updateref.InTransactionConflictError
	return errors.As(err, &fdErr) || errors.As(err, &inTxErr)
}

// IsLockError reports whether err from ApplyRefs means that a reference was
// locked by a concurrent writer (the caller retries).
func IsLockError(err error) bool {
	var locked updateref.AlreadyLockedError
	if errors.As(err, &locked) {
		return true
	}

	return errors.Is(err, updateref.ErrPackedRefsLocked)
}

// InitBare creates an empty bare repository at path with the object format
// ("sha1" or "sha256") and the reftable reference backend. path must not
// exist.
func InitBare(ctx context.Context, gitCmdFactory gitcmd.CommandFactory, path, objectFormat string) error {
	if _, err := git.ObjectHashByFormat(objectFormat); err != nil {
		return fmt.Errorf("init bare: %w", err)
	}

	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("init bare: %q already exists", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("init bare: %w", err)
	}

	// The reference backend on the context selects the ref-format git uses for
	// init, see gitcmd.ContextWithReferenceBackend.
	ctx = gitcmd.ContextWithReferenceBackend(ctx, git.ReferenceBackendReftables)

	var stderr bytes.Buffer
	cmd, err := gitCmdFactory.NewWithoutRepo(ctx,
		gitcmd.Command{
			Name: "init",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "--bare"},
				gitcmd.Flag{Name: "--object-format=" + objectFormat},
				gitcmd.Flag{Name: "--ref-format=" + git.ReferenceBackendReftables.Name},
			},
			Args: []string{path},
		},
		gitcmd.WithStderr(&stderr),
	)
	if err != nil {
		return fmt.Errorf("init bare: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return structerr.New("init bare: %w", err).WithMetadata("stderr", stderr.String())
	}

	return nil
}

// ValidateName checks a reference name read from the database: "HEAD" or a
// name that git.ValidateReference accepts.
func ValidateName(name string) error {
	if name == "HEAD" {
		return nil
	}

	return git.ValidateReference(name)
}

// ValidateTarget checks a Target read from the database: a full hex object ID
// of the object hash, or a symbolic target whose name passes ValidateName and
// is not HEAD (git refuses a symbolic reference to HEAD). The absent target
// (deletion) is valid.
func ValidateTarget(t corestore.Target, hash git.ObjectHash) error {
	switch {
	case t.Absent():
		return nil
	case t.IsSymbolic():
		if t.SymbolicName() == "HEAD" {
			return errors.New("a symbolic reference cannot point to HEAD")
		}
		return ValidateName(t.SymbolicName())
	default:
		return hash.ValidateHex(string(t))
	}
}

// WatermarkPath is the watermark file of a repository: <repoPath>/graft/storage/applied.
func WatermarkPath(repoPath string) string {
	return filepath.Join(repoPath, "graft", "storage", "applied")
}

// ReadWatermark reads the local watermark; ok is false when there is none.
func ReadWatermark(repoPath string) (w corestore.Watermark, ok bool, err error) {
	data, err := os.ReadFile(WatermarkPath(repoPath))
	if errors.Is(err, fs.ErrNotExist) {
		return corestore.Watermark{}, false, nil
	}
	if err != nil {
		return corestore.Watermark{}, false, fmt.Errorf("read watermark: %w", err)
	}

	if err := json.Unmarshal(data, &w); err != nil {
		return corestore.Watermark{}, false, fmt.Errorf("parse watermark: %w", err)
	}

	return w, true, nil
}

// WriteWatermark writes the watermark atomically (temporary file, fsync,
// rename, fsync of the directory), creating graft/storage when needed.
func WriteWatermark(repoPath string, w corestore.Watermark) error {
	dir := filepath.Dir(WatermarkPath(repoPath))
	if err := os.MkdirAll(dir, mode.Directory); err != nil {
		return fmt.Errorf("write watermark: %w", err)
	}
	return writeWatermarkFile(repoPath, w, true)
}

// WriteWatermarkWithoutSync writes the watermark atomically (temporary file,
// rename) without fsync, for a repository whose durable step makes it durable
// later (localrefs.Durability.DefersWatermark). When graft/storage is missing it
// writes as WriteWatermark does, so that the directory it creates is durable.
func WriteWatermarkWithoutSync(repoPath string, w corestore.Watermark) error {
	if _, err := os.Stat(filepath.Dir(WatermarkPath(repoPath))); err != nil {
		return WriteWatermark(repoPath, w)
	}
	return writeWatermarkFile(repoPath, w, false)
}

// writeWatermarkFile writes the watermark into graft/storage, which exists,
// through a temporary file and a rename; sync fsyncs the file before the rename
// and the directory after it.
func writeWatermarkFile(repoPath string, w corestore.Watermark, sync bool) (returnedErr error) {
	dir := filepath.Dir(WatermarkPath(repoPath))

	data, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("write watermark: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".applied-*.tmp")
	if err != nil {
		return fmt.Errorf("write watermark: %w", err)
	}

	defer func() {
		if returnedErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write watermark: %w", err)
	}
	if sync {
		if err := tmp.Sync(); err != nil {
			return fmt.Errorf("sync watermark: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close watermark: %w", err)
	}
	if err := os.Rename(tmp.Name(), WatermarkPath(repoPath)); err != nil {
		return fmt.Errorf("rename watermark: %w", err)
	}
	if !sync {
		return nil
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open watermark directory: %w", err)
	}
	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync watermark directory: %w", err)
	}

	return nil
}

// The reference backend of a repository (files, reftable or mvcc) is
// localrepo.Repo.ReferenceBackend; compare with git.ReferenceBackendFiles,
// git.ReferenceBackendReftables and git.ReferenceBackendMVCC by Name.
