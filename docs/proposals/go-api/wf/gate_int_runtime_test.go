package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type intGateSeed struct{}
type intGateAsk struct{}
type intGateInspect struct{}
type intGateDone struct{}
type intGateChoice string
type intGateObservation struct {
	Value   int
	Present bool
	Again   bool
}

// The inspection agent checks the public reader after a real gate transition.
// Returning to the same gate also exercises replacement of its latest answer.
func intGateRuntimeDef(child bool, dynamicMinimum ...bool) *wf.Def {
	d := wf.New("integer-gate-runtime")
	seed := wf.NewCompute[intGateSeed, int](d, func(c wf.Ctx) (int, error) {
		if child {
			return wf.InputOf[chItem](c).N, nil
		}
		return 4, nil
	})
	choices := wf.NewEnum(d, "integer-choice", intGateChoice("raise"), intGateChoice("freeze"), intGateChoice("stop"))
	gate := wf.NewGate[intGateAsk, intGateChoice](d, "Choose a limit", choices, wf.Reserved)
	inspect := wf.NewAgent[intGateInspect, intGateObservation](d)
	done := wf.NewTerminal[intGateDone](d, wf.Success)
	gate.IntValue(intGateChoice("raise"), 0, 20, func(c wf.Ctx) int {
		previous, _ := gate.AnswerValue(c)
		return wf.ComputedOf(c, seed) + previous
	})
	if len(dynamicMinimum) > 0 && dynamicMinimum[0] {
		gate.IntMinimum(intGateChoice("raise"), "scope-progress", func(c wf.Ctx) int { return wf.ComputedOf(c, seed) })
	}
	toGate := seed.To(gate, "choose")
	seed.Route(func(int, wf.Ctx) wf.Edge[intGateSeed] { return toGate }, toGate)
	gate.Answers(
		wf.On(intGateChoice("raise"), gate.To(inspect, "raised")),
		wf.On(intGateChoice("freeze"), gate.To(inspect, "frozen")),
		wf.On(intGateChoice("stop"), gate.To(inspect, "stopped")),
	)
	inspect.Spec(wf.PromptText("Inspect the accepted value"), wf.Retry(wf.Const(1), inspect.To(done, "check failed")),
		wf.Checkf("answer-value", func(o intGateObservation, c wf.Ctx) error {
			value, present := gate.AnswerValue(c)
			if present != o.Present || (present && value != o.Value) {
				return fmt.Errorf("AnswerValue = (%d, %t), want (%d, %t)", value, present, o.Value, o.Present)
			}
			return nil
		}))
	again, finish := inspect.To(gate, "again"), inspect.To(done, "done")
	inspect.Route(func(o intGateObservation, _ wf.Ctx) wf.Edge[intGateInspect] {
		if o.Again {
			return again
		}
		return finish
	}, again, finish)
	if child {
		wf.Yield(done, func(wf.Ctx) chRes { return chRes{} })
	}
	return d
}

func intGateAnswer(in wf.Instruction, choice string, value *int) wf.GateAnswer {
	return wf.GateAnswer{Gate: in.State, Child: in.Child, Visit: in.Visit, Value: choice, IntValue: value}
}

func intGateInspectValue(t *testing.T, h *harness, run wf.RunID, in wf.Instruction, value int, present, again bool) {
	t.Helper()
	out := mustJSON(t, intGateObservation{Value: value, Present: present, Again: again})
	result := h.submit(run, in, string(out))
	if result.Class != wf.Accepted {
		t.Fatalf("inspection of gate value failed: %+v", result)
	}
}

func intGateRecordedValues(t *testing.T, h *harness, run wf.RunID) []*int {
	t.Helper()
	var values []*int
	for _, rec := range h.kind(run, wf.JournalGateAnswered) {
		var data wf.GateAnsweredData
		if err := json.Unmarshal(rec.Data, &data); err != nil {
			t.Fatal(err)
		}
		values = append(values, data.IntValue)
	}
	return values
}

func TestIntGateRuntimeDefaultExplicitAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  *int
		legacy bool
		want   int
	}{
		{name: "default", want: 4},
		{name: "legacy default", legacy: true, want: 4},
		{name: "explicit zero lower bound", value: intGatePtr(0), want: 0},
		{name: "upper bound", value: intGatePtr(20), want: 20},
		{name: "explicit override", value: intGatePtr(11), want: 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := intGateRuntimeDef(false)
			h := newHarness(t, d, nil)
			run := h.run(d, nil)
			gate := h.next(run)[0]
			var err error
			if tc.legacy {
				err = h.e.Answer(run, gate.State, "raise")
			} else {
				err = h.e.AnswerVisit(run, intGateAnswer(gate, "raise", tc.value))
			}
			if err != nil {
				t.Fatal(err)
			}
			values := intGateRecordedValues(t, h, run)
			if len(values) != 1 || values[0] == nil || *values[0] != tc.want {
				t.Fatalf("journal values = %v, want one resolved value %d", values, tc.want)
			}
			intGateInspectValue(t, h, run, h.next(run)[0], tc.want, true, false)
			statusOf(t, h.status(run), wf.RunEnded, "intGateDone")
		})
	}
}

