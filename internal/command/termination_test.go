package command

import (
	"bufio"
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/testhelper"
)

// terminationGiveUp only stops a test that went wrong: without an escalation, the wait never ends.
const terminationGiveUp = time.Minute

// waitBounded returns the result of the command's Wait and how long it took after start. A group that
// is still there at the bound is killed, so that the failing test leaves nothing behind.
func waitBounded(t *testing.T, cmd *Command, start time.Time) (time.Duration, error) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return time.Since(start), err
	case <-time.After(terminationGiveUp):
		_ = syscall.Kill(-cmd.Pid(), syscall.SIGKILL)
		<-done
		require.FailNow(t, "Wait did not end: a cancelled command hangs its callers")
		return 0, nil
	}
}

// spawnComplete returns the log entry of the end of a command, whose fields tell how it ended. The
// entry comes after Wait in the same goroutine that reaps the process, so it is there once Wait returned.
func spawnComplete(t *testing.T, hook testhelper.LoggerHook) *logrus.Entry {
	t.Helper()

	for _, entry := range hook.AllEntries() {
		if entry.Message == "spawn complete" {
			return entry
		}
	}
	require.FailNow(t, "no log entry of the end of the command")
	return nil
}

// requireGroupGone waits for the process group to be empty. A killed descendant is not reaped by Wait
// but by the process it was reparented to, so it counts as a member a little longer: the test waits
// for a state, and the bound only stops a test that went wrong.
func requireGroupGone(t *testing.T, pgid int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
	}, terminationGiveUp, time.Millisecond, "nothing of the process group is left")
}

// TestCommandTermination verifies how a command whose context is done ends: with SIGTERM, which lets
// git remove its locks, and with SIGKILL to its process group only when the group has outlived the
// grace period, so that a process that ignores SIGTERM cannot hang the callers of the command.
func TestCommandTermination(t *testing.T) {
	t.Parallel()

	const shortGrace = 200 * time.Millisecond

	start := func(t *testing.T, script string, grace time.Duration) (*Command, context.CancelFunc, testhelper.LoggerHook) {
		t.Helper()

		ctx, cancel := context.WithCancel(testhelper.Context(t))
		t.Cleanup(cancel)

		logger := testhelper.NewLogger(t)
		logger.LogrusEntry().Logger.SetLevel(logrus.DebugLevel)
		hook := testhelper.AddLoggerHook(logger)

		cmd, err := New(ctx, logger, []string{"sh", "-c", script}, WithSetupStdout(), func(cfg *config) {
			cfg.terminationGrace = grace
		})
		require.NoError(t, err)

		// Every script says ready once its signal handling is set up.
		line, err := bufio.NewReader(cmd).ReadString('\n')
		require.NoError(t, err)
		require.Equal(t, "ready\n", line)

		return cmd, cancel, hook
	}

	t.Run("a process that ignores SIGTERM gets SIGKILL after the grace period", func(t *testing.T) {
		t.Parallel()

		cmd, cancel, hook := start(t, `trap '' TERM; echo ready; exec sleep 1000`, shortGrace)
		pid := cmd.Pid()

		cancelled := time.Now()
		cancel()
		took, err := waitBounded(t, cmd, cancelled)

		require.ErrorIs(t, err, context.Canceled)
		require.GreaterOrEqual(t, took, shortGrace, "it had its grace period")
		require.Equal(t, syscall.SIGKILL.String(), spawnComplete(t, hook).Data["command.signal"])
		requireGroupGone(t, pid)
	})

	t.Run("a descendant that ignores SIGTERM and holds the streams gets SIGKILL too", func(t *testing.T) {
		t.Parallel()

		// The leader ends with SIGTERM. exec.Cmd.Wait still waits for the stderr pipe that the
		// descendant holds, and only a signal to the group reaches it.
		cmd, cancel, hook := start(t, `(trap '' TERM; echo ready; exec sleep 1000) & wait`, shortGrace)
		pid := cmd.Pid()

		cancelled := time.Now()
		cancel()
		took, err := waitBounded(t, cmd, cancelled)

		require.ErrorIs(t, err, context.Canceled)
		require.GreaterOrEqual(t, took, shortGrace)
		require.Equal(t, syscall.SIGTERM.String(), spawnComplete(t, hook).Data["command.signal"], "the leader died of SIGTERM")
		requireGroupGone(t, pid)
	})

	t.Run("a process that ends on SIGTERM cleans up and gets no SIGKILL", func(t *testing.T) {
		t.Parallel()

		// A grace period that the test cannot reach, so that only SIGTERM can end the process.
		cmd, cancel, hook := start(t, `trap 'exit 7' TERM; echo ready; while :; do :; done`, 10*terminationGiveUp)
		pid := cmd.Pid()

		cancelled := time.Now()
		cancel()
		_, err := waitBounded(t, cmd, cancelled)

		var exitErr interface{ ExitCode() int }
		require.True(t, errors.As(err, &exitErr), "%v", err)
		require.Equal(t, 7, exitErr.ExitCode(), "the handler of SIGTERM ran")
		require.Equal(t, 7, spawnComplete(t, hook).Data["command.exitCode"])
		requireGroupGone(t, pid)
	})

	t.Run("a process that exits by itself is not signalled", func(t *testing.T) {
		t.Parallel()

		cmd, cancel, hook := start(t, `echo ready`, shortGrace)
		defer cancel()

		require.NoError(t, cmd.Wait())
		require.Equal(t, 0, spawnComplete(t, hook).Data["command.exitCode"])
	})
}
