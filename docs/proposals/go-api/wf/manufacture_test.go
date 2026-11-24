package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// manufacturedRun starts a run of d marked manufactured with m.
func manufacturedRun(t *testing.T, d *wf.Def, reg *wf.Adapters, ws, state string, m *wf.ManufacturedData) (*wf.Engine, wf.RunID, string) {
	t.Helper()
	opts := wf.WithManufactured(wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunID: "made",
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}, m)
	e := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return e, run, state
}

// remark starts a fresh run whose mark is the script's: the engine guard
// admits only a run started for that script.
func remark(t *testing.T, s *wf.ManufactureScript) (*wf.Engine, wf.RunID, string) {
	t.Helper()
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	d := fanDef(s.Registration, chItems(1, 2, 3), 2, wf.AllSettled())
	s.Hash = d.Hash()
	return manufacturedRun(t, d, reg, ws, state, &wf.ManufacturedData{Edge: s.Edge, Until: s.Until, Hash: s.Hash, Script: wf.ScriptDigest(s)})
}

func script(t *testing.T, raw string) *wf.ManufactureScript {
	t.Helper()
	s, err := wf.ParseManufactureScript([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func manufacture(t *testing.T, e *wf.Engine, run wf.RunID, s *wf.ManufactureScript, apply bool) *wf.ManufactureReport {
	t.Helper()
	rep, err := e.Manufacture(run, s, wf.ManufactureOptions{ApplyTrigger: apply})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

const workOK = `{"state":"fan/c-item/chWork","edge":"fan/c-item/chWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}`

// The driver answers every offer on the way from the script's queue and
// stops at the target offer, which has a checkpoint a fanout can fork.
func TestManufactureReachesTargetOffer(t *testing.T) {
	s := script(t, `{"edge":"fan/c-item/chWork/edge[1]","registration":"fan","target_state":"fan/c-item/chWork","hash":"x",
		"target":{"instance":"chFan[3]"},"steps":[`+workOK+`],"trigger":{"op":"submit","output":{"OK":true}},"driver":"unit-result"}`)
	e, run, _ := remark(t, s)
	rep := manufacture(t, e, run, s, false)
	if !rep.Reached || rep.Target == nil || rep.Target.Instance != "chFan[3]" || rep.Target.Seq == 0 || rep.Target.SnapshotSeq != rep.Target.Seq-1 || rep.CoverageTier != "model" {
		b, _ := json.Marshal(rep)
		t.Fatalf("report %s", b)
	}
	if string(rep.Carried["driver"]) != `"unit-result"` {
		t.Fatalf("carried %v", rep.Carried)
	}
	plan, err := e.PlanFanout(run, "all")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tg := range plan.Targets {
		found = found || (tg.IssueSeq == rep.Target.Seq && tg.Refused == "")
	}
	if !found {
		t.Fatalf("the target offer %d is not forkable: %+v", rep.Target.Seq, plan)
	}
}

// With ApplyTrigger the target's own step is applied and the edge it took
// is checked against the script's edge.
func TestManufactureApplyTriggerChecksEdge(t *testing.T) {
	for _, tc := range []struct {
		edge string
		ok   bool
	}{{"fan/c-item/chWork/edge[1]", true}, {"fan/c-item/chWork/edge[2]", false}} {
		s := script(t, `{"edge":"`+tc.edge+`","registration":"fan","target_state":"fan/c-item/chWork","hash":"x",
			"target":{"instance":"chFan[3]"},"steps":[`+workOK+`],"trigger":{"op":"submit","output":{"OK":true}}}`)
		e, run, _ := remark(t, s)
		rep := manufacture(t, e, run, s, true)
		if !rep.TriggerApplied || rep.TriggerEdgeTaken != "fan/c-item/chWork/edge[1]" || rep.Reached != tc.ok {
			b, _ := json.Marshal(rep)
			t.Fatalf("%s: report %s", tc.edge, b)
		}
	}
}

// A state that takes another edge than its step's is the first divergence:
// the drive stops there, naming the expected and the taken edge.
func TestManufactureReportsDivergence(t *testing.T) {
	s := script(t, `{"edge":"fan/c-item/chWork/edge[1]","registration":"fan","target_state":"fan/c-item/chWork","hash":"x","target":{"instance":"chFan[3]"},
		"steps":[{"state":"fan/c-item/chWork","edge":"fan/c-item/chWork/edge[2]","step":{"op":"submit","output":{"OK":true}}}],"trigger":{"op":"submit","output":{"OK":true}}}`)
	e, run, _ := remark(t, s)
	rep := manufacture(t, e, run, s, false)
	if rep.Reached || rep.Divergence == nil || rep.Divergence.Expected != "fan/c-item/chWork/edge[2]" || rep.Divergence.Taken != "fan/c-item/chWork/edge[1]" || rep.Divergence.NeedsStub {
		b, _ := json.Marshal(rep)
		t.Fatalf("report %s", b)
	}
}

// A submit step whose output the engine refuses stops the drive and names
// the step and the refusal.
func TestManufactureReportsRefusedStep(t *testing.T) {
	s := script(t, `{"edge":"e","registration":"fan","target_state":"fan/c-item/chWork","hash":"x","target":{"instance":"chFan[3]"},
		"steps":[{"state":"fan/c-item/chWork","edge":null,"step":{"op":"submit","output":{"OK":"not a bool"}}}],"trigger":{"op":"submit","output":{"OK":true}}}`)
	e, run, _ := remark(t, s)
	rep := manufacture(t, e, run, s, false)
	if rep.Reached || rep.RefusedStep == nil || rep.RefusedStep.State != "fan/c-item/chWork" || len(rep.RefusedStep.Reasons) == 0 {
		b, _ := json.Marshal(rep)
		t.Fatalf("report %s", b)
	}
}

// A predecessor script drives its run to its end.
func TestManufacturePredecessorRunsToEnd(t *testing.T) {
	s := script(t, `{"until":"ended","registration":"fan","hash":"x","steps":[`+workOK+`]}`)
	e, run, _ := remark(t, s)
	rep := manufacture(t, e, run, s, false)
	if !rep.Reached || rep.Ended == nil || rep.Ended.Terminal == "" || rep.Divergence != nil {
		b, _ := json.Marshal(rep)
		t.Fatalf("report %s", b)
	}
}

// Outcomes go through the real verbs: a fail step with times answers that
// many successive offers of its state.
func TestManufactureFailOutcome(t *testing.T) {
	s := script(t, `{"until":"ended","registration":"fan","hash":"x","steps":[
		{"state":"fan/c-item/chWork","instance":"chFan[1]","edge":null,"step":{"op":"fail","class":"blocked","times":1}},
		`+workOK+`]}`)
	e, run, state := remark(t, s)
	rep := manufacture(t, e, run, s, false)
	if !rep.Reached || rep.Ended == nil {
		b, _ := json.Marshal(rep)
		t.Fatalf("report %s", b)
	}
	recs, _ := wf.ReadJournal(state, run)
	failed := 0
	for _, r := range recs {
		if r.Kind == wf.JournalRefused && strings.HasSuffix(r.Instance, "[1]") {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("item 1 failed %d times", failed)
	}
}

// manufactureCLI is a CLI config over fanDef "made" with the snapshot
// adapter, a fixture workspace that is a git-free directory, and a script
// file.
func manufactureCLI(t *testing.T) (wf.MainConfig, *wf.Def, string) {
	t.Helper()
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	d := fanDef("made", chItems(1, 2, 3), 2, wf.AllSettled())
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Adapters: reg}, d, ws
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func manufacturedStart(t *testing.T, state string, run wf.RunID) wf.RunStartedData {
	t.Helper()
	recs, err := wf.ReadJournal(state, run)
	if err != nil || len(recs) == 0 {
		t.Fatalf("journal of %s in %s: %v", run, state, err)
	}
	var s wf.RunStartedData
	if err := json.Unmarshal(recs[0].Data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// wf dev manufacture copies the fixture, starts a run marked manufactured
// and drives it to the target; the fixture is untouched, the report says
// where the target is, and a fork of it inherits the mark.
func TestManufactureCLIReachesTargetAndMarksForks(t *testing.T) {
	cfg, d, ws := manufactureCLI(t)
	p := writeScript(t, `{"edge":"made/c-item/chWork/edge[1]","registration":"made","target_state":"made/c-item/chWork","hash":"`+d.Hash()+`","run_id":"m",
		"target":{"instance":"chFan[3]"},"steps":[{"state":"made/c-item/chWork","edge":"made/c-item/chWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}],
		"trigger":{"op":"submit","output":{"OK":true}}}`)
	out := filepath.Join(t.TempDir(), "made")
	code, stdout, stderr := cliCall(cfg, "dev", "manufacture", "made", "--script", p, "--workspace", ws, "--out", out, "--state-dir", "state")
	if code != 0 {
		t.Fatalf("manufacture %d:\n%s\n%s", code, stdout, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "data")); string(b) != "fixture" {
		t.Fatal("the fixture was written")
	}
	if entries, _ := os.ReadDir(ws); len(entries) != 1 {
		t.Fatalf("the fixture gained entries: %v", entries)
	}
	var rep wf.ManufactureReport
	raw, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil || json.Unmarshal(raw, &rep) != nil || !rep.Reached || rep.Target == nil || rep.Target.SnapshotSeq == 0 {
		t.Fatalf("report %s %v", raw, err)
	}
	state := filepath.Join(out, "workspace", "state")
	if m := manufacturedStart(t, state, rep.Run).Manufactured; m == nil || m.Edge != "made/c-item/chWork/edge[1]" || m.Hash != d.Hash() {
		t.Fatalf("run_started mark %+v", m)
	}
	reg := cfg.Adapters
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: filepath.Join(out, "workspace"), StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	fork, err := e.ForkSnapshot(rep.Run, rep.Target.SnapshotSeq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	if m := manufacturedStart(t, fork.StateDir, rep.Run).Manufactured; m == nil || m.Edge != rep.Edge {
		t.Fatalf("the fork lost the mark: %+v", m)
	}
	if st, err := e.Status(rep.Run); err != nil || st.Manufactured == nil {
		t.Fatalf("status shows no mark: %+v %v", st.Manufactured, err)
	}
	code, stdout, stderr = cliCall(cfg, "dev", "fanout", string(rep.Run), "--state-dir", state, "--workspace", filepath.Join(out, "workspace"), "--out", filepath.Join(t.TempDir(), "b"),
		"--parallel", "1", "--coordinator-cmd", `["true"]`, "--dry-run", "--states", "all")
	if code != 0 || !strings.Contains(stdout, "Manufactured source run") {
		t.Fatalf("fanout dry run %d:\n%s\n%s", code, stdout, stderr)
	}
}

// A script written for another definition hash is refused before anything
// is copied.
func TestManufactureCLIRefusesStaleHash(t *testing.T) {
	cfg, _, ws := manufactureCLI(t)
	p := writeScript(t, `{"edge":"e","registration":"made","target_state":"made/c-item/chWork","hash":"sha256:stale","steps":[],"trigger":{"op":"submit","output":{}}}`)
	out := filepath.Join(t.TempDir(), "made")
	code, _, stderr := cliCall(cfg, "dev", "manufacture", "made", "--script", p, "--workspace", ws, "--out", out)
	if code != 2 || !strings.Contains(stderr, "sha256:stale") {
		t.Fatalf("%d %s", code, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a refused script left an --out directory")
	}
}

// A predecessor script runs its run to the end; the target script then
// starts the successor in the same copied world.
func TestManufactureCLIChainsAPredecessor(t *testing.T) {
	cfg, _, ws := manufactureCLI(t)
	d := fanDef("made", chItems(1, 2, 3), 2, wf.AllSettled())
	d.RunKey(d.Param("topic"))
	cfg.Defs = []wf.Registration{{Def: d}}
	work := `{"state":"made/c-item/chWork","edge":"made/c-item/chWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}`
	pre := writeScript(t, `{"until":"ended","registration":"made","hash":"`+d.Hash()+`","params":{"topic":"t"},"steps":[`+work+`]}`)
	target := writeScript(t, `{"edge":"made/c-item/chWork/edge[1]","registration":"made","target_state":"made/c-item/chWork","hash":"`+d.Hash()+`","params":{"topic":"t"},
		"steps":[],"trigger":{"op":"submit","output":{"OK":true}}}`)
	out := filepath.Join(t.TempDir(), "made")
	code, stdout, stderr := cliCall(cfg, "dev", "manufacture", "made", "--script", pre, "--script", target, "--workspace", ws, "--out", out, "--state-dir", "state")
	if code != 0 {
		t.Fatalf("manufacture %d:\n%s\n%s", code, stdout, stderr)
	}
	var rep wf.ManufactureReport
	raw, _ := os.ReadFile(filepath.Join(out, "report.json"))
	if json.Unmarshal(raw, &rep) != nil || !rep.Reached || len(rep.Predecessors) != 1 || rep.Predecessors[0].Ended == nil {
		t.Fatalf("report %s", raw)
	}
	state := filepath.Join(out, "workspace", "state")
	if s := manufacturedStart(t, state, rep.Run); s.Predecessor != rep.Predecessors[0].Run {
		t.Fatalf("run %s is not the successor of %s: %+v", rep.Run, rep.Predecessors[0].Run, s.Predecessor)
	}
}

// A start that lands on an existing run (a fixed run_id, or a live run under
// the run key) is refused: manufacture drives only a fresh run or a
// successor.
func TestManufactureCLIRefusesAnExistingRun(t *testing.T) {
	cfg, d, ws := manufactureCLI(t)
	work := `{"state":"made/c-item/chWork","edge":"made/c-item/chWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}`
	pre := writeScript(t, `{"until":"ended","registration":"made","hash":"`+d.Hash()+`","run_id":"m","steps":[`+work+`]}`)
	again := writeScript(t, `{"edge":"e","registration":"made","target_state":"made/c-item/chWork","hash":"`+d.Hash()+`","run_id":"m","steps":[],"trigger":{"op":"submit","output":{"OK":true}}}`)
	code, stdout, stderr := cliCall(cfg, "dev", "manufacture", "made", "--script", pre, "--script", again, "--workspace", ws, "--out", filepath.Join(t.TempDir(), "made"), "--state-dir", "state")
	if code != 2 || !strings.Contains(stderr, "existing run m") {
		t.Fatalf("%d\n%s\n%s", code, stdout, stderr)
	}
}

// dev restore writes a checkpoint's world into a new directory and refuses
// a seq that is no checkpoint, naming the nearest one after it.
func TestDevRestore(t *testing.T) {
	cfg, d, ws := manufactureCLI(t)
	p := writeScript(t, `{"edge":"made/c-item/chWork/edge[1]","registration":"made","target_state":"made/c-item/chWork","hash":"`+d.Hash()+`","run_id":"m",
		"target":{"instance":"chFan[3]"},"steps":[{"state":"made/c-item/chWork","edge":"made/c-item/chWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}],
		"trigger":{"op":"submit","output":{"OK":true}}}`)
	out := filepath.Join(t.TempDir(), "made")
	if code, stdout, stderr := cliCall(cfg, "dev", "manufacture", "made", "--script", p, "--workspace", ws, "--out", out, "--state-dir", "state"); code != 0 {
		t.Fatalf("%d %s %s", code, stdout, stderr)
	}
	var rep wf.ManufactureReport
	raw, _ := os.ReadFile(filepath.Join(out, "report.json"))
	_ = json.Unmarshal(raw, &rep)
	state := filepath.Join(out, "workspace", "state")
	code, _, stderr := cliCall(cfg, "dev", "restore", string(rep.Run), "--state-dir", state, "--seq", "1", "--out", filepath.Join(t.TempDir(), "r"))
	if code != 2 || !strings.Contains(stderr, "not a world_snapshot") || !strings.Contains(stderr, "the nearest checkpoint after it is") {
		t.Fatalf("%d %s", code, stderr)
	}
	dst := filepath.Join(t.TempDir(), "r")
	code, stdout, stderr := cliCall(cfg, "dev", "restore", string(rep.Run), "--state-dir", state, "--seq", fmt.Sprint(rep.Target.SnapshotSeq), "--out", dst)
	if code != 0 || !strings.Contains(stdout, filepath.Join(dst, "workspace")) {
		t.Fatalf("%d %s %s", code, stdout, stderr)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "workspace", "data")); err != nil || string(b) != "fixture" {
		t.Fatalf("restored world: %q %v", b, err)
	}
}

// markedRun starts a run of d marked for script s (its hash set to d's).
func markedRun(t *testing.T, d *wf.Def, s *wf.ManufactureScript) (*wf.Engine, wf.RunID, string) {
	t.Helper()
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	s.Hash = d.Hash()
	return manufacturedRun(t, d, reg, ws, state, &wf.ManufacturedData{Edge: s.Edge, Until: s.Until, Hash: s.Hash, Script: wf.ScriptDigest(s)})
}

// The engine admits a drive only on a fresh run manufactured for this very
// script (review round 1, P1 #1): an ordinary run, a run marked for another
// script, a run already bound to a driver, and a run another coordinator
// took units from are refused before anything is journaled.
func TestManufactureAdmitsOnlyAFreshRunOfItsScript(t *testing.T) {
	s := func() *wf.ManufactureScript {
		return script(t, `{"until":"ended","registration":"fan","hash":"x","steps":[`+workOK+`]}`)
	}
	drive := func(e *wf.Engine, run wf.RunID, sc *wf.ManufactureScript) error {
		_, err := e.Manufacture(run, sc, wf.ManufactureOptions{})
		return err
	}
	refused := func(t *testing.T, e *wf.Engine, run wf.RunID, state string, sc *wf.ManufactureScript, why string) {
		t.Helper()
		before, _ := wf.ReadJournal(state, run)
		err := drive(e, run, sc)
		if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), why) {
			t.Fatalf("drive: %v, want refused with %q", err, why)
		}
		if after, _ := wf.ReadJournal(state, run); len(after) != len(before) {
			t.Fatalf("a refused drive journaled %d records", len(after)-len(before))
		}
	}
	t.Run("ordinary run", func(t *testing.T) {
		ws, state := t.TempDir(), t.TempDir()
		reg := wf.NewAdapters()
		d := fanDef("fan", chItems(1, 2), 2, wf.AllSettled())
		e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, RunID: "plain"}).(*wf.Engine)
		run, err := e.Run(d, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		sc := s()
		sc.Hash = d.Hash()
		refused(t, e, run, state, sc, "not a manufactured run")
	})
	t.Run("another script's run", func(t *testing.T) {
		e, run, state := remark(t, s())
		other := script(t, `{"edge":"fan/c-item/chWork/edge[1]","registration":"fan","target_state":"fan/c-item/chWork","hash":"x","steps":[],"trigger":{"op":"submit","output":{"OK":true}}}`)
		other.Hash = manufacturedStart(t, state, run).Manufactured.Hash
		refused(t, e, run, state, other, "started for another script")
	})
	t.Run("same edge, other steps", func(t *testing.T) {
		e, run, state := remark(t, s())
		other := s()
		other.Steps = nil
		other.Hash = manufacturedStart(t, state, run).Manufactured.Hash
		refused(t, e, run, state, other, "started for another script")
	})
	t.Run("already bound", func(t *testing.T) {
		sc := s()
		e, run, state := remark(t, sc)
		if err := drive(e, run, sc); err != nil {
			t.Fatal(err)
		}
		refused(t, e, run, state, sc, "not fresh")
	})
	t.Run("driven by another coordinator", func(t *testing.T) {
		sc := s()
		e, run, state := remark(t, sc)
		if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "someone"}); err != nil {
			t.Fatal(err)
		}
		refused(t, e, run, state, sc, "not fresh")
	})
}

// A requested trigger counts only when it ran on the exact target and took
// the script's edge (review round 1, P1 #3): a gate target is answered at
// its own instance and visit, and an agent target with no trigger fails.
func TestManufactureTriggerFailsClosed(t *testing.T) {
	gate := func(edge string) *wf.ManufactureReport {
		d := hoGateFan(2)
		sc := script(t, `{"edge":"`+edge+`","registration":"ho-gate-fan","target_state":"ho-gate-fan/g-item/hoCheck","hash":"x","target":{"instance":"chFan[2]"},
			"steps":[{"state":"ho-gate-fan/g-item/hoWork","edge":null,"step":{"op":"submit","output":{"OK":true}}},
			         {"state":"ho-gate-fan/g-item/hoCheck","edge":null,"step":{"op":"answer","value":"yes"}}],
			"trigger":{"op":"answer","value":"no"}}`)
		e, run, state := markedRun(t, d, sc)
		rep := manufacture(t, e, run, sc, true)
		recs, _ := wf.ReadJournal(state, run)
		answered := map[string]string{}
		for _, r := range recs {
			if r.Kind == wf.JournalGateAnswered {
				var a struct{ Answer, Value string }
				_ = json.Unmarshal(r.Data, &a)
				answered[r.Instance] = string(r.Data)
			}
		}
		if !strings.Contains(answered["chFan[2]"], `"no"`) {
			b, _ := json.Marshal(rep)
			kinds := []string{}
			for _, r := range recs {
				kinds = append(kinds, r.Kind.String()+"@"+r.Instance)
			}
			t.Fatalf("the target gate was not answered with the trigger: %v\n%s\n%v", answered, b, kinds)
		}
		return rep
	}
	if rep := gate("ho-gate-fan/g-item/hoCheck/edge[1]"); !rep.Reached || !rep.TriggerApplied || rep.TriggerEdgeTaken != "ho-gate-fan/g-item/hoCheck/edge[1]" {
		b, _ := json.Marshal(rep)
		t.Fatalf("gate trigger: %s", b)
	}
	if rep := gate("ho-gate-fan/g-item/hoCheck/edge[0]"); rep.Reached {
		b, _ := json.Marshal(rep)
		t.Fatalf("a gate trigger that took another edge counted: %s", b)
	}
	sc := script(t, `{"edge":"fan/c-item/chWork/edge[1]","registration":"fan","target_state":"fan/c-item/chWork","hash":"x","target":{"instance":"chFan[3]"},"steps":[`+workOK+`]}`)
	e, run, _ := remark(t, sc)
	if rep := manufacture(t, e, run, sc, true); rep.Reached || rep.Stuck == nil || rep.Stuck.Kind != "trigger" {
		b, _ := json.Marshal(rep)
		t.Fatalf("--apply-trigger with no trigger counted: %s", b)
	}
}

type (
	rtWork struct{}
	rtOK   struct{}
	rtBad  struct{}
)

// retryFan is fanDef whose child agent has three attempts.
func retryFan() *wf.Def {
	c := wf.New("r-item")
	w := wf.NewAgent[rtWork, chOut](c)
	ok := wf.NewTerminal[rtOK](c, wf.Success)
	bad := wf.NewTerminal[rtBad](c, wf.Stopped)
	c.OnFailure(wf.Blocked, bad)
	w.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[rtWork](wf.Const(3), w.To(bad, "gave up")))
	toOK, toBad := w.To(ok, "ok"), w.To(bad, "bad")
	w.Route(func(o chOut, _ wf.Ctx) wf.Edge[rtWork] {
		if o.OK {
			return toOK
		}
		return toBad
	}, toOK, toBad)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	d := wf.New("rt")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, wf.Typed[chItem, chRes](c))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// A transition is checked against the step its visit actually consumed
// (review round 1, P2 #4): an instance whose first offer failed and whose
// re-offer submitted is checked against the submit's edge, not the first
// instance step's.
func TestManufactureChecksTheConsumedStep(t *testing.T) {
	sc := script(t, `{"until":"ended","registration":"rt","hash":"x","steps":[
		{"state":"rt/r-item/rtWork","instance":"chFan[1]","edge":"rt/r-item/rtWork/edge[0]","step":{"op":"fail","class":"transient","times":1}},
		{"state":"rt/r-item/rtWork","instance":"chFan[1]","edge":"rt/r-item/rtWork/edge[1]","step":{"op":"submit","output":{"OK":true}}},
		{"state":"rt/r-item/rtWork","edge":"rt/r-item/rtWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}]}`)
	e, run, _ := markedRun(t, retryFan(), sc)
	if rep := manufacture(t, e, run, sc, false); !rep.Reached || rep.Divergence != nil || len(rep.Unverified) != 0 {
		b, _ := json.Marshal(rep)
		t.Fatalf("report %s", b)
	}
}

// The copy is confined (review round 1, P1 #2): the state root must resolve
// inside it, a link out of the fixture refuses it, and links and local
// remotes into the fixture are moved onto the copy; one outside refuses it.
func TestManufactureCLIConfinesTheCopy(t *testing.T) {
	cfg, d, _ := manufactureCLI(t)
	p := writeScript(t, `{"until":"ended","registration":"made","hash":"`+d.Hash()+`","run_id":"m","steps":[{"state":"made/c-item/chWork","edge":"made/c-item/chWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}]}`)
	fixture := func(t *testing.T) string {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		return ws
	}
	run := func(ws string, extra ...string) (int, string, string, string) {
		out := filepath.Join(t.TempDir(), "made")
		code, stdout, stderr := cliCall(cfg, append([]string{"dev", "manufacture", "made", "--script", p, "--workspace", ws, "--out", out}, extra...)...)
		return code, stdout, stderr, out
	}
	for _, sd := range []string{"../outside", "/abs/state"} {
		if code, _, stderr, _ := run(fixture(t), "--state-dir", sd); code != 2 || !strings.Contains(stderr, "--state-dir") {
			t.Fatalf("--state-dir %s: %d %s", sd, code, stderr)
		}
	}
	ws := fixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(ws, "escape")); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr, out := run(ws, "--state-dir", "state"); code != 2 || !strings.Contains(stderr, "outside the copy") {
		t.Fatalf("a link out of the fixture: %d %s", code, stderr)
	} else if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a refused copy was left behind")
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	ws = fixture(t)
	git(ws, "init", "-q")
	git(ws, "init", "-q", "--bare", filepath.Join(ws, "origin.git"))
	git(ws, "remote", "add", "origin", filepath.Join(ws, "origin.git"))
	if err := os.Symlink(filepath.Join(ws, "data"), filepath.Join(ws, "inner")); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr, out := run(ws, "--state-dir", "state")
	if code != 0 {
		t.Fatalf("a confinable git fixture: %d %s %s", code, stdout, stderr)
	}
	copyWS := filepath.Join(out, "workspace")
	if url, _ := exec.Command("git", "-C", copyWS, "remote", "get-url", "origin").Output(); strings.TrimSpace(string(url)) != filepath.Join(copyWS, "origin.git") {
		t.Fatalf("the copy's origin is %s", url)
	}
	if target, _ := os.Readlink(filepath.Join(copyWS, "inner")); target != filepath.Join(copyWS, "data") {
		t.Fatalf("the copy's link leads to %s", target)
	}
	ws = fixture(t)
	git(ws, "init", "-q")
	git(ws, "remote", "add", "origin", t.TempDir())
	if code, _, stderr, _ := run(ws, "--state-dir", "state"); code != 2 || !strings.Contains(stderr, "outside the fixture") {
		t.Fatalf("a local remote outside the fixture: %d %s", code, stderr)
	}
}

// World actions run only through a boundary the caller names, or on the
// host when the caller says so, which the report records (review round 1,
// P1 #2).
func TestManufactureCLIWorldActionsNeedABoundary(t *testing.T) {
	cfg, d, ws := manufactureCLI(t)
	p := writeScript(t, `{"until":"ended","registration":"made","hash":"`+d.Hash()+`","run_id":"m","steps":[{"state":"made/c-item/chWork","edge":"made/c-item/chWork/edge[1]",
		"step":{"op":"submit","output":{"OK":true},"do":[{"argv":["sh","-c","echo acted >> acted.txt"],"dir":"{{workdir}}"}]}}]}`)
	call := func(extra ...string) (int, string, string) {
		out := filepath.Join(t.TempDir(), "made")
		code, _, stderr := cliCall(cfg, append([]string{"dev", "manufacture", "made", "--script", p, "--workspace", ws, "--out", out, "--state-dir", "state"}, extra...)...)
		return code, stderr, out
	}
	if code, stderr, out := call(); code != 2 || !strings.Contains(stderr, "--do-sandbox") {
		t.Fatalf("no boundary: %d %s", code, stderr)
	} else if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a refused script copied the fixture")
	}
	report := func(out string) wf.ManufactureReport {
		var rep wf.ManufactureReport
		raw, _ := os.ReadFile(filepath.Join(out, "report.json"))
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	code, stderr, out := call("--do-sandbox", `["sh","-c","echo wrapped >> {dir}/wrapped.txt && exec \"$@\"","wrap"]`)
	if code != 0 {
		raw, _ := os.ReadFile(filepath.Join(out, "report.json"))
		t.Fatalf("sandboxed: %d %s %s", code, stderr, raw)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "workspace", "wrapped.txt")); strings.Count(string(b), "wrapped") != 3 {
		t.Fatalf("the actions did not all run under the wrapper: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "workspace", "acted.txt")); strings.Count(string(b), "acted") != 3 {
		t.Fatalf("the actions did not run: %q", b)
	}
	if rep := report(out); rep.DoOnHost {
		t.Fatal("a sandboxed drive reported on-host actions")
	}
	code, stderr, out = call("--do-on-host")
	if code != 0 || !report(out).DoOnHost {
		t.Fatalf("on host: %d %s", code, stderr)
	}
}

// An instance's fail step with times answers that many of the instance's
// offers, each a real failure (review round 2, reproduced): two transient
// failures, then the ordinary submit.
func TestManufactureInstanceFailTimes(t *testing.T) {
	sc := script(t, `{"until":"ended","registration":"rt","hash":"x","steps":[
		{"state":"rt/r-item/rtWork","instance":"chFan[1]","edge":null,"step":{"op":"fail","class":"transient","times":2}},
		{"state":"rt/r-item/rtWork","edge":"rt/r-item/rtWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}]}`)
	e, run, state := markedRun(t, retryFan(), sc)
	rep := manufacture(t, e, run, sc, false)
	recs, _ := wf.ReadJournal(state, run)
	failed := 0
	for _, r := range recs {
		if r.Kind == wf.JournalRefused && r.Instance == "chFan[1]" {
			failed++
		}
	}
	if !rep.Reached || rep.Divergence != nil || failed != 2 {
		b, _ := json.Marshal(rep)
		t.Fatalf("item 1 failed %d times; report %s", failed, b)
	}
}

// A manufactured run's segment is bound to the engine-internal scripted
// harness at admission (review round 2, P1 1): a coordinator of any other
// harness is refused inside the hand-out's own lock.
func TestManufacturedRunRefusesOtherCoordinators(t *testing.T) {
	sc := script(t, `{"edge":"fan/c-item/chWork/edge[1]","registration":"fan","target_state":"fan/c-item/chWork","hash":"x","target":{"instance":"chFan[3]"},"steps":[`+workOK+`],"trigger":{"op":"submit","output":{"OK":true}}}`)
	e, run, state := remark(t, sc)
	// Right after admission, before the driver's first hand-out, the
	// segment is already the driver's.
	var early error
	defer wf.SetManufactureAdmittedHook(func(r wf.RunID) {
		_, early = e.HandOutAs(r, 1, wf.Coordinator{Harness: "codex", Session: "early"})
	})()
	if rep := manufacture(t, e, run, sc, false); !rep.Reached {
		b, _ := json.Marshal(rep)
		t.Fatalf("not reached: %s", b)
	}
	if !errors.Is(early, wf.ErrRejected) {
		t.Fatalf("a coordinator right after admission: %v", early)
	}
	before, _ := wf.ReadJournal(state, run)
	for _, h := range []string{"codex", "claude", "generic"} {
		if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: h, Session: "outsider-" + h}); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("%s coordinator on a manufactured run: %v", h, err)
		}
	}
	if after, _ := wf.ReadJournal(state, run); len(after) != len(before) {
		t.Fatalf("refused coordinators journaled %d records", len(after)-len(before))
	}
}

