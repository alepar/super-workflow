//go:build !linux

package wf

import "syscall"

// procStart is unknown off Linux.
func procStart(int) uint64 { return 0 }

// procLive reports whether pid runs; off Linux a reused pid cannot be told.
func procLive(pid int, _ uint64) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// groupMembers is unknown off Linux: only the leader is checked.
func groupMembers(int) bool { return false }
