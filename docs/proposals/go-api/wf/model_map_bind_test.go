package wf_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// an earlier revision: a --model-map value is checked once, at bind, against the
// harness that binds the segment. Every boundary verb accepts a key-valid map
// whatever it holds; the first coordinator's next refuses a value its harness
// does not take, journaling nothing, and the boundary verb re-issued with a
// corrected map replaces the refused one.

const (
	agyBadValue = "gemini-3.8-flash-high"
	agyValues   = "inherit, flash_lite, flash, pro"
)

var agyBadMap = map[string]string{"light": agyBadValue}

// stoppedOn is a run of tieredFan started under harness start whose first
// unit was handed out to a coordinator of start (so its segment is bound),
// then stopped.
func stoppedOn(t *testing.T, name, start string) (*harness, *wf.Def, *wf.Engine, wf.RunID) {
	t.Helper()
	d := tieredFan(name, 1, wf.TierLight, 1, 2)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: start})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	if ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: start, Session: start + "-1"}); err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("%s hand-out = %+v, %v", start, ins, err)
	}
	if err := e.Stop(run, "move"); err != nil {
		t.Fatal(err)
	}
	return h, d, e, run
}

// journaledBindings counts what a binding journals: its coordinator record
// and any degraded note.
func journaledBindings(h *harness, run wf.RunID) int {
	return len(h.kind(run, wf.JournalCoordinator)) + len(h.kind(run, wf.JournalDegraded))
}

// lastBinding is the latest coordinator record that bound a segment.
func lastBinding(t *testing.T, h *harness, run wf.RunID) wf.CoordinatorData {
	t.Helper()
	var out wf.CoordinatorData
	found := false
	for _, r := range h.kind(run, wf.JournalCoordinator) {
		var d wf.CoordinatorData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.SegmentStart != 0 && d.Models != nil {
			out, found = d, true
		}
	}
	if !found {
		t.Fatalf("run %s has no binding record", run)
	}
	return out
}

// refusedAtBind hands run out to a coordinator of harness hn and requires the
// bind refusal naming kv and the harness's accepted values, with nothing
// journaled.
func refusedAtBind(t *testing.T, h *harness, e *wf.Engine, run wf.RunID, hn, kv string) {
	t.Helper()
	before := journaledBindings(h, run)
	records := len(h.records(run))
	_, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: hn, Session: hn + "-bind"})
	if err == nil {
		t.Fatalf("%s bound run %s with %s", hn, run, kv)
	}
	for _, want := range []string{"bad --model-map", kv, agyValues, "re-issue the boundary verb"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%s bind refusal %q lacks %q", hn, err, want)
		}
	}
	if got := journaledBindings(h, run); got != before {
		t.Fatalf("the refused bind journaled %d binding record(s)", got-before)
	}
	if got := len(h.records(run)); got != records {
		t.Fatalf("the refused bind journaled %d record(s)", got-records)
	}
}

