package command

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/feder-cr/git-ai/internal/burdenmonitor"
	"github.com/feder-cr/git-ai/internal/command/commandcounter"
	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/requestinfohandler"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/tracing"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var (
	cpuSecondsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitaly_command_cpu_seconds_total",
			Help: "Sum of CPU time spent by shelling out",
		},
		[]string{"grpc_service", "grpc_method", "cmd", "subcmd", "mode", "git_version", "reference_backend"},
	)
	realSecondsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitaly_command_real_seconds_total",
			Help: "Sum of real time spent by shelling out",
		},
		[]string{"grpc_service", "grpc_method", "cmd", "subcmd", "git_version", "reference_backend"},
	)
	minorPageFaultsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitaly_command_minor_page_faults_total",
			Help: "Sum of minor page faults performed while shelling out",
		},
		[]string{"grpc_service", "grpc_method", "cmd", "subcmd", "git_version", "reference_backend"},
	)
	majorPageFaultsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitaly_command_major_page_faults_total",
			Help: "Sum of major page faults performed while shelling out",
		},
		[]string{"grpc_service", "grpc_method", "cmd", "subcmd", "git_version", "reference_backend"},
	)
	signalsReceivedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitaly_command_signals_received_total",
			Help: "Sum of signals received while shelling out",
		},
		[]string{"grpc_service", "grpc_method", "cmd", "subcmd", "git_version", "reference_backend"},
	)
	contextSwitchesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitaly_command_context_switches_total",
			Help: "Sum of context switches performed while shelling out",
		},
		[]string{"grpc_service", "grpc_method", "cmd", "subcmd", "ctxswitchtype", "git_version", "reference_backend"},
	)
	spawnForkingTimeHistogram = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "gitaly_spawn_forking_time_seconds",
			Help:    "Histogram of actual forking time after spawn tokens are acquired",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 10),
		},
	)

	// exportedEnvVars contains a list of environment variables
	// that are always exported to child processes on spawn
	exportedEnvVars = []string{
		"HOME",
		"PATH",
		"LD_LIBRARY_PATH",
		"TZ",

		// Export git tracing variables for easier debugging
		"GIT_TRACE",
		"GIT_TRACE_PACK_ACCESS",
		"GIT_TRACE_PACKET",
		"GIT_TRACE_PERFORMANCE",
		"GIT_TRACE_SETUP",

		// GIT_EXEC_PATH tells Git where to find its binaries. This must be exported
		// especially in the case where we use bundled Git executables given that we cannot
		// rely on a complete Git installation in that case.
		"GIT_EXEC_PATH",

		// GIT_DEFAULT_REF_FORMAT ...
		"GIT_DEFAULT_REF_FORMAT",

		// Git HTTP proxy settings:
		// https://git-scm.com/docs/git-config#git-config-httpproxy
		"all_proxy",
		"http_proxy",
		"HTTP_PROXY",
		"https_proxy",
		"HTTPS_PROXY",
		// libcurl settings: https://curl.haxx.se/libcurl/c/CURLOPT_NOPROXY.html
		"no_proxy",
		"NO_PROXY",

		// We must export this variable to child processes or otherwise we end up in
		// an inconsistent state, where the parent process has all feature flags
		// force-enabled while the child is using the usual defaults.
		featureflag.EnableAllFeatureFlagsEnvVar,
	}

	// envInjector is responsible for injecting environment variables required for tracing into
	// the child process.
	envInjector = tracing.InjectIntoEnv
)

const (
	// maxStderrBytes is at most how many bytes will be written to stderr
	maxStderrBytes = 10000 // 10kb
	// maxStderrLineLength is at most how many bytes a single line will be
	// written to stderr. Lines exceeding this limit should be truncated
	maxStderrLineLength = 4096
	// maxLoggedArgsElements is at most how many elements of a command's argument
	// list will be included in log entries. The argument list can grow large in
	// some RPCs like CommitDiff, where individual paths are provided as arguments
	// to perform selective diffing.
	maxLoggedArgsElements = 100
	// defaultTerminationGrace is how long the process group of a command whose context is done has
	// to end after SIGTERM, before it gets SIGKILL. git takes milliseconds to remove its locks, and
	// SIGKILL leaves them behind, so this only has to be long for a machine under load. It is how
	// long a caller of a cancelled command waits for a group that ignores SIGTERM.
	defaultTerminationGrace = 10 * time.Second
)

