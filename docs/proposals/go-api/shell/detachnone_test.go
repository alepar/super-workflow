package shell

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// noneRunner is a host where nothing outlives its command.
type noneRunner struct{ wf.EffectRunner }

func (noneRunner) DetachMode() wf.DetachFact {
	return wf.DetachFact{Mode: wf.DetachNone, NSpidFields: 2, Systemd: "user bus"}
}

func (n noneRunner) Start(wf.DetachedRequest) (wf.DetachedProcess, error) {
	panic("detach none: Start must not be called")
}

// With detach none a detached suite runs inline in the caller's command (G3):
// no launch, its result journaled directly, and the run journals the mode and
// its inputs at start.
func TestDetachNoneRunsInline(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sh", "-c", "echo inline")
	e := engineOver(t, stateDir, d, noneRunner{DetachedRunner(testBin(t))})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var st wf.RunStatus
	for i := 0; i < 50; i++ {
		if _, err := e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
		if st, err = e.Status(run); err != nil {
			t.Fatal(err)
		}
		if st.Phase == wf.RunEnded {
			break
		}
	}
	if st.Phase != wf.RunEnded || st.Terminal != "cOK" {
		t.Fatalf("status = %s/%s, want ended/cOK", st.Phase, st.Terminal)
	}
	if n := len(kinds(t, stateDir, run, wf.JournalEffectStarted)); n != 0 {
		t.Errorf("effect_started records = %d, want 0: detach none never launches", n)
	}
	started := kinds(t, stateDir, run, wf.JournalRunStarted)
	var rs wf.RunStartedData
	if len(started) != 1 || json.Unmarshal(started[0].Data, &rs) != nil || rs.Detach == nil ||
		rs.Detach.Mode != wf.DetachNone || rs.Detach.NSpidFields != 2 || rs.Detach.Systemd != "user bus" {
		t.Fatalf("run_started detach = %+v, want none with its inputs", rs.Detach)
	}
}

// While a detached command runs inline at detach none, the caller's command
// prints progress lines naming the effect.
func TestDetachNoneInlinePrintsProgress(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sh", "-c", "sleep 0.3; echo inline")
	var progress syncBuffer
	e := engineOverWith(t, stateDir, d, noneRunner{DetachedRunner(testBin(t))}, func(o *wf.EngineOptions) { o.Progress, o.ProgressEvery = &progress, 50*time.Millisecond })
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
		if st, _ := e.Status(run); st.Phase == wf.RunEnded {
			break
		}
	}
	out := progress.String()
	for _, want := range []string{"running inline (detach: none): sh -c sleep 0.3; echo inline", "still running inline", "finished inline after"} {
		if !strings.Contains(out, want) {
			t.Errorf("progress lacks %q:\n%s", want, out)
		}
	}
}