// claude -> agy: the verb accepts a value agy refuses; agy's bind refuses it;
// a corrected map binds under agy; the same map binds under codex.
func TestResumeClaudeToAgyRefusesAtBind(t *testing.T) {
	h, d, e, run := stoppedOn(t, "mm-claude-agy", "claude")
	if err := boundaryEngine(h, d, agyBadMap).Resume(run); err != nil {
		t.Fatalf("the verb refused a key-valid map: %v", err)
	}
	refusedAtBind(t, h, e, run, "agy", "light="+agyBadValue)
	st, err := e.Status(run)
	if err != nil || len(st.Segments) != 2 || st.Segments[1].Harness != "" {
		t.Fatalf("status after the refused bind = %+v, %v; want the second segment unbound", st.Segments, err)
	}

	// A corrected map, given at resume, binds under agy.
	h, d, e, run = stoppedOn(t, "mm-claude-agy-ok", "claude")
	if err := boundaryEngine(h, d, map[string]string{"light": "flash"}).Resume(run); err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "agy", Session: "agy-1"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("agy hand-out with light=flash = %+v, %v", ins, err)
	}
	if got := lastBinding(t, h, run); got.Harness != "agy" || got.Models.Models["light"] != "flash" {
		t.Fatalf("agy binding = %+v / %+v, want light=flash", got, got.Models)
	}
	if m := agentsOf(ins)[0].Dispatch.Model; m != "flash" {
		t.Fatalf("agy unit spawns %q, want flash", m)
	}

	// The same value, bound by codex, is codex's to take.
	h, d, e, run = stoppedOn(t, "mm-claude-codex", "claude")
	if err := boundaryEngine(h, d, agyBadMap).Resume(run); err != nil {
		t.Fatal(err)
	}
	ins, err = e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "fixture-session-a"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("codex hand-out with light=%s = %+v, %v", agyBadValue, ins, err)
	}
	if got := lastBinding(t, h, run); got.Harness != "codex" || got.Models.Models["light"] != agyBadValue {
		t.Fatalf("codex binding = %+v / %+v, want light=%s", got, got.Models, agyBadValue)
	}
}

// agy -> codex: a value only codex takes is accepted at the verb (it used to
// be refused there, by the harness the run was last in), refused by an agy
// coordinator at bind, and taken by codex.
func TestResumeAgyToCodexAcceptsACodexValue(t *testing.T) {
	h, d, e, run := stoppedOn(t, "mm-agy-codex", "agy")
	codexMap := map[string]string{"light": "gpt-6.1-terra"}
	if err := boundaryEngine(h, d, codexMap).Resume(run); err != nil {
		t.Fatalf("the verb refused a key-valid map: %v", err)
	}
	refusedAtBind(t, h, e, run, "agy", "light=gpt-6.1-terra")
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "fixture-session-a"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("codex hand-out = %+v, %v", ins, err)
	}
	if m := agentsOf(ins)[0].Dispatch.Model; m != "gpt-6.1-terra" {
		t.Fatalf("codex unit spawns %q, want gpt-6.1-terra", m)
	}
	if got := lastBinding(t, h, run); got.Harness != "codex" || got.Models.Models["light"] != "gpt-6.1-terra" {
		t.Fatalf("codex binding = %+v / %+v", got, got.Models)
	}
}

// After a bind refusal the same boundary verb, re-issued with a corrected map,
// replaces the map though the run is not stopped; the next bind journals the
// corrected models; the refused map is journaled in no binding. A bound
// segment still refuses a map as not stopped.
func TestReissueAfterABindRefusal(t *testing.T) {
	h, d, e, run := stoppedOn(t, "mm-reissue", "claude")
	if err := boundaryEngine(h, d, agyBadMap).Resume(run); err != nil {
		t.Fatal(err)
	}
	refusedAtBind(t, h, e, run, "agy", "light="+agyBadValue)
	if err := boundaryEngine(h, d, map[string]string{"light": "flash"}).Resume(run); err != nil {
		t.Fatalf("re-issued resume on the unbound segment = %v", err)
	}
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "agy", Session: "agy-1"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("agy hand-out after the re-issue = %+v, %v", ins, err)
	}
	if got := lastBinding(t, h, run); got.Harness != "agy" || got.Models.Models["light"] != "flash" || got.Models.ModelMap["light"] != "flash" {
		t.Fatalf("binding after the re-issue = %+v / %+v, want light=flash", got, got.Models)
	}
	for _, r := range h.kind(run, wf.JournalCoordinator) {
		if strings.Contains(string(r.Data), agyBadValue) {
			t.Fatalf("a binding record carries the refused map: %s", r.Data)
		}
	}
	st, err := e.Status(run)
	if err != nil || len(st.Segments) == 0 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if last := st.Segments[len(st.Segments)-1]; last.Harness != "agy" || last.ModelsFrom != wf.ModelsFromMap || last.Models["light"] != "flash" {
		t.Fatalf("last segment = %+v, want bound by agy from the map with light=flash", last)
	}
	// The segment is bound now: a map changes only at a boundary.
	err = boundaryEngine(h, d, map[string]string{"light": "pro"}).Resume(run)
	if err == nil || !strings.Contains(err.Error(), "is not stopped") {
		t.Fatalf("resume --model-map on a bound, running segment = %v, want the not-stopped refusal", err)
	}
}

