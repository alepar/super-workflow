//go:build linux

package shell

import (
	"syscall"
	"testing"
)

func checkPdeathsig(t *testing.T, attr *syscall.SysProcAttr) {
	t.Helper()
	if attr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("Pdeathsig = %v, want SIGKILL", attr.Pdeathsig)
	}
}
