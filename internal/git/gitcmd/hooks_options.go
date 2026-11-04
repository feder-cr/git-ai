package gitcmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/grpc/metadata"
	"github.com/feder-cr/git-ai/internal/tracing"
	"github.com/feder-cr/git-ai/internal/transaction/txinfo"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// envInjector is responsible for injecting environment variables required for tracing into
// the child process.
var envInjector = tracing.InjectIntoEnv

// WithDisabledHooks returns an option that satisfies the requirement to set up
// hooks, but won't in fact set up hook execution.
func WithDisabledHooks() CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, cc *cmdCfg) error {
		cc.hooksConfigured = true
		return nil
	}
}

// WithRefTxHook returns an option that populates the safe command with the
// environment variables necessary to properly execute a reference hook for
// repository changes that may possibly update references
func WithRefTxHook(objectHash git.ObjectHash, repo storage.Repository) CmdOpt {
	return func(ctx context.Context, cfg config.Cfg, gitCmdFactory CommandFactory, cc *cmdCfg) error {
		if repo == nil {
			return fmt.Errorf("missing repo: %w", ErrInvalidArg)
		}

		// The reference-transaction hook does not need any project-specific information
		// about the repository. So in order to make the hook usable by sites which do not
		// have a project repository available (e.g. object pools), this function accepts a
		// `storage.Repository` and just creates an ad-hoc proto repo.
		if err := cc.configureHooks(ctx, cfg, objectHash, &gitalypb.Repository{
			StorageName:                   repo.GetStorageName(),
			GitAlternateObjectDirectories: repo.GetGitAlternateObjectDirectories(),
			GitObjectDirectory:            repo.GetGitObjectDirectory(),
			RelativePath:                  repo.GetRelativePath(),
		}, gitCmdFactory, nil, ReferenceTransactionHook); err != nil {
			return fmt.Errorf("ref hook env var: %w", err)
		}

		return nil
	}
}

// WithPackObjectsHookEnv provides metadata for gitaly-hooks so it can act as a pack-objects hook.
func WithPackObjectsHookEnv(objectHash git.ObjectHash, repo *gitalypb.Repository, protocol string) CmdOpt {
	return func(ctx context.Context, cfg config.Cfg, gitCmdFactory CommandFactory, cc *cmdCfg) error {
		if !cfg.PackObjectsCache.Enabled {
			return nil
		}

		if repo == nil {
			return fmt.Errorf("missing repo: %w", ErrInvalidArg)
		}

		userDetails := &UserDetails{
			Protocol: protocol,
			UserID:   metadata.GetValue(ctx, "user_id"),
			Username: metadata.GetValue(ctx, "username"),
			RemoteIP: metadata.GetValue(ctx, "remote_ip"),
		}

		if err := cc.configureHooks(
			ctx,
			cfg,
			objectHash,
			repo,
			gitCmdFactory,
			userDetails,
			PackObjectsHook,
		); err != nil {
			return fmt.Errorf("pack-objects hook configuration: %w", err)
		}

		cc.globals = append(cc.globals, ConfigPair{
			Key:   "uploadpack.packObjectsHook",
			Value: cfg.BinaryPath("gitaly-hooks"),
		})

		return nil
	}
}

// ReceivePackRequest abstracts away the different requests that end up
// spawning git-receive-pack.
type ReceivePackRequest interface {
	GetGlId() string
	GetGlUsername() string
	GetGlRepository() string
	GetRepository() *gitalypb.Repository
}

// WithReceivePackHooks returns an option that populates the safe command with the environment
// variables necessary to properly execute the pre-receive, update and post-receive hooks for
// git-receive-pack(1).
func WithReceivePackHooks(objectHash git.ObjectHash, req ReceivePackRequest, protocol string) CmdOpt {
	return func(ctx context.Context, cfg config.Cfg, gitCmdFactory CommandFactory, cc *cmdCfg) error {
		if err := cc.configureHooks(ctx, cfg, objectHash, req.GetRepository(), gitCmdFactory, &UserDetails{
			UserID:   req.GetGlId(),
			Username: req.GetGlUsername(),
			Protocol: protocol,
			RemoteIP: metadata.GetValue(ctx, "remote_ip"),
		}, ReceivePackHooks); err != nil {
			return err
		}

		return nil
	}
}

// configureHooks updates the command configuration to include all environment
// variables required by the reference transaction hook and any other needed
// options to successfully execute hooks.
func (cc *cmdCfg) configureHooks(
	ctx context.Context,
	cfg config.Cfg,
	objectHash git.ObjectHash,
	repo storage.Repository,
	gitCmdFactory CommandFactory,
	userDetails *UserDetails,
	requestedHooks Hook,
) error {
	if cc.hooksConfigured {
		return errors.New("hooks already configured")
	}

	var transaction *txinfo.Transaction
	if tx, err := txinfo.TransactionFromContext(ctx); err == nil {
		transaction = &tx
	} else if !errors.Is(err, txinfo.ErrTransactionNotFound) {
		return err
	}

	hooksPayload := NewHooksPayload(
		ctx,
		cfg,
		repo,
		objectHash,
		transaction,
		userDetails,
		requestedHooks,
		featureflag.FromContext(ctx),
	)

	// Git runs the reference-transaction hook of this command itself, possibly
	// for several transactions. The changeset log and the storage core tell them
	// apart from the transactions of other processes by a value unique to this
	// command. newCommand ties the value to the life of the process.
	if requestedHooks&ReferenceTransactionHook != 0 && InvocationRequired(cfg) {
		invocation, err := NewChangelogInvocation()
		if err != nil {
			return err
		}
		hooksPayload.Changelog.Invocation = invocation
		cc.invocation = invocation
	}

	payload, err := hooksPayload.Env()
	if err != nil {
		return err
	}

	cc.env = append(
		cc.env,
		payload,
		// Setting GOTRACEBACK=none ensures that if the gitaly-hooks process hits a fatal Go
		// runtime error, the runtime does not dump a verbose stacktrace to stderr. This causes
		// a lot of noise on the Git client side.
		"GOTRACEBACK=none",
	)
	cc.env = envInjector(ctx, cc.env)

	cc.globals = append(cc.globals, ConfigPair{Key: "core.hooksPath", Value: gitCmdFactory.HooksPath(ctx)})
	cc.hooksConfigured = true

	return nil
}
