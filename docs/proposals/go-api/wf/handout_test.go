package wf_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	hoWork  struct{}
	hoCheck struct{}
	hoDone  struct{}
	hoStop  struct{}
	hoAns   string
)

// hoGateChild is an agent, then a reserved human gate, then a terminal.
func hoGateChild() wf.Child[chItem, chRes] {
	return hoGateChildObserved(false)
}

func hoGateChildObserved(observed bool) wf.Child[chItem, chRes] {
	d := wf.New("g-item")
	w := wf.NewAgent[hoWork, chOut](d)
	if observed {
		wf.DoneWhen(w, wf.Observe[bool]("x", "done", nil), "already done")
	}
	ans := wf.NewEnum(d, "ho-ans", hoAns("yes"), hoAns("no"))
	g := wf.NewGate[hoCheck, hoAns](d, "Keep?", ans, wf.Reserved)
	ok := wf.NewTerminal[hoDone](d, wf.Success)
	no := wf.NewTerminal[hoStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, no)
	w.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[hoWork](wf.Const(1), w.To(no, "gave up")))
	toGate := w.To(g, "ok")
	w.Route(func(chOut, wf.Ctx) wf.Edge[hoWork] { return toGate }, toGate)
	g.Answers(wf.On(hoAns("yes"), g.To(ok, "yes")), wf.On(hoAns("no"), g.To(no, "no")))
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(no, res)
	return wf.Typed[chItem, chRes](d)
}

func hoGateFan(par int) *wf.Def {
	d := wf.New("ho-gate-fan")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, hoGateChild())
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(par).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

func (h *harness) handOut(run wf.RunID, n int) []wf.Instruction {
	h.t.Helper()
	ins, err := h.e.HandOut(run, n)
	if err != nil {
		h.t.Fatal(err)
	}
	return ins
}

func handedNames(t *testing.T, h *harness, run wf.RunID) []string {
	t.Helper()
	var names []string
	for _, r := range h.kind(run, wf.JournalHandedOut) {
		var d wf.HandedOutData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		names = append(names, d.Name)
	}
	return names
}

func TestHandOutTakesNInOrder(t *testing.T) {
	d := fanDef("ho-fan", chItems(1, 2, 3, 4, 5), 5, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.handOut(run, 0) // drive to the open instructions without handing any out
	type child struct{ Child string }
	var wave []child
	for _, in := range h.status(run).Instances {
		if in.Phase == "issued" {
			wave = append(wave, child{in.ID})
		}
	}
	if len(wave) != 5 {
		t.Fatalf("%d agents are issued, want 5", len(wave))
	}

	first := h.handOut(run, 2)
	if len(first) != 2 {
		t.Fatalf("HandOut(2) = %d instructions, want 2", len(first))
	}
	for i, in := range first {
		if in.Kind != wf.InstructionAgent || in.Name == "" || in.Class == "" || in.Brief == "" {
			t.Errorf("instruction %d = %+v, want an agent with name, class and brief", i, in)
		}
		if in.Prompt != "" || in.Token != "" || in.Submit != "" {
			t.Errorf("instruction %d carries prompt %q, token %q or submit %q", i, in.Prompt, in.Token, in.Submit)
		}
		if in.Child != wave[i].Child {
			t.Errorf("instruction %d is %s, want definition order %s", i, in.Child, wave[i].Child)
		}
		if cmd, ok := wf.BriefCommand(in.Brief); !ok || cmd != "workflow prompt "+string(run)+" --claim "+claimToken(t, in) {
			t.Errorf("brief does not carry this unit's command: %q", in.Brief)
		}
		// The finishing lines and the rules are in the prompt the child fetches.
		prompt, err := h.e.PromptClaim(run, "", claimToken(t, in))
		if err != nil {
			t.Fatalf("prompt by claim of instruction %d: %v", i, err)
		}
		for _, verb := range []string{"You are subagent " + in.Name + ".", "workflow done " + string(run) + " --claim " + claimToken(t, in) + " --output <file>", "workflow failed " + string(run) + " --claim " + claimToken(t, in) + " --reason <text>",
			"workflow failed " + string(run) + " --claim " + claimToken(t, in) + " --class blocked --reason <cause>",
			// D1, D3: refused versus ran-and-failed, a result is validated before
			// done (C5); a sandbox block is escalated and reported, and a refused
			// escalation is blocked.
			"workflow validate " + string(run) + " --claim " + claimToken(t, in) + " --output <file>",
			"Check your result first (it checks exactly as done does and records nothing); fix what it names and run it again until it says ok:",
			"is blocked: never work around it; finish with:", "A command that ran and failed",
			"When the sandbox blocks a command the task needs, request escalation", "--escalated '<command>: <reason>: <outcome>' to done or failed for each one, the outcome a short phrase without \": \"",
			"If escalation is refused, that is still blocked unless your coordinator can present the approval prompt you could not access.",
		} {
			if !strings.Contains(prompt, verb) {
				t.Errorf("prompt lacks %q:\n%s", verb, prompt)
			}
		}
		if strings.Contains(prompt, "Never request sandbox escalation") {
			t.Errorf("prompt keeps the old no-escalation rule:\n%s", prompt)
		}
		if strings.Contains(prompt, "Run each workflow command once") {
			t.Errorf("prompt keeps the old run-once rule:\n%s", prompt)
		}
	}
	if got, want := handedNames(t, h, run), []string{first[0].Name, first[1].Name}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("handed_out records = %v, want exactly %v", got, want)
	}

	second := h.handOut(run, 2)
	if len(second) != 2 || second[0].Child != wave[2].Child || second[1].Child != wave[3].Child {
		t.Fatalf("second HandOut = %+v, want the next two children %s, %s", second, wave[2].Child, wave[3].Child)
	}
	if rest := h.handOut(run, -1); len(rest) != 1 || rest[0].Child != wave[4].Child {
		t.Fatalf("HandOut(-1) = %+v, want the last child", rest)
	}
	if again := h.handOut(run, 5); len(again) != 0 {
		t.Fatalf("a handed unit was handed again: %+v", again)
	}
	if n := len(handedNames(t, h, run)); n != 5 {
		t.Fatalf("handed_out records = %d, want 5", n)
	}
}

