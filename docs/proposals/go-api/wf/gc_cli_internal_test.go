package wf

import (
	"os"
	"runtime/debug"
	"testing"
)

// restoreGC puts the collector settings, the environment variables and the
// tuned mark back as they were when the test began.
func restoreGC(t *testing.T) {
	t.Helper()
	pct := debug.SetGCPercent(100)
	limit := debug.SetMemoryLimit(-1)
	tuned := cliMemoryTuned.Load()
	t.Cleanup(func() {
		debug.SetGCPercent(pct)
		debug.SetMemoryLimit(limit)
		cliMemoryTuned.Store(tuned)
	})
	for _, k := range []string{"GOGC", "GOMEMLIMIT"} {
		old, had := os.LookupEnv(k)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
}

func TestTuneGCForCLIKeepsEnvironmentGOGC(t *testing.T) {
	restoreGC(t)
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

// With GOMEMLIMIT unset the CLI sets the 1 GiB soft limit; with it set (the
// runtime applied it at start) the limit is left as it is, and no journal
// read raises it.
func TestTuneGCForCLIKeepsEnvironmentGOMEMLIMIT(t *testing.T) {
	restoreGC(t)
	const env = 3 << 30

	os.Setenv("GOMEMLIMIT", "3GiB")
	debug.SetMemoryLimit(env) // what the runtime made of it at start
	cliMemoryTuned.Store(false)
	TuneGCForCLI()
	if got := debug.SetMemoryLimit(-1); got != env {
		t.Fatalf("limit %d with GOMEMLIMIT set, want it left at %d", got, int64(env))
	}
	raiseMemoryLimit(1 << 30)
	if got := debug.SetMemoryLimit(-1); got != env {
		t.Fatalf("a journal read moved the limit to %d with GOMEMLIMIT set, want %d", got, int64(env))
	}

	os.Unsetenv("GOMEMLIMIT")
	debug.SetMemoryLimit(1<<63 - 1) // the runtime's default: no limit
	TuneGCForCLI()
	if got := debug.SetMemoryLimit(-1); got != cliMemoryLimit {
		t.Fatalf("limit %d with GOMEMLIMIT unset, want %d", got, int64(cliMemoryLimit))
	}
}

// A journal read raises the tuned limit to 4x its bytes when that is more,
// and never lowers it; a process that was not tuned is left alone.
func TestRaiseMemoryLimitOnlyRaises(t *testing.T) {
	restoreGC(t)
	os.Unsetenv("GOMEMLIMIT")
	debug.SetMemoryLimit(1<<63 - 1)
	TuneGCForCLI()

	raiseMemoryLimit(100 << 20) // 400 MiB < 1 GiB
	if got := debug.SetMemoryLimit(-1); got != cliMemoryLimit {
		t.Fatalf("limit %d after a small read, want %d", got, int64(cliMemoryLimit))
	}
	raiseMemoryLimit(512 << 20)
	if got, want := debug.SetMemoryLimit(-1), int64(512<<20)*cliMemoryPerByte; got != want {
		t.Fatalf("limit %d after a 512 MiB read, want %d", got, want)
	}
	raiseMemoryLimit(300 << 20)
	if got, want := debug.SetMemoryLimit(-1), int64(512<<20)*cliMemoryPerByte; got != want {
		t.Fatalf("a smaller read lowered the limit to %d, want %d", got, want)
	}

	cliMemoryTuned.Store(false)
	debug.SetMemoryLimit(1<<63 - 1)
	raiseMemoryLimit(512 << 20)
	if got := debug.SetMemoryLimit(-1); got != 1<<63-1 {
		t.Fatalf("an untuned process's limit moved to %d", got)
	}
}