// Command encapsulates a running exec.Cmd. The embedded exec.Cmd is
// terminated and reaped automatically when the context.Context that
// created it is canceled.
type Command struct {
	logger log.Logger

	reader       io.ReadCloser
	writer       io.WriteCloser
	stderrBuffer *stderrBuffer
	cmd          *exec.Cmd
	context      context.Context
	startTime    time.Time

	waitError    error
	waitOnce     sync.Once
	teardownOnce sync.Once
	// subprocessLoggerDone is closed once the subprocess log consuming
	// goroutine has finished.
	subprocessLoggerDone chan struct{}
	processExitedCh      chan struct{}

	finalizers []func(context.Context, *Command)
	// exitHooks run when wait ends, see WithExitHook.
	exitHooks []func()

	span trace.Span

	metricsCmd     string
	metricsSubCmd  string
	metricsService string
	metricsMethod  string
	cgroupPath     string
	cmdGitVersion  string
	refBackend     string

	maxRssAnon atomic.Int64

	completionErrorLogFilter func(cmd *Command, stderr string) bool
}

func (c *Command) commandName() string {
	name := path.Base(c.cmd.Path)
	if c.metricsCmd != "" {
		name = c.metricsCmd
	}
	if c.metricsSubCmd != "" {
		name += " " + c.metricsSubCmd
	}
	return name
}