// No caller can name the scripted harness (review round 2, P1 1).
func TestScriptedHarnessCannotBeNamed(t *testing.T) {
	cfg, _, ws := manufactureCLI(t)
	state := t.TempDir()
	if code, _, stderr := cliCall(cfg, "run", "made", "--run-id", "x", "--workspace", ws, "--state-dir", state, "--harness", "Scripted"); code != 2 || !strings.Contains(stderr, "internal to wf dev manufacture") {
		t.Fatalf("run --harness scripted: %d %s", code, stderr)
	}
	if code, _, stderr := cliCall(cfg, "dev", "started", "x", "u", "--state-dir", state, "--harness", "scripted"); code != 2 || !strings.Contains(stderr, "internal to wf dev manufacture") {
		t.Fatalf("dev started --harness scripted: %d %s", code, stderr)
	}
}

// The copy's git runs nothing the fixture controls (review round 2, P1 2):
// hooks are off, local settings that name a program, filter or file are
// dropped and reported, includes and per-worktree config refuse the copy,
// and relative local remotes resolve inside the copy or refuse it.
func TestManufactureCLIIsolatesTheCopysGit(t *testing.T) {
	cfg, d, _ := manufactureCLI(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	p := writeScript(t, `{"until":"ended","registration":"made","hash":"`+d.Hash()+`","run_id":"m","steps":[{"state":"made/c-item/chWork","edge":"made/c-item/chWork/edge[1]",
		"step":{"op":"submit","output":{"OK":true},"do":[{"argv":["git","commit","-q","--allow-empty","-m","in the copy"],"dir":"{{workdir}}"}]}}]}`)
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	fixture := func(t *testing.T) string {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		git(ws, "init", "-q")
		git(ws, "config", "user.name", "f")
		git(ws, "config", "user.email", "f@f")
		return ws
	}
	run := func(ws string) (int, string, string, string) {
		out := filepath.Join(t.TempDir(), "made")
		code, stdout, stderr := cliCall(cfg, "dev", "manufacture", "made", "--script", p, "--workspace", ws, "--out", out, "--state-dir", "state", "--do-on-host")
		return code, stdout, stderr, out
	}
	ws := fixture(t)
	hooks := filepath.Join(ws, ".hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(ws, "config", "core.hooksPath", hooks)
	git(ws, "config", "filter.x.clean", "touch "+marker)
	git(ws, "config", "core.fsmonitor", "touch "+marker)
	git(ws, "config", "commit.gpgsign", "true")
	git(ws, "config", "beads.role", "maintainer")
	git(ws, "init", "-q", "--bare", filepath.Join(ws, "inner.git"))
	git(ws, "remote", "add", "inner", "./inner.git")
	code, stdout, stderr, out := run(ws)
	if code != 0 {
		t.Fatalf("an isolable git fixture: %d %s %s", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a fixture hook, filter or fsmonitor ran")
	}
	copyWS := filepath.Join(out, "workspace")
	get := func(key string) string {
		b, _ := exec.Command("git", "-C", copyWS, "config", "--local", "--get", key).Output()
		return strings.TrimSpace(string(b))
	}
	if get("core.hookspath") != "/dev/null" || get("filter.x.clean") != "" || get("core.fsmonitor") != "" || get("commit.gpgsign") != "" || get("beads.role") != "maintainer" {
		t.Fatalf("the copy's git config: hooks %q filter %q fsmonitor %q gpgsign %q beads %q", get("core.hookspath"), get("filter.x.clean"), get("core.fsmonitor"), get("commit.gpgsign"), get("beads.role"))
	}
	if get("remote.inner.url") != filepath.Join(copyWS, "inner.git") {
		t.Fatalf("the relative remote became %q", get("remote.inner.url"))
	}
	var rep wf.ManufactureReport
	raw, _ := os.ReadFile(filepath.Join(out, "report.json"))
	_ = json.Unmarshal(raw, &rep)
	sort.Strings(rep.GitConfigDropped)
	if strings.Join(rep.GitConfigDropped, ",") != "commit.gpgsign,core.fsmonitor,core.hookspath,filter.x.clean" {
		t.Fatalf("dropped %v", rep.GitConfigDropped)
	}
	for name, setup := range map[string]func(string){
		"include":         func(ws string) { git(ws, "config", "include.path", filepath.Join(t.TempDir(), "x")) },
		"relative remote": func(ws string) { git(ws, "remote", "add", "out", "../elsewhere.git") },
		"worktree config": func(ws string) { git(ws, "config", "extensions.worktreeConfig", "true") },
	} {
		ws := fixture(t)
		setup(ws)
		if code, _, stderr, _ := run(ws); code != 2 || !strings.Contains(stderr, "cannot be confined") {
			t.Fatalf("%s: %d %s", name, code, stderr)
		}
	}
}

// Every local repository a remote reaches is isolated like the primary
// (final review, P1): a push to an in-copy remote whose own config names a
// hook runs no fixture hook, and that repository's dropped settings are
// reported. A mixed-case remote name keeps its case when its URL moves
// onto the copy.
func TestManufactureCLIIsolatesEveryReachableRepository(t *testing.T) {
	cfg, d, _ := manufactureCLI(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	p := writeScript(t, `{"until":"ended","registration":"made","hash":"`+d.Hash()+`","run_id":"m","steps":[{"state":"made/c-item/chWork","edge":"made/c-item/chWork/edge[1]",
		"step":{"op":"submit","output":{"OK":true},"do":[{"argv":["sh","-c","git commit -q --allow-empty -m x && git push -q Inner HEAD:refs/heads/pushed-$$"],"dir":"{{workdir}}"}]}}]}`)
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	git(ws, "init", "-q")
	git(ws, "config", "user.name", "f")
	git(ws, "config", "user.email", "f@f")
	inner := filepath.Join(ws, "inner.git")
	git(ws, "init", "-q", "--bare", inner)
	hooks := filepath.Join(ws, ".receiving-hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "post-receive"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "hooks", "pre-receive"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(inner, "config", "core.hooksPath", hooks)
	git(ws, "remote", "add", "Inner", inner)
	out := filepath.Join(t.TempDir(), "made")
	code, stdout, stderr := cliCall(cfg, "dev", "manufacture", "made", "--script", p, "--workspace", ws, "--out", out, "--state-dir", "state", "--do-on-host")
	if code != 0 {
		t.Fatalf("%d %s %s", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the receiving repository's hook ran")
	}
	copyWS := filepath.Join(out, "workspace")
	if url, _ := exec.Command("git", "-C", copyWS, "config", "--get", "remote.Inner.url").Output(); strings.TrimSpace(string(url)) != filepath.Join(copyWS, "inner.git") {
		t.Fatalf("the mixed-case remote's URL is %q", url)
	}
	if hp, _ := exec.Command("git", "--git-dir", filepath.Join(copyWS, "inner.git"), "config", "--get", "core.hooksPath").Output(); strings.TrimSpace(string(hp)) != "/dev/null" {
		t.Fatalf("the in-copy remote's hooks path is %q", hp)
	}
	var rep wf.ManufactureReport
	raw, _ := os.ReadFile(filepath.Join(out, "report.json"))
	_ = json.Unmarshal(raw, &rep)
	if !slices.Contains(rep.GitConfigDropped, "inner.git:core.hookspath") {
		t.Fatalf("dropped %v", rep.GitConfigDropped)
	}
	// A remote that is no repository the copy can isolate refuses it.
	ws2 := t.TempDir()
	git(ws2, "init", "-q")
	if err := os.MkdirAll(filepath.Join(ws2, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(ws2, "remote", "add", "plain", filepath.Join(ws2, "plain"))
	if code, _, stderr := cliCall(cfg, "dev", "manufacture", "made", "--script", p, "--workspace", ws2, "--out", filepath.Join(t.TempDir(), "m2"), "--state-dir", "state", "--do-on-host"); code != 2 || !strings.Contains(stderr, "cannot be confined") {
		t.Fatalf("an unisolable remote: %d %s", code, stderr)
	}
}

// An instance-glob step's repetitions count per matched instance: each
// child it matches fails that many times.
func TestManufactureInstanceGlobTimesPerInstance(t *testing.T) {
	sc := script(t, `{"until":"ended","registration":"rt","hash":"x","steps":[
		{"state":"rt/r-item/rtWork","instance":"chFan[*]","edge":null,"step":{"op":"fail","class":"transient","times":2}},
		{"state":"rt/r-item/rtWork","edge":"rt/r-item/rtWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}]}`)
	e, run, state := markedRun(t, retryFan(), sc)
	rep := manufacture(t, e, run, sc, false)
	recs, _ := wf.ReadJournal(state, run)
	failed := map[string]int{}
	for _, r := range recs {
		if r.Kind == wf.JournalRefused {
			failed[r.Instance]++
		}
	}
	if !rep.Reached || failed["chFan[1]"] != 2 || failed["chFan[2]"] != 2 {
		b, _ := json.Marshal(rep)
		t.Fatalf("failed %v; report %s", failed, b)
	}
}

// A repository whose common directory is elsewhere (a commondir file) takes
// its effective config and hooks from there; it cannot be isolated by its
// own config, so it refuses the copy (final review, commondir).
func TestManufactureCLIRefusesASplitRepository(t *testing.T) {
	cfg, d, _ := manufactureCLI(t)
	p := writeScript(t, `{"until":"ended","registration":"made","hash":"`+d.Hash()+`","run_id":"m","steps":[{"state":"made/c-item/chWork","edge":"made/c-item/chWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}]}`)
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	for _, where := range []string{"remote", "primary"} {
		ws := t.TempDir()
		git(ws, "init", "-q")
		git(ws, "init", "-q", "--bare", filepath.Join(ws, "shared.git"))
		split := filepath.Join(ws, ".git")
		if where == "remote" {
			split = filepath.Join(ws, "split.git")
			git(ws, "init", "-q", "--bare", split)
			git(ws, "remote", "add", "split", split)
		}
		if err := os.WriteFile(filepath.Join(split, "commondir"), []byte(filepath.Join(ws, "shared.git")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := cliCall(cfg, "dev", "manufacture", "made", "--script", p, "--workspace", ws, "--out", filepath.Join(t.TempDir(), "m"), "--state-dir", "state")
		if code != 2 || !strings.Contains(stderr, "commondir") {
			t.Fatalf("%s with a commondir: %d %s", where, code, stderr)
		}
	}
}

// A retry-exhausted failure edge: the trigger fails the target's attempts
// until the engine offers recovery, and its "then" step answers that offer
// (give-up), so the target visit's transition takes the failure edge.
// Without "then" the drive is stuck at the recovery offer, and a "then" on
// a queued step is refused, since only the trigger's is applied.
func TestManufactureTriggerThenRecovers(t *testing.T) {
	body := func(then string) string {
		return `{"edge":"fan/c-item/chWork/edge[0]","registration":"fan","target_state":"fan/c-item/chWork","hash":"x",
		"target":{"instance":"chFan[3]"},"steps":[` + workOK + `],"trigger":{"op":"fail","class":"transient","times":1` + then + `}}`
	}
	s := script(t, body(`,"then":{"op":"recover","value":"give-up"}`))
	e, run, _ := remark(t, s)
	rep := manufacture(t, e, run, s, true)
	if !rep.Reached || rep.TriggerEdgeTaken != "fan/c-item/chWork/edge[0]" || rep.Stuck != nil {
		b, _ := json.Marshal(rep)
		t.Fatalf("report %s", b)
	}

	s = script(t, body(""))
	e, run, _ = remark(t, s)
	rep = manufacture(t, e, run, s, true)
	if rep.Reached || rep.Stuck == nil || rep.Stuck.Kind != string(wf.InstructionRecovery) {
		b, _ := json.Marshal(rep)
		t.Fatalf("no then: report %s", b)
	}

	for _, bad := range []string{
		`{"edge":"e","registration":"fan","target_state":"s","hash":"x","steps":[{"state":"s","edge":"e","step":{"op":"fail","class":"transient","times":1,"then":{"op":"recover","value":"give-up"}}}]}`,
		body(`,"then":{"op":"submit","output":{"OK":true}}`),
	} {
		if _, err := wf.ParseManufactureScript([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// The target is pinned once: with the default target (the first offer of
// the target state once its steps are used) and two parallel offers of it,
// the first is the target, and its sibling neither takes the trigger nor
// rearms it; a sibling with no step of its own waits while the target is
// pending. Only the target's attempt fails, and its recovery is answered.
func TestManufactureTriggerIsPinnedToOneTarget(t *testing.T) {
	s := script(t, `{"edge":"fan/c-item/chWork/edge[0]","registration":"fan","target_state":"fan/c-item/chWork","hash":"x","steps":[],
		"trigger":{"op":"fail","class":"transient","times":1,"then":{"op":"recover","value":"give-up"}}}`)
	e, run, state := remark(t, s)
	rep := manufacture(t, e, run, s, true)
	if !rep.Reached || rep.TriggerEdgeTaken != "fan/c-item/chWork/edge[0]" || rep.Target == nil || rep.Stuck != nil {
		b, _ := json.Marshal(rep)
		t.Fatalf("report %s", b)
	}
	recs, _ := wf.ReadJournal(state, run)
	failed := map[string]int{}
	for _, r := range recs {
		if r.Kind == wf.JournalRefused {
			failed[r.Instance]++
		}
	}
	if len(failed) != 1 || failed[rep.Target.Instance] != 1 {
		t.Fatalf("failed attempts by instance %v, want only the target %s's", failed, rep.Target.Instance)
	}
}

// The driver's coordinator operations (recover, gate answers) carry its
// admitted scripted coordinator, never the invoking process's identity:
// a manufacture run from inside a named Claude session reaches a give-up
// edge and a gate's edge.
func TestManufactureCLIUnderANamedSession(t *testing.T) {
	cfg, d, ws := manufactureCLI(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent-session")
	p := writeScript(t, `{"edge":"made/c-item/chWork/edge[0]","registration":"made","target_state":"made/c-item/chWork","hash":"`+d.Hash()+`","run_id":"m",
		"target":{"instance":"chFan[3]"},"steps":[{"state":"made/c-item/chWork","edge":"made/c-item/chWork/edge[1]","step":{"op":"submit","output":{"OK":true}}}],
		"trigger":{"op":"fail","class":"transient","times":1,"then":{"op":"recover","value":"give-up"}}}`)
	out := filepath.Join(t.TempDir(), "made")
	if code, stdout, stderr := cliCall(cfg, "dev", "manufacture", "made", "--script", p, "--workspace", ws, "--out", out, "--state-dir", "state", "--apply-trigger"); code != 0 {
		t.Fatalf("recover under a named session: %d\n%s\n%s", code, stdout, stderr)
	}

	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	gd := policyDef(wf.PolicyAnswerable)
	gcfg := wf.MainConfig{Defs: []wf.Registration{{Def: gd}}, Adapters: reg}
	p = writeScript(t, `{"edge":"policy/pAsk/edge[0]","registration":"policy","target_state":"policy/pAsk","hash":"`+gd.Hash()+`","run_id":"g",
		"steps":[],"trigger":{"op":"answer","value":"go"}}`)
	out = filepath.Join(t.TempDir(), "gate")
	if code, stdout, stderr := cliCall(gcfg, "dev", "manufacture", "policy", "--script", p, "--workspace", ws, "--out", out, "--state-dir", "state", "--apply-trigger"); code != 0 {
		t.Fatalf("gate answer under a named session: %d\n%s\n%s", code, stdout, stderr)
	}
}

// then is a fail trigger's only: on any other trigger it is refused.
func TestManufactureThenOnlyOnAFailTrigger(t *testing.T) {
	raw := `{"edge":"e","registration":"fan","target_state":"s","hash":"x","steps":[],"trigger":{"op":"submit","output":{},"then":{"op":"recover","value":"give-up"}}}`
	if _, err := wf.ParseManufactureScript([]byte(raw)); err == nil {
		t.Fatal("then accepted on a submit trigger")
	}
}

type (
	bvReview struct{}
	bvRepair struct{}
	bvDone   struct{}
	bvStop   struct{}
)

// budgetDef loops Review (issues) and Repair; Repair's visit budget is 2,
// so Review's third route to Repair is diverted by Repair's budget edge.
func budgetDef() *wf.Def {
	d := wf.New("bud")
	review := wf.NewAgent[bvReview, string](d)
	repair := wf.NewAgent[bvRepair, string](d)
	done := wf.NewTerminal[bvDone](d, wf.Success)
	stop := wf.NewTerminal[bvStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	review.Spec(wf.PromptText("review"), wf.Retry[bvReview](wf.Const(1), review.To(stop, "gave up")))
	toRepair, toDone := review.To(repair, "issues"), review.To(done, "clean")
	review.Route(func(o string, _ wf.Ctx) wf.Edge[bvReview] {
		if o == "issues" {
			return toRepair
		}
		return toDone
	}, toRepair, toDone)
	repair.Spec(wf.PromptText("repair"), wf.Retry[bvRepair](wf.Const(1), repair.To(stop, "gave up")))
	back := repair.To(review, "repaired")
	repair.Route(func(string, wf.Ctx) wf.Edge[bvRepair] { return back }, back)
	wf.MaxVisits(wf.Const(2), repair.To(stop, "issues not fixed"))
	return d
}

// A visit-budget edge is taken where no transition starts: Review's third
// route to Repair is replaced, in the same edge plan, by Repair's budget
// edge. The arrival is read from that later plan step: the target is
// reached at Repair's third visit, and its edge is the budget edge.
func TestManufactureBudgetEdgeArrivesFromALaterPlanStep(t *testing.T) {
	for _, apply := range []bool{false, true} {
		s := script(t, `{"edge":"bud/bvRepair/edge[2]","registration":"bud","target_state":"bud/bvRepair","hash":"x","target":{"visit":3},"steps":[
			{"state":"bud/bvReview","edge":"bud/bvReview/edge[1]","step":{"op":"submit","output":"issues"}},
			{"state":"bud/bvRepair","edge":"bud/bvRepair/edge[1]","step":{"op":"submit","output":"fixed"}}],
			"trigger":{"op":"visit"}}`)
		ws, state := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		reg := wf.NewAdapters()
		if err := reg.Register(&snapshotRuntime{}); err != nil {
			t.Fatal(err)
		}
		d := budgetDef()
		s.Hash = d.Hash()
		e, run, _ := manufacturedRun(t, d, reg, ws, state, &wf.ManufacturedData{Edge: s.Edge, Hash: s.Hash, Script: wf.ScriptDigest(s)})
		rep := manufacture(t, e, run, s, apply)
		ok := rep.Reached && rep.CoverageTier == "engine" && rep.Target != nil && rep.Divergence == nil && rep.Stuck == nil
		if apply {
			ok = ok && rep.TriggerApplied && rep.TriggerEdgeTaken == "bud/bvRepair/edge[2]"
		}
		if !ok {
			b, _ := json.Marshal(rep)
			t.Fatalf("apply=%v: report %s", apply, b)
		}
	}
}

// Only a visit trigger is credited by a budget arrival: an offer trigger
// (here an agent submit) at the visit the budget diverted never ran, since
// no offer was made, so the target is not reached, with or without
// --apply-trigger, and the drive says why.
func TestManufactureBudgetArrivalNeedsAVisitTrigger(t *testing.T) {
	for _, apply := range []bool{false, true} {
		s := script(t, `{"edge":"bud/bvRepair/edge[2]","registration":"bud","target_state":"bud/bvRepair","hash":"x","target":{"visit":3},"steps":[
			{"state":"bud/bvReview","edge":"bud/bvReview/edge[1]","step":{"op":"submit","output":"issues"}},
			{"state":"bud/bvRepair","edge":"bud/bvRepair/edge[1]","step":{"op":"submit","output":"fixed"}}],
			"trigger":{"op":"submit","output":"fixed"}}`)
		ws, state := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		reg := wf.NewAdapters()
		if err := reg.Register(&snapshotRuntime{}); err != nil {
			t.Fatal(err)
		}
		d := budgetDef()
		s.Hash = d.Hash()
		e, run, _ := manufacturedRun(t, d, reg, ws, state, &wf.ManufacturedData{Edge: s.Edge, Hash: s.Hash, Script: wf.ScriptDigest(s)})
		rep := manufacture(t, e, run, s, apply)
		if rep.Reached || rep.TriggerApplied || rep.TriggerEdgeTaken != "" || rep.Stuck == nil || rep.Stuck.Kind != "budget" {
			b, _ := json.Marshal(rep)
			t.Fatalf("apply=%v: report %s", apply, b)
		}
	}
}

// A script's options reach the run's start as run --options does: a gate
// the registration's default (autonomous) options answer by policy is
// offered, and reached, only under the script's non-autonomous options.
// Options the definition does not declare refuse the script before
// anything is copied.
func TestManufactureCLIStartsWithTheScriptsOptions(t *testing.T) {
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	d := policyDef(wf.PolicyAnswerable)
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d, DefaultOptions: pOpts{Autonomous: true, Mode: "slow"}}}, Adapters: reg}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	drive := func(options string) (int, wf.ManufactureReport, string) {
		t.Helper()
		p := writeScript(t, `{"edge":"policy/pAsk/edge[0]","registration":"policy","target_state":"policy/pAsk","hash":"`+d.Hash()+`","run_id":"m",
			"steps":[],"trigger":{"op":"answer","value":"go"}`+options+`}`)
		out := filepath.Join(t.TempDir(), "made")
		code, stdout, stderr := cliCall(cfg, "dev", "manufacture", "policy", "--script", p, "--workspace", ws, "--out", out, "--state-dir", "state", "--apply-trigger")
		var rep wf.ManufactureReport
		if raw, err := os.ReadFile(filepath.Join(out, "report.json")); err == nil {
			_ = json.Unmarshal(raw, &rep)
		}
		return code, rep, stdout + stderr
	}
	if code, rep, log := drive(""); code == 0 || rep.Reached || rep.Ended == nil {
		t.Fatalf("default options: %d %+v\n%s", code, rep, log)
	}
	if code, rep, log := drive(`,"options":{"Autonomous":false,"Mode":"slow"}`); code != 0 || !rep.Reached || rep.TriggerEdgeTaken != "policy/pAsk/edge[0]" {
		t.Fatalf("script options: %d %+v\n%s", code, rep, log)
	}
	if code, _, log := drive(`,"options":{"Autonomus":false}`); code != 2 || !strings.Contains(log, "options") {
		t.Fatalf("undeclared option: %d\n%s", code, log)
	}
}

// A world action's item and input templates read the offer's issued
// inputs from the offer. The handed-out unit instruction does not carry them.
func TestManufactureDoTemplatesReadTheIssuedInputs(t *testing.T) {
	s := script(t, `{"until":"ended","registration":"fan","hash":"x","steps":[{"state":"fan/c-item/chWork","edge":"fan/c-item/chWork/edge[1]",
		"step":{"op":"submit","output":{"OK":true},"do":[{"argv":["echo","{{item.N}}"]}]}}]}`)
	e, run, _ := remark(t, s)
	var got []string
	rep, err := e.Manufacture(run, s, wf.ManufactureOptions{Do: func(argv []string, dir string) (string, error) {
		got = append(got, argv[1])
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Reached || rep.Stuck != nil {
		b, _ := json.Marshal(rep)
		t.Fatalf("report %s", b)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "1,2,3" {
		t.Fatalf("do argv items %q", got)
	}
}

// A template naming nothing known refuses the step as stuck, kind
// template: it never reaches a command or an output.
func TestManufactureUnknownTemplateIsStuck(t *testing.T) {
	for _, step := range []string{
		`{"op":"submit","output":{"OK":true},"do":[{"argv":["touch","{{input.REPORT_FILE}}"]}]}`,
		`{"op":"submit","output":{"OK":true},"do":[{"argv":["true"],"dir":"{{workdir}}/{{nope}}"}]}`,
		`{"op":"submit","output":{"OK":"{{do.0.stdout}}"}}`,
	} {
		s := script(t, `{"until":"ended","registration":"fan","hash":"x","steps":[{"state":"fan/c-item/chWork","edge":"fan/c-item/chWork/edge[1]","step":`+step+`}]}`)
		e, run, _ := remark(t, s)
		ran := 0
		rep, err := e.Manufacture(run, s, wf.ManufactureOptions{Do: func([]string, string) (string, error) { ran++; return "", nil }})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Stuck == nil || rep.Stuck.Kind != "template" || !strings.Contains(rep.Stuck.Why, "{{") || ran != 0 {
			b, _ := json.Marshal(rep)
			t.Fatalf("%s: ran %d, report %s", step, ran, b)
		}
	}
}

// A template never reaches shell program text or the executable: a value
// goes in as its own positional argument (an earlier revision review round 1, P1).
func TestManufactureTemplatesStayOutOfShellSource(t *testing.T) {
	body := func(step string) string {
		return `{"until":"ended","registration":"fan","hash":"x","steps":[{"state":"fan/c-item/chWork","edge":"fan/c-item/chWork/edge[1]","step":` + step + `}]}`
	}
	for _, argv := range []string{
		`["sh","-c","touch {{item.N}}"]`,
		`["/bin/bash","-ec","echo {{input.REPORT_FILE}}"]`,
		`["zsh","-e","-c","echo {{item.N}}","x"]`,
		`["dash","-c","x{{item.N}}"]`,
		`["{{input.TOOL}}","a"]`,
		// Round 2: a templated option before the program can change which
		// argument the shell takes as its program text.
		`["bash","{{input.OPT}}","-c","echo ok","x"]`,
		`["sh","-o","{{input.X}}","-c","true"]`,
		`["sh","-e","{{input.X}}","-c","true"]`,
		// With no -c the operands are a script and its code.
		`["sh","run.sh","{{item.N}}"]`,
		// Round 3: a value-taking option can consume the word that looks
		// like the program text.
		`["bash","-o","pipefail","-c","echo \"$1\"","x","{{item.N}}"]`,
		`["bash","-O","extglob","-c","true","{{item.N}}"]`,
		`["sh","-oc","pipefail","echo","{{item.N}}"]`,
	} {
		step := `{"op":"submit","output":{"OK":true},"do":[{"argv":` + argv + `}]}`
		if _, err := wf.ParseManufactureScript([]byte(body(step))); err == nil || !strings.Contains(err.Error(), "positional") {
			t.Errorf("%s: %v", argv, err)
		}
		trig := `{"edge":"fan/c-item/chWork/edge[1]","registration":"fan","target_state":"fan/c-item/chWork","hash":"x","steps":[],"trigger":{"op":"submit","output":{"OK":true},"do":[{"argv":` + argv + `}]}}`
		if _, err := wf.ParseManufactureScript([]byte(trig)); err == nil {
			t.Errorf("trigger %s accepted", argv)
		}
	}
	for _, argv := range []string{
		`["sh","-c","touch \"$1\"","x","{{item.N}}"]`,
		`["bash","-ec","echo \"$1\"","x","{{input.REPORT_FILE}}"]`,
		`["echo","{{item.N}}"]`,
		`["python3","-c","print(1)","{{item.N}}"]`,
	} {
		step := `{"op":"submit","output":{"OK":true},"do":[{"argv":` + argv + `}]}`
		if _, err := wf.ParseManufactureScript([]byte(body(step))); err != nil {
			t.Errorf("%s refused: %v", argv, err)
		}
	}
}

// A script built in code is held to the same rule at engine admission,
// before the run is bound (an earlier revision review round 2).
func TestManufactureRefusesAConstructedScriptsTemplatedShellSource(t *testing.T) {
	s := script(t, `{"until":"ended","registration":"fan","hash":"x","steps":[`+workOK+`]}`)
	e, run, state := remark(t, s)
	s.Steps[0].Step.Do = []wf.ScriptCommand{{Argv: []string{"sh", "-c", "touch {{item.N}}"}}}
	ran := 0
	_, err := e.Manufacture(run, s, wf.ManufactureOptions{Do: func([]string, string) (string, error) { ran++; return "", nil }})
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "positional") || ran != 0 {
		t.Fatalf("constructed script: %v, ran %d", err, ran)
	}
	recs, _ := wf.ReadJournal(state, run)
	for _, r := range recs {
		if r.Kind == wf.JournalInstructionIssued {
			t.Fatal("a refused script drove the run")
		}
	}
}

// An item's integer field reaches its template exactly, past float64's
// range (an earlier revision review round 1, P2).
func TestManufactureItemTemplateKeepsLargeIntegers(t *testing.T) {
	s := script(t, `{"until":"ended","registration":"fan","hash":"x","steps":[{"state":"fan/c-item/chWork","edge":"fan/c-item/chWork/edge[1]",
		"step":{"op":"submit","output":{"OK":true},"do":[{"argv":["echo","{{item.N}}"]}]}}]}`)
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	d := fanDef(s.Registration, chItems(9007199254740993), 2, wf.AllSettled())
	s.Hash = d.Hash()
	e, run, _ := manufacturedRun(t, d, reg, ws, state, &wf.ManufacturedData{Edge: s.Edge, Until: s.Until, Hash: s.Hash, Script: wf.ScriptDigest(s)})
	var got []string
	rep, err := e.Manufacture(run, s, wf.ManufactureOptions{Do: func(argv []string, _ string) (string, error) {
		got = append(got, argv[1])
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Reached || len(got) != 1 || got[0] != "9007199254740993" {
		b, _ := json.Marshal(rep)
		t.Fatalf("item.N %q, report %s", got, b)
	}
}
