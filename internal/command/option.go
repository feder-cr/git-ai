package command

import (
	"context"
	"io"
	"time"

	"github.com/feder-cr/git-ai/internal/cgroups"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/log"
)

type config struct {
	stdin       io.Reader
	stdinFilter func(io.Reader) io.Reader
	stdout      io.Writer
	stderr      io.Writer
	dir         string
	environment []string

	finalizers []func(context.Context, *Command)
	exitHooks  []func()

	commandName    string
	subcommandName string
	gitVersion     string
	refBackend     string

	cgroupsManager        cgroups.Manager
	cgroupsAddCommandOpts []cgroups.AddCommandOption
	// logConfiguration contains the logging configuration to pass to the
	// command if subprocess logging is in use.
	logConfiguration         log.Config
	completionErrorLogFilter func(*Command, string) bool
	// terminationGrace replaces defaultTerminationGrace when set; only tests set it.
	terminationGrace time.Duration
	// beforeEscalation is called when the grace period has passed, before it is decided whether the
	// group gets SIGKILL; afterEscalation is called with the decision. Only tests set them.
	beforeEscalation func()
	afterEscalation  func(signalled bool)
}

// Option is an option that can be passed to `New()` for controlling how the command is being
// created.
type Option func(cfg *config)

// WithStdin sets up the command to read from the given reader.
func WithStdin(stdin io.Reader) Option {
	return func(cfg *config) {
		cfg.stdin = stdin
	}
}

// WithSetupStdin instructs New() to configure the stdin pipe of the command it is creating. This
// allows you call Write() on the command as if it is an ordinary io.Writer, sending data directly
// to the stdin of the process.
func WithSetupStdin() Option {
	return func(cfg *config) {
		cfg.stdin = stdinSentinel{}
	}
}

// WithStdinFilter makes the command read its stdin through filter: the reader given with
// WithStdin, or what is written to the command when it was set up with WithSetupStdin.
func WithStdinFilter(filter func(io.Reader) io.Reader) Option {
	return func(cfg *config) {
		cfg.stdinFilter = filter
	}
}

// WithStdout sets up the command to write standard output to the given writer.
func WithStdout(stdout io.Writer) Option {
	return func(cfg *config) {
		cfg.stdout = stdout
	}
}

// WithSetupStdout instructs New() to configure the standard output pipe of the command it is creating. This allowsyou
// to call Read() on the command as if it is an ordinary io.Reader, reading output directly from the stdout of the
// process.
func WithSetupStdout() Option {
	return func(cfg *config) {
		cfg.stdout = stdoutSentinel{}
	}
}

// WithStderr sets up the command to write standard error to the given writer.
func WithStderr(stderr io.Writer) Option {
	return func(cfg *config) {
		cfg.stderr = stderr
	}
}

// WithDir will set up the command to be ran in the specific directory.
func WithDir(dir string) Option {
	return func(cfg *config) {
		cfg.dir = dir
	}
}

// WithEnvironment sets up environment variables that shall be set for the command.
func WithEnvironment(environment []string) Option {
	return func(cfg *config) {
		cfg.environment = environment
	}
}

// WithCommandName overrides the "cmd" and "subcmd" label used in metrics.
func WithCommandName(commandName, subcommandName string) Option {
	return func(cfg *config) {
		cfg.commandName = commandName
		cfg.subcommandName = subcommandName
	}
}

// WithCommandGitVersion overrides the "git_version" label used in metrics.
func WithCommandGitVersion(gitCmdVersion string) Option {
	return func(cfg *config) {
		cfg.gitVersion = gitCmdVersion
	}
}

// WithReferenceBackend overrides the "reference_backend" label used in metrics.
func WithReferenceBackend(refBackend git.ReferenceBackend) Option {
	return func(cfg *config) {
		cfg.refBackend = refBackend.Name
	}
}

// WithCgroup adds the spawned command to a Cgroup. The bucket used will be derived from the
// command's arguments and/or from the repository.
func WithCgroup(cgroupsManager cgroups.Manager, opts ...cgroups.AddCommandOption) Option {
	return func(cfg *config) {
		cfg.cgroupsManager = cgroupsManager
		cfg.cgroupsAddCommandOpts = opts
	}
}

// WithFinalizer sets up the finalizer to be run when the command is being wrapped up. It will be
// called after `Wait()` has returned.
func WithFinalizer(finalizer func(context.Context, *Command)) Option {
	return func(cfg *config) {
		cfg.finalizers = append(cfg.finalizers, finalizer)
	}
}

// WithExitHook sets up a function to run when exec.Cmd.Wait has returned, however the process ended:
// a normal exit, an error, a kill through the context, or a panic in the goroutine that reaps it.
// exec.Cmd.Wait also waits for the goroutines that copy a standard stream which is not a file, so a
// command whose stdin comes from a client takes it from DetachStdin: otherwise the hook waits for
// the client to close its stream, long after the process exited. Unlike a finalizer it runs even when
// the rest of the wrap-up panics. It does not run for a process that never started: New then returns
// an error and the caller keeps the responsibility. It must not block.
func WithExitHook(hook func()) Option {
	return func(cfg *config) {
		cfg.exitHooks = append(cfg.exitHooks, hook)
	}
}

// WithSubprocessLogger sets up a goroutine that consumes logs from the subprocess through a pipe
// and outputs them in Logger's output.
func WithSubprocessLogger(logConfig log.Config) Option {
	return func(cfg *config) {
		cfg.logConfiguration = logConfig
	}
}

// WithCompletionErrorLogFilter configures a function that should return true if an errored
// command should not produce logs.
func WithCompletionErrorLogFilter(fn func(cmd *Command, stderr string) bool) Option {
	return func(cfg *config) {
		cfg.completionErrorLogFilter = fn
	}
}