// New creates a Command from the given executable name and arguments On success, the Command
// contains a running subprocess. When ctx is canceled the embedded process will be terminated and
// reaped automatically.
func New(ctx context.Context, logger log.Logger, nameAndArgs []string, opts ...Option) (_ *Command, returnedErr error) {
	if ctx.Done() == nil {
		panic("command spawned with context without Done() channel")
	}

	// Don't launch the command if the context is already canceled. This matches
	// Go's own CommandContext's behavior which doesn't start a command if the
	// context is already canceled. We can't use it currently due to it sending a
	// SIGKILL to the command when the context is canceled during execution. This is not
	// fine as we don't have proper logic to recover from git crashes by for example
	// cleaning stale reference locks.
	//
	// Without this, racy behavior will emerge as the command execution will race with the
	// context cancellation. This really only helps with cases when the context is already
	// canceled before calling New. Raciness still ensues if the context is canceled after
	// this check. More details at: https://gitlab.com/gitlab-org/gitaly/-/issues/5021
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(nameAndArgs) == 0 {
		panic("command spawned without name")
	}

	if err := checkNullArgv(nameAndArgs); err != nil {
		return nil, err
	}

	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}

	cmdName := path.Base(nameAndArgs[0])

	startForking := time.Now()
	defer func() {
		delta := time.Since(startForking)
		spawnForkingTimeHistogram.Observe(delta.Seconds())

		if customFields := log.CustomFieldsFromContext(ctx); customFields != nil {
			customFields.RecordSum("command.spawn_token_fork_ms", int(delta.Milliseconds()))
		}
	}()

	logPid := -1
	defer func() {
		logger.WithFields(log.Fields{
			"pid":  logPid,
			"path": nameAndArgs[0],
			"args": nameAndArgs[1:],
		}).DebugContext(ctx, "spawn")
	}()

	var spanName string
	if cfg.commandName != "" && cfg.subcommandName != "" {
		spanName = fmt.Sprintf("%s-%s", cfg.commandName, cfg.subcommandName)
	} else {
		spanName = cmdName
	}
	span, ctx := tracing.StartSpanIfHasParent(
		ctx,
		spanName,
		[]attribute.KeyValue{
			attribute.String("path", nameAndArgs[0]),
			attribute.String("args", strings.Join(nameAndArgs[1:], " ")),
		},
	)
	// Not exec.CommandContext: it sends SIGKILL when ctx is done, and a git that dies that way leaves
	// its reference lock behind (see the check above). The goroutine at the end of this function
	// ends the process when ctx is done, with SIGTERM to its process group.
	cmd := exec.Command(nameAndArgs[0], nameAndArgs[1:]...)
	service, method := methodFromContext(ctx)

	command := &Command{
		logger:                   logger,
		cmd:                      cmd,
		startTime:                time.Now(),
		context:                  ctx,
		span:                     span,
		finalizers:               cfg.finalizers,
		exitHooks:                cfg.exitHooks,
		metricsCmd:               cfg.commandName,
		metricsSubCmd:            cfg.subcommandName,
		metricsService:           service,
		metricsMethod:            method,
		cmdGitVersion:            cfg.gitVersion,
		refBackend:               cfg.refBackend,
		subprocessLoggerDone:     make(chan struct{}),
		processExitedCh:          make(chan struct{}),
		completionErrorLogFilter: cfg.completionErrorLogFilter,
	}

	cmd.Dir = cfg.dir

	// Export allowed environment variables as set in the Gitaly process.
	cmd.Env = AllowedEnvironment(os.Environ())
	// Append environment variables explicitly requested by the caller.
	cmd.Env = append(cmd.Env, cfg.environment...)
	// And finally inject environment variables required for tracing into the command.
	cmd.Env = envInjector(ctx, cmd.Env)

	closeLogWriter := func() {}
	if (cfg.logConfiguration != log.Config{}) {
		logReader, logWriter, err := func() (_ io.ReadCloser, _ io.WriteCloser, returnedErr error) {
			var err error
			// Create a pipe the process will send its logs over.
			logReader, logWriter, err := os.Pipe()
			if err != nil {
				return nil, nil, fmt.Errorf("create log pipe: %w", err)
			}
			defer func() {
				if returnedErr != nil {
					if err := logReader.Close(); err != nil {
						returnedErr = errors.Join(returnedErr, fmt.Errorf("close log reader: %w", err))
					}
					if err := logWriter.Close(); err != nil {
						returnedErr = errors.Join(returnedErr, fmt.Errorf("close log writer: %w", err))
					}
				}
			}()

			// Pass the log writer to the command so it can write logs to it.
			cmd.ExtraFiles = append(cmd.ExtraFiles, logWriter)
			loggerEnv, err := envSubprocessLoggerConfiguration(subprocessConfiguration{
				// The first three file descriptors, indexed from zero, are
				// stdin, stdout and stderr. The log file descriptor is the
				// last one in the extra files.
				// The first three file descriptors, indexed from zero, are:
				// (0) `stdin`, (1) `stdout` and (2) `stderr`.
				// The log file descriptor is the last one in the extra files.
				// To get the file descriptor of the log writer in the new sub-process,
				// we sum the `stderr` index with the number of extra files in the slice.
				FileDescriptor: uintptr(syscall.Stderr + len(cmd.ExtraFiles)),
				Config:         cfg.logConfiguration,
			})
			if err != nil {
				return nil, nil, fmt.Errorf("subprocess logger env: %w", err)
			}
			cmd.Env = append(cmd.Env, loggerEnv)

			return logReader, logWriter, nil
		}()
		if err != nil {
			return nil, err
		}

		// Close the file descriptor after spawning the command as we're not
		// the ones writing into it.
		closeLogWriter = func() {
			// We always close the write end after starting the command but we also
			// close it on error to ensure the log processing goroutine will terminate
			// if the command wasn't started. If there's an error after the command was
			// started, this could lead to a double close. Ignore the fs.ErrClosed here
			// to accommodate for this.
			if err := logWriter.Close(); err != nil && !errors.Is(err, fs.ErrClosed) {
				returnedErr = errors.Join(returnedErr, fmt.Errorf("close log writer: %w", err))
			}
		}

		defer func() {
			// If starting the command fails, we have to explicitly clean up the
			// the logger as we won't return the command, and .Wait() won't be
			// called on it. If we don't do this, we'd leak the logger goroutine
			// from this constructor.
			if returnedErr != nil {
				closeLogWriter()
				<-command.subprocessLoggerDone
			}
		}()
		go func() {
			defer close(command.subprocessLoggerDone)
			if err := command.handleSubprocessLogs(logReader); err != nil {
				logger.WithError(err).Error("failed handling subprocess logs")
			}
		}()
	} else {
		// If subprocess logger wasn't enabled, close the channel immediately as
		// we don't have a log consuming goroutine to wait for.
		close(command.subprocessLoggerDone)
	}

	// Start the command in its own process group (nice for signalling)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	useCloneIntoCgroup := cfg.cgroupsManager != nil && cfg.cgroupsManager.SupportsCloneIntoCgroup()
	if useCloneIntoCgroup {
		// Configure the command to be executed in the correct cgroup.
		cgroupPath, fd, err := cfg.cgroupsManager.CloneIntoCgroup(cmd, cfg.cgroupsAddCommandOpts...)
		if err != nil {
			return nil, fmt.Errorf("clone into cgroup: %w", err)
		}
		defer func() {
			if err := fd.Close(); err != nil {
				logger.WithError(err).ErrorContext(ctx, "failed to close cgroup file descriptor")
			}
		}()
		command.cgroupPath = cgroupPath
	}

	// If requested, we will set up the command such that `Write()` can be called on it directly. Otherwise,
	// we simply pass as stdin whatever the user has asked us to set up. If no `stdin` was set up, the command
	// will implicitly read from `/dev/null`.
	// startStdinCopy starts the copy of a filtered stdin, once the process started: until then
	// nothing runs that could outlive a command that failed to start.
	startStdinCopy := func() {}
	switch _, ok := cfg.stdin.(stdinSentinel); {
	case ok && cfg.stdinFilter != nil:
		pipe, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("creating stdin pipe: %w", err)
		}
		command.writer, startStdinCopy = filteredStdin(pipe, cfg.stdinFilter)
	case ok:
		pipe, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("creating stdin pipe: %w", err)
		}

		command.writer = pipe
	case cfg.stdin != nil && cfg.stdinFilter != nil:
		cmd.Stdin = cfg.stdinFilter(cfg.stdin)
	default:
		cmd.Stdin = cfg.stdin
	}

	// Similar, if requested, we will set up the command such that `Read()` can be called on it directly.
	// Otherwise, we simply pass as stdout whatever the user has asked us to set up. If no `stdout` was set
	// up, the command will implicitly write to `/dev/null`.
	if _, ok := cfg.stdout.(stdoutSentinel); ok {
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("creating stdout pipe: %w", err)
		}

		command.reader = pipe
	} else {
		cmd.Stdout = cfg.stdout
	}

	if cfg.stderr != nil {
		cmd.Stderr = cfg.stderr
	} else {
		var err error
		command.stderrBuffer, err = newStderrBuffer(maxStderrBytes, maxStderrLineLength, []byte("\n"))
		if err != nil {
			return nil, fmt.Errorf("creating stderr buffer: %w", err)
		}

		cmd.Stderr = command.stderrBuffer
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting process %v: %w", cmd.Args, err)
	}
	group := newProcGroup(cmd.Process.Pid)
	startStdinCopy()

	go trackMaxRssAnon(ctx, cmd.Process.Pid, &command.maxRssAnon, command.processExitedCh)

	inFlightCommandGauge.WithLabelValues(command.metricsService, command.metricsMethod, command.metricsCmd, command.metricsSubCmd).Inc()
	commandcounter.Increment()

	defer func() {
		// The command has been successfully started and it now has the write
		// end of the log pipe open. Close our writer so the reader will correctly
		// finish when the command closes its write end.
		closeLogWriter()

		terminationGrace := defaultTerminationGrace
		if cfg.terminationGrace > 0 {
			terminationGrace = cfg.terminationGrace
		}

		teardown := func() {
			// Before we kill the child process we need to close the process' standard streams. If
			// we don't, it may happen that the signal gets delivered and that the process exits
			// before we close the streams in `command.Wait()`. This would cause downstream readers
			// to potentially miss those errors when reading stdout.
			command.teardownStandardStreams()

			// If the context has been cancelled and we didn't explicitly reap
			// the child process then we need to manually kill it and release
			// all associated resources.
			if cmd.Process.Pid > 0 {
				// Before the signal, while the leader cannot have been reaped because of it.
				proof := group.proof()

				//nolint:errcheck // TODO: do we want to report errors?
				// Send SIGTERM to the process group of cmd
				syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)

				// SIGTERM lets git remove its locks, but a process may ignore it (a custom hook
				// that traps it, and the git waiting for that hook). While Wait below is blocked,
				// a group that outlives the grace period gets SIGKILL, if its number is provably
				// still its own: exec.Cmd.Wait reaps the leader before it waits for its copy
				// goroutines, so the group may be gone and its number reused by then.
				//
				// This bounds a group that ignores SIGTERM, not Wait: a copy goroutine can stay
				// blocked on the caller's reader or writer, or on a stream held by a process that
				// left the group. And a member that ignores SIGTERM and holds no stream is not
				// killed once Wait has returned.
				escalation := time.AfterFunc(terminationGrace, func() {
					select {
					case <-command.processExitedCh:
						return
					default:
					}

					if cfg.beforeEscalation != nil {
						cfg.beforeEscalation()
					}

					signalled := proof.stillOurs()
					if signalled {
						logger.WithField("pid", cmd.Process.Pid).WarnContext(ctx, "process group outlived SIGTERM, sending SIGKILL")
						//nolint:errcheck // The group may be gone by now, which is what we want.
						syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					}

					if cfg.afterEscalation != nil {
						cfg.afterEscalation(signalled)
					}
				})
				defer escalation.Stop()
			}

			// We do not care for any potential error code, but just want to
			// make sure that the subprocess gets properly killed and processed.
			_ = command.Wait()
		}

		if returnedErr != nil {
			teardown()
			return
		}

		// The goroutine below is responsible for terminating and reaping the process when ctx is
		// canceled. While we must ensure that it does run when `cmd.Start()` was successful, it
		// must not run before have fully set up the command. Otherwise, we may end up with racy
		// access patterns when the context gets terminated early.
		//
		// We thus defer spawning the Goroutine.
		go func() {
			select {
			case <-ctx.Done():
				teardown()
			case <-command.processExitedCh:
				// Otherwise, if the process has exited via a call to `wait()`
				// already then there is nothing we need to do.
			}
		}()
	}()

	if cfg.cgroupsManager != nil && !useCloneIntoCgroup {
		cgroupPath, err := cfg.cgroupsManager.AddCommand(command.cmd, cfg.cgroupsAddCommandOpts...)
		if err != nil {
			return nil, err
		}

		command.cgroupPath = cgroupPath
	}

	logPid = cmd.Process.Pid

	burdenmonitor.NotifyCommandStarted(ctx, cmd.Process.Pid, command.commandName(), command.startTime)

	return command, nil
}

