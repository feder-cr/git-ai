package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/feder-cr/git-ai/internal/cgroups"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_environment(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	extraVar := "FOOBAR=123456"

	var buf bytes.Buffer
	cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"/usr/bin/env"}, WithStdout(&buf), WithEnvironment([]string{extraVar}))

	require.NoError(t, err)
	require.NoError(t, cmd.Wait())

	require.Contains(t, strings.Split(buf.String(), "\n"), extraVar)
}

func TestNew_exportedEnvironment(t *testing.T) {
	ctx := testhelper.Context(t)

	for _, tc := range []struct {
		key   string
		value string
	}{
		{
			key:   "HOME",
			value: "/home/git",
		},
		{
			key:   "PATH",
			value: "/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin",
		},
		{
			key:   "LD_LIBRARY_PATH",
			value: "/path/to/your/lib",
		},
		{
			key:   "TZ",
			value: "foobar",
		},
		{
			key:   "GIT_TRACE",
			value: "true",
		},
		{
			key:   "GIT_TRACE_PACK_ACCESS",
			value: "true",
		},
		{
			key:   "GIT_TRACE_PACKET",
			value: "true",
		},
		{
			key:   "GIT_TRACE_PERFORMANCE",
			value: "true",
		},
		{
			key:   "GIT_TRACE_SETUP",
			value: "true",
		},
		{
			key:   "all_proxy",
			value: "http://localhost:4000",
		},
		{
			key:   "http_proxy",
			value: "http://localhost:5000",
		},
		{
			key:   "HTTP_PROXY",
			value: "http://localhost:6000",
		},
		{
			key:   "https_proxy",
			value: "https://localhost:5000",
		},
		{
			key:   "HTTPS_PROXY",
			value: "https://localhost:6000",
		},
		{
			key:   "no_proxy",
			value: "https://excluded:5000",
		},
		{
			key:   "NO_PROXY",
			value: "https://excluded:5000",
		},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if tc.key == "LD_LIBRARY_PATH" && runtime.GOOS == "darwin" {
				t.Skip("System Integrity Protection prevents using dynamic linker (dyld) environment variables on macOS. https://apple.co/2XDH4iC")
			}

			t.Setenv(tc.key, tc.value)

			var buf bytes.Buffer
			cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"/usr/bin/env"}, WithStdout(&buf))
			require.NoError(t, err)
			require.NoError(t, cmd.Wait())

			expectedEnv := fmt.Sprintf("%s=%s", tc.key, tc.value)
			require.Contains(t, strings.Split(buf.String(), "\n"), expectedEnv)
		})
	}
}

func TestNew_unexportedEnv(t *testing.T) {
	ctx := testhelper.Context(t)

	unexportedEnvKey, unexportedEnvVal := "GITALY_UNEXPORTED_ENV", "foobar"
	t.Setenv(unexportedEnvKey, unexportedEnvVal)

	var buf bytes.Buffer
	cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"/usr/bin/env"}, WithStdout(&buf))
	require.NoError(t, err)
	require.NoError(t, cmd.Wait())

	require.NotContains(t, strings.Split(buf.String(), "\n"), fmt.Sprintf("%s=%s", unexportedEnvKey, unexportedEnvVal))
}

func TestNew_dontSpawnWithCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(testhelper.Context(t))
	cancel()

	cmd, err := New(ctx, testhelper.SharedLogger(t), nil)
	require.Equal(t, err, ctx.Err())
	require.Nil(t, cmd)
}

func TestNew_rejectContextWithoutDone(t *testing.T) {
	t.Parallel()

	require.PanicsWithValue(t, "command spawned with context without Done() channel", func() {
		_, err := New(testhelper.ContextWithoutCancel(), testhelper.SharedLogger(t), []string{"true"})
		require.NoError(t, err)
	})
}

