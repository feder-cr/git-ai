package gitcmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// GetReferencesConfig is configuration that can be passed to GetReferences in order to change its default behaviour.
type GetReferencesConfig struct {
	// Patterns limits the returned references to only those which match the given pattern. If no patterns are given
	// then all references will be returned.
	Patterns []string
	// Limit limits
	Limit uint
}

// GetReferences enumerates references in the given repository. By default, it returns all references that exist in the
// repository. This behaviour can be tweaked via the `GetReferencesConfig`.
func GetReferences(ctx context.Context, repoExecutor RepositoryExecutor, cfg GetReferencesConfig) ([]git.Reference, error) {
	flags := []Option{Flag{Name: "--format=%(refname)%00%(objectname)%00%(symref)"}}
	if cfg.Limit > 0 {
		flags = append(flags, Flag{Name: fmt.Sprintf("--count=%d", cfg.Limit)})
	}

	cmd, err := repoExecutor.Exec(ctx, Command{
		Name:  "for-each-ref",
		Flags: flags,
		Args:  cfg.Patterns,
	}, WithSetupStdout())
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(cmd)

	var refs []git.Reference
	for scanner.Scan() {
		line := bytes.SplitN(scanner.Bytes(), []byte{0}, 3)
		if len(line) != 3 {
			return nil, errors.New("unexpected reference format")
		}

		if len(line[2]) == 0 {
			refs = append(refs, git.NewReference(git.ReferenceName(line[0]), git.ObjectID(line[1])))
		} else {
			refs = append(refs, git.NewSymbolicReference(git.ReferenceName(line[0]), git.ReferenceName(line[1])))
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading standard input: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}

	return refs, nil
}

// GetSymbolicRef reads the symbolic reference.
func GetSymbolicRef(ctx context.Context, repoExecutor RepositoryExecutor, refname git.ReferenceName) (git.Reference, error) {
	var stdout strings.Builder
	if err := repoExecutor.ExecAndWait(ctx, Command{
		Name: "symbolic-ref",
		Args: []string{string(refname)},
	}, WithDisabledHooks(), WithStdout(&stdout)); err != nil {
		return git.Reference{}, err
	}

	symref, trailing, ok := strings.Cut(stdout.String(), "\n")
	if !ok {
		return git.Reference{}, fmt.Errorf("expected symbolic reference to be terminated by newline")
	} else if len(trailing) > 0 {
		return git.Reference{}, fmt.Errorf("symbolic reference has trailing data")
	}

	return git.NewSymbolicReference(refname, git.ReferenceName(symref)), nil
}

// ReferenceNameLengthRefusal returns the permanent error of a git process whose stderr holds the refusal
// of a reference name by its length, and nil for any other stderr. The hook refuses such a name in a
// fetch of existing references, which fails the same way until the name is deleted at the source.
func ReferenceNameLengthRefusal(stderr []byte) error {
	if !git.IsReferenceNameLengthRefusal(stderr) {
		return nil
	}

	return structerr.NewInvalidArgument(
		"a reference to write has a name over the limit of %d bytes: delete it at the source (doc/graft/LIMITATIONS.md)",
		git.MaxReferenceNameLength,
	)
}