// handleSubprocessLogs consumes logs from the command. The command is passed a pipe which
// is connected to logReader. It sends fully formatted logs over the pipe to Gitaly. Here
// we read those logs and output them to the general log output.
func (c *Command) handleSubprocessLogs(logReader io.ReadCloser) (returnedErr error) {
	defer func() {
		if err := logReader.Close(); err != nil {
			returnedErr = errors.Join(returnedErr, fmt.Errorf("close log reader: %w", err))
		}
	}()

	r := bufio.NewReader(logReader)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) != 0 {
					// Partial lines without a line ending must not be logged. They
					// would only occur if the subprocess is killed before outputting
					// the proper log line and would lead to possibly printing invalid
					// JSON in the logs.
					return fmt.Errorf("incomplete log line: %q", line)
				}

				return nil
			}

			return fmt.Errorf("read line: %w", err)
		}

		if _, err := c.logger.Output().Write(line); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
}

// Read calls Read() on the stdout pipe of the command.
func (c *Command) Read(p []byte) (int, error) {
	if c.reader == nil {
		panic("command has no reader")
	}

	return c.reader.Read(p)
}

// Write calls Write() on the stdin pipe of the command.
func (c *Command) Write(p []byte) (int, error) {
	return c.Stdin().Write(p)
}

