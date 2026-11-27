//go:build linux

package wf

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// procStart is the kernel's start time of pid (field 22 of /proc/<pid>/stat,
// in clock ticks since boot), 0 when it cannot be read.
func procStart(pid int) uint64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// The command name (field 2) may hold spaces and parentheses: count
	// fields after its closing parenthesis.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0
	}
	n, _ := strconv.ParseUint(f[19], 10, 64)
	return n
}

// procLive reports whether pid runs and, when start is known, is the process
// that started then (not a reused pid). A zombie is not live.
func procLive(pid int, start uint64) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		if f := strings.Fields(s[i+1:]); len(f) > 0 && f[0] == "Z" {
			return false
		}
	}
	return start == 0 || procStart(pid) == start
}

// groupMembers reports whether a process other than its leader belongs to
// process group pgid.
func groupMembers(pgid int) bool {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == pgid {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		// After the command: state, ppid, pgrp.
		if f := strings.Fields(s[i+1:]); len(f) > 2 && f[0] != "Z" && f[2] == strconv.Itoa(pgid) {
			return true
		}
	}
	return false
}
