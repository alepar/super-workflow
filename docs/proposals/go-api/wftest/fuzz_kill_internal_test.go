package wftest

import (
	"encoding/json"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// TestKillPropertiesFlagWrongLosses: a killed unit that spends a retry, or is
// lost for a cause its kind does not expect, is a finding; another unit's
// losses are not.
func TestKillPropertiesFlagWrongLosses(t *testing.T) {
	data := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	cases := []struct {
		name string
		kind FuzzKillKind
		rec  wf.JournalRecord
		rule string
	}{
		{"spent retry", KillWhileRunning, wf.JournalRecord{Kind: wf.JournalRefused, Instance: "c", State: "Work", Data: data(wf.RefusedData{Spent: true})}, "kill-retry-spent"},
		{"died is not an orphan", KillWhileRunning, wf.JournalRecord{Kind: wf.JournalAttemptLost, Instance: "c", State: "Work", Data: data(wf.AttemptLostData{Name: "Work#1", Cause: wf.LostCauseDied})}, "kill-lost-cause"},
		{"a refused spawn loses nothing", KillSpawnRefused, wf.JournalRecord{Kind: wf.JournalAttemptLost, Instance: "c", State: "Work", Data: data(wf.AttemptLostData{Name: "Work#1", Cause: wf.LostCauseSpawn})}, "kill-lost-cause"},
		{"orphan is expected", KillWhileRunning, wf.JournalRecord{Kind: wf.JournalAttemptLost, Instance: "c", State: "Work", Data: data(wf.AttemptLostData{Name: "Work#1", Cause: wf.LostCauseOrphan})}, ""},
		{"another unit's retry", KillWhileRunning, wf.JournalRecord{Kind: wf.JournalRefused, Instance: "other", State: "Work", Data: data(wf.RefusedData{Spent: true})}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fuzzRun{
				sim:      &Sim{def: wf.New("d")},
				result:   &FuzzResult{config: FuzzConfig{Kill: &FuzzKill{State: "Work", Kind: c.kind}}, KillInjected: true, Trace: &Trace{journal: []wf.JournalRecord{c.rec}}},
				killName: "Work#1", killInstance: "c", killState: "Work",
			}
			f.killProperties()
			got := ""
			if f.result.Finding != nil {
				got = f.result.Finding.Rule
			}
			if got != c.rule {
				t.Errorf("finding %q, want %q", got, c.rule)
			}
		})
	}
}
