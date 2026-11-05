package localrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// FetchOptsTags controls what tags needs to be imported on fetch.
type FetchOptsTags string

func (t FetchOptsTags) String() string {
	return string(t)
}

var (
	// FetchOptsTagsDefault enables importing of tags only on fetched branches.
	FetchOptsTagsDefault = FetchOptsTags("")
	// FetchOptsTagsAll enables importing of every tag from the remote repository.
	FetchOptsTagsAll = FetchOptsTags("--tags")
	// FetchOptsTagsNone disables importing of tags from the remote repository.
	FetchOptsTagsNone = FetchOptsTags("--no-tags")
)

// FetchOpts is used to configure invocation of the 'FetchRemote' command.
type FetchOpts struct {
	// Env is a list of env vars to pass to the cmd.
	Env []string
	// CommandOptions is a list of options to use with 'git' command.
	CommandOptions []gitcmd.CmdOpt
	// Prune if set fetch removes any remote-tracking references that no longer exist on the remote.
	// https://git-scm.com/docs/git-fetch#Documentation/git-fetch.txt---prune
	Prune bool
	// Force if set fetch overrides local references with values from remote that's
	// doesn't have the previous commit as an ancestor.
	// https://git-scm.com/docs/git-fetch#Documentation/git-fetch.txt---force
	Force bool
	// Verbose controls how much information is written to stderr. The list of
	// refs updated by the fetch will only be listed if verbose is true.
	// https://git-scm.com/docs/git-fetch#Documentation/git-fetch.txt---quiet
	// https://git-scm.com/docs/git-fetch#Documentation/git-fetch.txt---verbose
	Verbose bool
	// Tags controls whether tags will be fetched as part of the remote or not.
	// https://git-scm.com/docs/git-fetch#Documentation/git-fetch.txt---tags
	// https://git-scm.com/docs/git-fetch#Documentation/git-fetch.txt---no-tags
	Tags FetchOptsTags
	// DryRun, if enabled, performs the `git-fetch(1)` command without updating any references.
	DryRun bool
	// Porcelain controls `git-fetch(1)` command output and when enabled prints output in an
	// easy-to-parse format. By default, `git-fetch(1)` output is suppressed by the `--quiet` flag.
	// Therefore, the Verbose option must also be enabled to receive output.
	Porcelain bool
	// Stdout if set it would be used to redirect stdout stream into it.
	Stdout io.Writer
	// Stderr if set it would be used to redirect stderr stream into it.
	Stderr io.Writer
	// DisableTransactions will disable the reference-transaction hook and atomic transactions.
	DisableTransactions bool
	// NegotiationTips restricts what is used as negotiation tips during
	// the fetch. When set, `git-fetch(1)` only advertises objects reachable
	// from the specified commits or refs as "have" objects to the server,
	// rather than advertising all local refs.
	// https://git-scm.com/docs/git-fetch#Documentation/git-fetch.txt---negotiation-tipltcommitidgt
	NegotiationTips []string
	// HideInternalRefs excludes GitLab's internal reference namespaces (e.g.
	// keep-around, merge-request or pipeline refs) from the post-fetch
	// connectivity check. This can significantly speed up fetches into
	// repositories with a huge number of internal references.
	//
	// The connectivity check verifies the fetched objects using
	//     git rev-list --objects --stdin --not --exclude-hidden=fetch --all
	// The fetched tips are fed on stdin, while `--not --all` enumerates every
	// local reference and traverses its history to mark those objects as
	// uninteresting.
	//
	// Excluding references does not weaken the check. By hiding references
	// there are fewer revisions marked uninteresting in the revision walk.
	// This might make the check consider more objects, not fewer. But
	// because the internal references are numerous, and the fetched tips
	// usually target a local branch, this should result in a speedup.
	HideInternalRefs bool
}

// FetchFailedError indicates that the fetch has failed.
type FetchFailedError struct {
	err error
}

// Error returns the error message.
func (e FetchFailedError) Error() string {
	return e.err.Error()
}

// Unwrap returns the cause of the failure.
func (e FetchFailedError) Unwrap() error {
	return e.err
}

