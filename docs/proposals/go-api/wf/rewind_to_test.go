package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	rwFirst  struct{}
	rwCheck  struct{}
	rwDecide struct{}
	rwDone   struct{}
	rwStop   struct{}
)

// markerDef runs a command that succeeds only once marker exists: a
// failure whose cause lies outside the snapshotted world.
func markerDef(marker string) *wf.Def {
	d := wf.New("rewind-to")
	first := wf.NewCommand[rwFirst](d, "true")
	check := wf.NewCommand[rwCheck](d, "test", "-f", marker)
	done := wf.NewTerminal[rwDone](d, wf.Success)
	stop := wf.NewTerminal[rwStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	// The failure is journaled at the check; a later state decides the stop
	// from it, as a review deciding on a failed effect's record does.
	decide := wf.NewCompute[rwDecide, string](d, func(wf.Ctx) (string, error) { return "stop", nil })
	first.RouteExit(first.To(check, "go"), first.To(stop, "red"))
	check.RouteExit(check.To(done, "green"), check.To(decide, "red"))
	toStop := decide.To(stop, "stop")
	decide.Route(func(string, wf.Ctx) wf.Edge[rwDecide] { return toStop }, toStop)
	return d
}

type markerRun struct {
	ws, state, marker string
	run               wf.RunID
	cfg               wf.MainConfig
	recs              []wf.JournalRecord
}

// failedMarkerRun runs markerDef with no marker to its stop.
func failedMarkerRun(t *testing.T) markerRun {
	t.Helper()
	m := markerRun{ws: t.TempDir(), state: t.TempDir(), marker: filepath.Join(t.TempDir(), "fixed")}
	if err := os.WriteFile(filepath.Join(m.ws, "data"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{&snapshotRuntime{}, shell.Runtime(shell.Config{})} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	d := markerDef(m.marker)
	m.cfg = wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: d}}, Binary: "super"}
	e := wf.NewEngine(wf.FileJournal(m.state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: m.ws, StateRoot: m.state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(m.state, "runs", string(r)) }})
	var err error
	if m.run, err = e.Run(d, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Next(m.run, ""); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status(m.run)
	if err != nil || st.Terminal != "rwStop" {
		t.Fatalf("source = %+v %v, want stopped on the failed check", st, err)
	}
	if m.recs, err = wf.ReadJournal(m.state, m.run); err != nil {
		t.Fatal(err)
	}
	return m
}

// checkpointBefore is the last available checkpoint before rwCheck's first
// record of kind; other is the first record that is no checkpoint.
func (m markerRun) checkpointBefore(t *testing.T, kind wf.JournalKind) (cp, other int64) {
	t.Helper()
	for _, r := range m.recs {
		if r.Kind == kind && r.State == "rwCheck" {
			break
		}
		var w wf.WorldSnapshotData
		if r.Kind == wf.JournalWorldSnapshot && json.Unmarshal(r.Data, &w) == nil && w.Unavailable == "" {
			cp = r.Seq
		} else if other == 0 && r.Seq > 1 {
			other = r.Seq
		}
	}
	if cp == 0 {
		t.Fatalf("no checkpoint before rwCheck %s", kind)
	}
	return cp, other
}

// rewind runs dev rewind --to to into a new directory and drives the fork
// to its end, returning its terminal and the fork's records.
func (m markerRun) rewind(t *testing.T, to string) (string, []wf.JournalRecord) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fork")
	code, stdout, er := cliCall(m.cfg, "wf", "dev", "rewind", "--to", to, "--out", out, "--state-dir", m.state, "--workspace", m.ws, "--run-id", string(m.run))
	if code != 0 {
		t.Fatalf("rewind --to %s = %d %s %s", to, code, stdout, er)
	}
	reg := m.cfg.Adapters
	d := m.cfg.Defs[0].Def
	ws, state := filepath.Join(out, "workspace"), filepath.Join(out, "state")
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }})
	if _, err := e.Next(m.run, ""); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status(m.run)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := wf.ReadJournal(state, m.run)
	if err != nil {
		t.Fatal(err)
	}
	return st.Terminal, recs
}

// rewound is the fork's rewound record.
func rewound(t *testing.T, recs []wf.JournalRecord) wf.RewoundData {
	t.Helper()
	for _, r := range recs {
		if r.Kind == wf.JournalRewound {
			var d wf.RewoundData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatal("no rewound record")
	return wf.RewoundData{}
}

// A fork rewound to the checkpoint before a journaled failure re-runs the
// failing command in its world; rewind --to died re-decides from the
// journaled failure. The fork records the target and that the operator
// chose it; the source is untouched.
func TestRewindToACheckpointRerunsTheFailure(t *testing.T) {
	m := failedMarkerRun(t)
	cp, _ := m.checkpointBefore(t, wf.JournalEffectIntent)
	if err := os.WriteFile(m.marker, nil, 0o600); err != nil { // the defect is fixed
		t.Fatal(err)
	}
	if term, _ := m.rewind(t, "died"); term != "rwStop" {
		t.Fatalf("rewind --to died ended at %q: want the journaled failure re-decided", term)
	}
	term, recs := m.rewind(t, strconv.FormatInt(cp, 10))
	if term != "rwDone" {
		t.Fatalf("rewind --to %d ended at %q, want the check re-run green", cp, term)
	}
	rd := rewound(t, recs)
	if rd.Seq != cp || rd.To != strconv.FormatInt(cp, 10) || !rd.OperatorChosen {
		t.Fatalf("rewound = %+v, want the target %d recorded as operator-chosen", rd, cp)
	}
	after, err := wf.ReadJournal(m.state, m.run)
	if err != nil || len(after) != len(m.recs) {
		t.Fatalf("source journal changed: %d -> %d records (%v)", len(m.recs), len(after), err)
	}
}

// A target that is no available checkpoint is refused, naming the nearest
// checkpoints before and after it.
func TestRewindToRefusesATargetWithNoCheckpoint(t *testing.T) {
	m := failedMarkerRun(t)
	cp, other := m.checkpointBefore(t, wf.JournalEffectIntent)
	if other == 0 {
		t.Fatal("no non-checkpoint record to target")
	}
	out := filepath.Join(t.TempDir(), "fork")
	code, _, er := cliCall(m.cfg, "wf", "dev", "rewind", "--to", strconv.FormatInt(other, 10), "--out", out, "--state-dir", m.state, "--workspace", m.ws, "--run-id", string(m.run))
	if code == 0 || !strings.Contains(er, "not a checkpoint") || !strings.Contains(er, strconv.FormatInt(cp, 10)) {
		t.Fatalf("rewind --to %d = %d %s: want a refusal naming checkpoint %d", other, code, er, cp)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a refused rewind left a fork")
	}
	for _, bad := range []string{"0", "999999", "x"} {
		if code, _, er := cliCall(m.cfg, "wf", "dev", "rewind", "--to", bad, "--out", out, "--state-dir", m.state, "--workspace", m.ws, "--run-id", string(m.run)); code == 0 {
			t.Fatalf("rewind --to %s accepted: %s", bad, er)
		}
	}
}
