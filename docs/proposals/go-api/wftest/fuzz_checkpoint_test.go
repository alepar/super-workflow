package wftest

import (
	"encoding/json"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type checkpointWork struct{}
type checkpointDone struct{}
type checkpointStop struct{}

func checkpointFactory() *Sim {
	d := wf.New("checkpoint")
	work := wf.NewAgent[checkpointWork, bool](d)
	done := wf.NewTerminal[checkpointDone](d, wf.Success)
	stop := wf.NewTerminal[checkpointStop](d, wf.Stopped)
	work.Spec(wf.PromptText("answer"), wf.Retry[checkpointWork](wf.Const(2), work.To(stop, "exhausted")))
	work.Then(work.To(done, "done"))
	return New(d)
}

func recoveryCapPrefix() []FuzzAction {
	var prefix []FuzzAction
	for i := 0; i < 5; i++ {
		prefix = append(prefix, FuzzAction{State: "checkpointWork", Kind: FuzzEndedWithoutDone}, FuzzAction{Kind: FuzzRecover, Output: json.RawMessage(`"restart"`)})
	}
	return append(prefix, FuzzAction{Kind: FuzzRecover, Output: json.RawMessage(`"restart"`)}, FuzzAction{State: "checkpointWork", Kind: FuzzEndedWithoutDone}, FuzzAction{Kind: FuzzRecover, Output: json.RawMessage(`"restart"`)})
}

// Five F1 restarts exhaust the lost cap; the next B2 restart grants one final
// attempt. A later F1 restart must take the declared stop, even with good output
// available. A random all-good checkpoint belongs before that reserve is spent.
func TestRecoveryCheckpointExcludesSpentVisitFromRealJournal(t *testing.T) {
	result := Fuzz(checkpointFactory, FuzzConfig{Seed: 17, MaxSteps: 20, Prefix: recoveryCapPrefix(), Valid: map[string][]json.RawMessage{"checkpointWork": {json.RawMessage(`true`)}}})
	if result.Status.Terminal != "checkpointStop" {
		t.Fatalf("prefix did not spend recovery reserve: %s", result)
	}
	status := wf.RunStatus{Phase: wf.RunRunning, Instances: []wf.InstanceStatus{{State: "checkpointWork", Phase: "issued"}}}
	records := result.Trace.Journal()
	sawF1, sawB2 := false, false
	for i, record := range records {
		if record.Kind == wf.JournalRecoveryOffered && !sawB2 {
			sawF1 = true
			if !recoveryCheckpointEligible(status, records[:i+1]) {
				t.Fatal("F1 offer has no Restart count, but is still recoverable")
			}
		}
		if record.Kind == wf.JournalRetried {
			sawB2 = true
			if !recoveryCheckpointEligible(status, records[:i]) {
				t.Fatal("pending B2 restart must remain eligible")
			}
			if recoveryCheckpointEligible(status, records[:i+1]) {
				t.Fatal("checkpoint after B2 restart spent current visit's reserve")
			}
		}
	}
	if !sawF1 || !sawB2 {
		t.Fatalf("missing actual recovery lifecycle: F1=%v B2=%v", sawF1, sawB2)
	}
}

func checkpointRecord(kind wf.JournalKind, instance, state string, data any) wf.JournalRecord {
	raw, _ := json.Marshal(data)
	return wf.JournalRecord{Kind: kind, Instance: instance, State: state, Data: raw}
}

func TestRecoveryCheckpointTracksVisitsAndInstances(t *testing.T) {
	spent := []wf.JournalRecord{
		checkpointRecord(wf.JournalRefused, "kid1", "Work", wf.RefusedData{Halt: true, Recovery: true, Exhausted: wf.ExhaustedBudget}),
		checkpointRecord(wf.JournalRecoveryAnswered, "kid1", "Work", wf.RecoveryAnsweredData{Choice: wf.RecoveryRestart}),
		checkpointRecord(wf.JournalRetried, "kid1", "Work", wf.RetriedData{Child: "kid1"}),
	}
	status := wf.RunStatus{Phase: wf.RunRunning, Instances: []wf.InstanceStatus{{ID: "kid1", State: "Work", Phase: "issued"}, {ID: "kid2", State: "Work", Phase: "issued"}}}
	if recoveryCheckpointEligible(status, spent) {
		t.Fatal("spent active child visit accepted")
	}
	// A transitioned state has a new per-visit reserve, but run-wide counts remain.
	transition := checkpointRecord(wf.JournalTransition, "kid1", "Work", wf.TransitionData{To: "Work"})
	firstVisit := append(append([]wf.JournalRecord{}, spent...), transition)
	if !recoveryCheckpointEligible(status, firstVisit) {
		t.Fatal("new visit lost its remaining run-wide restart")
	}
	bothVisits := append(append(append([]wf.JournalRecord{}, firstVisit...), spent...), transition)
	if recoveryCheckpointEligible(status, bothVisits) {
		t.Fatal("run-wide restart cap reset by transition")
	}
	// A settled child's used reserve does not constrain a sibling's recovery.
	settled := append(append([]wf.JournalRecord{}, spent...), checkpointRecord(wf.JournalChildSettled, "", "Fan", wf.ChildSettledData{ID: "kid1"}))
	if !recoveryCheckpointEligible(status, settled) {
		t.Fatal("settled child incorrectly blocks active sibling")
	}
	status.Instances = status.Instances[1:]
	if !recoveryCheckpointEligible(status, spent) {
		t.Fatal("same state name conflated distinct child instances")
	}
}

func TestRecoveryCheckpointDoesNotTreatOrdinaryRetryAsExhaustion(t *testing.T) {
	status := wf.RunStatus{Phase: wf.RunRunning, Instances: []wf.InstanceStatus{{State: "Work", Phase: "issued"}}}
	records := []wf.JournalRecord{
		checkpointRecord(wf.JournalRefused, "", "Work", wf.RefusedData{Halt: true}),
		checkpointRecord(wf.JournalRetried, "", "Work", wf.RetriedData{}),
	}
	if !recoveryCheckpointEligible(status, records) {
		t.Fatal("ordinary operator retry spent an exhaustion recovery reserve")
	}
}

func TestRandomLivenessCheckpointPrecedesSpentRecoveryReserve(t *testing.T) {
	valid := map[string][]json.RawMessage{"checkpointWork": {json.RawMessage(`true`)}}
	for seed := int64(0); seed < 24; seed++ {
		cfg := FuzzConfig{Seed: seed, MaxSteps: 20, Prefix: recoveryCapPrefix(), Valid: valid, Progress: valid, Liveness: true, LivenessAfter: -1}
		result := Fuzz(checkpointFactory, cfg)
		if result.Finding != nil || result.Status.Outcome != wf.Success {
			t.Fatalf("seed %d start=%d: %s", seed, result.LivenessStart, result)
		}
		if result.LivenessStart == 0 {
			continue
		}
		// Replay just the hostile prefix up to the selected boundary. This inspects
		// the actual journal/status at phase entry rather than inferring budget
		// state from the later all-good run (which may legitimately spend a restart).
		snapshot := Fuzz(checkpointFactory, FuzzConfig{Seed: seed, MaxSteps: result.LivenessStart, Prefix: recoveryCapPrefix(), Valid: valid})
		if !recoveryCheckpointEligible(snapshot.Status, snapshot.Trace.Journal()) {
			t.Fatalf("seed %d selected spent checkpoint %d", seed, result.LivenessStart)
		}
	}
}

func TestExplicitLivenessCheckpointReportsSpentPrefix(t *testing.T) {
	valid := map[string][]json.RawMessage{"checkpointWork": {json.RawMessage(`true`)}}
	// Step 10 answers the exhaustion offer; step 11 ends that final attempt.
	// Step 12 therefore cannot start a phase promised fresh recovery headroom.
	result := Fuzz(checkpointFactory, FuzzConfig{Seed: 17, MaxSteps: 20, Prefix: recoveryCapPrefix(), Valid: valid, Progress: valid, Liveness: true, LivenessAfter: 12})
	if result.Finding == nil || result.Finding.Rule != "checkpoint-ineligible" {
		t.Fatalf("spent prefix mislabeled as a workflow liveness defect: %s", result)
	}
}