func TestCommand_Wait_contextCancellationKillsCommand(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	ctx, cancel := context.WithCancel(ctx)

	cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"cat", "/dev/urandom"}, WithSetupStdout())
	require.NoError(t, err)

	// Read one byte to ensure the process is running.
	n, err := cmd.Read(make([]byte, 1))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	cancel()

	// Verify that the cancelled context causes us to receive a read error when consuming the command's output. In
	// older days this used to succeed just fine, which had the consequence that we obtain partial stdout of the
	// killed process without noticing that it in fact got killed.
	_, err = io.ReadAll(cmd)

	require.Equal(t, &fs.PathError{
		Op: "read", Path: "|0", Err: os.ErrClosed,
	}, err)

	err = cmd.Wait()
	// Depending on whether the closed file descriptor or the sent signal arrive first, cat(1) may fail
	// either with SIGPIPE or with SIGKILL. We can also get SIGKILL depending on context cancellation.
	require.Contains(t, []error{
		fmt.Errorf("signal: %s: %w", syscall.SIGTERM, context.Canceled),
		fmt.Errorf("signal: %s: %w", syscall.SIGPIPE, context.Canceled),
		fmt.Errorf("signal: %s: %w", syscall.SIGKILL, context.Canceled),
	}, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestNew_setupStdin(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	stdin := "Test value"

	var buf bytes.Buffer
	cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"cat"}, WithSetupStdin(), WithStdout(&buf))
	require.NoError(t, err)

	_, err = fmt.Fprintf(cmd, "%s", stdin)
	require.NoError(t, err)

	require.NoError(t, cmd.Wait())
	require.Equal(t, stdin, buf.String())
}

// TestNew_stdinFilter: the command reads its stdin through the filter, both a reader given to it
// and what is written to it; a writer whose process stopped reading gets an error and never blocks.
func TestNew_stdinFilter(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	upper := func(r io.Reader) io.Reader {
		raw, err := io.ReadAll(r)
		if err != nil {
			return iotest.ErrReader(err)
		}
		return strings.NewReader(strings.ToUpper(string(raw)))
	}

	var buf bytes.Buffer
	cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"cat"}, WithStdin(strings.NewReader("given")), WithStdinFilter(upper), WithStdout(&buf))
	require.NoError(t, err)
	require.NoError(t, cmd.Wait())
	require.Equal(t, "GIVEN", buf.String())

	buf.Reset()
	cmd, err = New(ctx, testhelper.SharedLogger(t), []string{"cat"}, WithSetupStdin(), WithStdinFilter(upper), WithStdout(&buf))
	require.NoError(t, err)
	_, err = fmt.Fprintf(cmd, "written")
	require.NoError(t, err)
	require.NoError(t, cmd.Wait())
	require.Equal(t, "WRITTEN", buf.String())

	// A process that exits without reading: the writes fail once the copy into its stdin failed.
	passThrough := func(r io.Reader) io.Reader { return r }
	cmd, err = New(ctx, testhelper.SharedLogger(t), []string{"true"}, WithSetupStdin(), WithStdinFilter(passThrough))
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		chunk := bytes.Repeat([]byte("x"), 1<<16)
		var err error
		for i := 0; i < 1<<10 && err == nil; i++ {
			_, err = cmd.Write(chunk)
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "the writes blocked on a process that exited")
	}
	require.NoError(t, cmd.Wait())
}

// TestNew_stdinFilterStartsAfterTheProcess: a command that fails to start leaves no goroutine that
// copies a filtered stdin: the copy, which blocks until the writer is closed, starts only when the
// process did.
func TestNew_stdinFilterStartsAfterTheProcess(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	var filtered atomic.Bool
	filter := func(r io.Reader) io.Reader {
		filtered.Store(true)
		return r
	}

	_, err := New(ctx, testhelper.SharedLogger(t), []string{"/no/such/binary"}, WithSetupStdin(), WithStdinFilter(filter))
	require.Error(t, err)
	time.Sleep(200 * time.Millisecond)
	require.False(t, filtered.Load(), "the filter ran although the process never started")

	// And it does run once the process started.
	var buf bytes.Buffer
	cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"cat"}, WithSetupStdin(), WithStdinFilter(filter), WithStdout(&buf))
	require.NoError(t, err)
	_, err = fmt.Fprint(cmd, "x")
	require.NoError(t, err)
	require.NoError(t, cmd.Wait())
	require.True(t, filtered.Load())
	require.Equal(t, "x", buf.String())
}

