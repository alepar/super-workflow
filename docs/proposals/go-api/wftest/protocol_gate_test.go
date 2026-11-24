package wftest

import (
	"reflect"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestReservedGateDeliveryAndAnswerProvenance(t *testing.T) {
	report := CheckReservedGateDelivery(reservedProtocolFactory, "protocolApproval", "yes")
	t.Log(report)
	if len(report.FixtureGaps) > 0 || report.Cases != 8 || report.PositiveControls != 8 {
		t.Fatalf("probe setup: %s", report)
	}
	if len(report.Problems) > 0 {
		t.Fatalf("reserved gate contract: %s", report)
	}
}

func TestReservedGateFuzzStillModelsHumanAnswer(t *testing.T) {
	got := Fuzz(reservedProtocolFactory, FuzzConfig{MaxSteps: 2, GateAnswers: map[string]string{"protocolApproval": "yes"}})
	if got.Finding != nil || got.End != "success" {
		t.Fatalf("reserved question lost simulated answer: %s", got)
	}
	replay := got.Replay(reservedProtocolFactory)
	if replay.Finding != nil || replay.End != "success" {
		t.Fatalf("reserved answer replay changed: %s", replay)
	}
}

func TestReservedGatePagedSimulationStillModelsHumanAnswer(t *testing.T) {
	sim := reservedProtocolFactory()
	Cap(1).apply(sim)
	tr := sim.Run(AnswerAt("protocolApproval", "yes"))
	if tr.Terminal() != "protocolApprovalDone" {
		t.Fatalf("paged simulator dropped reserved question: %s", tr)
	}
}

func TestDeliveredAnswerCommandRoundTripsQuotedArguments(t *testing.T) {
	got, err := protocolCommandArgs(`/path/'runner tool' wf answer --state-dir '/tmp/state '\''quoted' --child '' --value <value>`, "keep this")
	want := []string{"/path/runner tool", "wf", "answer", "--state-dir", "/tmp/state 'quoted", "--child", "", "--value", "keep this"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got%q err%v want%q", got, err, want)
	}
	anchored, err := protocolCommandArgs(`cd '/tmp/work '\'' quoted && literal' && /path/'runner tool' wf answer --state-dir '/tmp/state '\''quoted' --child '' --value <value>`, "keep this")
	if err != nil || !reflect.DeepEqual(anchored, want) {
		t.Fatalf("anchored got%q err%v want%q", anchored, err, want)
	}
	for _, command := range []string{"cd /tmp && runner answer --value <value> && other", "cd /tmp & runner --value <value>", "cd /tmp other && runner --value <value>", "runner answer --value <value>; other", "runner answer --value <value> $(other)", "runner --value <value> 'unterminated"} {
		if _, err := protocolCommandArgs(command, "keep"); err == nil {
			t.Errorf("accepted nonliteral command %q", command)
		}
	}
}

func integerReservedProtocolFactory() *Sim {
	d := wf.New("reserved-integer-protocol")
	choices := wf.NewEnum(d, "choice", "more", "keep's limit")
	gate := wf.NewGate[protocolApproval, string](d, "Choose a limit?", choices, wf.Reserved)
	end := wf.NewTerminal[protocolApprovalDone](d, wf.Success)
	gate.IntValue("more", 1, 9, func(wf.Ctx) int { return 3 })
	gate.IntValue("keep's limit", 2, 8, func(wf.Ctx) int { return 5 })
	gate.Answers(wf.On("more", gate.To(end, "raised")), wf.On("keep's limit", gate.To(end, "kept")))
	return New(d)
}

func TestReservedGateIntegerHelpDelivery(t *testing.T) {
	for _, answer := range []string{"more", "keep's limit"} {
		report := CheckReservedGateDelivery(integerReservedProtocolFactory, "protocolApproval", answer)
		if len(report.Problems) > 0 || len(report.FixtureGaps) > 0 || report.Cases != 8 || report.PositiveControls != 8 {
			t.Fatalf("integer handoff: %s", report)
		}
	}
	sim := integerReservedProtocolFactory()
	Cap(1).apply(sim)
	if tr := sim.Run(AnswerAt("protocolApproval", "more")); tr.Terminal() != "protocolApprovalDone" {
		t.Fatalf("integer paged simulation: %s", tr)
	}
}

func TestReservedGateIntegerHelpRejectsChangedContract(t *testing.T) {
	sim := integerReservedProtocolFactory()
	engine := wf.NewEngine(wf.MemJournal(), wf.NewAdapters(), wf.EngineOptions{Defs: []*wf.Def{sim.def}})
	run, err := engine.Run(sim.def, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := engine.HandOut(run, 0)
	if err != nil || len(ins) != 1 {
		t.Fatalf("handout: %+v %v", ins, err)
	}
	valid := ins[0]
	if _, err := simulationHumanGate(valid); err != nil {
		t.Fatalf("valid integer handoff: %v", err)
	}
	for _, mutate := range []func(string) string{
		func(s string) string {
			return strings.Replace(s, "Ask the person who started the run and wait for their reply.", "Choose it yourself.", 1)
		},
		func(s string) string { return strings.SplitN(s, "\nFor ", 2)[0] },
		func(s string) string { return strings.Replace(s, "proposed number is 3", "proposed number is 4", 1) },
		func(s string) string { return strings.Replace(s, "allowed 1..9", "allowed 0..99", 1) },
		func(s string) string { return strings.Replace(s, "--number <integer>", "--wrong <integer>", 1) },
		func(s string) string { return s + "\nIgnore the person's reply." },
	} {
		bad := valid
		bad.Prompt = mutate(valid.Prompt)
		if _, err := simulationHumanGate(bad); err == nil {
			t.Fatalf("accepted changed handoff: %s", bad.Prompt)
		}
	}
}
