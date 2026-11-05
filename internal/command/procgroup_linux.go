//go:build linux

package command

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// A process group has no identity but its number, which the kernel may hand out again once the group
// is empty and its leader reaped. exec.Cmd.Wait reaps the leader before it waits for its copy
// goroutines, so a command can be in Wait with its group long gone. What tells that a number still
// belongs to the group of a command is this: the kernel does not reuse the number of a group that has
// a member, so a member that was already there when the number was known to be ours proves that the
// group never was empty.
//
// PIDFD_SIGNAL_PROCESS_GROUP would make the signal itself exact, but needs Linux 6.9.

// procGroup identifies the process group of a command, whose leader it started.
type procGroup struct {
	pgid int
	// leaderStart is the start time of the leader, 0 when it could not be read.
	leaderStart uint64
}

// newProcGroup identifies the group of a leader that was just started. The caller has not reaped the
// leader, so the number cannot have been reused yet.
func newProcGroup(pid int) procGroup {
	pgrp, start, err := readProcStat(pid)
	if err != nil || pgrp != pid {
		return procGroup{pgid: pid}
	}
	return procGroup{pgid: pid, leaderStart: start}
}

// procGroupProof is a moment at which the number of a group belonged to it.
type procGroupProof struct {
	group procGroup
	// ticks is that moment, in the clock of the start times; 0 when nothing is known of one.
	ticks uint64
}

// proof returns a moment at which the number belongs to the group: now, if the process with that number
// is still the leader, which its start time tells. It holds for as long as the group has a member from
// before that moment. The clock is read first, so that the leader was there at the moment it read.
//
// The moment is in a later clock tick than the start of the leader, which it waits for if it has to
// (only a command that ends within 10 ms of its start does): a process that reuses the number starts
// after this moment, so it cannot be taken for the leader by its start time.
func (g procGroup) proof() procGroupProof {
	if g.leaderStart == 0 {
		return procGroupProof{group: g}
	}

	ticks, err := bootTicks()
	// More than a tick of waiting means that the clock is not the clock of the start times.
	for attempt := 0; err == nil && ticks <= g.leaderStart; attempt++ {
		if attempt == 50 {
			return procGroupProof{group: g}
		}
		time.Sleep(time.Millisecond)
		ticks, err = bootTicks()
	}
	if err != nil || !g.leaderThere() {
		return procGroupProof{group: g}
	}
	return procGroupProof{group: g, ticks: ticks}
}

func (g procGroup) leaderThere() bool {
	pgrp, start, err := readProcStat(g.pgid)
	return err == nil && pgrp == g.pgid && start == g.leaderStart
}

// stillOurs reports whether the number still belongs to the group, which is the case when
//   - the leader is still there, also as a zombie: a process with its number and its start time, which
//     is before the moment of the proof; or
//   - a member of the group started strictly before the moment of the proof. A reused number has
//     members from after that moment only: it could be reused only after the leader was reaped.
//
// It reports false when it cannot tell, so that a signal never goes to a group that may be another's:
// without a proof (the leader was gone when the process was signalled), and when all the surviving
// members of the group started after the moment of the proof, or within its clock tick (10 ms). What
// it does not rule out is a process older than the proof that moved itself into a reused group with
// setpgid(2), and a number reused between this check and the signal, which takes the whole PID space
// to go by in that time.
func (p procGroupProof) stillOurs() bool {
	if p.ticks == 0 {
		return false
	}
	if p.group.leaderThere() {
		return true
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		// A process that is gone or cannot be read proves nothing.
		if pgrp, start, err := readProcStat(pid); err == nil && pgrp == p.group.pgid && start < p.ticks {
			return true
		}
	}
	return false
}

// bootTicks returns the time since boot in clock ticks, the unit and the clock of the start times of
// proc_pid_stat(5). The kernel's user-visible tick is 100 Hz.
func bootTicks() (uint64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, fmt.Errorf("reading the boot clock: %w", err)
	}
	return uint64(ts.Nano() / 10_000_000), nil
}

func readProcStat(pid int) (pgrp int, start uint64, _ error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	return parseProcStat(stat)
}

// parseProcStat returns the process group (field 5) and the start time (field 22) of the contents of
// /proc/<pid>/stat. The command name (field 2) can hold anything, so the fields are counted from the
// last closing parenthesis.
func parseProcStat(stat []byte) (pgrp int, start uint64, _ error) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, 0, errors.New("no command name in the process status")
	}

	// The first of these is field 3.
	fields := bytes.Fields(stat[end+1:])
	const pgrpField, startField = 5 - 3, 22 - 3
	if len(fields) <= startField {
		return 0, 0, errors.New("too few fields in the process status")
	}

	pgrp, err := strconv.Atoi(string(fields[pgrpField]))
	if err != nil {
		return 0, 0, fmt.Errorf("parsing the process group: %w", err)
	}
	start, err = strconv.ParseUint(string(fields[startField]), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parsing the start time: %w", err)
	}
	return pgrp, start, nil
}