func TestCommand_closeStdin(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"cat"}, WithSetupStdin(), WithSetupStdout())
	require.NoError(t, err)

	defer func() { require.NoError(t, cmd.Wait()) }()

	input := []byte("test value")
	_, err = cmd.Write(input)
	require.NoError(t, err)

	// Close stdin after writing the input. With stdin closed, we expect the `cat` command to return
	// after outputting input. Since `cat` finished processing input from stdin, it will then exit
	// closing its stdout. The closed stdout will lead to ReadAll unblocking.
	require.NoError(t, cmd.Stdin().Close())

	output, err := io.ReadAll(cmd)
	require.NoError(t, err)

	require.Equal(t, input, output)
}

func TestCommand_read(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	t.Run("without stdout set up", func(t *testing.T) {
		require.PanicsWithValue(t, "command has no reader", func() {
			cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"echo", "test value"})
			require.NoError(t, err)
			_, _ = io.ReadAll(cmd)
		})
	})

	t.Run("with stdout set up", func(t *testing.T) {
		cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"echo", "test value"}, WithSetupStdout())
		require.NoError(t, err)

		output, err := io.ReadAll(cmd)
		require.NoError(t, err)
		require.Equal(t, "test value\n", string(output))

		require.NoError(t, cmd.Wait())
	})
}

func TestCommand_readPartial(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"head", "-c", "1000000", "/dev/urandom"}, WithSetupStdout())
	require.NoError(t, err)
	require.EqualError(t, cmd.Wait(), "signal: broken pipe")
}

func TestNew_nulByteInArgument(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"sh", "-c", "hello\x00world"})
	require.Equal(t, fmt.Errorf("detected null byte in command argument %q", "hello\x00world"), err)
	require.Nil(t, cmd)
}

func TestNew_missingBinary(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)

	cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"command-non-existent"})
	require.EqualError(t, err, "starting process [command-non-existent]: exec: \"command-non-existent\": executable file not found in $PATH")
	require.Nil(t, cmd)
}