// The engine sets no cap: with no n (n < 0) HandOut hands out every ready
// unit, however many, on any harness; with n it hands out at most n.
func TestHandOutNoCountHandsOutEveryReadyUnit(t *testing.T) {
	const width = 30 // above any harness's old default budget
	items := make([]int, width)
	for i := range items {
		items[i] = i + 1
	}
	for _, harness := range []string{"claude", "codex", "generic"} {
		t.Run(harness, func(t *testing.T) {
			d := fanDef("ho-all-"+harness, chItems(items...), width, wf.AllSettled())
			h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: harness})
			run := h.run(d, nil)
			if got := h.handOut(run, 7); len(got) != 7 {
				t.Fatalf("HandOut(7) = %d units, want at most and exactly 7", len(got))
			}
			if got := h.handOut(run, -1); len(got) != width-7 {
				t.Fatalf("HandOut(-1) = %d units, want every ready unit left (%d)", len(got), width-7)
			}
			if n := len(handedNames(t, h, run)); n != width {
				t.Fatalf("handed_out records = %d, want %d", n, width)
			}
			if st := h.status(run); st.UnitCounts != (wf.UnitCounts{Handed: width}) {
				t.Fatalf("counts = %+v, want %d handed", st.UnitCounts, width)
			}
		})
	}
}

func TestHandOutReservedGateStopsBeforeMoreAgents(t *testing.T) {
	d := hoGateFan(2)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.handOut(run, 1)
	if len(ins) != 1 {
		t.Fatalf("HandOut(1) = %+v, want one agent", ins)
	}
	if err := h.e.Started(run, ins[0].Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil { // the first child now waits at its gate
		t.Fatal(err)
	}

	got := h.handOut(run, 1)
	if len(got) != 1 || got[0].Kind != wf.InstructionStopped || got[0].Question != "Keep?" || !strings.HasPrefix(got[0].Counts, "subagents ") {
		t.Fatalf("reserved gate did not stop additional handouts: %+v", got)
	}
	if again := h.handOut(run, 0); len(again) != 1 || again[0].Kind != wf.InstructionStopped {
		t.Fatalf("gate handoff did not repeat: %+v", again)
	}
	if n := len(handedNames(t, h, run)); n != 1 {
		t.Fatalf("reserved gate handed unrelated unit: %d handouts", n)
	}
}

func TestHandOutReservedGateAfterCoordinatorRecovery(t *testing.T) {
	d := wf.New("reserved-after-recovery")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, hoGateChildObserved(true))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	rt := newFakeRuntime()
	rt.set("x", "done", nil, false)
	h := newHarness(t, d, rt)
	run := h.run(d, nil)
	offered, err := h.e.(*wf.Engine).HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "old"})
	if err != nil || len(offered) != 1 {
		t.Fatalf("initial offer: %+v, %v", offered, err)
	}
	if err := h.e.Started(run, offered[0].Name, wf.Claim{Harness: "codex", Session: "old", Agent: "child"}); err != nil {
		t.Fatal(err)
	}
	rt.set("x", "done", nil, true)
	for attempt := 0; attempt < 2; attempt++ {
		got, err := h.e.(*wf.Engine).HandOutAs(run, -1, wf.Coordinator{Harness: "codex", Session: "new"})
		if err != nil || len(got) != 1 || got[0].Kind != wf.InstructionStopped || got[0].Question != "Keep?" {
			t.Fatalf("recovery poll %d: %+v, %v", attempt, got, err)
		}
		in := got[0]
		if !strings.Contains(in.Submit, " --child '"+in.Child+"' --visit 1") {
			t.Fatalf("unscoped answer: %s", in.Submit)
		}
		want := fmt.Sprintf("This run is waiting for a person at %s: Keep? Choices: yes, no. Ask the person who started the run and wait for their reply. Answer only with what they tell you; do not choose for them. Run this with their choice as <value>:\n%s", in.State, in.Submit)
		if in.Prompt != want {
			t.Fatalf("prompt = %q, want %q", in.Prompt, want)
		}
	}
	if n := len(handedNames(t, h, run)); n != 1 {
		t.Fatalf("handouts = %d, want only original claim", n)
	}
}
