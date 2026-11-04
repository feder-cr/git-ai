package gitcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/x509"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"gitlab.com/gitlab-org/labkit/correlation"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	// InternalGitalyURL is a special URL that indicates Gitaly wants to push to or fetch from
	// another internal Gitaly instance.
	InternalGitalyURL = "ssh://gitaly/internal.git"
)

var flagRegex = regexp.MustCompile(`^(-|--)[[:alnum:]]`)

// GlobalOption is an interface for all options which can be globally applied
// to git commands. This is the command-inspecific part before the actual
// command that's being run, e.g. the `-c` part in `git -c foo.bar=value
// command`.
type GlobalOption interface {
	GlobalArgs() ([]string, error)
}

// Option is a git command line flag with validation logic
type Option interface {
	OptionArgs() ([]string, error)
}

// ConfigPair is a GlobalOption that can be passed to Git commands to inject per-command config
// entries via the `git -c` switch.
type ConfigPair = config.GitConfig

// ConfigPairsToGitEnvironment converts the given config pairs into a set of environment variables
// that can be injected into a Git executable.
func ConfigPairsToGitEnvironment(configPairs []ConfigPair) []string {
	env := make([]string, 0, len(configPairs)*2+1)

	for i, configPair := range configPairs {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, configPair.Key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, configPair.Value),
		)
	}

	return append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(configPairs)))
}

// Flag is a single token optional command line argument that enables or
// disables functionality (e.g. "-L")
type Flag struct {
	Name string
}

// GlobalArgs returns the arguments for the given flag, which should typically
// only be the flag itself. It returns an error if the flag is not sanitary.
func (f Flag) GlobalArgs() ([]string, error) {
	return f.OptionArgs()
}

// OptionArgs returns an error if the flag is not sanitary
func (f Flag) OptionArgs() ([]string, error) {
	if !flagRegex.MatchString(f.Name) {
		return nil, fmt.Errorf("flag %q failed regex validation: %w", f.Name, ErrInvalidArg)
	}
	return []string{f.Name}, nil
}

// ValueFlag is an optional command line argument that is comprised of pair of
// tokens (e.g. "-n 50")
type ValueFlag struct {
	Name  string
	Value string
}

// GlobalArgs returns the arguments for the given value flag, which should
// typically be two arguments: the flag and its value. It returns an error if the value flag is not sanitary.
func (vf ValueFlag) GlobalArgs() ([]string, error) {
	return vf.OptionArgs()
}

// OptionArgs returns an error if the flag is not sanitary
func (vf ValueFlag) OptionArgs() ([]string, error) {
	if !flagRegex.MatchString(vf.Name) {
		return nil, fmt.Errorf("value flag %q failed regex validation: %w", vf.Name, ErrInvalidArg)
	}
	return []string{vf.Name, vf.Value}, nil
}

// ConvertGlobalOptions converts a protobuf message to a CmdOpt.
func ConvertGlobalOptions(options *gitalypb.GlobalOptions) []CmdOpt {
	if options != nil && options.GetLiteralPathspecs() {
		return []CmdOpt{
			WithGlobalOption(Flag{
				Name: "--literal-pathspecs",
			}),
		}
	}

	return nil
}

// ConvertConfigOptions converts `<key>=<value>` config entries into `ConfigPairs`.
func ConvertConfigOptions(options []string) ([]ConfigPair, error) {
	configPairs := make([]ConfigPair, len(options))

	for i, option := range options {
		configPair := strings.SplitN(option, "=", 2)
		if len(configPair) != 2 {
			return nil, fmt.Errorf("cannot convert invalid config key: %q", option)
		}

		configPairs[i] = ConfigPair{Key: configPair[0], Value: configPair[1]}
	}

	return configPairs, nil
}

type cmdCfg struct {
	env             []string
	globals         []GlobalOption
	commandOpts     []command.Option
	hooksConfigured bool
	worktreePath    string
	// invocation is the ChangelogMeta.Invocation of the command, see BeginInvocation.
	invocation string
}

// CmdOpt is an option for running a command
type CmdOpt func(context.Context, config.Cfg, CommandFactory, *cmdCfg) error

// WithStdin sets the command's stdin. Pass `command.SetupStdin` to make the
// command suitable for `Write()`ing to.
func WithStdin(r io.Reader) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.commandOpts = append(c.commandOpts, command.WithStdin(r))
		return nil
	}
}

// WithSetupStdin sets up the command so that it can be `Write()`en to.
func WithSetupStdin() CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.commandOpts = append(c.commandOpts, command.WithSetupStdin())
		return nil
	}
}

// WithStdout sets the command's stdout.
func WithStdout(w io.Writer) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.commandOpts = append(c.commandOpts, command.WithStdout(w))
		return nil
	}
}

// WithSetupStdout sets up the command so that it can be `Read()` from.
func WithSetupStdout() CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.commandOpts = append(c.commandOpts, command.WithSetupStdout())
		return nil
	}
}