func TestCommand_stderr(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		script    string
		lastEntry *logrus.Entry
		opts      []Option
	}{
		{
			name: "basic stderr logging",
			script: `#!/usr/bin/env bash
				for i in {1..5}
				do
					echo 'hello world' 1>&2
				done
				exit 1
			`,
			lastEntry: &logrus.Entry{
				Level:   logrus.ErrorLevel,
				Message: strings.Repeat("hello world\n", 5),
			},
		},
		{
			name: "stderr logging truncation",
			script: `#!/usr/bin/env bash
				for i in {1..1000}
				do
					printf 'zzzzzzzzzz\n' $i >&2
				done
				exit 1
			`,
			lastEntry: &logrus.Entry{
				Level:   logrus.ErrorLevel,
				Message: strings.Repeat("zzzzzzzzzz\n", 1000)[:maxStderrBytes],
			},
		},
		{
			name: "stderr logging with null bytes",
			script: `#!/usr/bin/env bash
				dd if=/dev/zero bs=1000 count=1000 status=none >&2
				exit 1
			`,
			lastEntry: &logrus.Entry{
				Level:   logrus.ErrorLevel,
				Message: strings.Repeat("\x00", maxStderrLineLength),
			},
		},
		{
			name: "stderr with exit code 0",
			script: `#!/usr/bin/env bash
				echo "warning message" >&2
				exit 0
			`,
			lastEntry: &logrus.Entry{
				Level:   logrus.WarnLevel,
				Message: "warning message\n",
			},
		},
		{
			name: "stderr with non-zero exit code",
			script: `#!/usr/bin/env bash
				echo "error message" >&2
				exit 1
			`,
			lastEntry: &logrus.Entry{
				Level:   logrus.ErrorLevel,
				Message: "error message\n",
			},
		},
		{
			name: "stderr logging long line",
			script: `#!/usr/bin/env bash
				printf 'a%.0s' {1..8192} >&2
				printf '\n' >&2
				printf 'b%.0s' {1..8192} >&2
				exit 1
			`,
			lastEntry: &logrus.Entry{
				Level: logrus.ErrorLevel,
				Message: strings.Join([]string{
					strings.Repeat("a", maxStderrLineLength),
					strings.Repeat("b", maxStderrLineLength),
				}, "\n"),
			},
		},
		{
			name: "stderr logging max bytes",
			script: `#!/usr/bin/env bash
				# This script is used to test that a command writes at most maxBytes to stderr. It
				# simulates the edge case where the logwriter has already written MaxStderrBytes-1
				# (9999) bytes

				# This edge case happens when 9999 bytes are written. To simulate this,
				# stderr_max_bytes_edge_case has 4 lines of the following format:
				#
				# line1: 3333 bytes long
				# line2: 3331 bytes
				# line3: 3331 bytes
				# line4: 1 byte
				#
				# The first 3 lines sum up to 9999 bytes written, since we write a 2-byte escaped
				# "\n" for each \n we see. The 4th line can be any data.

				printf 'a%.0s' {1..3333} >&2
				printf '\n' >&2
				printf 'a%.0s' {1..3331} >&2
				printf '\n' >&2
				printf 'a%.0s' {1..3331} >&2
				printf '\na\n' >&2
				exit 1
			`,
			lastEntry: &logrus.Entry{
				Level: logrus.ErrorLevel,
				Message: fmt.Sprintf("%s\n%s\n%s\na\n",
					strings.Repeat("a", 3333),
					strings.Repeat("a", 3331),
					strings.Repeat("a", 3331)),
			},
		},
		{
			name: "doesn't log when a process exits with an error and a filter is specified",
			script: `#!/usr/bin/env bash
				echo "fatal: not a git repository" >&2
				exit 128
			`,
			opts: []Option{WithCompletionErrorLogFilter(func(cmd *Command, stderr string) bool {
				return strings.HasPrefix(stderr, "fatal: not a git repository") && cmd.cmd.ProcessState.ExitCode() == 128
			})},
		},
		{
			name: "logs when a process exits with an error and an ineffective filter is specified",
			script: `#!/usr/bin/env bash
				echo "fatal: not a git repository" >&2
				exit 128
			`,
			opts: []Option{WithCompletionErrorLogFilter(func(cmd *Command, stderr string) bool {
				return strings.HasPrefix(stderr, "fatal: not a git repository") && cmd.cmd.ProcessState.ExitCode() == 1
			})},
			lastEntry: &logrus.Entry{
				Level:   logrus.ErrorLevel,
				Message: "fatal: not a git repository\n",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			binaryPath := testhelper.WriteExecutable(t, filepath.Join(testhelper.TempDir(t), "script"), []byte(tc.script))

			logger := testhelper.NewLogger(t)
			hook := testhelper.AddLoggerHook(logger)
			ctx := testhelper.Context(t)

			var stdout bytes.Buffer
			cmd, err := New(ctx, logger, []string{binaryPath}, append(tc.opts, WithStdout(&stdout))...)
			require.NoError(t, err)

			err = cmd.Wait()

			if tc.lastEntry == nil {
				require.Nil(t, hook.LastEntry())
				return
			}

			require.Equal(t, tc.lastEntry.Level, hook.LastEntry().Level)
			require.Equal(t, tc.lastEntry.Message, hook.LastEntry().Message)

			if tc.lastEntry.Level == logrus.ErrorLevel {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			require.Empty(t, stdout.Bytes())
		})
	}
}

func TestCommand_cgroupFailure(t *testing.T) {
	for _, tc := range []struct {
		desc         string
		extraOptions []Option
	}{
		{desc: "without logger"},
		{desc: "with logger", extraOptions: []Option{WithSubprocessLogger(log.Config{Format: "text"})}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := testhelper.Context(t)

			expectedErr := errors.New("expected")

			var underlyingCmd *exec.Cmd
			cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"cat"}, append(
				tc.extraOptions,
				WithCgroup(mockCgroupManager{
					addCommandFunc: func(cmd *exec.Cmd, opts ...cgroups.AddCommandOption) (string, error) {
						underlyingCmd = cmd
						return "", expectedErr
					},
				}),
				WithSetupStdin(),
			)...)
			require.Equal(t, expectedErr, err, "err: %s", err)
			require.Nil(t, cmd)

			// ProcessState is not nil if the process has exited.
			require.NotNil(t, underlyingCmd.ProcessState)
		})
	}
}

