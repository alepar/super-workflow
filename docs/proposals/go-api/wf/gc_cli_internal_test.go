package wf

import (
	"os"
	"runtime/debug"
	"testing"
)

func TestTuneGCForCLIKeepsEnvironmentGOGC(t *testing.T) {
	prev := debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(prev) })
	old, had := os.LookupEnv("GOGC")
	t.Cleanup(func() {
		if had {
			os.Setenv("GOGC", old)
		} else {
			os.Unsetenv("GOGC")
		}
	})

	os.Unsetenv("GOGC")
	TuneGCForCLI()
	if got := debug.SetGCPercent(100); got != cliGCPercent {
		t.Fatalf("GC percent %d with GOGC unset, want %d", got, cliGCPercent)
	}
	os.Setenv("GOGC", "150")
	TuneGCForCLI()
	if got := debug.SetGCPercent(100); got != 100 {
		t.Fatalf("GC percent %d with GOGC set, want it left at 100", got)
	}
}
