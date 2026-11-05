//go:build linux

package command

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

		// With the leader reaped, what proves the group is a member older than the signal, in
		// the 10 ms ticks of the start times.
		waitForNextTick(t)

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

	t.Run("a group that is gone while Wait is blocked gets no signal and no warning", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(testhelper.Context(t))
		defer cancel()

		logger := testhelper.NewLogger(t)
		hook := testhelper.AddLoggerHook(logger)

		// The stdin copy holds Wait after the leader was reaped, which is when the number can be
		// reused. The escalation waits until the group is gone, so that the short grace period
		// decides nothing on a loaded machine.
		stdin := blockedReader{release: make(chan struct{})}
		groupGone := make(chan struct{})
		decided := make(chan bool, 1)
		cmd, err := New(ctx, logger, []string{"sh", "-c", `echo ready; exec sleep 1000`},
			WithSetupStdout(), WithStdin(stdin),
			func(cfg *config) {
				cfg.terminationGrace = shortGrace
				cfg.beforeEscalation = func() { <-groupGone }
				cfg.afterEscalation = func(signalled bool) { decided <- signalled }
			},
		)
		require.NoError(t, err)
		_, err = bufio.NewReader(cmd).ReadString('\n')
		require.NoError(t, err)

		cancel()
		requireGroupGone(t, cmd.Pid())
		close(groupGone)

		require.False(t, <-decided, "the number may be another group's")
		for _, entry := range hook.AllEntries() {
			require.NotEqual(t, logrus.WarnLevel, entry.Level, "%s", entry.Message)
		}

		select {
		case <-cmd.processExitedCh:
			require.FailNow(t, "the case of this test is Wait still blocked after the group ended")
		default:
		}

		close(stdin.release)
		require.ErrorIs(t, cmd.Wait(), context.Canceled)
	})
}

// blockedReader is a stdin whose copy goroutine stays blocked, and so Wait, until it is released.
type blockedReader struct{ release chan struct{} }

func (r blockedReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

// waitForNextTick waits until the clock of the start times has moved on, so that what started before
// this call is strictly older than what reads the clock after it.
func waitForNextTick(t *testing.T) {
	t.Helper()

	now, err := bootTicks()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		later, err := bootTicks()
		return err == nil && later > now
	}, terminationGiveUp, time.Millisecond)
}

// startInOwnGroup starts a process as the leader of its own group, unrelated to any command.
func startInOwnGroup(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

// TestProcGroupProof verifies, with real processes and proc(5), what tells that the number of a group
// still is the group's: a group whose number was reused is exactly a process group with that number
// whose leader has another start time and whose members are all younger than the proof.
func TestProcGroupProof(t *testing.T) {
	t.Parallel()

	t.Run("the leader is there", func(t *testing.T) {
		t.Parallel()

		group := newProcGroup(startInOwnGroup(t, "sleep", "1000").Process.Pid)
		require.NotZero(t, group.leaderStart)

		// Also within the clock tick in which it started: the moment of the proof comes after that
		// tick, so a process that reuses the number and starts after it never has the leader's
		// start time.
		proof := group.proof()
		require.Greater(t, proof.ticks, group.leaderStart)
		require.True(t, proof.stillOurs())
	})

	t.Run("the leader is a zombie", func(t *testing.T) {
		t.Parallel()

		leader := startInOwnGroup(t, "sleep", "1000")
		group := newProcGroup(leader.Process.Pid)
		proof := group.proof()

		require.NoError(t, syscall.Kill(leader.Process.Pid, syscall.SIGKILL))
		// Not reaped: the process stays, and with it its number.
		require.Eventually(t, func() bool {
			stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", leader.Process.Pid))
			return err == nil && strings.Contains(string(stat), ") Z ")
		}, terminationGiveUp, time.Millisecond)

		require.True(t, proof.stillOurs())
	})

	t.Run("the number was reused", func(t *testing.T) {
		t.Parallel()

		ticks, err := bootTicks()
		require.NoError(t, err)
		waitForNextTick(t)

		// An innocent group with the number of the group that ended, younger than the proof.
		victim := startInOwnGroup(t, "sleep", "1000")
		reused := newProcGroup(victim.Process.Pid)
		require.NotZero(t, reused.leaderStart)

		ours := procGroup{pgid: reused.pgid, leaderStart: reused.leaderStart - 1}
		require.False(t, procGroupProof{group: ours, ticks: ticks}.stillOurs())
	})

	t.Run("the leader was reaped and a member older than the proof is left", func(t *testing.T) {
		t.Parallel()

		leader, member := startGroupWithSurvivor(t)
		waitForNextTick(t)
		ticks, err := bootTicks()
		require.NoError(t, err)

		ours := procGroup{pgid: leader, leaderStart: 1}
		require.True(t, procGroupProof{group: ours, ticks: ticks}.stillOurs(), "member %d", member)
	})

	t.Run("the leader was reaped and the members are younger than the proof", func(t *testing.T) {
		t.Parallel()

		ticks, err := bootTicks()
		require.NoError(t, err)
		waitForNextTick(t)

		leader, _ := startGroupWithSurvivor(t)

		// This is how a reused group looks, so it gets no signal even if it is the command's own.
		ours := procGroup{pgid: leader, leaderStart: 1}
		require.False(t, procGroupProof{group: ours, ticks: ticks}.stillOurs())
	})

	t.Run("without a proof", func(t *testing.T) {
		t.Parallel()

		// The leader could not be identified at its start.
		leader := startInOwnGroup(t, "sleep", "1000")
		require.False(t, procGroup{pgid: leader.Process.Pid}.proof().stillOurs())

		// The leader was gone when the process was signalled: the number is another's by now.
		group := newProcGroup(leader.Process.Pid)
		group.leaderStart--
		require.False(t, group.proof().stillOurs())
	})
}

// startGroupWithSurvivor starts a group whose leader is reaped and whose only member left is a
// descendant. It returns the number of the group and the descendant.
func startGroupWithSurvivor(t *testing.T) (leader, member int) {
	t.Helper()

	cmd := exec.Command("sh", "-c", `sleep 1000 </dev/null >/dev/null 2>&1 & echo $!`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := cmd.Output()
	require.NoError(t, err)

	member, err = strconv.Atoi(strings.TrimSpace(string(output)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(member, syscall.SIGKILL) })

	return cmd.Process.Pid, member
}

func TestParseProcStat(t *testing.T) {
	t.Parallel()

	const rest = " S 1 77 77 0 -1 4194304 1 0 0 0 0 0 0 0 20 0 1 0 12345 1000 10\n"

	for _, comm := range []string{"sh", "a b", "a) S 9 9 (b", ")", "("} {
		pgrp, start, err := parseProcStat([]byte("42 (" + comm + ")" + rest))
		require.NoError(t, err, comm)
		require.Equal(t, 77, pgrp, comm)
		require.Equal(t, uint64(12345), start, comm)
	}

	for _, stat := range []string{"", "42 (sh", "42 (sh) S 1 77", "42 (sh) S 1 x 77 0 -1 4 1 0 0 0 0 0 0 0 20 0 1 0 12345", "42 (sh) S 1 77 77 0 -1 4 1 0 0 0 0 0 0 0 20 0 1 0 x"} {
		_, _, err := parseProcStat([]byte(stat))
		require.Error(t, err, stat)
	}
}