type mockCgroupManager struct {
	cgroups.Manager
	path            string
	cloneIntoCgroup bool
	addCommandFunc  func(*exec.Cmd, ...cgroups.AddCommandOption) (string, error)
}

func (m mockCgroupManager) AddCommand(cmd *exec.Cmd, opts ...cgroups.AddCommandOption) (string, error) {
	return m.addCommandFunc(cmd, opts...)
}

func (m mockCgroupManager) SupportsCloneIntoCgroup() bool {
	return m.cloneIntoCgroup
}

func (m mockCgroupManager) CloneIntoCgroup(*exec.Cmd, ...cgroups.AddCommandOption) (string, io.Closer, error) {
	return m.path, io.NopCloser(nil), nil
}

func TestCommand_logMessage(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	logger := testhelper.NewLogger(t)
	logger.LogrusEntry().Logger.SetLevel(logrus.DebugLevel)
	hook := testhelper.AddLoggerHook(logger)

	cgroupPath := "/sys/fs/cgroup/1"
	cmd, err := New(ctx, logger, []string{"echo", "hello world"},
		WithCgroup(mockCgroupManager{
			path:            cgroupPath,
			cloneIntoCgroup: true,
			addCommandFunc:  func(*exec.Cmd, ...cgroups.AddCommandOption) (string, error) { return cgroupPath, nil },
		}, nil),
	)
	require.NoError(t, err)

	require.NoError(t, cmd.Wait())
	logEntry := hook.LastEntry()
	assert.Equal(t, cmd.Pid(), logEntry.Data["pid"])
	assert.Equal(t, []string{"echo", "hello world"}, logEntry.Data["args"])
	assert.Equal(t, 0, logEntry.Data["command.exitCode"])
	assert.Equal(t, "/sys/fs/cgroup/1", logEntry.Data["command.cgroup_path"])
}

