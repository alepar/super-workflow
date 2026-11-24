package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// unitContext is the context mode status reports for unit name.
func unitContext(t *testing.T, h *harness, run wf.RunID, name string) string {
	t.Helper()
	for _, u := range h.status(run).Units {
		if u.Name == name {
			return u.Context
		}
	}
	t.Fatalf("unit %s missing from status", name)
	return ""
}

func TestIssuedAgentInstructionsCarryContextFresh(t *testing.T) {
	d := clAgentDef("ctx-issued")
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	if ins.Context != wf.ContextFresh {
		t.Fatalf("handed instruction context = %q, want fresh", ins.Context)
	}
	raw, err := json.Marshal(ins)
	if err != nil || !strings.Contains(string(raw), `"context":"fresh"`) {
		t.Fatalf("instruction JSON lacks context fresh: %s (%v)", raw, err)
	}
	next, err := h.e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	agents := 0
	for _, in := range next {
		if in.Kind == wf.InstructionAgent {
			agents++
			if in.Context != wf.ContextFresh {
				t.Errorf("Next agent instruction %s context = %q, want fresh", in.Name, in.Context)
			}
		}
	}
	if agents == 0 {
		t.Fatalf("Next returned no agent instruction: %+v", next)
	}
}

func TestParseContextMode(t *testing.T) {
	for _, s := range []string{"fresh", "inherited"} {
		if m, err := wf.ParseContextMode(s); err != nil || string(m) != s {
			t.Errorf("ParseContextMode(%q) = %q, %v", s, m, err)
		}
	}
	for _, s := range []string{"", "Fresh", "fork", "none"} {
		if _, err := wf.ParseContextMode(s); err == nil {
			t.Errorf("ParseContextMode(%q) accepted", s)
		}
	}
}

func TestStartedContextModeOnClaim(t *testing.T) {
	d := clAgentDef("ctx-claim")
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if got := unitContext(t, h, run, ins.Name); got != "unrecorded" {
		t.Fatalf("omitted --context shows %q, want unrecorded", got)
	}
	fresh := claim("a")
	fresh.Context = wf.ContextFresh
	if err := h.e.Started(run, ins.Name, fresh); err != nil {
		t.Fatalf("upgrade empty to fresh: %v", err)
	}
	if got := unitContext(t, h, run, ins.Name); got != "fresh" {
		t.Fatalf("after upgrade context = %q, want fresh", got)
	}
	got := attemptStarted(t, h, run)
	if len(got) != 2 || got[0].Context != "" || got[1].Context != wf.ContextFresh {
		t.Fatalf("attempt_started = %+v, want empty then fresh", got)
	}
	if err := h.e.Started(run, ins.Name, fresh); err != nil {
		t.Fatalf("identical repeat: %v", err)
	}
	if err := h.e.Started(run, ins.Name, claim("a")); err != nil {
		t.Fatalf("omitted context after fresh: %v", err)
	}
	if n := len(attemptStarted(t, h, run)); n != 2 {
		t.Fatalf("no-op repeats journaled: %d claims", n)
	}
	inh := claim("a")
	inh.Context = wf.ContextInherited
	if err := h.e.Started(run, ins.Name, inh); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("inherited after fresh = %v, want ErrRejected", err)
	}
	if got := unitContext(t, h, run, ins.Name); got != "fresh" {
		t.Fatalf("rejected conflict changed context to %q", got)
	}
}

func TestStartedContextInheritedThenFreshRejected(t *testing.T) {
	d := clAgentDef("ctx-reverse")
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	inh := claim("a")
	inh.Context = wf.ContextInherited
	if err := h.e.Started(run, ins.Name, inh); err != nil {
		t.Fatal(err)
	}
	fresh := claim("a")
	fresh.Context = wf.ContextFresh
	if err := h.e.Started(run, ins.Name, fresh); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("fresh after inherited = %v, want ErrRejected", err)
	}
	if got := unitContext(t, h, run, ins.Name); got != "inherited" {
		t.Fatalf("context = %q, want inherited", got)
	}
}