// died is a run of tieredFan under harness start whose one unit was handed to
// a coordinator of start, fetched and finished with a failing output, so that
// a rewind to died forks it.
func died(t *testing.T, name, start string) (*harness, *wf.Def, *wf.Engine, wf.RunID) {
	t.Helper()
	d := tieredFan(name, 1, wf.TierLight, 1)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: start})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: start, Session: start + "-1"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("%s hand-out = %+v, %v", start, ins, err)
	}
	u := agentsOf(ins)[0]
	if _, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), segChild(start, start+"-1", "kid")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{"OK":false}`)}); err != nil {
		t.Fatal(err)
	}
	return h, d, e, run
}

// Rewind forks take a key-valid map at the verb; the fork's bind decides, for
// a source of any harness; the rewind re-issued with a corrected map makes a
// new fork that binds.
func TestRewindForkBindDecides(t *testing.T) {
	for _, c := range []struct{ name, source, binder, value string }{
		{"codex-source", "codex", "agy", agyBadValue},
		{"agy-source", "agy", "agy", "gpt-6.1-terra"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, d, e, run := died(t, "mm-rewind-"+c.name, c.source)
			bad := map[string]string{"light": c.value}
			rep, err := boundaryEngine(h, d, bad).Rewind(run, "died")
			if err != nil {
				t.Fatalf("rewind refused a key-valid map at the verb: %v", err)
			}
			if c.source == "codex" {
				refusedAtBind(t, h, e, rep.Run, "agy", "light="+c.value)
				ins, err := e.HandOutAs(rep.Run, 1, wf.Coordinator{Harness: "codex", Session: "fixture-session-b"})
				if err != nil || len(agentsOf(ins)) != 1 {
					t.Fatalf("codex hand-out on the fork = %+v, %v", ins, err)
				}
				if got := lastBinding(t, h, rep.Run); got.Harness != "codex" || got.Models.Models["light"] != c.value {
					t.Fatalf("codex binding on the fork = %+v / %+v", got, got.Models)
				}
			} else {
				refusedAtBind(t, h, e, rep.Run, "agy", "light="+c.value)
				ins, err := e.HandOutAs(rep.Run, 1, wf.Coordinator{Harness: "codex", Session: "fixture-session-b"})
				if err != nil || len(agentsOf(ins)) != 1 || agentsOf(ins)[0].Dispatch.Model != c.value {
					t.Fatalf("codex hand-out on the fork = %+v, %v; want %s", ins, err, c.value)
				}
			}
			// The rewind re-issued on its source with a corrected map is a
			// new fork, and agy binds it.
			fix, err := boundaryEngine(h, d, map[string]string{"light": "flash"}).Rewind(run, "died")
			if err != nil || fix.Run == rep.Run {
				t.Fatalf("corrected rewind = %+v, %v; want a new fork", fix, err)
			}
			if _, err := e.HandOutAs(fix.Run, 1, wf.Coordinator{Harness: "agy", Session: "agy-2"}); err != nil {
				t.Fatalf("agy on the corrected fork: %v", err)
			}
			if got := lastBinding(t, h, fix.Run); got.Harness != "agy" || got.Models.Models["light"] != "flash" {
				t.Fatalf("agy binding on the corrected fork = %+v / %+v", got, got.Models)
			}
		})
	}
}

// A fanout unit is a snapshot fork whose coordinator binds its first segment
// (wf dev fanout --coordinator-cmd): a key-valid map is accepted at the
// source, and the unit's fork refuses the value when agy binds it. RewindInto
// goes through the same verb entry; the run here has not died, so it is
// refused for that, never for the map.
func TestFanoutUnitBoundByAgyRefuses(t *testing.T) {
	_, d, reg, run, ws, state, _ := liveSiblingsRun(t, false)
	opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }, BoundaryModelMap: agyBadMap}
	boundary := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
	if err := boundary.CheckBoundaryModelMap(run); err != nil {
		t.Fatalf("the source refused a key-valid map at the verb: %v", err)
	}
	if _, err := boundary.RewindInto(run, "died", filepath.Join(t.TempDir(), "rewind")); err != nil && strings.Contains(err.Error(), "bad --model-map") {
		t.Fatalf("rewind --out refused the map at the verb: %v", err)
	}
	_, fork := forkLatestOffer(t, boundary, run)
	unit := forkedEngine(d, reg, fork, 0)
	recs, err := wf.ReadJournal(fork.StateDir, run)
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		n := 0
		for _, r := range recs {
			// A fork's first cycle journals its lost orphans (degraded,
			// attempt_lost) whoever calls; a binding is a coordinator record.
			if r.Kind == wf.JournalCoordinator {
				var cd wf.CoordinatorData
				if json.Unmarshal(r.Data, &cd) == nil && cd.SegmentStart != 0 && cd.Models != nil {
					n++
				}
			}
		}
		return n
	}
	before := count()
	_, err = unit.HandOutAs(run, 1, wf.Coordinator{Harness: "agy", Session: "agy-unit"})
	if err == nil || !strings.Contains(err.Error(), "bad --model-map") || !strings.Contains(err.Error(), "light="+agyBadValue) || !strings.Contains(err.Error(), agyValues) {
		t.Fatalf("agy binding the fanout unit's fork = %v, want the bind refusal", err)
	}
	if recs, err = wf.ReadJournal(fork.StateDir, run); err != nil || count() != before {
		t.Fatalf("the refused bind journaled a binding on the fork (%v)", err)
	}
	if _, err := unit.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "codex-unit"}); err != nil {
		t.Fatalf("codex binding the same fork: %v", err)
	}
}

// A run_started whose harness is not one the run can be read as (an unknown
// name) leaves the verb entry key-only: the bind decides.
func TestCorruptRunStartedLeavesTheBindToDecide(t *testing.T) {
	h, d, e, src := stoppedOn(t, "mm-corrupt", "claude")
	recs := h.records(src)
	unlock, err := h.j.Lock("copy")
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range recs {
		r.Run = "copy"
		if i == 0 {
			var started wf.RunStartedData
			if err := json.Unmarshal(r.Data, &started); err != nil {
				t.Fatal(err)
			}
			started.Harness = "no-such-harness"
			r.Data, _ = json.Marshal(started)
		}
		if err := h.j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	unlock()
	boundary := boundaryEngine(h, d, agyBadMap)
	if err := boundary.CheckBoundaryModelMap("copy"); err != nil {
		t.Fatalf("verb entry on an unreadable harness: %v", err)
	}
	if err := boundary.Resume("copy"); err != nil {
		t.Fatalf("resume on an unreadable harness: %v", err)
	}
	refusedAtBind(t, h, e, "copy", "agy", "light="+agyBadValue)
	if _, err := e.HandOutAs("copy", 1, wf.Coordinator{Harness: "codex", Session: "fixture-session-a"}); err != nil {
		t.Fatalf("codex bind: %v", err)
	}
}

// run --run-id on a live agy run refuses a bad key as a usage error and
// accepts the map in force; when the map in force is one the run's harness
// refuses, it says so and names the boundary verbs.
func TestRunAttachOnAnAgyRun(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	def := tierDef("p", wf.ModelTier(wf.TierStrong))
	state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
	run := func(m string) (int, string) {
		code, _, er := singleCLI(def, "run", "--state-dir", state, "--workspace", ws, "--run-id", "r1", "--harness", "agy", "--model-map", m)
		return code, er
	}
	if code, er := run("all=flash"); code != 0 {
		t.Fatalf("create = %d %s", code, er)
	}
	if code, er := run("lite=x"); code != 2 || !strings.Contains(er, `key "lite"`) {
		t.Fatalf("attach with a bad key = %d %q", code, er)
	}
	if code, er := run("all=flash"); code != 0 {
		t.Fatalf("attach with the map in force = %d %q", code, er)
	}
	mustSingle(t, def, "wf", "stop", "--state-dir", state, "--reason", "operator")
	if code, _, er := singleCLI(def, "wf", "resume", "--state-dir", state, "--operator", "op", "--reason", "r", "--model-map", "all=gpt-6.1-terra"); code != 0 {
		t.Fatalf("resume with a codex value on an agy run = %d %s; the verb checks keys only", code, er)
	}
	if code, er := run("all=gpt-6.1-terra"); code != 2 || !strings.Contains(er, "refused by the harness that would bind it") || !strings.Contains(er, "all=gpt-6.1-terra") {
		t.Fatalf("attach with a map the run's harness refuses = %d %q", code, er)
	}
}

// an earlier revision: an attach resolves the map in force for the attaching
// caller's harness, not the last segment's: after an agy segment, a codex
// coordinator attaching with the map its resume gave is accepted, a map
// that differs is still refused, and an agy caller is still refused the
// value agy does not take.
func TestRunAttachFromAnotherHarness(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	def := tierDef("p", wf.ModelTier(wf.TierStrong))
	state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
	if code, _, er := singleCLI(def, "run", "--state-dir", state, "--workspace", ws, "--run-id", "r1", "--harness", "agy", "--model-map", "all=flash"); code != 0 {
		t.Fatalf("create = %d %s", code, er)
	}
	mustSingle(t, def, "wf", "stop", "--state-dir", state, "--reason", "operator")
	if code, _, er := singleCLI(def, "wf", "resume", "--state-dir", state, "--operator", "op", "--reason", "r", "--model-map", "all=gpt-6.1-terra"); code != 0 {
		t.Fatalf("resume = %d %s", code, er)
	}
	// An agy caller binds the segment, so agy's refusal of the value stands.
	if code, _, er := singleCLI(def, "run", "--state-dir", state, "--workspace", ws, "--run-id", "r1", "--harness", "agy", "--model-map", "all=gpt-6.1-terra"); code != 2 || !strings.Contains(er, "refused by the harness that would bind it") {
		t.Fatalf("agy caller attaching with a value agy refuses = %d %q", code, er)
	}
	// The done-line's literal form: an explicit --harness codex on an agy-started
	// run is refused by resumeHarnessMatches, after the model-map check (not
	// this task's to change); record that it is not the model-map check.
	if code, _, er := singleCLI(def, "run", "--state-dir", state, "--workspace", ws, "--run-id", "r1", "--harness", "codex", "--model-map", "all=gpt-6.1-terra"); strings.Contains(er, "refused by the harness that would bind it") {
		t.Fatalf("explicit --harness codex attach was refused by the model-map check = %d %q", code, er)
	}
	// A codex coordinator attaches as its environment shows it, with no
	// --harness: the root codex session the done-line's attach stands for.
	t.Setenv("CODEX_THREAD_ID", "boss")
	t.Setenv("CODEX_SESSION_ID", "boss")
	if code, _, er := singleCLI(def, "run", "--state-dir", state, "--workspace", ws, "--run-id", "r1", "--model-map", "all=flash"); code != 2 || !strings.Contains(er, "already running with --model-map") {
		t.Fatalf("codex caller attaching with a differing map = %d %q", code, er)
	}
	if code, _, er := singleCLI(def, "run", "--state-dir", state, "--workspace", ws, "--run-id", "r1", "--model-map", "all=gpt-6.1-terra"); code != 0 {
		t.Fatalf("codex caller attaching with the map in force = %d %q", code, er)
	}
}