func TestCommand_exitStatusReporting(t *testing.T) {
	t.Parallel()

	entryWithMessage := func(t *testing.T, hook testhelper.LoggerHook, message string) *logrus.Entry {
		t.Helper()

		for _, entry := range hook.AllEntries() {
			if entry.Message == message {
				return entry
			}
		}

		require.FailNowf(t, "expected log entry is missing", "no entry with message %q has been logged", message)
		return nil
	}

	spawnCompleteEntry := func(t *testing.T, hook testhelper.LoggerHook) *logrus.Entry {
		t.Helper()

		return entryWithMessage(t, hook, "spawn complete")
	}

	t.Run("command exiting with failure", func(t *testing.T) {
		t.Parallel()

		logger := testhelper.NewLogger(t)
		logger.LogrusEntry().Logger.SetLevel(logrus.DebugLevel)
		hook := testhelper.AddLoggerHook(logger)

		cmd, err := New(testhelper.Context(t), logger, []string{"sh", "-c", "exit 3"})
		require.NoError(t, err)

		waitErr := cmd.Wait()
		exitCode, ok := ExitStatus(waitErr)
		require.True(t, ok)
		require.Equal(t, 3, exitCode)

		entry := spawnCompleteEntry(t, hook)
		require.Equal(t, 3, entry.Data["command.exitCode"])
		require.NotContains(t, entry.Data, "command.signal")
	})

	t.Run("command terminated by context cancellation", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(testhelper.Context(t))

		logger := testhelper.NewLogger(t)
		logger.LogrusEntry().Logger.SetLevel(logrus.DebugLevel)
		hook := testhelper.AddLoggerHook(logger)

		// New only returns after the process has been spawned, so it is guaranteed to be
		// alive and to receive the signal we send it by cancelling the context.
		cmd, err := New(ctx, logger, []string{"sleep", "10"})
		require.NoError(t, err)

		cancel()

		// Callers must still see the cancellation instead of the exit status of the
		// process we have killed ourselves.
		waitErr := cmd.Wait()
		require.ErrorIs(t, waitErr, context.Canceled)
		_, ok := ExitStatus(waitErr)
		require.False(t, ok)

		// The reporting on the other hand knows that we have signalled the process, with
		// SIGTERM: a process that gets SIGKILL cannot clean up, and git would leave its
		// reference lock behind.
		entry := spawnCompleteEntry(t, hook)
		require.Equal(t, -1, entry.Data["command.exitCode"])
		require.Equal(t, syscall.SIGTERM.String(), entry.Data["command.signal"])
	})

	t.Run("stderr is non-empty but context was cancelled by us", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(testhelper.Context(t))

		logger := testhelper.NewLogger(t)
		logger.LogrusEntry().Logger.SetLevel(logrus.DebugLevel)
		hook := testhelper.AddLoggerHook(logger)

		// The process writes to stderr and only afterwards announces via stdout that it
		// is about to block. This guarantees that its stderr has been written by the
		// time we cancel the context.
		cmd, err := New(ctx, logger, []string{"sh", "-c", "echo 'fatal: interrupted' >&2 && echo ready && sleep 10"},
			WithSetupStdout(),
		)
		require.NoError(t, err)

		ready := make([]byte, len("ready\n"))
		_, err = io.ReadFull(cmd, ready)
		require.NoError(t, err)
		require.Equal(t, "ready\n", string(ready))

		cancel()

		require.ErrorIs(t, cmd.Wait(), context.Canceled)

		// The process failed with a non-zero exit status, but it only did so because we
		// have killed it ourselves. Its stderr must thus not be escalated to the error
		// level.
		entry := entryWithMessage(t, hook, "fatal: interrupted\n")
		require.Equal(t, logrus.WarnLevel, entry.Level)
		require.Equal(t, -1, entry.Data["command.exitCode"])
		require.Equal(t, syscall.SIGTERM.String(), entry.Data["command.signal"])
	})

	t.Run("stderr is non-empty but context was not cancelled", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		logger := testhelper.NewLogger(t)
		logger.LogrusEntry().Logger.SetLevel(logrus.DebugLevel)
		hook := testhelper.AddLoggerHook(logger)

		// The process writes to stderr and only afterwards announces via stdout that it
		// is about to block. This guarantees that its stderr has been written by the
		// time we cancel the context.
		cmd, err := New(ctx, logger, []string{"sh", "-c", "echo 'fatal: interrupted' >&2 && echo ready && sleep 10"},
			WithSetupStdout(),
		)
		require.NoError(t, err)

		ready := make([]byte, len("ready\n"))
		_, err = io.ReadFull(cmd, ready)
		require.NoError(t, err)
		require.Equal(t, "ready\n", string(ready))

		// Here we want to abort the command while not cancelling the
		// context, so we will send a signal to the PID.
		process, err := os.FindProcess(cmd.Pid())
		if err != nil {
			fmt.Printf("Failed to find process: %v\n", err)
			return
		}

		err = process.Signal(syscall.SIGTERM)
		if err != nil {
			fmt.Printf("Failed to send signal: %v\n", err)
			return
		}

		require.EqualError(t, cmd.Wait(), "signal: terminated")

		// The process failed with a non-zero exit status, but it only did so because we
		// have killed it ourselves. Its stderr must thus not be escalated to the error
		// level.
		entry := entryWithMessage(t, hook, "fatal: interrupted\n")
		require.Equal(t, logrus.ErrorLevel, entry.Level)
		require.Equal(t, -1, entry.Data["command.exitCode"])
		require.Contains(t,
			[]string{syscall.SIGTERM.String(), syscall.SIGKILL.String()},
			entry.Data["command.signal"],
		)
	})
}

