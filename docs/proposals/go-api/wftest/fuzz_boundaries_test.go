package wftest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestNamedHaltSkipsCoordinatorMetadata(t *testing.T) {
	for _, instance := range []string{"", "child#1"} {
		for _, boundary := range []bool{false, true} {
			j := wf.MemJournal()
			unlock, err := j.Lock("halt")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(wf.RefusedData{Halt: true, Reasons: []string{"named halt"}})
			records := []wf.JournalRecord{{Run: "halt", Kind: wf.JournalRefused, Instance: instance, State: "RunDir", Data: raw}}
			if boundary {
				records = append(records, wf.JournalRecord{Run: "halt", Kind: wf.JournalInstructionIssued, Instance: instance, State: "RunDir"})
			}
			records = append(records, wf.JournalRecord{Run: "halt", Kind: wf.JournalCoordinator})
			for _, record := range records {
				if err := j.Append(record); err != nil {
					t.Fatal(err)
				}
			}
			unlock()
			f := &fuzzRun{runner: &runner{j: j, cj: NewCrashJournal(j), s: New(wf.New("halt")), id: "halt"}}
			status := wf.RunStatus{Phase: wf.RunBlocked, Instances: []wf.InstanceStatus{{ID: instance, State: "RunDir", Phase: "blocked", Reason: "named halt"}}}
			if got := f.namedHalt(status); got == boundary {
				t.Fatalf("instance %q boundary=%v named=%v", instance, boundary, got)
			}
		}
	}
}

func TestUncertainManualVerificationIsHumanBoundary(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("manual")
		work := wf.NewCommand[uncertainWork](d, "work").Key("work")
		done := wf.NewTerminal[uncertainDone](d, wf.Success)
		work.Then(work.To(done, "done"))
		return New(d)
	}
	config := FuzzConfig{MaxSteps: 8, Fault: &FuzzFault{State: "uncertainWork", Class: "effect-uncertain", Adapter: "shell", Op: "run"}}
	result := Fuzz(factory, config)
	if result.Finding != nil || !result.FaultInjected || result.End != "human gate" {
		t.Fatalf("manual boundary rejected: %s", result)
	}
	if len(result.Advisories) != 1 || !strings.Contains(result.Advisories[0], "no Uncertain route: uncertain effect at uncertainWork ends in manual verification") {
		t.Fatalf("missing advisory: %+v", result.Advisories)
	}
	replay := result.Replay(factory)
	if replay.Finding != nil || replay.End != "human gate" || len(replay.Advisories) != 1 {
		t.Fatalf("manual boundary replay differs: %s", replay)
	}
	report := check(factory, CheckConfig{MaxRuns: 12, CalibrationConfig: CalibrationConfig{Seeds: 1, MaxSteps: 8}}, false)
	if len(report.Advisories) == 0 {
		t.Fatalf("Check omitted manual advisory: %s", report)
	}
}

func TestManualBoundaryRequiresNonTransientEffectWithoutRoute(t *testing.T) {
	for _, mode := range []string{"uncertain", "transient", "routed", "missing-intent", "waiting"} {
		t.Run(mode, func(t *testing.T) {
			d := wf.New("boundary")
			work := wf.NewCommand[uncertainWork](d, "work").Key("work")
			end := wf.NewTerminal[uncertainDone](d, wf.Success)
			work.Then(work.To(end, "done"))
			if mode == "routed" {
				d.OnFailure(wf.Uncertain, end)
			}
			view := wf.Inspect(d).States[0]
			f := &fuzzRun{good: true, sim: New(d), schemas: map[string]wf.StateView{"uncertainWork": view}, result: &FuzzResult{config: FuzzConfig{Liveness: true}}}
			in := wf.Instruction{Kind: wf.InstructionManualVerification, State: "uncertainWork", Effect: 2}
			switch mode {
			case "transient":
				in.Transient = true
			case "missing-intent":
				in.Effect = 0
			case "waiting":
				in.Kind = wf.InstructionWaiting
			}
			f.externalStop(in)
			if (f.result.Finding == nil) != (mode == "uncertain") {
				t.Fatalf("%s misclassified: %+v", mode, f.result)
			}
		})
	}
}
