package hook

import (
	"context"
	"fmt"
	"io"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

//nolint:revive // This is unintentionally missing documentation.
func (m *GitLabHookManager) UpdateHook(ctx context.Context, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error {
	payload, err := gitcmd.HooksPayloadFromEnv(env)
	if err != nil {
		return structerr.NewInternal("extracting hooks payload: %w", err)
	}

	if isPrimary(payload) {
		if err := m.updateHook(ctx, payload, repo, ref, oldValue, newValue, env, stdout, stderr); err != nil {
			m.logger.WithError(err).WarnContext(ctx, "stopping transaction because update hook failed")

			// If the update hook declines the push, then we need
			// to stop any secondaries voting on the transaction.
			if err := m.stopTransaction(ctx, payload); err != nil {
				m.logger.WithError(err).ErrorContext(ctx, "failed stopping transaction in update hook")
			}

			return err
		}
	}

	if err := m.synchronizeHookExecution(ctx, payload, "update"); err != nil {
		return fmt.Errorf("synchronizing update hook: %w", err)
	}

	return nil
}

func (m *GitLabHookManager) updateHook(ctx context.Context, payload gitcmd.HooksPayload, repo *gitalypb.Repository, ref, oldValue, newValue string, env []string, stdout, stderr io.Writer) error {
	objectHash, err := git.ObjectHashByFormat(payload.ObjectFormat)
	if err != nil {
		return fmt.Errorf("looking up object hash: %w", err)
	}

	if ref == "" {
		return structerr.NewInternal("hook got no reference")
	}
	if err := objectHash.ValidateHex(oldValue); err != nil {
		return structerr.NewInternal("hook got invalid old value: %w", err)
	}
	if err := objectHash.ValidateHex(newValue); err != nil {
		return structerr.NewInternal("hook got invalid new value: %w", err)
	}
	if payload.UserDetails == nil {
		return structerr.NewInternal("payload has no receive hooks info")
	}

	// The update hook runs for each reference of a push, so this refuses the one reference and
	// tells the client why, and the other references of a non-atomic push still land. Deleting an
	// existing long reference stays possible.
	if !objectHash.IsZeroOID(git.ObjectID(newValue)) {
		if err := git.ValidateReferenceNameLength(ref); err != nil {
			fmt.Fprintf(stderr, "%s\n", err)
			return structerr.NewInvalidArgument("%w", err)
		}
	}

	// The same holds for a name the storage core's database cannot record next to another one.
	created := objectHash.IsZeroOID(git.ObjectID(oldValue)) && !objectHash.IsZeroOID(git.ObjectID(newValue))
	if err := m.checkCreatedRefName(ctx, payload, env, ref, created, stderr); err != nil {
		return err
	}

	executor, err := m.newCustomHooksExecutor(ctx, repo, "update")
	if err != nil {
		return structerr.NewInternal("%w", err)
	}

	customEnv, err := m.customHooksEnv(ctx, payload, nil, env)
	if err != nil {
		return structerr.NewInternal("constructing custom hook environment: %w", err)
	}

	if err = executor(
		ctx,
		[]string{ref, oldValue, newValue},
		customEnv,
		nil,
		stdout,
		stderr,
	); err != nil {
		return fmt.Errorf("executing custom hooks: %w", err)
	}

	return nil
}