// TestCommand_withExitHook verifies that the exit hook runs once the process has been reaped, in
// every way a process ends, and exactly once.
func TestCommand_withExitHook(t *testing.T) {
	t.Parallel()

	t.Run("a normal exit", func(t *testing.T) {
		t.Parallel()

		var calls atomic.Int32
		cmd, err := New(testhelper.Context(t), testhelper.SharedLogger(t), []string{"true"}, WithExitHook(func() { calls.Add(1) }))
		require.NoError(t, err)

		require.NoError(t, cmd.Wait())
		require.Equal(t, int32(1), calls.Load())

		require.NoError(t, cmd.Wait())
		require.Equal(t, int32(1), calls.Load())
	})

	t.Run("an exit with an error", func(t *testing.T) {
		t.Parallel()

		var calls atomic.Int32
		cmd, err := New(testhelper.Context(t), testhelper.SharedLogger(t), []string{"false"}, WithExitHook(func() { calls.Add(1) }))
		require.NoError(t, err)

		require.Error(t, cmd.Wait())
		require.Equal(t, int32(1), calls.Load())
	})

	t.Run("a kill through the context, without a caller that waits", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(testhelper.Context(t))
		defer cancel()

		ran := make(chan struct{})
		_, err := New(ctx, testhelper.SharedLogger(t), []string{"cat"}, WithSetupStdin(), WithExitHook(func() { close(ran) }))
		require.NoError(t, err)

		select {
		case <-ran:
			require.FailNow(t, "the hook ran while the process was live")
		default:
		}

		cancel()
		<-ran
	})

	t.Run("a panic in the goroutine that waits", func(t *testing.T) {
		t.Parallel()

		var calls atomic.Int32
		cmd, err := New(testhelper.Context(t), testhelper.SharedLogger(t), []string{"true"},
			WithFinalizer(func(context.Context, *Command) { panic("the wrap-up panics") }),
			WithExitHook(func() { calls.Add(1) }),
		)
		require.NoError(t, err)

		require.Panics(t, func() { _ = cmd.Wait() })
		require.Equal(t, int32(1), calls.Load())
	})

	t.Run("a process that does not start", func(t *testing.T) {
		t.Parallel()

		var calls atomic.Int32
		_, err := New(testhelper.Context(t), testhelper.SharedLogger(t), []string{"/does/not/exist"}, WithExitHook(func() { calls.Add(1) }))
		require.Error(t, err)
		require.Zero(t, calls.Load(), "the caller owns a process that did not start")
	})
}

// TestDetachStdin verifies that a command with a detached stdin ends when its process exits, while
// the reader of the stdin has not ended, and that the copy runs its function when the reader ends.
func TestDetachStdin(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	defer writer.Close()

	copied := make(chan struct{})
	stdin, err := DetachStdin(reader, func() { close(copied) })
	require.NoError(t, err)
	defer stdin.Close()

	var calls atomic.Int32
	var stdout bytes.Buffer
	// head exits after its first line, with its stdin open.
	cmd, err := New(testhelper.Context(t), testhelper.SharedLogger(t), []string{"head", "-n1"},
		WithStdin(stdin), WithStdout(&stdout), WithExitHook(func() { calls.Add(1) }))
	require.NoError(t, err)

	_, err = writer.Write([]byte("line\n"))
	require.NoError(t, err)

	require.NoError(t, cmd.Wait())
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, "line\n", stdout.String())

	select {
	case <-copied:
		require.FailNow(t, "the copy ended while its reader was open")
	default:
	}

	require.NoError(t, writer.Close())
	<-copied
}

