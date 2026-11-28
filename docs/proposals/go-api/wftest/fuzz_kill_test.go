package wftest_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/dispatch"
	tourchecker "github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour/checker"
	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// killRun injects kind at state of pagedDef (a Map of three children, then a
// final agent) in an all-good run and requires it recovers to success.
func killRun(t *testing.T, state string, kind wftest.FuzzKillKind) *wftest.FuzzResult {
	t.Helper()
	d, _, _, _ := pagedDef()
	ok := []json.RawMessage{json.RawMessage(`{"OK":true}`)}
	progress := map[string][]json.RawMessage{"pg-item/pgWork": ok, "pgFinal": ok}
	r := wftest.Fuzz(func() *wftest.Sim { return wftest.New(d) }, wftest.FuzzConfig{
		MaxSteps: 64, Valid: progress, Progress: progress, Liveness: true,
		Kill: &wftest.FuzzKill{State: state, Kind: kind},
	})
	if !r.KillInjected {
		t.Fatalf("%s at %s was not injected:\n%s", kind, state, r)
	}
	if r.Finding != nil || r.End != "success" {
		t.Fatalf("%s at %s ended %s, want success:\n%s", kind, state, r.End, r)
	}
	return r
}

// journaled counts the records of kind whose data has every key=value in want.
func journaled(r *wftest.FuzzResult, kind wf.JournalKind, want map[string]string) int {
	n := 0
	for _, rec := range r.Trace.Journal() {
		if rec.Kind != kind {
			continue
		}
		var data map[string]any
		if json.Unmarshal(rec.Data, &data) != nil {
			continue
		}
		match := true
		for k, v := range want {
			if s, _ := data[k].(string); s != v {
				match = false
			}
		}
		if match {
			n++
		}
	}
	return n
}

func TestKillEveryKindRecoversAtEveryState(t *testing.T) {
	for _, state := range []string{"pg-item/pgWork", "pgFinal"} {
		for _, kind := range wftest.FuzzKillKinds {
			t.Run(state+"/"+string(kind), func(t *testing.T) {
				r := killRun(t, state, kind)
				if n := r.Trace.RetriesSpent(); n != 0 {
					t.Errorf("retries spent = %d, want 0", n)
				}
			})
		}
	}
}

func TestKillCoordinatorDeathReplacesTheSession(t *testing.T) {
	for _, kind := range []wftest.FuzzKillKind{wftest.KillAfterHandOut, wftest.KillAfterStarted, wftest.KillWhileRunning, wftest.KillAfterDone} {
		t.Run(string(kind), func(t *testing.T) {
			r := killRun(t, "pg-item/pgWork", kind)
			if n := journaled(r, wf.JournalCoordinator, map[string]string{"Session": "fuzz-session-2"}); n == 0 {
				t.Errorf("the replacement session never bound the run")
			}
		})
	}
}

func TestKillWhileRunningLosesAnOrphan(t *testing.T) {
	r := killRun(t, "pg-item/pgWork", wftest.KillWhileRunning)
	if got := r.Trace.Lost(); len(got) != 1 || !strings.HasSuffix(got[0], ": "+wf.LostCauseOrphan) {
		t.Errorf("lost = %v, want one session orphan", got)
	}
}

func TestKillChildDiedIsADiedLoss(t *testing.T) {
	r := killRun(t, "pg-item/pgWork", wftest.KillChildDied)
	if got := r.Trace.Lost(); len(got) != 1 || !strings.HasSuffix(got[0], ": "+wf.LostCauseDied) {
		t.Errorf("lost = %v, want one died loss", got)
	}
}

func TestKillSpawnRefusedOffersTheUnitAgain(t *testing.T) {
	r := killRun(t, "pgFinal", wftest.KillSpawnRefused)
	if got := r.Trace.Lost(); len(got) != 0 {
		t.Errorf("lost = %v, want nothing: a refused spawn is offered again", got)
	}
	var names []string
	for _, rec := range r.Trace.Journal() {
		var d struct{ Name string }
		if rec.Kind == wf.JournalHandedOut && json.Unmarshal(rec.Data, &d) == nil && strings.HasPrefix(d.Name, "pgFinal") {
			names = append(names, d.Name)
		}
	}
	if len(names) < 2 {
		t.Errorf("pgFinal handed out %v, want the refused hand-out and the offer after it", names)
	}
}

func TestKillTurnEndedIsAStallAnswered(t *testing.T) {
	r := killRun(t, "pgFinal", wftest.KillTurnEnded)
	if n := journaled(r, wf.JournalHeartbeat, map[string]string{"Status": "stalled", "By": "engine"}); n == 0 {
		t.Errorf("the silent child was never reported stalled")
	}
	if n := journaled(r, wf.JournalHeartbeat, map[string]string{"Status": "active", "By": "coordinator"}); n == 0 {
		t.Errorf("the next turn never answered the stall")
	}
	if got := r.Trace.Lost(); len(got) != 0 {
		t.Errorf("lost = %v, want nothing: a stall is never a loss", got)
	}
}

// dispatchCheck is the dispatch example under Check, with three items.
func dispatchCheck(cfg wftest.CheckConfig) *wftest.CheckReport {
	factory := func() *wftest.Sim {
		w := wftest.NewWorld()
		wftest.Set(w, fsx.ReadJSON[[]dispatch.Item]("work/items.json", "/items"), []dispatch.Item{{Name: "a"}, {Name: "b"}, {Name: "c"}})
		return wftest.New(dispatch.Definition(), wftest.WithWorld(w))
	}
	progress := map[string][]json.RawMessage{"dispatch-item/Review": {json.RawMessage(`{"Reviewed":true}`)}}
	cfg.CalibrationConfig = wftest.CalibrationConfig{Valid: progress, Progress: progress, Seeds: 2, MaxSteps: 64}
	return wftest.Check(factory, cfg)
}

