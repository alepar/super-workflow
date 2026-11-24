package wf_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	rpWrite   struct{}
	rpGate    struct{}
	rpRepair  struct{}
	rpDone    struct{}
	rpStopped struct{}
)

// rpDef is the rung-3 shape in small: a writer, a gate over a read, and a
// stop on refusal; fixed, the refusal goes to a repair agent instead.
func rpDef(fixed bool) *wf.Def {
	d := wf.New("replay-probe")
	write := wf.NewAgent[rpWrite, string](d)
	obs := shell.Observe(shell.Run("cat", "tree.json"))
	gate := wf.NewCompute[rpGate, bool](d, func(c wf.Ctx) (bool, error) {
		r, err := wf.Observed(c, obs)
		return strings.Contains(r.Stdout, "unsound"), err
	}).Observes(obs)
	done := wf.NewTerminal[rpDone](d, wf.Success)
	stop := wf.NewTerminal[rpStopped](d, wf.Stopped)
	written := write.To(gate, "written")
	write.Spec(wf.PromptText("write"), wf.Retry[rpWrite](wf.Const(2), write.To(stop, "writer failed")))
	write.Route(func(string, wf.Ctx) wf.Edge[rpWrite] { return written }, written)
	sound := gate.To(done, "sound")
	refused := gate.To(stop, "gate refused")
	if fixed {
		repair := wf.NewAgent[rpRepair, string](d)
		refused = gate.To(repair, "gate refused: repair")
		repaired := repair.To(done, "repaired")
		repair.Spec(wf.PromptText("repair"), wf.Retry[rpRepair](wf.Const(1), repair.To(stop, "repair failed")))
		repair.Route(func(string, wf.Ctx) wf.Edge[rpRepair] { return repaired }, repaired)
	}
	gate.Route(func(bad bool, _ wf.Ctx) wf.Edge[rpGate] {
		if bad {
			return refused
		}
		return sound
	}, sound, refused)
	return d
}

// recordProbe records a run of the unfixed definition that dies at the gate,
// after one failed writer attempt, the way a kept e2e journal would.
func recordProbe(t *testing.T) []wf.JournalRecord {
	t.Helper()
	w := wftest.NewWorld()
	wftest.Set(w, shell.Observe(shell.Run("cat", "tree.json")), shell.Result{Stdout: "unsound"})
	tr := wftest.New(rpDef(false), wftest.WithWorld(w), wftest.WithTB(t)).Run(
		wftest.Fail[rpWrite, string](nil, wf.FailTransient, "tool crashed"),
		wftest.SubmitAt("rpWrite", "tree v1"),
	)
	if tr.Terminal() != "rpStopped" {
		t.Fatalf("recorded run ended at %q:\n%s", tr.Terminal(), tr)
	}
	return tr.Journal()
}

func TestReplaySameDefinitionRetracesTheRun(t *testing.T) {
	rep, err := wf.Replay(recordProbe(t), rpDef(false), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Differences) != 0 || rep.Retraced < 2 || rep.Then != rep.Now || rep.Stop.Kind != "ended" {
		t.Fatalf("same definition:\n%s", rep)
	}
	if rep.Died != "rpGate" || rep.Passed != "" || rep.Verdict != wf.VerdictFails || !strings.Contains(rep.String(), "the definition does not pass it") {
		t.Fatalf("an unchanged definition dies where the run did:\n%s", rep)
	}
}