func TestStatusShowsSettledUnitContext(t *testing.T) {
	d := clAgentDef("ctx-settled")
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	fresh := claim("a")
	fresh.Context = wf.ContextFresh
	if err := h.e.Started(run, ins.Name, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Done(run, ins.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	if got := unitContext(t, h, run, ins.Name); got != "fresh" {
		t.Fatalf("settled unit context = %q, want fresh", got)
	}
}

func TestRenderStatusShowsContextMode(t *testing.T) {
	out := wf.RenderStatus(wf.RunStatus{Units: []wf.UnitStatus{
		{Name: "a", State: "running", Context: "fresh"},
		{Name: "b", State: "started", Context: "unrecorded"},
		{Name: "c", State: "pending"},
	}})
	for _, want := range []string{"context fresh", "context unrecorded"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "c ") && strings.Contains(line, "context") {
			t.Errorf("pending row shows a context: %q", line)
		}
		if strings.HasPrefix(line, "records:") || strings.HasPrefix(line, "report:") {
			t.Errorf("status prints %q", line)
		}
	}
}

func TestStartedCLIContextFlag(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	sd := []string{"--state-dir", e.state}
	code, o, er := e.cli(append([]string{"next", string(run), "1", "--json"}, sd...)...)
	var handed []wf.Instruction
	if err := json.Unmarshal([]byte(o), &handed); err != nil || code != 0 || len(handed) != 1 {
		t.Fatalf("next 1 = %d %q (%v) %s", code, o, err, er)
	}
	if handed[0].Context != wf.ContextFresh {
		t.Fatalf("CLI next instruction context = %q, want fresh", handed[0].Context)
	}
	name := handed[0].Name
	base := append([]string{"dev", "started", string(run), name, "--session", "s1", "--agent", "a"}, sd...)
	if code, _, er := e.cli(append(base, "--context", "forked")...); code != 2 || !strings.Contains(er, "fresh") {
		t.Fatalf("started --context forked = %d %q, want usage exit 2", code, er)
	}
	if code, _, er := e.cli(append(base, "--context", "inherited")...); code != 0 {
		t.Fatalf("started --context inherited = %d %s", code, er)
	}
	if code, _, _ := e.cli(append(base, "--context", "fresh")...); code != 4 {
		t.Fatalf("started --context fresh after inherited = %d, want 4", code)
	}
	code, o, er = e.cli(append([]string{"status", string(run), "--json"}, sd...)...)
	if code != 0 {
		t.Fatalf("status --json = %d %s", code, er)
	}
	var rs wf.RunStatus
	if err := json.Unmarshal([]byte(o), &rs); err != nil {
		t.Fatal(err)
	}
	if len(rs.Units) == 0 || rs.Units[0].Context != "inherited" {
		t.Fatalf("status --json units = %+v, want context inherited", rs.Units)
	}
	if !strings.Contains(o, `"Context": "inherited"`) && !strings.Contains(o, `"Context":"inherited"`) {
		t.Fatalf("status --json lacks Context field: %s", o)
	}
	code, o, _ = e.cli(append([]string{"status", string(run), "--full"}, sd...)...)
	if code != 0 || !strings.Contains(o, "context inherited") {
		t.Fatalf("status text = %d %q, want context inherited", code, o)
	}
}

// Context fresh arrives with each unit (C2): the spawn line is the harness's
// own fresh-context form.
func TestSpawnLineContextFresh(t *testing.T) {
	for _, tc := range []struct {
		harness, agentType string
		want, absent       []string
	}{
		{"claude", "general-purpose", []string{`Agent(subagent_type="general-purpose"`}, []string{"fork_turns"}},
		{"codex", "", []string{`spawn_agent(`, `fork_turns="none"`}, []string{"subagent_type"}},
		{"generic", "", []string{"fresh context"}, []string{"fork_turns", "subagent_type"}},
	} {
		out := spawnLine(tc.harness, wf.DispatchHints{AgentType: tc.agentType})
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s spawn line lacks %q: %s", tc.harness, w, out)
			}
		}
		for _, a := range tc.absent {
			if strings.Contains(out, a) {
				t.Errorf("%s spawn line has %q: %s", tc.harness, a, out)
			}
		}
	}
}

func spawnLine(harness string, d wf.DispatchHints) string {
	return wf.RenderNext(wf.NextResult{Instructions: []wf.Instruction{{Kind: wf.InstructionAgent, Name: "u#1", Brief: "run: x", Dispatch: d}}}, harness,
		func(v string, a ...string) string { return v })
}

// TestNextAllCLI: `next <run> all` hands out every ready unit and claims none.
func TestNextAllCLI(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("fan-cli")
	code, o, er := e.cli("next", string(run), "all", "--state-dir", e.state, "--json")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(o), &ins); err != nil || code != 0 {
		t.Fatalf("next all = %d %q (%v) %s", code, o, err, er)
	}
	if len(ins) != 2 {
		t.Fatalf("next all handed %d units, want 2: %+v", len(ins), ins)
	}
	for _, in := range ins {
		if in.Name == "" || in.Token != "" {
			t.Fatalf("next all gave a wave instruction, want a paged hand-out: %+v", in)
		}
	}
	if st := e.status(run); st.UnitCounts.Handed != 2 || st.UnitCounts.Running != 0 {
		t.Fatalf("after next all: %+v, want 2 handed, 0 running", st.UnitCounts)
	}
	if code, _, er := e.cli("next", string(run), "bogus", "--state-dir", e.state, "--json"); code != 2 || !strings.Contains(er, "all") {
		t.Fatalf("next bogus = %d %q, want usage exit 2", code, er)
	}
}