func TestCommand_withFinalizer(t *testing.T) {
	t.Parallel()

	t.Run("context cancellation runs finalizer", func(t *testing.T) {
		ctx, cancel := context.WithCancel(testhelper.Context(t))

		finalizerCh := make(chan struct{})
		_, err := New(ctx, testhelper.SharedLogger(t), []string{"echo"}, WithFinalizer(func(context.Context, *Command) {
			close(finalizerCh)
		}))
		require.NoError(t, err)

		cancel()

		<-finalizerCh
	})

	t.Run("Wait runs finalizer", func(t *testing.T) {
		ctx := testhelper.Context(t)

		finalizerCh := make(chan struct{})
		cmd, err := New(ctx, testhelper.SharedLogger(t), []string{"echo"}, WithFinalizer(func(context.Context, *Command) {
			close(finalizerCh)
		}))
		require.NoError(t, err)

		require.NoError(t, cmd.Wait())

		<-finalizerCh
	})

	t.Run("Wait runs multiple finalizers", func(t *testing.T) {
		ctx := testhelper.Context(t)

		wg := sync.WaitGroup{}
		wg.Add(2)
		cmd, err := New(
			ctx,
			testhelper.SharedLogger(t),
			[]string{"echo"},
			WithFinalizer(func(context.Context, *Command) { wg.Done() }),
			WithFinalizer(func(context.Context, *Command) { wg.Done() }),
		)
		require.NoError(t, err)
		require.NoError(t, cmd.Wait())

		wg.Wait()
	})

	t.Run("Wait runs finalizer with the latest context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(testhelper.Context(t))
		//nolint:staticcheck
		ctx = context.WithValue(ctx, "hello", "world")

		_, err := New(ctx, testhelper.SharedLogger(t), []string{"echo"}, WithFinalizer(func(ctx context.Context, _ *Command) {
			require.Equal(t, "world", ctx.Value("hello"))
		}))
		require.NoError(t, err)

		cancel()
	})

	t.Run("process exit does not run finalizer", func(t *testing.T) {
		ctx := testhelper.Context(t)

		finalizerCh := make(chan struct{})
		_, err := New(ctx, testhelper.SharedLogger(t), []string{"echo"}, WithFinalizer(func(context.Context, *Command) {
			close(finalizerCh)
		}))
		require.NoError(t, err)

		select {
		case <-finalizerCh:
			// Command finalizers should only be running when we have either explicitly
			// called `Wait()` on the command, or when the context has been cancelled.
			// Otherwise we may run into the case where finalizers have already been ran
			// on the exited process even though we may still be busy handling the
			// output of that command, which may result in weird races.
			require.FailNow(t, "finalizer should not have been ran")
		case <-time.After(50 * time.Millisecond):
		}
	})
}

func TestCommand_argsLogging(t *testing.T) {
	t.Parallel()

	// Write to stderr and exit 0 so that the command-completion log entry, which
	// carries the "args" field, is emitted at warn level.
	const script = `#!/usr/bin/env bash
		echo "warning message" >&2
		exit 0
	`

	for _, tc := range []struct {
		desc      string
		extraArgs int
	}{
		{
			desc:      "args within limit are logged in full",
			extraArgs: 5,
		},
		{
			desc:      "args exactly at limit are logged in full",
			extraArgs: maxLoggedArgsElements - 1, // +1 for the binary path
		},
		{
			desc:      "args beyond limit are truncated",
			extraArgs: maxLoggedArgsElements + 10,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			binaryPath := testhelper.WriteExecutable(t, filepath.Join(testhelper.TempDir(t), "script"), []byte(script))

			args := []string{binaryPath}
			for i := 0; i < tc.extraArgs; i++ {
				args = append(args, fmt.Sprintf("arg-%d", i))
			}

			logger := testhelper.NewLogger(t)
			hook := testhelper.AddLoggerHook(logger)
			ctx := testhelper.Context(t)

			cmd, err := New(ctx, logger, args)
			require.NoError(t, err)
			require.NoError(t, cmd.Wait())

			lastEntry := hook.LastEntry()
			require.NotNil(t, lastEntry)
			require.Equal(t, logrus.WarnLevel, lastEntry.Level)

			loggedArgs, ok := lastEntry.Data["args"].([]string)
			require.True(t, ok, "args field should be a []string")

			if len(args) <= maxLoggedArgsElements {
				require.Equal(t, args, loggedArgs)
			} else {
				expected := append([]string{}, args[:maxLoggedArgsElements]...)
				expected = append(expected, fmt.Sprintf("[%d more args truncated]", len(args)-maxLoggedArgsElements))
				require.Equal(t, expected, loggedArgs)
				require.Len(t, loggedArgs, maxLoggedArgsElements+1)
			}
		})
	}
}