// The fix takes the other route at the point of death, before any model runs.
func TestReplayShowsTheFixTakingTheOtherRoute(t *testing.T) {
	rep, err := wf.Replay(recordProbe(t), rpDef(true), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Differences) != 1 || rep.Then == rep.Now || rep.Verdict != wf.VerdictPasses {
		t.Fatalf("fixed definition:\n%s", rep)
	}
	d := rep.Differences[0]
	if d.Kind != "route" || d.State != "rpGate" || !strings.Contains(d.Then, "-> rpStopped") || !strings.Contains(d.Now, "-> rpRepair") {
		t.Fatalf("fixed definition:\n%s", rep)
	}
	// Past the point of death the run has nothing to feed the repair with.
	if rep.Stop.Kind != "unit" || rep.Stop.State != "rpRepair" {
		t.Fatalf("stop: %s", rep)
	}
	for _, want := range []string{"at rpGate", `then: -> rpStopped ("gate refused")`, `now:  -> rpRepair ("gate refused: repair")`,
		`died at rpGate; the definition passes it: rpGate -> rpRepair ("gate refused: repair")`} {
		if !strings.Contains(rep.String(), want) {
			t.Fatalf("report lacks %q:\n%s", want, rep)
		}
	}
}

// A definition that reads something the run never read stops there, naming it.
func TestReplayNamesAnUnrecordedRead(t *testing.T) {
	d := wf.New("replay-probe")
	write := wf.NewAgent[rpWrite, string](d)
	obs := shell.Observe(shell.Run("cat", "other.json"))
	gate := wf.NewCompute[rpGate, bool](d, func(c wf.Ctx) (bool, error) {
		_, err := wf.Observed(c, obs)
		return false, err
	}).Observes(obs)
	done := wf.NewTerminal[rpDone](d, wf.Success)
	stop := wf.NewTerminal[rpStopped](d, wf.Stopped)
	written := write.To(gate, "written")
	write.Spec(wf.PromptText("write"), wf.Retry[rpWrite](wf.Const(2), write.To(stop, "writer failed")))
	write.Route(func(string, wf.Ctx) wf.Edge[rpWrite] { return written }, written)
	end := gate.To(done, "sound")
	gate.Route(func(bool, wf.Ctx) wf.Edge[rpGate] { return end }, end)
	rep, err := wf.Replay(recordProbe(t), d, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stop.Kind != "read" || rep.Stop.State != "rpGate" || !strings.Contains(rep.Stop.Now, "other.json") {
		t.Fatalf("unrecorded read:\n%s", rep)
	}
	// converge E5: stopping for want of a record at the point of death says
	// nothing about whether the definition passes it.
	if rep.Verdict != wf.VerdictInconclusive || strings.Contains(rep.String(), "does not pass") ||
		!strings.Contains(rep.String(), `inconclusive: new read shell.run {"argv":["cat","other.json"]`) {
		t.Fatalf("an unrecorded read at the point of death reads as a verdict:\n%s", rep)
	}
}

// ReplayOptions.Observe answers a read the run never made, so a definition
// that added a read upstream still reaches the point a kept journal tests;
// the report lists each injected answer. A read it declines still stops the
// replay.
func TestReplayInjectsAnUnrecordedRead(t *testing.T) {
	d := wf.New("replay-probe")
	write := wf.NewAgent[rpWrite, string](d)
	obs := shell.Observe(shell.Run("cat", "other.json"))
	gate := wf.NewCompute[rpGate, bool](d, func(c wf.Ctx) (bool, error) {
		_, err := wf.Observed(c, obs)
		return false, err
	}).Observes(obs)
	done := wf.NewTerminal[rpDone](d, wf.Success)
	stop := wf.NewTerminal[rpStopped](d, wf.Stopped)
	written := write.To(gate, "written")
	write.Spec(wf.PromptText("write"), wf.Retry[rpWrite](wf.Const(2), write.To(stop, "writer failed")))
	write.Route(func(string, wf.Ctx) wf.Edge[rpWrite] { return written }, written)
	end := gate.To(done, "sound")
	gate.Route(func(bool, wf.Ctx) wf.Edge[rpGate] { return end }, end)
	asked := 0
	rep, err := wf.ReplayWith(recordProbe(t), d, "", wf.ReplayOptions{Observe: func(o wf.ObservationDesc) (json.RawMessage, bool) {
		asked++
		if argv, _ := o.Args["argv"].([]any); len(argv) == 2 && argv[1] == "other.json" {
			return json.RawMessage(`{"Exit":0,"Stdout":"","Stderr":""}`), true
		}
		return nil, false
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stop.Kind == "read" || len(rep.Injected) != 1 || !strings.Contains(rep.Injected[0], "other.json") || !strings.Contains(rep.String(), "injected (the run never read it):") {
		t.Fatalf("injected read:\n%s", rep)
	}
	if asked == 0 {
		t.Fatal("Observe was never asked")
	}
	declined, err := wf.ReplayWith(recordProbe(t), d, "", wf.ReplayOptions{Observe: func(wf.ObservationDesc) (json.RawMessage, bool) { return nil, false }})
	if err != nil || declined.Stop.Kind != "read" || len(declined.Injected) != 0 {
		t.Fatalf("a declined read: %+v, %v; want the replay stopped at it", declined, err)
	}
}

// converge E5: a fixture file answers the reads a run never recorded, so the
// CLI's `wf dev replay --observe` feeds what a test's Observe does. An entry
// matches by adapter, op and substrings of the read's arguments; the replay
// lists each read it answered.
func TestReplayFixtureAnswersUnrecordedReads(t *testing.T) {
	d := wf.New("replay-probe")
	write := wf.NewAgent[rpWrite, string](d)
	obs := shell.Observe(shell.Run("cat", "other.json"))
	gate := wf.NewCompute[rpGate, bool](d, func(c wf.Ctx) (bool, error) {
		_, err := wf.Observed(c, obs)
		return false, err
	}).Observes(obs)
	done := wf.NewTerminal[rpDone](d, wf.Success)
	stop := wf.NewTerminal[rpStopped](d, wf.Stopped)
	written := write.To(gate, "written")
	write.Spec(wf.PromptText("write"), wf.Retry[rpWrite](wf.Const(2), write.To(stop, "writer failed")))
	write.Route(func(string, wf.Ctx) wf.Edge[rpWrite] { return written }, written)
	end := gate.To(done, "sound")
	gate.Route(func(bool, wf.Ctx) wf.Edge[rpGate] { return end }, end)

	path := filepath.Join(t.TempDir(), "fixture.json")
	fixture := `[{"adapter": "shell", "op": "run", "contains": ["other.json"], "value": {"Exit": 0, "Stdout": "", "Stderr": ""}}]`
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	observe, err := wf.ReplayFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := wf.ReplayWith(recordProbe(t), d, "", wf.ReplayOptions{Observe: observe})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stop.Kind == "read" || len(rep.Injected) != 1 || rep.Verdict != wf.VerdictPasses {
		t.Fatalf("fixture-fed replay:\n%s", rep)
	}
	if err := os.WriteFile(path, []byte(`[{"op": "run"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wf.ReplayFixture(path); err == nil {
		t.Fatal("a fixture entry with no adapter or value was accepted")
	}
}

type rpA struct{}

// rpEdge is a one-state-name agent whose edge to done carries an effect and
// a label the tests change.
func rpEdge(cmd, lbl string) *wf.Def {
	d := wf.New("replay-edge")
	a := wf.NewAgent[rpA, string](d)
	done := wf.NewTerminal[rpDone](d, wf.Success)
	stop := wf.NewTerminal[rpStopped](d, wf.Stopped)
	a.Spec(wf.PromptText("write"), wf.Retry[rpA](wf.Const(1), a.To(stop, "failed")))
	edge := a.To(done, lbl).With(git.WorktreeAdd(wf.KeyOf("same-key"), cmd, "b", "main"))
	a.Route(func(string, wf.Ctx) wf.Edge[rpA] { return edge }, edge)
	return d
}

func recordEdge(t *testing.T) []wf.JournalRecord {
	t.Helper()
	tr := wftest.New(rpEdge("old-command", "old-route"), wftest.WithTB(t)).Run(wftest.SubmitAt("rpA", "x"))
	if tr.Terminal() != "rpDone" {
		t.Fatalf("recorded: %s", tr)
	}
	return tr.Journal()
}

type A struct{}

// A one-character root state names the point of death without a separator.
func TestReplayShortRootStateName(t *testing.T) {
	def := func() *wf.Def {
		d := wf.New("replay-short")
		a := wf.NewAgent[A, string](d)
		stop := wf.NewTerminal[rpStopped](d, wf.Stopped)
		a.Spec(wf.PromptText("write"), wf.Retry[A](wf.Const(1), a.To(stop, "failed")))
		end := a.To(stop, "stop")
		a.Route(func(string, wf.Ctx) wf.Edge[A] { return end }, end)
		return d
	}
	tr := wftest.New(def(), wftest.WithTB(t)).Run(wftest.SubmitAt("A", "x"))
	rep, err := wf.Replay(tr.Journal(), def(), "")
	if err != nil || rep.Died != "A" {
		t.Fatalf("short state: %v\n%s", err, rep)
	}
}

// A keyed effect whose arguments changed (its path, standing for a changed
// command) has no recorded result.
func TestReplayKeyedEffectWithChangedArgs(t *testing.T) {
	rep, err := wf.Replay(recordEdge(t), rpEdge("changed-command", "old-route"), "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stop.Kind != "effect" || !strings.Contains(rep.Stop.Now, "changed-command") {
		t.Fatalf("changed effect replayed from the old result:\n%s", rep)
	}
}

// The same target on a differently labelled edge is a different route.
func TestReplayLabelChangeIsADifference(t *testing.T) {
	rep, err := wf.Replay(recordEdge(t), rpEdge("old-command", "changed-route"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Differences) != 1 || !strings.Contains(rep.Differences[0].Then, "old-route") || !strings.Contains(rep.Differences[0].Now, "changed-route") {
		t.Fatalf("label change not reported:\n%s", rep)
	}
}

// Cross-review: two injected reads whose keys share their first 160 bytes
// (the same long script, a different last argument) are two entries.
func TestReplayInjectsReadsThatShareAPrefix(t *testing.T) {
	long := strings.Repeat("x", 200)
	d := wf.New("replay-probe")
	write := wf.NewAgent[rpWrite, string](d)
	a := shell.Observe(shell.Run("bash", "-c", long, "probe", "pr"))
	b := shell.Observe(shell.Run("bash", "-c", long, "probe", "design"))
	gate := wf.NewCompute[rpGate, bool](d, func(c wf.Ctx) (bool, error) {
		if _, err := wf.Observed(c, a); err != nil {
			return false, err
		}
		_, err := wf.Observed(c, b)
		return false, err
	}).Observes(a, b)
	done := wf.NewTerminal[rpDone](d, wf.Success)
	stop := wf.NewTerminal[rpStopped](d, wf.Stopped)
	written := write.To(gate, "written")
	write.Spec(wf.PromptText("write"), wf.Retry[rpWrite](wf.Const(2), write.To(stop, "writer failed")))
	write.Route(func(string, wf.Ctx) wf.Edge[rpWrite] { return written }, written)
	end := gate.To(done, "sound")
	gate.Route(func(bool, wf.Ctx) wf.Edge[rpGate] { return end }, end)
	rep, err := wf.ReplayWith(recordProbe(t), d, "", wf.ReplayOptions{Observe: func(o wf.ObservationDesc) (json.RawMessage, bool) {
		return json.RawMessage(`{"Exit":0,"Stdout":"","Stderr":""}`), true
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Injected) != 2 || !strings.Contains(rep.Injected[0], `"pr"`) || !strings.Contains(rep.Injected[1], `"design"`) {
		t.Fatalf("injected = %q, want both reads", rep.Injected)
	}
}

// ReplayOptions.SameKey answers a keyed effect whose arguments changed (text
// moved to stdin, say) with the recorded result, and lists it as rebound;
// off, the changed effect stops the replay as before.
func TestReplaySameKeyRebindsAChangedEffect(t *testing.T) {
	recs := recordEdge(t)
	off, err := wf.Replay(recs, rpEdge("new-command", "old-route"), "")
	if err != nil || off.Stop.Kind != "effect" {
		t.Fatalf("SameKey off: %+v, %v; want the replay stopped at the changed effect", off, err)
	}
	on, err := wf.ReplayWith(recs, rpEdge("new-command", "old-route"), "", wf.ReplayOptions{SameKey: true})
	if err != nil || on.Stop.Kind != "ended" || len(on.Rebound) != 1 || !strings.Contains(on.String(), "rebound (same key, other arguments)") {
		t.Fatalf("SameKey on:\n%s", on)
	}
}

// A recorded result the definition now refuses says so, naming the check:
// the replay is inconclusive there, and the line says why.
func TestReplayNamesTheCheckThatRefusesARecordedResult(t *testing.T) {
	d := wf.New("replay-probe")
	write := wf.NewAgent[rpWrite, string](d)
	obs := shell.Observe(shell.Run("cat", "tree.json"))
	gate := wf.NewCompute[rpGate, bool](d, func(c wf.Ctx) (bool, error) {
		r, err := wf.Observed(c, obs)
		return strings.Contains(r.Stdout, "unsound"), err
	}).Observes(obs)
	done := wf.NewTerminal[rpDone](d, wf.Success)
	stop := wf.NewTerminal[rpStopped](d, wf.Stopped)
	written := write.To(gate, "written")
	write.Spec(wf.PromptText("write"), wf.Retry[rpWrite](wf.Const(5), write.To(stop, "writer failed")),
		wf.Checkf("no-v1", func(s string, _ wf.Ctx) error {
			if s == "tree v1" {
				return errors.New("tree v1 is not allowed any more")
			}
			return nil
		}))
	write.Route(func(string, wf.Ctx) wf.Edge[rpWrite] { return written }, written)
	end := gate.To(done, "sound")
	gate.Route(func(bool, wf.Ctx) wf.Edge[rpGate] { return end }, end)
	rep, err := wf.Replay(recordProbe(t), d, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != wf.VerdictInconclusive || !strings.Contains(rep.String(), "inconclusive: recorded result refused by no-v1: tree v1 is not allowed any more") {
		t.Fatalf("a refused recorded result:\n%s", rep)
	}
}

// Cross-review: a renamed edge into the same stop at the point of death is
// a difference, not a pass.
func TestReplayLabelOnlyChangeAtDeathDoesNotPass(t *testing.T) {
	d := wf.New("replay-probe")
	write := wf.NewAgent[rpWrite, string](d)
	obs := shell.Observe(shell.Run("cat", "tree.json"))
	gate := wf.NewCompute[rpGate, bool](d, func(c wf.Ctx) (bool, error) {
		r, err := wf.Observed(c, obs)
		return strings.Contains(r.Stdout, "unsound"), err
	}).Observes(obs)
	done := wf.NewTerminal[rpDone](d, wf.Success)
	stop := wf.NewTerminal[rpStopped](d, wf.Stopped)
	written := write.To(gate, "written")
	write.Spec(wf.PromptText("write"), wf.Retry[rpWrite](wf.Const(2), write.To(stop, "writer failed")))
	write.Route(func(string, wf.Ctx) wf.Edge[rpWrite] { return written }, written)
	sound, refused := gate.To(done, "sound"), gate.To(stop, "gate refused, renamed")
	gate.Route(func(bad bool, _ wf.Ctx) wf.Edge[rpGate] {
		if bad {
			return refused
		}
		return sound
	}, sound, refused)
	rep, err := wf.Replay(recordProbe(t), d, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != wf.VerdictFails || len(rep.Differences) != 1 {
		t.Fatalf("a label-only change at the point of death:\n%s", rep)
	}
}
