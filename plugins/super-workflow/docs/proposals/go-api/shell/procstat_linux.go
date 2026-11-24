//go:build linux

package shell

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// detachedSupported reports whether the detached runner can identify a launch:
// it needs /proc start ticks.
const detachedSupported = true

// procStat reads /proc/<pid>/stat after the last ')' of the comm field: the
// state (field 3) and the start time in clock ticks since boot (field 22). ok
// is false when the process is gone or /proc is unreadable.
func procStat(pid int) (ticks uint64, state byte, ok bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, 0, false
	}
	// After the comm: state is index 0, so start time (field 22) is index 19.
	f := strings.Fields(string(stat[i+1:]))
	if len(f) < 20 || len(f[0]) != 1 {
		return 0, 0, false
	}
	t, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return t, f[0][0], true
}