// TestCheckKillsBudget: a negative Kills leaves the phase out; a positive one
// runs it on its own budget when MaxRuns is spent before it.
func TestCheckKillsBudget(t *testing.T) {
	if r := dispatchCheck(wftest.CheckConfig{Kills: -1}); r.KillSeeds != 0 {
		t.Errorf("Kills -1 made %d kill-recovery runs, want none", r.KillSeeds)
	}
	starved := dispatchCheck(wftest.CheckConfig{MaxRuns: 4})
	if starved.KillSeeds != 0 {
		t.Fatalf("MaxRuns 4 left %d runs for kills; the test needs it spent before them", starved.KillSeeds)
	}
	own := dispatchCheck(wftest.CheckConfig{MaxRuns: 4, Kills: 64})
	if own.KillSeeds < len(wftest.FuzzKillKinds) {
		t.Errorf("Kills 64 made %d kill-recovery runs, want at least %d", own.KillSeeds, len(wftest.FuzzKillKinds))
	}
	if own.Runs <= starved.Runs {
		t.Errorf("runs %d with a kill budget, %d without: the kill budget is not counted", own.Runs, starved.Runs)
	}
}

// TestCheckSweepsKillsAtEveryAgentState: Check injects every kind at the
// agent state of the dispatch example (a Map of one-agent children) and
// reports none unreached and nothing found.
func TestCheckSweepsKillsAtEveryAgentState(t *testing.T) {
	r := dispatchCheck(wftest.CheckConfig{})
	if r.KillSeeds < len(wftest.FuzzKillKinds) {
		t.Errorf("kill-recovery runs = %d, want at least %d:\n%s", r.KillSeeds, len(wftest.FuzzKillKinds), r)
	}
	if len(r.UnreachedKills) != 0 {
		t.Errorf("unreached kills: %v", r.UnreachedKills)
	}
	for _, f := range r.Findings {
		if f.KillInjected {
			t.Errorf("a kill run found a problem:\n%s", f)
		}
	}
	if !strings.Contains(r.String(), "kill-recovery runs: ") {
		t.Errorf("report does not count kill-recovery runs:\n%s", r)
	}
}

// TestBudgetStopMidFanOutSettlesTheUnstartedSiblings pins today's behaviour
// (wf engine_children.go stopOnBudget; whether it should change is sw-tj3y):
// a run budget spent after the first of three fan-out children is let out
// stops the two siblings not started, which journal budget_stopped and
// settle Unavailable for the join. The join then takes its partial edge, so
// the run ends at its stopped terminal rather than pausing for a resume.
func TestBudgetStopMidFanOutSettlesTheUnstartedSiblings(t *testing.T) {
	w := wftest.NewWorld()
	wftest.Set(w, fsx.ReadJSON[[]dispatch.Item]("work/items.json", "/items"), []dispatch.Item{{Name: "a"}, {Name: "b"}, {Name: "c"}})
	progress := map[string][]json.RawMessage{"dispatch-item/Review": {json.RawMessage(`{"Reviewed":true}`)}}
	r := wftest.Fuzz(func() *wftest.Sim { return wftest.New(dispatch.Definition(), wftest.WithWorld(w)) }, wftest.FuzzConfig{
		MaxSteps: 64, Valid: progress, Progress: progress, FailPercent: -1, SchemaPercent: -1, RunBudget: wf.RunBudget{Turns: 1},
	})
	if r.Finding != nil || r.End != "stopped" || r.Status.Terminal != "Partial" {
		t.Fatalf("end %q at %q, want the run ended at Partial:\n%s", r.End, r.Status.Terminal, r)
	}
	stopped, settled := map[string]bool{}, map[string]bool{}
	for _, rec := range r.Trace.Journal() {
		switch rec.Kind {
		case wf.JournalBudgetStopped:
			if rec.Instance != "" {
				stopped[rec.Instance] = true
			}
		case wf.JournalChildSettled:
			var d wf.ChildSettledData
			if json.Unmarshal(rec.Data, &d) == nil {
				settled[d.ID] = true
			}
		}
	}
	if len(stopped) != 2 {
		t.Errorf("budget-stopped children = %v, want the two siblings not started", stopped)
	}
	for id := range stopped {
		if !settled[id] {
			t.Errorf("budget-stopped child %s did not settle for the join", id)
		}
	}
}

// TestCheckKillsGoBreadthFirst: a kill budget of one run per agent state
// gives each of the tour's three agent states one kill, so the first target
// it refuses is the first state's second kind.
func TestCheckKillsGoBreadthFirst(t *testing.T) {
	factory, cfg := tourchecker.Scenario()
	cfg.Seeds = 1
	r := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: cfg, Kills: 3})
	if r.KillSeeds != 3 {
		t.Fatalf("kill-recovery runs = %d, want 3:\n%s", r.KillSeeds, r)
	}
	want := "kill Plan " + string(wftest.FuzzKillKinds[1])
	if !slices.ContainsFunc(r.NotExplored, func(s string) bool { return strings.HasSuffix(s, want) }) {
		t.Errorf("not explored %v, want it to start at %q", r.NotExplored, want)
	}
}