// FetchRemote fetches changes from the specified remote. Returns an FetchFailedError error in case
// the fetch itself failed.
func (repo *Repo) FetchRemote(ctx context.Context, remoteName string, opts FetchOpts) error {
	if err := validateNotBlank(remoteName, "remoteName"); err != nil {
		return err
	}

	var stderr bytes.Buffer
	if opts.Stderr == nil {
		opts.Stderr = &stderr
	}

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	commandOptions := []gitcmd.CmdOpt{
		gitcmd.WithEnv(opts.Env...),
		gitcmd.WithStdout(opts.Stdout),
		gitcmd.WithStderr(opts.Stderr),
		gitcmd.WithConfig(gitcmd.ConfigPair{
			// Git is so kind to point out that we asked it to not show forced updates
			// by default, so we need to ask it not to do that.
			Key: "advice.fetchShowForcedUpdates", Value: "false",
		}),
		gitcmd.WithConfig(gitcmd.ConfigPair{
			// The patch series https://lore.kernel.org/git/20240910203835.2288291-1-bence@ferdinandy.com/
			// introduces new behaviour that automatically sets the local HEAD to the remote's HEAD during
			// a fetch. This happens when the mirror refspec is used to fetch into a bare repository, which
			// we use in operations like `FetchBundle`.
			//
			// Setting the remote's `followremotehead` config to "never" will disable the new behaviour. We
			// do this temporarily until we're sure the new behaviour doesn't have any consequences.
			Key: fmt.Sprintf("remote.%s.followremotehead", remoteName), Value: "never",
		}),
		gitcmd.WithConfig(opts.buildConfig()...),
	}
	if opts.DisableTransactions {
		commandOptions = append(commandOptions, gitcmd.WithDisabledHooks())
	} else {
		commandOptions = append(commandOptions, gitcmd.WithRefTxHook(objectHash, repo))
	}
	commandOptions = append(commandOptions, opts.CommandOptions...)

	cmd, err := repo.Exec(ctx,
		gitcmd.Command{
			Name:  "fetch",
			Flags: opts.buildFlags(),
			Args:  []string{remoteName},
		},
		commandOptions...,
	)
	if err != nil {
		return err
	}

	if err := cmd.Wait(); err != nil {
		return opts.fetchFailed(err, stderr.Bytes())
	}

	return nil
}

// FetchInternal performs a fetch from an internal Gitaly-hosted repository. Returns an
// FetchFailedError error in case git-fetch(1) failed.
func (repo *Repo) FetchInternal(
	ctx context.Context,
	remoteRepo *gitalypb.Repository,
	refspecs []string,
	opts FetchOpts,
) error {
	if len(refspecs) == 0 {
		return fmt.Errorf("fetch internal called without refspecs")
	}

	var stderr bytes.Buffer
	if opts.Stderr == nil {
		opts.Stderr = &stderr
	}

	commandOptions := []gitcmd.CmdOpt{
		gitcmd.WithEnv(opts.Env...),
		gitcmd.WithStderr(opts.Stderr),
		gitcmd.WithInternalFetchWithSidechannel(
			&gitalypb.SSHUploadPackWithSidechannelRequest{
				Repository:       remoteRepo,
				GitConfigOptions: []string{"uploadpack.allowAnySHA1InWant=true"},
				GitProtocol:      gitcmd.ProtocolV2,
			},
		),
		gitcmd.WithConfig(gitcmd.ConfigPair{
			// Git is so kind to point out that we asked it to not show forced updates
			// by default, so we need to ask it not to do that.
			Key: "advice.fetchShowForcedUpdates", Value: "false",
		}),
	}

	commandOptions = append(commandOptions, gitcmd.WithConfig(opts.buildConfig()...))

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	if opts.DisableTransactions {
		commandOptions = append(commandOptions, gitcmd.WithDisabledHooks())
	} else {
		commandOptions = append(commandOptions, gitcmd.WithRefTxHook(objectHash, repo))
	}
	commandOptions = append(commandOptions, opts.CommandOptions...)

	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "fetch",
			Flags: opts.buildFlags(),
			Args: append(
				[]string{gitcmd.InternalGitalyURL},
				refspecs...,
			),
		},
		commandOptions...,
	); err != nil {
		return opts.fetchFailed(err, stderr.Bytes())
	}

	return nil
}

// fetchFailed returns the error of a fetch that failed with err and stderr. The hook's refusal of a
// reference name by its length is a permanent error. Without transactions no hook ran, so stderr can
// only hold text from elsewhere.
func (opts FetchOpts) fetchFailed(err error, stderr []byte) error {
	if !opts.DisableTransactions {
		if refusal := gitcmd.ReferenceNameLengthRefusal(stderr); refusal != nil {
			return FetchFailedError{refusal}
		}
	}

	return FetchFailedError{errorWithStderr(err, stderr)}
}