// Stdin returns the commands stdin pipe.
func (c *Command) Stdin() io.WriteCloser {
	if c.writer == nil {
		panic("command has no writer")
	}

	return c.writer
}

// Wait calls Wait() on the exec.Cmd instance inside the command. This
// blocks until the command has finished and reports the command exit
// status via the error return value. Use ExitStatus to get the integer
// exit status from the error returned by Wait().
//
// Wait returns a wrapped context error if the process was reaped due to
// the context being done.
//
// A process that handles the SIGTERM of a done context and exits with
// status 0 makes Wait return nil: it succeeded, for all Wait can tell. A
// caller that must not take a cancelled command for a finished one checks
// its context.
func (c *Command) Wait() error {
	c.waitOnce.Do(c.wait)

	return c.waitError
}

// This function should never be called directly, use Wait().
func (c *Command) wait() {
	defer close(c.processExitedCh)
	defer func() { <-c.subprocessLoggerDone }()
	defer func() {
		for _, hook := range c.exitHooks {
			hook()
		}
	}()

	c.teardownStandardStreams()
	c.waitError = c.cmd.Wait()

	// If the context is done, the process was likely terminated due to it. If so,
	// we return the context error to correctly report the reason.
	if c.context.Err() != nil {
		// The standard library sets exit status -1 if the process was terminated by a signal,
		// such as the SIGTERM sent when context is done.
		if exitCode, ok := ExitStatus(c.waitError); ok && exitCode == -1 {
			// This intentionally drops the `exec.ExitError` from the error chain so that
			// callers see the cancellation instead of the exit status. Reporting of the
			// exit status must thus not be derived from `waitError`, but from the reaped
			// process' state via `exitStatus()`.
			c.waitError = fmt.Errorf("%s: %w", c.waitError.Error(), c.context.Err())
		}
	}

	inFlightCommandGauge.WithLabelValues(c.metricsService, c.metricsMethod, c.metricsCmd, c.metricsSubCmd).Dec()

	burdenmonitor.NotifyCommandCompleted(
		c.context,
		c.cmd.ProcessState.Pid(),
		c.cmd.ProcessState.UserTime(),
		c.cmd.ProcessState.SystemTime(),
	)

	c.logProcessComplete()

	// This is a bit out-of-place here given that the `commandcounter.Increment()` call is in
	// `New()`. But in `New()`, we have to resort waiting on the context being finished until we
	// would be able to decrement the number of in-flight commands. Given that in some
	// cases we detach processes from their initial context such that they run in the
	// background, this would cause us to take longer than necessary to decrease the wait group
	// counter again. So we instead do it here to accelerate the process, even though it's less
	// idiomatic.
	commandcounter.Decrement()

	for _, finalizer := range c.finalizers {
		finalizer(c.context, c)
	}
}

