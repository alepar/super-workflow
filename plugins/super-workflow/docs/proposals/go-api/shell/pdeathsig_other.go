//go:build !linux

package shell

import "syscall"

// setPdeathsig is a no-op where the kernel has no parent-death signal.
func setPdeathsig(*syscall.SysProcAttr) {}