func (opts FetchOpts) buildFlags() []gitcmd.Option {
	flags := []gitcmd.Option{
		// We don't need FETCH_HEAD, and it can potentially be hundreds of megabytes when
		// doing a mirror-sync of repos with huge numbers of references.
		gitcmd.Flag{Name: "--no-write-fetch-head"},
	}

	if !opts.Verbose {
		flags = append(flags, gitcmd.Flag{Name: "--quiet"})
	}

	if opts.Prune {
		flags = append(flags, gitcmd.Flag{Name: "--prune"})
	}

	if opts.Force {
		flags = append(flags, gitcmd.Flag{Name: "--force"})
	}

	if opts.Tags != FetchOptsTagsDefault {
		flags = append(flags, gitcmd.Flag{Name: opts.Tags.String()})
	}

	if !opts.DisableTransactions {
		flags = append(flags, gitcmd.Flag{Name: "--atomic"})
	}

	if opts.DryRun {
		flags = append(flags, gitcmd.Flag{Name: "--dry-run"})
	}

	if opts.Porcelain {
		flags = append(flags, gitcmd.Flag{Name: "--porcelain"})
	}

	// Even if we ask Git to not print any output and to force-update branches it will still
	// compute whether branches have been force-updated only to discard that information again.
	// Let's ask it not to given that this check can be quite expensive.
	if !opts.Verbose && opts.Force {
		flags = append(flags, gitcmd.Flag{Name: "--no-show-forced-updates"})
	}

	for _, tip := range opts.NegotiationTips {
		flags = append(flags, gitcmd.ValueFlag{Name: "--negotiation-tip", Value: tip})
	}

	return flags
}

// buildConfig returns the git configuration that is common to all git-fetch(1) invocations.
func (opts FetchOpts) buildConfig() []gitcmd.ConfigPair {
	var configPairs []gitcmd.ConfigPair

	// The connectivity check's `--exclude-hidden=fetch` consults
	// `fetch.hideRefs` (see git-rev-list(1)), so listing the internal
	// namespaces here excludes them from that check.
	if opts.HideInternalRefs {
		for refPrefix := range git.InternalRefPrefixes {
			configPairs = append(configPairs, gitcmd.ConfigPair{
				Key: "fetch.hideRefs", Value: refPrefix,
			})
		}
	}

	return configPairs
}

func validateNotBlank(val, name string) error {
	if strings.TrimSpace(val) == "" {
		return fmt.Errorf("%w: %q is blank or empty", gitcmd.ErrInvalidArg, name)
	}
	return nil
}

func envGitSSHCommand(cmd string) string {
	return "GIT_SSH_COMMAND=" + cmd
}

// PushOptions are options that can be configured for a push.
type PushOptions struct {
	// SSHCommand is the command line to use for git's SSH invocation. The command line is used
	// as is and must be verified by the caller to be safe.
	SSHCommand string
	// Force decides whether to force push all of the refspecs.
	Force bool
	// Config is the Git configuration which gets passed to the git-push(1) invocation.
	// Configuration is set up via `WithConfigEnv()`, so potential credentials won't be leaked
	// via the command line.
	Config []gitcmd.ConfigPair
}

// Push force pushes the refspecs to the remote.
func (repo *Repo) Push(ctx context.Context, remote string, refspecs []string, options PushOptions) error {
	if len(refspecs) == 0 {
		return errors.New("refspecs to push must be explicitly specified")
	}

	var env []string
	if options.SSHCommand != "" {
		env = append(env, envGitSSHCommand(options.SSHCommand))
	}

	var flags []gitcmd.Option
	if options.Force {
		flags = append(flags, gitcmd.Flag{Name: "--force"})
	}

	stderr := &bytes.Buffer{}
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name:  "push",
			Flags: flags,
			Args:  append([]string{remote}, refspecs...),
		},
		gitcmd.WithStderr(stderr),
		gitcmd.WithEnv(env...),
		gitcmd.WithConfigEnv(options.Config...),
	); err != nil {
		return fmt.Errorf("git push: %w, stderr: %q", err, stderr)
	}

	return nil
}
