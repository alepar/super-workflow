//go:build !linux

package shell

import (
	"syscall"
	"testing"
)

func checkPdeathsig(*testing.T, *syscall.SysProcAttr) {}
