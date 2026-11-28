//go:build linux

package shell

import "syscall"

// setPdeathsig makes the child die with the thread that started it, so an
// inline command does not outlive its CLI call.
func setPdeathsig(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }
