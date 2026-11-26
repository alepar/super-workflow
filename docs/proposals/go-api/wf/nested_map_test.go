package wf_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The nested Map guarantee (sw-fcsg): a Map whose child holds another Map.
// One fixture, a panel per candidate: the outer Map nmCand runs one child per
// candidate, and each child runs an inner Map nmSeat with one child per seat.
// The cases below pin identity, limits, results, budget and recovery.

type (
	nmCand  struct{} // outer Map
	nmSeat  struct{} // inner Map, in each candidate child
	nmVote  struct{} // the seat's agent
	nmYes   struct{} // seat terminals
	nmNo    struct{}
	nmMet   struct{} // candidate terminals
	nmShort struct{}
	nmAll   struct{} // run terminals
	nmSome  struct{}
)

type nmBallot struct{ OK bool }

// nmConfig shapes the fixture: the candidate keys, seats per candidate, both
// Parallel limits, and whether each seat writes an allocated artifact.
type nmConfig struct {
	cands              []string
	seats              int
	outerPar, innerPar int
	artifact           bool
}

// nmSeatChild votes once: OK true ends at nmYes, false at nmNo.
func nmSeatChild(artifact bool) wf.Child[int, bool] {
	d := wf.New("nm-seat")
	v := wf.NewAgent[nmVote, nmBallot](d)
	yes := wf.NewTerminal[nmYes](d, wf.Success)
	no := wf.NewTerminal[nmNo](d, wf.Stopped)
	d.OnFailure(wf.Blocked, no)
	if artifact {
		wf.NewArtifact[string](v, "ballot.md")
	}
	v.Spec(wf.PromptText("vote as seat {{item}}", wf.ChildInput()), wf.Retry[nmVote](wf.Const(1), v.To(no, "gave up")))
	toYes, toNo := v.To(yes, "yes"), v.To(no, "no")
	v.Route(func(b nmBallot, _ wf.Ctx) wf.Edge[nmVote] {
		if b.OK {
			return toYes
		}
		return toNo
	}, toYes, toNo)
	wf.Yield(yes, func(wf.Ctx) bool { return true })
	wf.Yield(no, func(wf.Ctx) bool { return false })
	return wf.Typed[int, bool](d)
}

// nmDef builds the nested fixture.
func nmDef(c nmConfig) *wf.Def {
	var seats []int
	for i := 1; i <= c.seats; i++ {
		seats = append(seats, i)
	}
	cd := wf.New("nm-candidate")
	inner := wf.NewMap[nmSeat, int, string, bool](cd, wf.FromValues(seats...), func(i int) string { return fmt.Sprintf("s%d", i) }, nmSeatChild(c.artifact))
	met := wf.NewTerminal[nmMet](cd, wf.Success)
	short := wf.NewTerminal[nmShort](cd, wf.Stopped)
	inner.Parallel(c.innerPar).Join(wf.AllOK(), inner.To(met, "panel agreed"), inner.To(short, "panel split"))
	wf.Yield(met, func(wf.Ctx) bool { return true })
	wf.Yield(short, func(wf.Ctx) bool { return false })

	d := wf.New("nm-panels")
	outer := wf.NewMap[nmCand, string, string, bool](d, wf.FromValues(c.cands...), func(s string) string { return s }, wf.Typed[string, bool](cd))
	all := wf.NewTerminal[nmAll](d, wf.Success)
	some := wf.NewTerminal[nmSome](d, wf.Stopped)
	outer.Parallel(c.outerPar).Join(wf.AllOK(), outer.To(all, "every panel agreed"), outer.To(some, "a panel split"))
	return d
}

// nmSeatID is the instance id of seat s of candidate c.
func nmSeatID(c string, s int) string { return fmt.Sprintf("nmCand[%s]/nmSeat[s%d]", c, s) }

// nmVoteNo answers every seat yes except the listed seat ids.
func nmVoteNo(no ...string) func(wf.Instruction) string {
	return func(in wf.Instruction) string {
		if slices.Contains(no, in.Child) {
			return `{"OK":false}`
		}
		return `{"OK":true}`
	}
}

// nmAcceptedIn decodes the accepted Map result of state in instance inst.
func nmAcceptedIn(t *testing.T, h *harness, run wf.RunID, state, inst string) wf.MapResult[string, bool] {
	t.Helper()
	var n int
	var mr wf.MapResult[string, bool]
	for _, r := range h.kind(run, wf.JournalAccepted) {
		if r.State != state || r.Instance != inst {
			continue
		}
		n++
		var ad wf.AcceptedData
		if err := json.Unmarshal(r.Data, &ad); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(ad.Output, &mr); err != nil {
			t.Fatal(err)
		}
	}
	if n != 1 {
		t.Fatalf("%d accepted records of %s in %q, want 1", n, state, inst)
	}
	return mr
}