// The bootstrap says so on a detach-none host, and only there.
func TestBootstrapNamesDetachNone(t *testing.T) {
	if b := wf.Bootstrap(wf.RunStatus{Run: "r", Harness: "codex", Detach: wf.DetachNone}, "./super", "/s"); !strings.Contains(b, "detach: none on this host") || !strings.Contains(b, "longest tool timeout") {
		t.Errorf("detach none bootstrap lacks the long-timeout line:\n%s", b)
	}
	if b := wf.Bootstrap(wf.RunStatus{Run: "r", Harness: "claude", Detach: wf.DetachSystemd}, "./super", "/s"); strings.Contains(b, "detach: none") {
		t.Errorf("systemd host bootstrap names detach none:\n%s", b)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// H1: an intent launched detached by one process settles through the runner
// even when a later process probes none: never a second, inline run.
func TestLaunchedIntentSettlesDetachedUnderALaterNoneProcess(t *testing.T) {
	for name, starter := range map[string]wf.EffectRunner{
		"journaled process": processRunner{DetachedRunner(testBin(t))},
		// A run from before G1 journals no mode: the later process's none is
		// all the engine knows, and the launched intent must still settle
		// through the runner.
		"unjournaled": plainRunner{DetachedRunner(testBin(t))},
	} {
		t.Run(name, func(t *testing.T) { launchedThenNone(t, starter) })
	}
}

func launchedThenNone(t *testing.T, starter wf.EffectRunner) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sh", "-c", "sleep 0.5; echo once >> \"$0\"", filepath.Join(stateDir, "count"))
	first := engineOver(t, stateDir, d, starter)
	run, err := first.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	killGroupsOf(t, stateDir, run)
	if n := len(kinds(t, stateDir, run, wf.JournalEffectStarted)); n != 1 {
		t.Fatalf("effect_started = %d, want the detached launch", n)
	}
	later := engineOver(t, stateDir, d, noneProber{DetachedRunner(testBin(t))})
	var st wf.RunStatus
	for i := 0; i < 200; i++ {
		if _, err := later.Next(run, ""); err != nil {
			t.Fatal(err)
		}
		if st, _ = later.Status(run); st.Phase == wf.RunEnded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Phase != wf.RunEnded || st.Terminal != "cOK" {
		t.Fatalf("status = %s/%s, want ended/cOK", st.Phase, st.Terminal)
	}
	if b, _ := os.ReadFile(filepath.Join(stateDir, "count")); string(b) != "once\n" {
		t.Fatalf("the command ran %q, want exactly once", b)
	}
	if _, plain := starter.(plainRunner); !plain && st.Detach != wf.DetachProcess {
		t.Errorf("status detach %q, want the run's journaled process", st.Detach)
	}
}

// plainRunner hides the runner's DetachMode, as a runner from before G1.
type plainRunner struct{ wf.EffectRunner }

// H2: inline at detach none, a .Detached(max) command is bounded by its max,
// not the runtime's default timeout, and its expiry is exit 124.
func TestInlineDetachedHonoursItsMax(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(400*time.Millisecond, "sh", "-c", "sleep 0.2; echo ran")
	reg := wf.NewAdapters()
	if err := reg.Register(Runtime(Config{Timeout: 100 * time.Millisecond})); err != nil {
		t.Fatal(err)
	}
	e := wf.NewEngine(wf.FileJournal(stateDir), reg, wf.EngineOptions{
		Defs: []*wf.Def{d}, EffectRunner: noneRunner{DetachedRunner(testBin(t))},
		RunDir: func(run wf.RunID) string { return filepath.Join(stateDir, "runs", string(run)) },
	})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
		if st, _ := e.Status(run); st.Phase == wf.RunEnded {
			break
		}
	}
	var rr wf.RunResult
	res := kinds(t, stateDir, run, wf.JournalEffectResult)
	var data wf.EffectResultData
	if len(res) != 1 || json.Unmarshal(res[0].Data, &data) != nil || json.Unmarshal(data.Result, &rr) != nil || rr.Exit != 0 || rr.Stdout != "ran\n" {
		t.Fatalf("result %+v (%d records), want the 0.2s command to finish under its 0.4s max despite the 0.1s runtime default", rr, len(res))
	}

	slow := detachedDef(200*time.Millisecond, "sleep", "5")
	e2 := wf.NewEngine(wf.FileJournal(t.TempDir()), reg, wf.EngineOptions{
		Defs: []*wf.Def{slow}, EffectRunner: noneRunner{DetachedRunner(testBin(t))},
		RunDir: func(run wf.RunID) string { return filepath.Join(stateDir, "runs2", string(run)) },
	})
	run2, err := e2.Run(slow, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	if _, err := e2.Next(run2, ""); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 3*time.Second {
		t.Fatalf("inline command ran %v, want it killed at its 200ms max", d)
	}
}

// processRunner and noneProber pin the mode a process probes.
type processRunner struct{ wf.EffectRunner }

func (processRunner) DetachMode() wf.DetachFact { return wf.DetachFact{Mode: wf.DetachProcess} }

type noneProber struct{ wf.EffectRunner }

func (noneProber) DetachMode() wf.DetachFact { return wf.DetachFact{Mode: wf.DetachNone} }

// Inline, the engine cuts a long command's streams the same way, with the full
// output beside the effect's other files: a failing suite whose failure
// prints at the end shows it in the receipt (artifact-identity C).
func TestInlineLongOutputKeepsTheTail(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sh", "-c", "head -c 100000 /dev/zero | tr '\\0' x; echo; echo 'FAIL: TestLast'")
	e := engineOver(t, stateDir, d, nil)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
		if st, _ := e.Status(run); st.Phase == wf.RunEnded {
			break
		}
	}
	res := kinds(t, stateDir, run, wf.JournalEffectResult)
	var data wf.EffectResultData
	var rr wf.RunResult
	if len(res) != 1 || json.Unmarshal(res[0].Data, &data) != nil || json.Unmarshal(data.Result, &rr) != nil {
		t.Fatalf("effect results %d", len(res))
	}
	if !strings.HasPrefix(rr.Stdout, "... <") || !strings.HasSuffix(rr.Stdout, "FAIL: TestLast\n") || len(rr.Stdout) > wf.OutputTail+64 {
		t.Fatalf("journaled stdout len %d, want the marked tail ending in the failure", len(rr.Stdout))
	}
	full, err := os.ReadFile(rr.Output)
	if err != nil || strings.Count(string(full), "x") != 100000 {
		t.Fatalf("full output %q (%v), want every byte", rr.Output, err)
	}
}
