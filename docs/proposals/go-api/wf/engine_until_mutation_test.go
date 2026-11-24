package wf_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func untilNoMutation(t *testing.T, h *harness, run wf.RunID, call func() error) {
	t.Helper()
	before := len(h.records(run))
	if err := call(); !errors.Is(err, wf.ErrRejected) {
		t.Errorf("mutation not rejected: %v", err)
	}
	if after := len(h.records(run)); after != before {
		t.Errorf("rejected mutation appended %d records", after-before)
	}
}

// Historical halt/recovery records can coexist with a fresh sibling offer.
// Replaying that frontier must not let operator verbs advance the sibling.
func TestUntilUnitRejectsSiblingRetryAndRecoveryWithoutWrites(t *testing.T) {
	for _, verb := range []string{"retry", "recover"} {
		t.Run(verb, func(t *testing.T) {
			d := fanDef("until-mutation", chItems(1, 2), 2, wf.AllSettled())
			h := newHarness(t, d, nil)
			h.manualRecovery = true
			run := h.run(d, nil)
			h.handOut(run, 0)
			var children []wf.InstanceStatus
			for _, in := range h.status(run).Instances {
				if in.ID != "" {
					children = append(children, in)
				}
			}
			sibling, target := children[0], children[1]
			appendUntilHalt(t, h, run, d, sibling)
			engine := h.e.(*wf.Engine)
			if err := engine.BindUntilUnit(run, wf.UntilUnitTarget{Instance: target.ID, State: target.State}); err != nil {
				t.Fatal(err)
			}
			state, err := engine.Status(run)
			if err != nil || len(state.Recoveries) != 1 {
				t.Fatalf("recovery fixture: %+v %v", state, err)
			}
			untilNoMutation(t, h, run, func() error {
				if verb == "retry" {
					return engine.Retry(run, sibling.ID, false)
				}
				return engine.Recover(run, state.Recoveries[0].Unit, wf.RecoveryRestart)
			})
		})
	}
}

func appendUntilHalt(t *testing.T, h *harness, run wf.RunID, d *wf.Def, in wf.InstanceStatus) {
	t.Helper()
	raw, err := json.Marshal(wf.RefusedData{Halt: true, Recovery: true, Exhausted: wf.ExhaustedBudget, Reasons: []string{"historical halt"}})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := h.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	err = h.j.Append(wf.JournalRecord{Run: run, Kind: wf.JournalRefused, Instance: in.ID, State: in.State, Attempt: in.Attempt, Class: wf.Refused, DefHash: d.Hash(), At: h.now, Data: raw})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestUntilUnitRejectsSettledTargetMutationsWithoutWrites(t *testing.T) {
	calls := 0
	d := untilDef(&calls)
	h := newHarness(t, d, nil)
	h.manualRecovery = true
	run := h.run(d, nil)
	engine := h.e.(*wf.Engine)
	if err := engine.BindUntilUnitName(run, "untilWork#1"); err != nil {
		t.Fatal(err)
	}
	ins, err := engine.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	appendUntilHalt(t, h, run, d, state.Instances[0])
	for name, call := range map[string]func() error{
		"retry":             func() error { return engine.Retry(run, "", false) },
		"recover":           func() error { return engine.Recover(run, ins[0].Name, wf.RecoveryRestart) },
		"heartbeat":         func() error { return engine.Heartbeat(run, wf.HeartbeatTarget{Name: ins[0].Name}, true) },
		"heartbeat-session": func() error { return engine.Heartbeat(run, wf.HeartbeatTarget{Session: "wave"}, false) },
		"submit": func() error {
			_, err := engine.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`true`)})
			return err
		},
		"settle":     func() error { return settleAs(engine, run, 1, true) },
		"settle-run": func() error { return settleRunAs(engine, run, 1, wf.RunResult{}) },
	} {
		t.Run(name, func(t *testing.T) { untilNoMutation(t, h, run, call) })
	}
}

type untilGate struct{}
type untilGateAnswer string

func TestUntilUnitRejectsSiblingGateWithoutWrites(t *testing.T) {
	child := wf.New("until-gate-child")
	work := wf.NewAgent[untilWork, bool](child)
	gate := wf.NewGate[untilGate, untilGateAnswer](child, "Go?", wf.NewEnum(child, "until-answer", untilGateAnswer("yes")), wf.Reserved)
	done := wf.NewTerminal[untilDone](child, wf.Success)
	edge := work.To(gate, "ready")
	work.Spec(wf.PromptText("work"), wf.Retry[untilWork](wf.Const(1), work.To(done, "stop")))
	work.Route(func(bool, wf.Ctx) wf.Edge[untilWork] { return edge }, edge)
	gate.Answers(wf.On(untilGateAnswer("yes"), gate.To(done, "done")))
	wf.Yield(done, func(wf.Ctx) chRes { return chRes{} })
	d := wf.New("until-gate-parent")
	fan := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string {
		if i.N == 1 {
			return "one"
		}
		return "two"
	}, wf.Typed[chItem, chRes](child))
	finish := wf.NewTerminal[untilDone](d, wf.Success)
	fan.Parallel(2).Join(wf.AllSettled(), fan.To(finish, "done"), fan.To(finish, "partial"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.handOut(run, 0)
	var children []wf.InstanceStatus
	for _, in := range h.status(run).Instances {
		if in.ID != "" {
			children = append(children, in)
		}
	}
	engine := h.e.(*wf.Engine)
	first, err := engine.Next(run, children[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Submit(run, wf.Submission{Child: children[0].ID, Token: first[0].Token, Output: json.RawMessage(`true`)}); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindUntilUnit(run, wf.UntilUnitTarget{Instance: children[1].ID, State: "untilWork"}); err != nil {
		t.Fatal(err)
	}
	untilNoMutation(t, h, run, func() error { return engine.Started(run, first[0].Name, wf.Claim{Transcript: "unrelated"}) })
	untilNoMutation(t, h, run, func() error { return engine.AttachUsage(run, first[0].Name, 123) })
	untilNoMutation(t, h, run, func() error { return engine.Answer(run, "until-gate-child/untilGate", "yes") })
	untilNoMutation(t, h, run, func() error {
		return engine.AnswerVisit(run, wf.GateAnswer{Child: children[0].ID, Gate: "until-gate-child/untilGate", Value: "yes"})
	})
}

func TestUntilUnitAllowsSettledTargetTelemetryWithoutProgression(t *testing.T) {
	calls := 0
	d := untilDef(&calls)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	engine := h.e.(*wf.Engine)
	if err := engine.BindUntilUnitName(run, "untilWork#1"); err != nil {
		t.Fatal(err)
	}
	ins, err := engine.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`true`)}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Started(run, ins[0].Name, wf.Claim{Transcript: "late-transcript"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.AttachUsage(run, ins[0].Name, 123); err != nil {
		t.Fatal(err)
	}
	if err := engine.Degrade(run, ins[0].Name, []wf.DegradedData{{Kind: wf.DegradedEscalated, Cause: "late evidence", Action: "recorded"}}); err != nil {
		t.Fatal(err)
	}
	if len(h.kind(run, wf.JournalMetadataAttached)) != 2 {
		t.Fatal("target late evidence was not recorded")
	}
	status, err := engine.UntilUnitStatus(run)
	if err != nil || !status.Settled {
		t.Fatalf("telemetry changed boundary: %+v %v", status, err)
	}
	if calls != 0 || len(h.kind(run, wf.JournalTransition)) != 0 || len(h.kind(run, wf.JournalEffectIntent)) != 0 {
		t.Fatal("late telemetry advanced target")
	}
}