// Identity and limits: each candidate's inner Parallel applies to that
// candidate alone, the outer Parallel bounds the candidates, and both are
// reached, not undershot: two candidates of two seats each run at once.
func TestNestedMapParallelPerOuterItem(t *testing.T) {
	d := nmDef(nmConfig{cands: []string{"c1", "c2", "c3"}, seats: 3, outerPar: 2, innerPar: 2})
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	most := 0
	for i := 0; i < 50; i++ {
		ins := h.next(run)
		if len(ins) == 0 {
			break
		}
		outer, inner := 0, map[string]int{}
		for id, in := range kids(h.status(run)) {
			if in.Phase == "done" || in.Phase == "budget_stopped" {
				continue
			}
			if parent, _, ok := strings.Cut(id, "/"); ok {
				inner[parent]++
			} else {
				outer++
			}
		}
		if outer > 2 {
			t.Fatalf("%d candidates active, outer Parallel is 2", outer)
		}
		for parent, n := range inner {
			if n > 2 {
				t.Fatalf("%d seats active in %s, inner Parallel is 2", n, parent)
			}
		}
		if len(ins) > 4 {
			t.Fatalf("%d instructions open, want at most 4 (2 candidates x 2 seats): %v", len(ins), childIDs(ins))
		}
		most = max(most, len(ins))
		h.submit(run, ins[0], `{"OK":true}`)
	}
	if most != 4 {
		t.Fatalf("most open at once = %d, want 4", most)
	}
	st := h.status(run)
	statusOf(t, st, wf.RunEnded, "nmAll")
	if n := len(kids(st)); n != 12 {
		t.Fatalf("children = %d, want 12 (3 candidates, 9 seats)", n)
	}
}

// Results: a seat that ends at a stopped terminal stops its own panel only.
// The inner Map result records the seat's terminal, the outer Map result
// records each candidate's terminal, and the run takes the outer join's
// partial edge.
func TestNestedMapStoppedTerminals(t *testing.T) {
	d := nmDef(nmConfig{cands: []string{"c1", "c2"}, seats: 2, outerPar: 2, innerPar: 2})
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if n := answerAll(h, run, nmVoteNo(nmSeatID("c2", 2))); n != 4 {
		t.Fatalf("answered %d, want 4 seats", n)
	}
	st := h.status(run)
	statusOf(t, st, wf.RunEnded, "nmSome")
	if st.Outcome != wf.Stopped {
		t.Fatalf("outcome = %s, want stopped", st.Outcome)
	}

	c1 := nmAcceptedIn(t, h, run, "nmSeat", "nmCand[c1]")
	c2 := nmAcceptedIn(t, h, run, "nmSeat", "nmCand[c2]")
	if len(c1.Items) != 2 || c1.Items["s1"].Terminal != "nmYes" || c1.Items["s2"].Terminal != "nmYes" {
		t.Fatalf("c1 panel = %+v, want both seats at nmYes", c1)
	}
	if c2.Items["s1"].Terminal != "nmYes" || !c2.Items["s1"].Result {
		t.Fatalf("c2 seat s1 = %+v, want nmYes with result true", c2.Items["s1"])
	}
	if it := c2.Items["s2"]; it.Terminal != "nmNo" || it.Result || it.Class == wf.Accepted {
		t.Fatalf("c2 seat s2 = %+v, want nmNo with result false", it)
	}

	var out wf.MapResult[string, bool]
	if err := json.Unmarshal(acceptedOf(h, run, "nmCand"), &out); err != nil {
		t.Fatal(err)
	}
	if it := out.Items["c1"]; it.Terminal != "nmMet" || !it.Result {
		t.Fatalf("candidate c1 = %+v, want nmMet with result true", it)
	}
	if it := out.Items["c2"]; it.Terminal != "nmShort" || it.Result {
		t.Fatalf("candidate c2 = %+v, want nmShort with result false", it)
	}
}