func (c *Command) teardownStandardStreams() {
	c.teardownOnce.Do(func() {
		if c.writer != nil {
			// Prevent the command from blocking on waiting for stdin to be closed
			c.writer.Close()
		}

		if c.reader != nil {
			// Close stdout of the command. This causes us to receive an error when trying to consume the
			// output and will also cause an error when stdout hasn't been fully consumed at the time of
			// calling `Wait()`.
			c.reader.Close()
		}
	})
}

// truncateArgsForLogging truncates the provided args list to at most limit
// elements. A final entry is added to the returned list indicating how many
// elements have been removed, if this occurs.
func truncateArgsForLogging(args []string, limit int) []string {
	if len(args) <= limit {
		return args
	}

	truncated := make([]string, 0, limit+1)
	truncated = append(truncated, args[:limit]...)
	truncated = append(truncated, fmt.Sprintf("[%d more args truncated]", len(args)-limit))
	return truncated
}

func (c *Command) logProcessComplete() {
	exitCode, signal := c.exitStatus()

	ctx := c.context
	cmd := c.cmd

	systemTime := cmd.ProcessState.SystemTime()
	userTime := cmd.ProcessState.UserTime()
	realTime := time.Since(c.startTime)

	fields := log.Fields{
		"pid":                    cmd.ProcessState.Pid(),
		"path":                   cmd.Path,
		"args":                   truncateArgsForLogging(cmd.Args, maxLoggedArgsElements),
		"command.exitCode":       exitCode,
		"command.system_time_ms": systemTime.Milliseconds(),
		"command.user_time_ms":   userTime.Milliseconds(),
		"command.cpu_time_ms":    systemTime.Milliseconds() + userTime.Milliseconds(),
		"command.real_time_ms":   realTime.Milliseconds(),
	}

	if c.cgroupPath != "" {
		fields["command.cgroup_path"] = c.cgroupPath
	}

	if signal != 0 {
		fields["command.signal"] = signal.String()
	}

	entry := c.logger.WithFields(fields)

	rusage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if ok {
		entry = entry.WithFields(log.Fields{
			"command.inblock": rusage.Inblock,
			"command.oublock": rusage.Oublock,
		})
	}

	if c.maxRssAnon.Load() > 0 {
		entry = entry.WithFields(log.Fields{
			"command.maxrssanon_bytes": c.maxRssAnon.Load(),
		})
	}

	entry.DebugContext(ctx, "spawn complete")
	if c.stderrBuffer != nil && c.stderrBuffer.Len() > 0 {
		// Processes that were signalled because their context was done were killed by us
		// and are thus expected to fail. We don't escalate their output to the error level.
		killedByContext := signal != 0 && ctx.Err() != nil

		logLevel := entry.WarnContext
		if exitCode != 0 && !killedByContext {
			logLevel = entry.ErrorContext
		}

		if c.completionErrorLogFilter == nil || !c.completionErrorLogFilter(c, c.stderrBuffer.String()) {
			logLevel(ctx, c.stderrBuffer.String())
		}
	}

	if customFields := log.CustomFieldsFromContext(ctx); customFields != nil {
		customFields.RecordSum("command.count", 1)
		customFields.RecordSum("command.system_time_ms", int(systemTime.Milliseconds()))
		customFields.RecordSum("command.user_time_ms", int(userTime.Milliseconds()))
		customFields.RecordSum("command.cpu_time_ms", int(systemTime.Milliseconds()+userTime.Milliseconds()))
		customFields.RecordSum("command.real_time_ms", int(realTime.Milliseconds()))

		if ok {
			customFields.RecordSum("command.inblock", int(rusage.Inblock))
			customFields.RecordSum("command.oublock", int(rusage.Oublock))
			customFields.RecordSum("command.minflt", int(rusage.Minflt))
			customFields.RecordSum("command.majflt", int(rusage.Majflt))
		}

		customFields.RecordMax("command.maxrssanon_bytes", int(c.maxRssAnon.Load()))

		if c.cgroupPath != "" {
			customFields.RecordMetadata("command.cgroup_path", c.cgroupPath)
		}
	}

	service, method := methodFromContext(ctx)
	cmdName := path.Base(c.cmd.Path)
	if c.metricsCmd != "" {
		cmdName = c.metricsCmd
	}
	cpuSecondsTotal.WithLabelValues(service, method, cmdName, c.metricsSubCmd, "system", c.cmdGitVersion, c.refBackend).Add(systemTime.Seconds())
	cpuSecondsTotal.WithLabelValues(service, method, cmdName, c.metricsSubCmd, "user", c.cmdGitVersion, c.refBackend).Add(userTime.Seconds())
	realSecondsTotal.WithLabelValues(service, method, cmdName, c.metricsSubCmd, c.cmdGitVersion, c.refBackend).Add(realTime.Seconds())
	if ok {
		minorPageFaultsTotal.WithLabelValues(service, method, cmdName, c.metricsSubCmd, c.cmdGitVersion, c.refBackend).Add(float64(rusage.Minflt))
		majorPageFaultsTotal.WithLabelValues(service, method, cmdName, c.metricsSubCmd, c.cmdGitVersion, c.refBackend).Add(float64(rusage.Majflt))
		signalsReceivedTotal.WithLabelValues(service, method, cmdName, c.metricsSubCmd, c.cmdGitVersion, c.refBackend).Add(float64(rusage.Nsignals))
		contextSwitchesTotal.WithLabelValues(service, method, cmdName, c.metricsSubCmd, "voluntary", c.cmdGitVersion, c.refBackend).Add(float64(rusage.Nvcsw))
		contextSwitchesTotal.WithLabelValues(service, method, cmdName, c.metricsSubCmd, "nonvoluntary", c.cmdGitVersion, c.refBackend).Add(float64(rusage.Nivcsw))
	}

	attributes := []attribute.KeyValue{
		attribute.Int("pid", cmd.ProcessState.Pid()),
		attribute.Int("exit_code", exitCode),
		attribute.Int64("system_time_ms", systemTime.Milliseconds()),
		attribute.Int64("user_time_ms", userTime.Milliseconds()),
		attribute.Int64("real_time_ms", realTime.Milliseconds()),
	}

	if ok {
		attributes = append(attributes,
			attribute.Int64("inblock", rusage.Inblock),
			attribute.Int64("oublock", rusage.Oublock),
			attribute.Int64("minflt", rusage.Minflt),
			attribute.Int64("majflt", rusage.Majflt),
		)
	}
	if signal != 0 {
		attributes = append(attributes, attribute.String("signal", signal.String()))
	}
	attributes = append(attributes, attribute.Int64("maxrssanon_bytes", c.maxRssAnon.Load()))
	if c.cgroupPath != "" {
		attributes = append(attributes, attribute.String("cgroup_path", c.cgroupPath))
	}
	c.span.SetAttributes(attributes...)
	c.span.End()
}