// WithStderr sets the command's stderr.
func WithStderr(w io.Writer) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.commandOpts = append(c.commandOpts, command.WithStderr(w))
		return nil
	}
}

// WithEnv adds environment variables to the command.
func WithEnv(envs ...string) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.env = append(c.env, envs...)
		return nil
	}
}

// WithConfig adds git configuration entries to the command.
func WithConfig(configPairs ...ConfigPair) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		for _, configPair := range configPairs {
			c.globals = append(c.globals, configPair)
		}
		return nil
	}
}

// WithConfigEnv adds git configuration entries to the command's environment. This should be used
// in place of `WithConfig()` in case config entries may contain secrets which shouldn't leak e.g.
// via the process's command line.
func WithConfigEnv(configPairs ...ConfigPair) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.env = append(c.env, ConfigPairsToGitEnvironment(configPairs)...)
		return nil
	}
}

// WithGlobalOption adds the global options to the command. These are universal options which work
// across all git commands.
func WithGlobalOption(opts ...GlobalOption) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.globals = append(c.globals, opts...)
		return nil
	}
}

// WithInternalFetchWithSidechannel returns an option which sets up git-fetch(1) to fetch from
// another internal Gitaly node. In contrast to WithInternalFetch, this will call
// SSHUploadPackWithSidechannel instead of SSHUploadPack.
func WithInternalFetchWithSidechannel(req *gitalypb.SSHUploadPackWithSidechannelRequest) CmdOpt {
	return func(ctx context.Context, cfg config.Cfg, _ CommandFactory, c *cmdCfg) error {
		payload, err := protojson.Marshal(req)
		if err != nil {
			return structerr.NewInternal("marshalling payload failed: %w", err)
		}

		serversInfo, err := storage.ExtractGitalyServers(ctx)
		if err != nil {
			return structerr.NewInternal("extracting Gitaly servers: %w", err)
		}

		storageInfo, ok := serversInfo[req.GetRepository().GetStorageName()]
		if !ok {
			return structerr.NewInvalidArgument("no storage info for %q", req.GetRepository().GetStorageName())
		}

		if storageInfo.Address == "" {
			return structerr.NewInvalidArgument("empty Gitaly address")
		}

		var flagsWithValue []string
		for flag, value := range featureflag.FromContext(ctx) {
			flagsWithValue = append(flagsWithValue, flag.FormatWithValue(value))
		}

		c.env = append(c.env,
			fmt.Sprintf("GITALY_PAYLOAD=%s", payload),
			fmt.Sprintf("GIT_SSH_COMMAND=%s %s", cfg.BinaryPath("gitaly-ssh"), "upload-pack"),
			fmt.Sprintf("GITALY_ADDRESS=%s", storageInfo.Address),
			fmt.Sprintf("GITALY_TOKEN=%s", storageInfo.Token),
			fmt.Sprintf("GITALY_FEATUREFLAGS=%s", strings.Join(flagsWithValue, ",")),
			fmt.Sprintf("CORRELATION_ID=%s", correlation.ExtractFromContextOrGenerate(ctx)),
			// please see https://github.com/git/git/commit/0da0e49ba12225684b75e86a4c9344ad121652cb for mote details
			"GIT_SSH_VARIANT=simple",
			// Pass through the SSL_CERT_* variables that indicate which
			// system certs to trust
			fmt.Sprintf("%s=%s", x509.SSLCertDir, os.Getenv(x509.SSLCertDir)),
			fmt.Sprintf("%s=%s", x509.SSLCertFile, os.Getenv(x509.SSLCertFile)),
		)

		return nil
	}
}

// WithGitalyGPG sets gpg.prgoram to gitaly-gpg for commit signing
func WithGitalyGPG() CmdOpt {
	return func(_ context.Context, cfg config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.globals = append(
			c.globals,
			&ConfigPair{
				Key:   "gpg.program",
				Value: cfg.BinaryPath("gitaly-gpg"),
			},
		)

		return nil
	}
}

// WithFinalizer sets up the finalizer to be run when the command is being wrapped up. It will be
// called after `Wait()` has returned.
func WithFinalizer(finalizer func(context.Context, *command.Command)) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.commandOpts = append(c.commandOpts, command.WithFinalizer(finalizer))
		return nil
	}
}

// WithReferenceBackend sets the 'reference_backend' metric label on the command.
func WithReferenceBackend(refBackend git.ReferenceBackend) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.commandOpts = append(c.commandOpts, command.WithReferenceBackend(refBackend))
		return nil
	}
}

// WithCompletionErrorLogFilter configures a function that should return true if an errored
// command should not produce logs.
func WithCompletionErrorLogFilter(fn func(cmd *command.Command, stderr string) bool) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.commandOpts = append(c.commandOpts, command.WithCompletionErrorLogFilter(fn))
		return nil
	}
}

// WithDir configures the working directory of the process.
func WithDir(dir string) CmdOpt {
	return func(_ context.Context, _ config.Cfg, _ CommandFactory, c *cmdCfg) error {
		c.commandOpts = append(c.commandOpts, command.WithDir(dir))
		return nil
	}
}