// Budget: the run budget is one budget across both levels. A seat the budget
// stops is marked BudgetStopped in its panel's result, never Refused, and its
// panel joins on what the other seats returned.
func TestNestedMapBudgetStop(t *testing.T) {
	d := nmDef(nmConfig{cands: []string{"c1", "c2"}, seats: 2, outerPar: 2, innerPar: 2})
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 3}})
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 3 {
		t.Fatalf("instructions = %v, want 3 (the fourth seat is stopped on the budget)", childIDs(ins))
	}
	for _, in := range ins {
		h.submit(run, in, `{"OK":true}`)
	}
	answerAll(h, run, okOut)

	stops := h.kind(run, wf.JournalBudgetStopped)
	if len(stops) != 1 {
		t.Fatalf("budget_stopped records = %+v, want 1", stops)
	}
	stopped := stops[0].Instance
	cand, seat, ok := strings.Cut(stopped, "/")
	if !ok {
		t.Fatalf("budget-stopped instance %q, want a seat", stopped)
	}
	st := h.status(run)
	if got := kids(st)[stopped].Phase; got != "budget_stopped" {
		t.Fatalf("stopped seat phase = %q", got)
	}
	panel := nmAcceptedIn(t, h, run, "nmSeat", cand)
	key := strings.TrimSuffix(strings.TrimPrefix(seat, "nmSeat["), "]")
	if it := panel.Items[key]; !it.BudgetStopped || it.Class == wf.Refused || it.Class == wf.Accepted || it.Terminal != "" {
		t.Fatalf("budget-stopped seat %s = %+v", key, it)
	}
	statusOf(t, st, wf.RunEnded, "nmSome")
	var out wf.MapResult[string, bool]
	if err := json.Unmarshal(acceptedOf(h, run, "nmCand"), &out); err != nil {
		t.Fatal(err)
	}
	ckey := strings.TrimSuffix(strings.TrimPrefix(cand, "nmCand["), "]")
	if it := out.Items[ckey]; it.Terminal != "nmShort" {
		t.Fatalf("candidate %s = %+v, want nmShort: its panel lost a seat to the budget", ckey, it)
	}
}

// Artifact paths: every seat of every candidate writes its allocated artifact
// at its own path under the run's records, including under an outer key that
// spells an inner instance id.
func TestNestedMapArtifactPaths(t *testing.T) {
	d := nmDef(nmConfig{cands: []string{"c1", "c1]/nmSeat[s1"}, seats: 2, outerPar: 2, innerPar: 2, artifact: true})
	runDir := t.TempDir()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }})
	run := h.run(d, nil)
	root := filepath.Join(runDir, "records", "artifacts") + string(filepath.Separator)
	paths, dirs := map[string]string{}, map[string]bool{}
	for i := 0; i < 20; i++ {
		ins := h.next(run)
		if len(ins) == 0 {
			break
		}
		for _, in := range ins {
			var path string
			for _, f := range strings.Fields(in.Prompt) {
				if strings.HasSuffix(f, "ballot.md") {
					path = f
				}
			}
			if !strings.HasPrefix(path, root) {
				t.Fatalf("%s: allocated path %q, want it under %s", in.Child, path, root)
			}
			if prev, ok := paths[path]; ok && prev != in.Child {
				t.Fatalf("%s and %s share the artifact path %s", prev, in.Child, path)
			}
			paths[path] = in.Child
			dirs[filepath.Dir(path)] = true
			if err := os.WriteFile(path, []byte(in.Child), 0o644); err != nil {
				t.Fatal(err)
			}
			if res := h.submit(run, in, `{"OK":true}`); res.Class == wf.Refused {
				t.Fatalf("%s refused with its artifact written: %+v", in.Child, res)
			}
		}
	}
	if len(paths) != 4 || len(dirs) != 4 {
		t.Fatalf("paths %v in %d directories, want 4 seats in 4 directories", paths, len(dirs))
	}
	statusOf(t, h.status(run), wf.RunEnded, "nmAll")
}

// nthCrashJournal dies at its at-th append once: before writing the record,
// or with after set, just after it is durable.
type nthCrashJournal struct {
	wf.Journal
	at, n int
	after bool
	fired bool
}

func (j *nthCrashJournal) Append(r wf.JournalRecord) error {
	j.n++
	if j.n != j.at {
		return j.Journal.Append(r)
	}
	j.fired = true
	if j.after {
		if err := j.Journal.Append(r); err != nil {
			return err
		}
	}
	return errCrash
}

// nmOutcome is what a nested run settles to: its terminal, the outer Map's
// accepted result, the spawned child ids in order, and how many times each
// state of each instance was accepted.
type nmOutcome struct {
	terminal string
	outer    string
	spawned  []string
	accepts  map[string]int
}

