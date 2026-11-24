package wf_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type recordedIntGate struct{}
type recordedIntCheck struct{}
type recordedIntDone struct{}
type recordedIntOutput struct {
	Value   int
	Present bool
}

func recordedIntDef(proposal int) *wf.Def {
	return recordedIntDefWithCallbacks(func(wf.Ctx) int { return proposal }, func(wf.Ctx) int { return 0 })
}

func recordedIntDefWithCallbacks(proposal, minimum func(wf.Ctx) int) *wf.Def {
	d := wf.New("recorded-int")
	gate := wf.NewGate[recordedIntGate](d, "Amount?", wf.NewEnum(d, "answer", "set", "skip"), wf.Reserved)
	gate.IntValue("set", 0, 100, proposal).IntMinimum("set", "floor", minimum)
	check := wf.NewAgent[recordedIntCheck, recordedIntOutput](d)
	done := wf.NewTerminal[recordedIntDone](d, wf.Success)
	gate.Answers(wf.On("set", gate.To(check, "set")), wf.On("skip", gate.To(check, "skip")))
	check.Spec(wf.PromptText("check"), wf.Retry[recordedIntCheck](wf.Const(1), check.To(done, "failed")), wf.Checkf("amount", func(out recordedIntOutput, c wf.Ctx) error {
		value, present := gate.AnswerValue(c)
		if out.Value != value || out.Present != present {
			return fmt.Errorf("amount mismatch: %d %t", value, present)
		}
		return nil
	}))
	check.Then(check.To(done, "done"))
	return d
}

func TestGateIntRecordedContextAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		value        *int
		want         recordedIntOutput
	}{
		{"default", "set", nil, recordedIntOutput{7, true}},
		{"explicit-zero", "set", new(int), recordedIntOutput{0, true}},
		{"plain-choice", "skip", nil, recordedIntOutput{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := recordedIntDef(7)
			h := newHarness(t, d, nil)
			run := h.run(d, nil)
			in := h.next(run)[0]
			if err := h.e.AnswerVisit(run, wf.GateAnswer{Gate: in.State, Child: in.Child, Visit: in.Visit, Value: tc.answer, IntValue: tc.value}); err != nil {
				t.Fatal(err)
			}
			out, _ := json.Marshal(tc.want)
			if got := h.submit(run, h.next(run)[0], string(out)); got.Class != wf.Accepted {
				t.Fatalf("submit: %+v", got)
			}
			records := h.records(run)
			entries, err := wf.Harvest(records, t.TempDir())
			if err != nil || len(entries) != 1 {
				t.Fatalf("harvest: %+v %v", entries, err)
			}
			reads := entries[0].Context.Checks[0].Reads
			if len(reads) != 1 || reads[0].Kind != "gateInt" || reads[0].Seq == 0 {
				t.Fatalf("reads: %+v", reads)
			}
			var saved *int
			if err := json.Unmarshal(reads[0].Value, &saved); err != nil {
				t.Fatal(err)
			}
			if (saved != nil) != tc.want.Present || (saved != nil && *saved != tc.want.Value) {
				t.Fatalf("saved amount: %s", reads[0].Value)
			}
			replayCapturedEntry(t, recordedIntDef(99), entries[0])
			for _, state := range wf.Inspect(d).States {
				if state.Name != entries[0].State {
					continue
				}
				failures, err := state.CheckRecordedOutput(json.RawMessage(`{"Value":-1,"Present":true}`), entries[0].Context)
				if err != nil || failures["amount"] == "" {
					t.Fatalf("calibration lost gate constraint: %v %v", failures, err)
				}
				old := reads[0].Value
				reads[0].Value = json.RawMessage(`"not-an-integer"`)
				if _, err := state.CheckRecordedOutput(out, entries[0].Context); err == nil {
					t.Fatal("accepted malformed integer context")
				}
				reads[0].Value = old
				oldState := reads[0].State
				reads[0].State = "recordedIntCheck"
				if _, err := state.CheckRecordedOutput(out, entries[0].Context); err == nil {
					t.Fatal("accepted integer context from non-gate state")
				}
				reads[0].State = oldState
			}
			rep, err := wf.Replay(records, recordedIntDef(99), "")
			if err != nil || len(rep.Differences) != 0 || rep.Stop.Kind != "ended" {
				t.Fatalf("replay with changed proposal: %+v %v", rep, err)
			}
		})
	}
}