// Args is an accessor for the command arguments
func (c *Command) Args() []string {
	return c.cmd.Args
}

// Env is an accessor for the environment variables
func (c *Command) Env() []string {
	return c.cmd.Env
}

// Pid is an accessor for the pid
func (c *Command) Pid() int {
	return c.cmd.Process.Pid
}

// exitStatus returns the exit code of the reaped process, as well as the signal that terminated it.
// The signal is zero if the process wasn't terminated by one.
// The exit code is -1 if the process was terminated by a signal.
//
// Unlike ExitStatus, which is derived from the error returned by Wait, this reports the exit status
// even when the process was killed because its context was done.
func (c *Command) exitStatus() (int, syscall.Signal) {
	state := c.cmd.ProcessState
	if state == nil {
		return -1, 0
	}

	if waitStatus, ok := state.Sys().(syscall.WaitStatus); ok && waitStatus.Signaled() {
		return state.ExitCode(), waitStatus.Signal()
	}

	return state.ExitCode(), 0
}

// filteredStdin returns the writer of a command's stdin that reaches pipe, the stdin of the
// process, through filter, and the function that starts the copy, which the caller calls once the
// process started. The copy is a goroutine that copies the filtered stream into pipe and closes
// pipe when the writer is closed. When the process stops reading, the copy fails, and the writes
// after that fail with its error, as writes to pipe itself would: a writer never blocks on a
// process that is gone. A command that fails to start never calls the function, and so leaves no
// goroutine behind.
func filteredStdin(pipe io.WriteCloser, filter func(io.Reader) io.Reader) (io.WriteCloser, func()) {
	r, w := io.Pipe()
	return w, func() { go copyFilteredStdin(r, pipe, filter) }
}