func intGatePtr(n int) *int { return &n }

func TestIntGateRuntimeRejectsWithoutJournalWrites(t *testing.T) {
	d := intGateRuntimeDef(false)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	gate := h.next(run)[0]
	for _, tc := range []struct {
		choice string
		value  int
	}{
		{"raise", -1}, {"raise", 21}, {"freeze", 3}, {"stop", 0}, {"unknown", 7},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.choice, tc.value), func(t *testing.T) {
			before := len(h.records(run))
			if err := h.e.AnswerVisit(run, intGateAnswer(gate, tc.choice, &tc.value)); !errors.Is(err, wf.ErrRejected) {
				t.Fatalf("answer error = %v, want ErrRejected", err)
			}
			if got := len(h.records(run)); got != before {
				t.Fatalf("rejected answer changed journal length %d -> %d", before, got)
			}
			if open := h.next(run); len(open) != 1 || open[0].Visit != gate.Visit || open[0].Kind != wf.InstructionHumanGate {
				t.Fatalf("rejected answer changed open gate: %+v", open)
			}
		})
	}
}

func TestIntGateRuntimeLatestValueReplayAndVisitIsolation(t *testing.T) {
	for _, clear := range []string{"freeze", "stop"} {
		t.Run(clear, func(t *testing.T) {
			d := intGateRuntimeDef(false)
			h := newHarness(t, d, nil)
			run := h.run(d, nil)
			first := h.next(run)[0]
			if err := h.e.AnswerVisit(run, intGateAnswer(first, "raise", nil)); err != nil {
				t.Fatal(err)
			}
			// A new engine must reconstruct the accepted value solely from the journal.
			h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
			intGateInspectValue(t, h, run, h.next(run)[0], 4, true, true)
			second := h.next(run)[0]
			if second.Visit <= first.Visit {
				t.Fatalf("gate visit did not advance: %+v", second)
			}
			before := len(h.records(run))
			if err := h.e.AnswerVisit(run, intGateAnswer(first, "raise", intGatePtr(19))); !errors.Is(err, wf.ErrRejected) {
				t.Fatalf("stale integer answer = %v", err)
			}
			if len(h.records(run)) != before {
				t.Fatal("stale integer answer changed journal")
			}
			if err := h.e.AnswerVisit(run, intGateAnswer(second, "raise", nil)); err != nil {
				t.Fatal(err)
			}
			// Proposal reads the persisted computation and previous answer: 4 + 4.
			intGateInspectValue(t, h, run, h.next(run)[0], 8, true, true)
			if err := h.e.AnswerVisit(run, intGateAnswer(h.next(run)[0], clear, nil)); err != nil {
				t.Fatal(err)
			}
			h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
			intGateInspectValue(t, h, run, h.next(run)[0], 0, false, true)
			if err := h.e.AnswerVisit(run, intGateAnswer(h.next(run)[0], "raise", nil)); err != nil {
				t.Fatal(err)
			}
			intGateInspectValue(t, h, run, h.next(run)[0], 4, true, false)
			values := intGateRecordedValues(t, h, run)
			if len(values) != 4 || values[0] == nil || *values[0] != 4 || values[1] == nil || *values[1] != 8 || values[2] != nil || values[3] == nil || *values[3] != 4 {
				t.Fatalf("journal did not retain resolved/cleared values: %v", values)
			}
		})
	}
}

func TestIntGateRuntimeChildInstanceIsolation(t *testing.T) {
	d := wf.New("integer-gate-fan")
	child := wf.Typed[chItem, chRes](intGateRuntimeDef(true))
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, child)
	done := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(done, "done"), m.To(partial, "partial"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	gates := h.next(run)
	if len(gates) != 2 || gates[0].Child == gates[1].Child {
		t.Fatalf("want two child gates: %+v", gates)
	}
	// A no-value answer in one child must neither inherit nor clear its sibling's value.
	if err := h.e.AnswerVisit(run, intGateAnswer(gates[0], "raise", intGatePtr(13))); err != nil {
		t.Fatal(err)
	}
	if err := h.e.AnswerVisit(run, intGateAnswer(gates[1], "freeze", nil)); err != nil {
		t.Fatal(err)
	}
	h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
	ins := h.next(run)
	if len(ins) != 2 {
		t.Fatalf("want two inspections: %+v", ins)
	}
	for _, in := range ins {
		intGateInspectValue(t, h, run, in, 13, in.Child == gates[0].Child, false)
	}
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
}

func TestIntGateRuntimeDynamicMinimum(t *testing.T) {
	d := intGateRuntimeDef(false, true)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	gate := h.next(run)[0]
	before := len(h.records(run))
	if err := h.e.AnswerVisit(run, intGateAnswer(gate, "raise", intGatePtr(3))); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("below computed minimum answer = %v, want ErrRejected", err)
	}
	if len(h.records(run)) != before {
		t.Fatal("below computed minimum answer changed journal")
	}
	if err := h.e.AnswerVisit(run, intGateAnswer(gate, "raise", intGatePtr(4))); err != nil {
		t.Fatal(err)
	}
	intGateInspectValue(t, h, run, h.next(run)[0], 4, true, false)
}