func TestGateIntLegacyReplayKeepsAbsentValue(t *testing.T) {
	d := recordedIntDef(7)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	in := h.next(run)[0]
	if err := h.e.AnswerVisit(run, wf.GateAnswer{Gate: in.State, Child: in.Child, Visit: in.Visit, Value: "skip"}); err != nil {
		t.Fatal(err)
	}
	if got := h.submit(run, h.next(run)[0], `{"Value":0,"Present":false}`); got.Class != wf.Accepted {
		t.Fatalf("submit: %+v", got)
	}
	records := h.records(run)
	// An old gate answer had no typed payload even when its choice is now numeric.
	for i := range records {
		if records[i].Kind == wf.JournalGateAnswered {
			var data wf.GateAnsweredData
			_ = json.Unmarshal(records[i].Data, &data)
			data.Answer = "set"
			data.IntValue = nil
			records[i].Data, _ = json.Marshal(data)
		}
	}
	rep, err := wf.Replay(records, recordedIntDef(99), "")
	if err != nil || rep.Stop.Kind != "ended" {
		t.Fatalf("legacy replay: %+v %v", rep, err)
	}
	for _, diff := range rep.Differences {
		if diff.Kind == "refusal" || diff.State == "recordedIntCheck" {
			t.Fatalf("legacy missing amount synthesized: %+v", rep)
		}
	}
}

func TestGateIntPreAnswerAbsenceRecording(t *testing.T) {
	d := wf.New("pre-answer-int")
	check := wf.NewAgent[recordedIntCheck, bool](d)
	gate := wf.NewGate[recordedIntGate](d, "Amount?", wf.NewEnum(d, "answer", "set"), wf.Reserved)
	gate.IntValue("set", 0, 100, func(wf.Ctx) int { return 7 })
	done := wf.NewTerminal[recordedIntDone](d, wf.Success)
	check.Spec(wf.PromptText("check"), wf.Retry[recordedIntCheck](wf.Const(1), check.To(done, "failed")), wf.Checkf("absent", func(_ bool, c wf.Ctx) error {
		if value, present := gate.AnswerValue(c); value != 0 || present {
			return fmt.Errorf("unexpected prior answer: %d %t", value, present)
		}
		return nil
	}))
	check.Then(check.To(gate, "ask"))
	gate.Answers(wf.On("set", gate.To(done, "done")))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if got := h.submit(run, h.next(run)[0], `true`); got.Class != wf.Accepted {
		t.Fatalf("submit: %+v", got)
	}
	// A later answer must not alter the earlier check's known absence.
	in := h.next(run)[0]
	if err := h.e.AnswerVisit(run, wf.GateAnswer{Gate: in.State, Child: in.Child, Visit: in.Visit, Value: "set"}); err != nil {
		t.Fatal(err)
	}
	records := h.records(run)
	entries, err := wf.Harvest(records, t.TempDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("harvest: %+v %v", entries, err)
	}
	context := entries[0].Context.Checks[0]
	if len(context.Incomplete) != 0 || len(context.Reads) != 1 || context.Reads[0].Seq != 0 || string(context.Reads[0].Value) != "null" {
		t.Fatalf("absent context: %+v", context)
	}
	replayCapturedEntry(t, d, entries[0])
	// The absence claim is invalid if any earlier answer exists, including a
	// malformed answer that cannot supply a numeric producer.
	for _, data := range []json.RawMessage{json.RawMessage(`{"Answer":"set","IntValue":7}`), json.RawMessage(`{"Answer":"set"}`), json.RawMessage(`{"IntValue":"invalid"}`)} {
		tampered := append([]wf.JournalRecord(nil), records...)
		for i := range tampered {
			if tampered[i].Kind == wf.JournalAccepted {
				tampered = append(tampered, wf.JournalRecord{Run: run, Seq: tampered[i].Seq - 1, Kind: wf.JournalGateAnswered, State: "recordedIntGate", Data: data})
				break
			}
		}
		if _, err := wf.Harvest(tampered, t.TempDir()); err == nil {
			t.Fatalf("absence accepted despite earlier answer: %s", data)
		}
	}
}

func TestGateIntReplayDoesNotEvaluateCurrentCallbacks(t *testing.T) {
	d := recordedIntDef(7)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	in := h.next(run)[0]
	if err := h.e.AnswerVisit(run, wf.GateAnswer{Gate: in.State, Child: in.Child, Visit: in.Visit, Value: "set"}); err != nil {
		t.Fatal(err)
	}
	if got := h.submit(run, h.next(run)[0], `{"Value":7,"Present":true}`); got.Class != wf.Accepted {
		t.Fatalf("submit: %+v", got)
	}
	panicCallback := func(wf.Ctx) int { panic("offline replay evaluated current gate callback") }
	for _, callbacks := range []struct {
		name              string
		proposal, minimum func(wf.Ctx) int
	}{
		{"proposal", panicCallback, func(wf.Ctx) int { return 0 }},
		{"minimum", func(wf.Ctx) int { return 99 }, panicCallback},
	} {
		t.Run(callbacks.name, func(t *testing.T) {
			rep, err := wf.Replay(h.records(run), recordedIntDefWithCallbacks(callbacks.proposal, callbacks.minimum), "")
			if err != nil || len(rep.Differences) != 0 || rep.Stop.Kind != "ended" {
				t.Fatalf("replay: %+v %v", rep, err)
			}
		})
	}
}