func copyFilteredStdin(r *io.PipeReader, pipe io.WriteCloser, filter func(io.Reader) io.Reader) {
	_, err := io.Copy(pipe, filter(r))
	if closeErr := pipe.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = io.ErrClosedPipe
	}
	r.CloseWithError(err)
}

type stdinSentinel struct{}

func (stdinSentinel) Read([]byte) (int, error) {
	return 0, errors.New("stdin sentinel should not be read from")
}

type stdoutSentinel struct{}

func (stdoutSentinel) Write([]byte) (int, error) {
	return 0, errors.New("stdout sentinel should not be written to")
}

// AllowedEnvironment filters the given slice of environment variables and
// returns all variables which are allowed per the variables defined above.
// This is useful for constructing a base environment in which a command can be
// run.
func AllowedEnvironment(envs []string) []string {
	var filtered []string

	for _, env := range envs {
		for _, exportedEnv := range exportedEnvVars {
			if strings.HasPrefix(env, exportedEnv+"=") {
				filtered = append(filtered, env)
			}
		}
	}

	return filtered
}

// ExitStatus will return the exit-code from an error returned by Wait().
func ExitStatus(err error) (int, bool) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}

	return 0, false
}

func methodFromContext(ctx context.Context) (service string, method string) {
	if info := requestinfohandler.Extract(ctx); info != nil {
		return info.ExtractServiceAndMethodName()
	}

	return "", ""
}

// Command arguments will be passed to the exec syscall as null-terminated C strings. That means the
// arguments themselves may not contain a null byte. The go stdlib checks for null bytes but it
// returns a cryptic error. This function returns a more explicit error.
func checkNullArgv(args []string) error {
	for _, arg := range args {
		if strings.IndexByte(arg, 0) > -1 {
			// Use %q so that the null byte gets printed as \x00
			return fmt.Errorf("detected null byte in command argument %q", arg)
		}
	}

	return nil
}