// nmDriveCrashing drives d to its end over a journal that dies at its at-th
// append (0: never; after: once the record is durable) and resumes on a fresh Engine after the crash, whether
// or not the verb reports it: a Submit whose decision is durable does not
// report a crash in the drive after it. It returns the outcome, the appends
// Run made, and the appends in all.
func nmDriveCrashing(t *testing.T, d *wf.Def, at int, after bool, out func(wf.Instruction) string) (o nmOutcome, started, appends int) {
	t.Helper()
	h := newHarness(t, d, nil)
	cj := &nthCrashJournal{Journal: h.j, at: at, after: after}
	fresh := func() {
		h.e = wf.NewEngine(cj, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }})
	}
	fresh()
	run, err := h.e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	started = cj.n
	crashed := false
	check := func(err error) bool {
		if cj.fired && !crashed {
			crashed = true
			fresh()
			return true
		}
		if err != nil {
			t.Fatalf("crash %d: %v", at, err)
		}
		return false
	}
loop:
	for i := 0; i < 200; i++ {
		ins, err := h.e.Next(run, "")
		if check(err) {
			continue
		}
		ins = withoutEnded(ins)
		if len(ins) == 0 {
			st, err := h.e.Status(run)
			if check(err) {
				continue
			}
			if st.Phase != wf.RunEnded {
				t.Fatalf("crash %d: nothing to do and the run is %s", at, st.Phase)
			}
			break
		}
		for _, in := range ins {
			if in.Kind != wf.InstructionAgent {
				t.Fatalf("crash %d: instruction %s of kind %v, want only agent work", at, in.Name, in.Kind)
			}
			_, err := h.e.Submit(run, wf.Submission{Token: in.Token, Output: json.RawMessage(out(in))})
			if check(err) {
				continue loop
			}
		}
	}
	if at > 0 && !crashed {
		t.Fatalf("crash %d never fired", at)
	}
	st, err := h.e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	o = nmOutcome{terminal: st.Terminal, outer: string(acceptedOf(h, run, "nmCand")), accepts: map[string]int{}}
	for _, r := range h.records(run) {
		switch r.Kind {
		case wf.JournalChildSpawned:
			var sp wf.ChildSpawnedData
			if err := json.Unmarshal(r.Data, &sp); err != nil {
				t.Fatal(err)
			}
			o.spawned = append(o.spawned, sp.ID)
		case wf.JournalAccepted:
			o.accepts[r.Instance+" "+r.State]++
		}
	}
	return o, started, cj.n
}

// Recovery: a crash before or after any one append of a nested run, resumed on a fresh
// Engine, settles exactly as the uncrashed run does: the same terminal, the
// same outer result, every child spawned once, every state accepted once.
func TestNestedMapCrashSweep(t *testing.T) {
	d := nmDef(nmConfig{cands: []string{"c1", "c2"}, seats: 2, outerPar: 2, innerPar: 1})
	out := nmVoteNo(nmSeatID("c2", 1))
	want, started, appends := nmDriveCrashing(t, d, 0, false, out)
	if want.terminal != "nmSome" || len(want.spawned) != 6 {
		t.Fatalf("uncrashed run = %+v, want nmSome with 6 children", want)
	}
	for id, n := range want.accepts {
		if n != 1 {
			t.Fatalf("uncrashed run accepted %s %d times", id, n)
		}
	}
	// A crash inside Run is the run's creation, covered elsewhere; the sweep
	// starts at the first append of the nested drive.
	for _, after := range []bool{false, true} {
		when := map[bool]string{false: "before", true: "after"}[after]
		for at := started + 1; at <= appends; at++ {
			got, _, _ := nmDriveCrashing(t, d, at, after, out)
			if got.terminal != want.terminal {
				t.Errorf("crash %s append %d: terminal %q, want %q", when, at, got.terminal, want.terminal)
			}
			if got.outer != want.outer {
				t.Errorf("crash %s append %d: outer result %s, want %s", when, at, got.outer, want.outer)
			}
			if !slices.Equal(got.spawned, want.spawned) {
				t.Errorf("crash %s append %d: spawned %v, want %v", when, at, got.spawned, want.spawned)
			}
			for id, n := range got.accepts {
				if n != 1 {
					t.Errorf("crash %s append %d: %s accepted %d times", when, at, id, n)
				}
			}
			if len(got.accepts) != len(want.accepts) {
				t.Errorf("crash %s append %d: %d accepted states, want %d", when, at, len(got.accepts), len(want.accepts))
			}
		}
	}
}
