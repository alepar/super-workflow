package wf_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/agentless"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/fanout"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/orchestrator"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tournament"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types for the Sub and Map tests.
type (
	chFan     struct{}
	chMerged  struct{}
	chPartial struct{}
	chWork    struct{}
	chStep    struct{}
	chOK      struct{}
	chBad     struct{}
	chSubRun  struct{}
	chFin     struct{}
	chDown    struct{}
	chSeed    struct{}
)

type chItem struct {
	N     int
	Files []string
}
type chRes struct{ N int }
type chOut struct{ OK bool }
type chList struct{ Items []chItem }

// chChild is a one-agent child: OK true ends at chOK, false at chBad.
func chChild(hints ...wf.DispatchOption) wf.Child[chItem, chRes] {
	d := wf.New("c-item")
	w := wf.NewAgent[chWork, chOut](d)
	ok := wf.NewTerminal[chOK](d, wf.Success)
	bad := wf.NewTerminal[chBad](d, wf.Stopped)
	d.OnFailure(wf.Blocked, bad)
	w.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[chWork](wf.Const(1), w.To(bad, "gave up")))
	toOK, toBad := w.To(ok, "ok"), w.To(bad, "bad")
	w.Route(func(o chOut, _ wf.Ctx) wf.Edge[chWork] {
		if o.OK {
			return toOK
		}
		return toBad
	}, toOK, toBad)
	if len(hints) > 0 {
		w.Dispatch(hints...)
	}
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	return wf.Typed[chItem, chRes](d)
}

func chItems(ns ...int) wf.Source[chItem] {
	var xs []chItem
	for _, n := range ns {
		xs = append(xs, chItem{N: n})
	}
	return wf.FromValues(xs...)
}

// fanDef is a Map over src with chChild and two terminals.
func fanDef(name string, src wf.Source[chItem], par int, join wf.JoinPolicy, mut ...func(*wf.Map[chFan, chItem, string, chRes])) *wf.Def {
	d := wf.New(name)
	m := wf.NewMap[chFan, chItem, string, chRes](d, src, func(i chItem) string { return fmt.Sprint(i.N) }, chChild())
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(par).Join(join, m.To(merged, "met"), m.To(partial, "partial"))
	for _, f := range mut {
		f(m)
	}
	return d
}

// kids maps child id to its status.
func kids(st wf.RunStatus) map[string]wf.InstanceStatus {
	out := map[string]wf.InstanceStatus{}
	for _, in := range st.Instances {
		if in.ID != "" {
			out[in.ID] = in
		}
	}
	return out
}

func activeKids(st wf.RunStatus) int {
	n := 0
	for _, in := range kids(st) {
		if in.Phase != "done" && in.Phase != "budget_stopped" {
			n++
		}
	}
	return n
}

// answerAll answers every open instruction with out until none is left and
// returns how many it answered; it answers each exhaustion halt with stop.
func answerAll(h *harness, run wf.RunID, out func(wf.Instruction) string) int {
	h.t.Helper()
	n := 0
	for i := 0; i < 200; i++ {
		ins := h.next(run)
		if len(ins) == 0 {
			if h.stopExhausted(run) {
				continue
			}
			return n
		}
		for _, in := range ins {
			h.submit(run, in, out(in))
			n++
		}
	}
	h.t.Fatal("answerAll did not settle")
	return n
}

func okOut(wf.Instruction) string { return `{"OK":true}` }

func childIDs(ins []wf.Instruction) []string {
	var ids []string
	for _, in := range ins {
		ids = append(ids, in.Child)
	}
	return ids
}

// acceptedOf decodes the accepted record of the root instance at state.
func acceptedOf(h *harness, run wf.RunID, state string) json.RawMessage {
	h.t.Helper()
	for _, r := range h.kind(run, wf.JournalAccepted) {
		if r.State == state && r.Instance == "" {
			var d wf.AcceptedData
			if err := json.Unmarshal(r.Data, &d); err != nil {
				h.t.Fatal(err)
			}
			return d.Output
		}
	}
	h.t.Fatalf("no accepted record at %s", state)
	return nil
}

func TestFanoutOnEngine(t *testing.T) {
	for _, tested := range []bool{true, false} {
		d := fanout.Definition()
		w := newWorld()
		w.set("bd", "ready", map[string]any{"epic": "E1"}, []bd.Issue{{ID: "a"}, {ID: "b"}})
		h := newHarness(t, d, w)
		run := h.run(d, map[string]string{"epic": "E1"})
		answerAll(h, run, func(in wf.Instruction) string {
			if in.Child == "Fan[b]" && !tested {
				return `{"Summary":"s","Tested":false}`
			}
			return `{"Summary":"s","Tested":true}`
		})
		want, outcome := "Merged", wf.Success
		if !tested {
			want, outcome = "Partial", wf.Stopped
		}
		st := h.status(run)
		statusOf(t, st, wf.RunEnded, want)
		if st.Outcome != outcome {
			t.Fatalf("outcome = %s, want %s", st.Outcome, outcome)
		}
		if len(kids(st)) != 2 {
			t.Fatalf("children = %v, want 2", kids(st))
		}
	}
}

func TestOrchestratorOnEngine(t *testing.T) {
	d := orchestrator.Definition()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].State != "Lead" {
		t.Fatalf("first instructions = %+v, want the lead", ins)
	}
	h.submit(run, ins[0], `{"Tasks":[{"ID":"t1","Brief":"x"},{"ID":"t2","Brief":"y"},{"ID":"t3","Brief":"z"}]}`)
	answerAll(h, run, func(in wf.Instruction) string { return `{"Summary":"done"}` })
	statusOf(t, h.status(run), wf.RunEnded, "Done")
	if got := string(acceptedOf(h, run, "Synthesize")); got != "3" {
		t.Fatalf("synthesize saw %s workers, want 3", got)
	}
}

func TestAgentlessOnEngine(t *testing.T) {
	for _, exit := range []int{0, 1} {
		d := agentless.Definition()
		w := newWorld()
		w.runs = []wf.RunResult{{Exit: exit}}
		h := newHarness(t, d, w)
		run := h.run(d, map[string]string{"candidate": "cand1"})
		ins := h.next(run)
		h.submit(run, ins[0], `{"Sites":[{"File":"a.go","Symbol":"F"},{"File":"b.go","Symbol":"G"}]}`)
		if got := len(h.next(run)); got != 2 {
			t.Fatalf("repair children = %d, want 2", got)
		}
		answerAll(h, run, func(wf.Instruction) string { return `{"Diff":"d"}` })
		// The ranking compute is a stub that ranks nothing, so selection finds no
		// winner either way; exit 0 still passes through Rank and Select.
		statusOf(t, h.status(run), wf.RunEnded, "NoFix")
		if len(w.applied) != 1 || !strings.Contains(fmt.Sprint(w.applied[0].Args["argv"]), "-run cand1") {
			t.Fatalf("validator runs = %+v, want one with the bound candidate", w.applied)
		}
		_, ranked := recordAt(h, run, wf.JournalAccepted, "Rank")
		if ranked != (exit == 0) {
			t.Fatalf("exit %d: Rank accepted = %v", exit, ranked)
		}
	}
}

func recordAt(h *harness, run wf.RunID, k wf.JournalKind, state string) (wf.JournalRecord, bool) {
	for _, r := range h.kind(run, k) {
		if r.State == state {
			return r, true
		}
	}
	return wf.JournalRecord{}, false
}

func tournamentRun(t *testing.T) (*harness, wf.RunID) {
	d := tournament.Definition()
	w := newWorld()
	w.set("bd", "list", map[string]any{"label": "candidate"}, []bd.Issue{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}})
	h := newHarness(t, d, w)
	return h, h.run(d, map[string]string{"seed": "7"})
}

func TestTournamentOnEngine(t *testing.T) {
	h, run := tournamentRun(t)
	ins := h.next(run)
	if len(ins) != 3 {
		t.Fatalf("judges = %d, want 3", len(ins))
	}
	if !strings.Contains(ins[0].Prompt, "sampling seed 7") {
		t.Fatalf("prompt lacks the seed:\n%s", ins[0].Prompt)
	}
	for _, in := range ins {
		h.submit(run, in, `{"Candidate":"c","Score":4}`)
	}
	statusOf(t, h.status(run), wf.RunEnded, "Decided")
}

func TestQuorumJoinsOnSecondSuccess(t *testing.T) {
	h, run := tournamentRun(t)
	ins := h.next(run)
	h.submit(run, ins[0], `{"Candidate":"c1","Score":1}`)
	if st := h.status(run); st.Phase != wf.RunBlocked && st.Phase != wf.RunRunning {
		t.Fatalf("joined after one success: %+v", st)
	}
	h.submit(run, ins[1], `{"Candidate":"c2","Score":2}`)
	st := h.status(run)
	statusOf(t, st, wf.RunEnded, "Decided")
	if len(h.kind(run, wf.JournalChildSettled)) != 3 {
		t.Fatalf("settled records = %d, want 3 (two judges and the cancelled third)", len(h.kind(run, wf.JournalChildSettled)))
	}
	res := h.submit(run, ins[2], `{"Candidate":"c3","Score":3}`)
	if res.Class != wf.Rejected || len(res.Refusal) == 0 {
		t.Fatalf("submission to the cancelled judge = %+v, want Rejected with a reason", res)
	}
	var mr wf.MapResult[string, struct{}]
	if err := json.Unmarshal(acceptedOf(h, run, "Judges"), &mr); err != nil {
		t.Fatal(err)
	}
	if c := mr.Items["c3"].Class; c != wf.Rejected {
		t.Fatalf("cancelled judge class = %v, want Rejected", c)
	}
	if mr.Items["c1"].Class != wf.Accepted || mr.Items["c1"].Terminal != "JudgeDone" {
		t.Fatalf("judge c1 = %+v", mr.Items["c1"])
	}
}

func TestQuorumUnreachable(t *testing.T) {
	h, run := tournamentRun(t)
	ins := h.next(run)
	for _, in := range ins[:2] {
		tok := in.Token
		for i := 0; i < 2; i++ {
			if _, err := h.e.Fail(run, tok, "gave up"); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				for _, n := range h.next(run) {
					if n.Child == in.Child {
						tok = n.Token
					}
				}
			}
		}
	}
	for h.stopExhausted(run) {
		// the human takes each spent judge's exhausted edge
	}
	statusOf(t, h.status(run), wf.RunEnded, "NoQuorum")
}

func TestParallelNeverExceeded(t *testing.T) {
	d := fanDef("par", chItems(1, 2, 3, 4, 5), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	most := 0
	for i := 0; i < 20; i++ {
		ins := h.next(run)
		if len(ins) == 0 {
			break
		}
		if len(ins) > 2 || activeKids(h.status(run)) > 2 {
			t.Fatalf("%d instructions, %d active children, Parallel is 2", len(ins), activeKids(h.status(run)))
		}
		most = max(most, len(ins))
		h.submit(run, ins[0], `{"OK":true}`)
	}
	if most != 2 {
		t.Fatalf("most concurrent = %d, want 2 (the limit is reached, not undershot)", most)
	}
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
	if len(kids(h.status(run))) != 5 {
		t.Fatalf("children = %d, want 5", len(kids(h.status(run))))
	}
}

func TestBudgetStoppedChildNotRefused(t *testing.T) {
	d := fanDef("budget", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 2}})
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 2 {
		t.Fatalf("instructions = %v, want 2 (the third child is stopped on the budget)", childIDs(ins))
	}
	for _, in := range ins {
		h.submit(run, in, `{"OK":true}`)
	}
	st := h.status(run)
	statusOf(t, st, wf.RunEnded, "chPartial")
	if got := kids(st)["chFan[3]"].Phase; got != "budget_stopped" {
		t.Fatalf("stopped child phase = %q", got)
	}
	stops := h.kind(run, wf.JournalBudgetStopped)
	if len(stops) != 1 || stops[0].Instance != "chFan[3]" {
		t.Fatalf("budget_stopped records = %+v", stops)
	}
	var mr wf.MapResult[string, chRes]
	if err := json.Unmarshal(acceptedOf(h, run, "chFan"), &mr); err != nil {
		t.Fatal(err)
	}
	it := mr.Items["3"]
	if !it.BudgetStopped || it.Class == wf.Refused || it.Class == wf.Accepted || it.Terminal != "" {
		t.Fatalf("budget-stopped item = %+v", it)
	}
}

func TestFreshCountersPerEntryAndItem(t *testing.T) {
	// Map: every item loops once and is capped by MaxVisits at its second arrival.
	loop := func() wf.Child[chItem, chRes] {
		d := wf.New("c-loop")
		rounds := d.Counter("rounds")
		w := wf.NewAgent[chWork, chOut](d)
		ok := wf.NewTerminal[chOK](d, wf.Success)
		bad := wf.NewTerminal[chBad](d, wf.Stopped)
		w.Spec(wf.PromptText("x"), wf.Retry[chWork](wf.Const(1), w.To(bad, "gave up")))
		again := w.To(w, "again").With(rounds.Inc())
		w.Route(func(chOut, wf.Ctx) wf.Edge[chWork] { return again }, again)
		wf.MaxVisits(wf.Const(2), w.To(ok, "cap"))
		res := func(wf.Ctx) chRes { return chRes{} }
		wf.Yield(ok, res)
		wf.Yield(bad, res)
		return wf.Typed[chItem, chRes](d)
	}
	d := wf.New("fresh-map")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, loop())
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllOK(), m.To(merged, "met"), m.To(partial, "partial"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if n := answerAll(h, run, okOut); n != 4 {
		t.Fatalf("submissions = %d, want 2 per item", n)
	}
	st := h.status(run)
	statusOf(t, st, wf.RunEnded, "chMerged")
	for id, k := range kids(st) {
		if k.Counters["rounds"] != 2 {
			t.Fatalf("%s rounds = %d, want 2 (a shared counter would reach 4)", id, k.Counters["rounds"])
		}
	}

	// Sub: entered twice, each entry's child starts with empty counters.
	sd := wf.New("c-subchild")
	n := sd.Counter("n")
	w := wf.NewAgent[chWork, chOut](sd)
	ok := wf.NewTerminal[chOK](sd, wf.Success)
	bad := wf.NewTerminal[chBad](sd, wf.Stopped)
	w.Spec(wf.PromptText("x"), wf.Retry[chWork](wf.Const(1), w.To(bad, "gave up")))
	done := w.To(ok, "done").With(n.Inc())
	w.Route(func(chOut, wf.Ctx) wf.Edge[chWork] { return done }, done)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	d2 := wf.New("fresh-sub")
	sub := wf.NewSub[chSubRun, chItem, chRes](d2, wf.Typed[chItem, chRes](sd), func(wf.Ctx) chItem { return chItem{N: 1} })
	fin := wf.NewTerminal[chFin](d2, wf.Success)
	again := sub.To(sub, "again")
	wf.MaxVisits(wf.Const(2), sub.To(fin, "cap"))
	sub.Exits(wf.OnExit[chOK](again), wf.OnExit[chBad](sub.To(fin, "bad")))
	h2 := newHarness(t, d2, nil)
	run2 := h2.run(d2, nil)
	if got := answerAll(h2, run2, okOut); got != 2 {
		t.Fatalf("sub submissions = %d, want 2", got)
	}
	st2 := h2.status(run2)
	statusOf(t, st2, wf.RunEnded, "chFin")
	ks := kids(st2)
	if len(ks) != 2 || ks["chSubRun#1"].Counters["n"] != 1 || ks["chSubRun#2"].Counters["n"] != 1 {
		t.Fatalf("sub children = %+v, want chSubRun#1 and chSubRun#2 each with n=1", ks)
	}
}

func TestSubObserves(t *testing.T) {
	obs := wf.Observe[int]("kv", "get", map[string]any{"k": "a"})
	other := wf.Observe[int]("kv", "get", map[string]any{"k": "b"})
	build := func(read wf.Observation[int], retry bool) *wf.Def {
		d := wf.New("sub-obs")
		sub := wf.NewSub[chSubRun, chItem, chRes](d, chChild(), func(c wf.Ctx) chItem {
			v, _ := wf.Observed(c, read)
			return chItem{N: v}
		}).Observes(obs)
		fin := wf.NewTerminal[chFin](d, wf.Success)
		down := wf.NewTerminal[chDown](d, wf.Stopped)
		sub.Exits(wf.OnExit[chOK](sub.To(fin, "ok")), wf.OnExit[chBad](sub.To(fin, "bad")))
		if retry {
			sub.Retry(wf.Retry[chSubRun](wf.Const(1), sub.To(down, "exhausted")).OnClass(wf.Unavailable, sub.To(down, "unavailable")))
		}
		return d
	}
	hasRule := func(d *wf.Def) bool {
		for _, p := range d.Lint() {
			if p.Rule == wf.RuleComputeUnavailable {
				return true
			}
		}
		return false
	}
	if !hasRule(build(obs, false)) {
		t.Fatal("a Sub that observes with no unavailable route is not linted")
	}
	d := build(obs, true)
	if hasRule(d) {
		t.Fatalf("lint complains about a Sub with a Retry: %v", d.Lint())
	}
	if out := d.Render(); !strings.Contains(out, "observes") || !strings.Contains(out, "kv.get") {
		t.Fatalf("render lacks the Sub's observation:\n%s", out)
	}
	plain := wf.New("sub-obs")
	plainSub := wf.NewSub[chSubRun, chItem, chRes](plain, chChild(), func(wf.Ctx) chItem { return chItem{} })
	pf := wf.NewTerminal[chFin](plain, wf.Success)
	plainSub.Exits(wf.OnExit[chOK](plainSub.To(pf, "ok")), wf.OnExit[chBad](plainSub.To(pf, "bad")))
	if plain.Hash() == d.Hash() {
		t.Fatal("declaring Observes does not change the hash")
	}

	// A declared read reaches the closure, and so the child's input.
	w := newWorld()
	w.set("kv", "get", map[string]any{"k": "a"}, 5)
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 || !strings.Contains(ins[0].Prompt, `"N":5`) {
		t.Fatalf("child instructions = %+v, want one carrying N=5", ins)
	}

	// An unavailable read takes the Unavailable route.
	w2 := newWorld()
	w2.down["kv.get"] = true
	h2 := newHarness(t, d, w2)
	run2 := h2.run(d, nil)
	h2.next(run2)
	statusOf(t, h2.status(run2), wf.RunEnded, "chDown")

	// Reading an undeclared observation in the input closure is Blocked.
	w3 := newWorld()
	w3.set("kv", "get", map[string]any{"k": "a"}, 5)
	w3.set("kv", "get", map[string]any{"k": "b"}, 6)
	bd3 := build(other, false)
	h3 := newHarness(t, bd3, w3)
	run3 := h3.run(bd3, nil)
	h3.next(run3)
	st := h3.status(run3)
	if st.Phase != wf.RunBlocked || !strings.Contains(st.Instances[0].Reason, "not declared") {
		t.Fatalf("status = %+v, want blocked on the undeclared read", st)
	}
	if len(kids(st)) != 0 {
		t.Fatal("a child started despite the blocked input")
	}
}

func TestChildInstructionsCarryHints(t *testing.T) {
	hints := []wf.DispatchOption{wf.Model("sonnet"), wf.Effort("low"), wf.AgentType("reviewer")}
	want := wf.DispatchHints{Model: "sonnet", Effort: "low", AgentType: "reviewer"}
	d := wf.New("hints")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, chChild(hints...))
	mid := wf.NewTerminal[chMerged](d, wf.Success)
	part := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Join(wf.AllSettled(), m.To(mid, "met"), m.To(part, "partial"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 2 {
		t.Fatalf("map instructions = %d", len(ins))
	}
	for _, in := range ins {
		if in.Dispatch != want || in.Child == "" {
			t.Fatalf("map child instruction = %+v, want dispatch %+v and a child id", in, want)
		}
	}

	d2 := wf.New("hints-sub")
	sub := wf.NewSub[chSubRun, chItem, chRes](d2, chChild(hints...), func(wf.Ctx) chItem { return chItem{} })
	fin := wf.NewTerminal[chFin](d2, wf.Success)
	sub.Exits(wf.OnExit[chOK](sub.To(fin, "ok")), wf.OnExit[chBad](sub.To(fin, "bad")))
	h2 := newHarness(t, d2, nil)
	ins2 := h2.next(h2.run(d2, nil))
	if len(ins2) != 1 || ins2[0].Dispatch != want || ins2[0].Child != "chSubRun#1" {
		t.Fatalf("sub child instruction = %+v, want dispatch %+v", ins2, want)
	}
}

func TestHoldSerialisesAcrossChildren(t *testing.T) {
	d := wf.New("hold-child")
	lock := d.Lock("merge", wf.LockRun)
	a := wf.NewAgent[chWork, chOut](d)
	b := wf.NewAgent[chStep, chOut](d)
	ok := wf.NewTerminal[chOK](d, wf.Success)
	bad := wf.NewTerminal[chBad](d, wf.Stopped)
	wf.Hold(b, lock)
	a.Spec(wf.PromptText("a"), wf.Retry[chWork](wf.Const(1), a.To(bad, "gave up")))
	b.Spec(wf.PromptText("b"), wf.Retry[chStep](wf.Const(1), b.To(bad, "gave up")))
	toB, done := a.To(b, "to b"), b.To(ok, "done")
	a.Route(func(chOut, wf.Ctx) wf.Edge[chWork] { return toB }, toB)
	b.Route(func(chOut, wf.Ctx) wf.Edge[chStep] { return done }, done)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	child := wf.Typed[chItem, chRes](d)

	pd := wf.New("hold")
	m := wf.NewMap[chFan, chItem, string, chRes](pd, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, child)
	mid := wf.NewTerminal[chMerged](pd, wf.Success)
	part := wf.NewTerminal[chPartial](pd, wf.Stopped)
	m.Parallel(2).Join(wf.AllOK(), m.To(mid, "met"), m.To(part, "partial"))
	h := newHarness(t, pd, nil)
	run := h.run(pd, nil)
	ins := h.next(run)
	if len(ins) != 2 {
		t.Fatalf("first instructions = %v, want both children at A", childIDs(ins))
	}
	for _, in := range ins {
		h.submit(run, in, `{}`)
	}
	atB := h.next(run)
	if len(atB) != 1 || atB[0].State != "hold-child/chStep" {
		t.Fatalf("after both A: %+v, want exactly one child holding the lock at B", atB)
	}
	first := atB[0].Child
	h.submit(run, atB[0], `{}`)
	atB = h.next(run)
	if len(atB) != 1 || atB[0].Child == first {
		t.Fatalf("after the holder left B: %+v, want the other child", atB)
	}
	h.submit(run, atB[0], `{}`)
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
}

func TestWriteSetsKeepChildrenApart(t *testing.T) {
	src := wf.FromValues(chItem{N: 1, Files: []string{"src/*"}}, chItem{N: 2, Files: []string{"src/a.go"}}, chItem{N: 3, Files: []string{"docs/x"}})
	d := fanDef("writes", src, 3, wf.AllSettled(), func(m *wf.Map[chFan, chItem, string, chRes]) {
		m.ItemWrites(func(i chItem) []string { return i.Files })
	})
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if got := childIDs(ins); len(got) != 2 || got[0] != "chFan[1]" || got[1] != "chFan[3]" {
		t.Fatalf("active children = %v, want 1 and 3 (2 overlaps 1's glob)", got)
	}
	h.submit(run, ins[0], `{"OK":true}`)
	ins = h.next(run)
	if got := childIDs(ins); len(got) != 2 || got[0] != "chFan[3]" || got[1] != "chFan[2]" {
		t.Fatalf("after 1 settled: %v, want 3 and 2", got)
	}
	answerAll(h, run, okOut)
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")

	// A static write set applies to every item: one child at a time.
	d2 := fanDef("static-writes", chItems(1, 2, 3), 3, wf.AllSettled(), func(m *wf.Map[chFan, chItem, string, chRes]) {
		m.Writes("out/report")
	})
	h2 := newHarness(t, d2, nil)
	run2 := h2.run(d2, nil)
	for i := 0; i < 3; i++ {
		ins := h2.next(run2)
		if len(ins) != 1 {
			t.Fatalf("round %d: %v active, want 1", i, childIDs(ins))
		}
		h2.submit(run2, ins[0], `{"OK":true}`)
	}
	statusOf(t, h2.status(run2), wf.RunEnded, "chMerged")
}

func TestJoinPolicies(t *testing.T) {
	failFirst := func(in wf.Instruction) string {
		if in.Child == "chFan[1]" {
			return `{"OK":false}`
		}
		return `{"OK":true}`
	}
	count := func(want int) wf.JoinPolicy {
		return wf.JoinWhen("enough", func(r wf.MapResult[string, chRes]) bool {
			n := 0
			for _, it := range r.Items {
				if it.Class == wf.Accepted {
					n++
				}
			}
			return n >= want
		})
	}
	cancel := func(m *wf.Map[chFan, chItem, string, chRes]) { m.OnSiblingFailure(wf.CancelSiblings) }
	for _, tc := range []struct {
		name string
		d    *wf.Def
		out  func(wf.Instruction) string
		want string
		step bool // submit one instruction at a time
	}{
		{"all_ok continues siblings then partial", fanDef("p1", chItems(1, 2, 3), 3, wf.AllOK()), failFirst, "chPartial", false},
		{"all_ok cancels siblings at once", fanDef("p2", chItems(1, 2, 3), 3, wf.AllOK(), cancel), failFirst, "chPartial", true},
		{"all_ok met", fanDef("p3", chItems(1, 2), 2, wf.AllOK()), okOut, "chMerged", false},
		{"predicate met", fanDef("p4", chItems(1, 2, 3), 3, count(2)), failFirst, "chMerged", false},
		{"predicate never held", fanDef("p5", chItems(1, 2, 3), 3, count(3)), failFirst, "chPartial", false},
		{"empty list meets the join", fanDef("p6", chItems(), 3, wf.AllOK()), okOut, "chMerged", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.d, nil)
			run := h.run(tc.d, nil)
			if tc.step {
				ins := h.next(run)
				for _, in := range ins {
					if in.Child == "chFan[1]" {
						h.submit(run, in, tc.out(in))
					}
				}
				st := h.status(run)
				statusOf(t, st, wf.RunEnded, tc.want)
				if activeKids(st) != 0 {
					t.Fatalf("siblings still active after the cancelling failure: %+v", st.Instances)
				}
				return
			}
			answerAll(h, run, tc.out)
			statusOf(t, h.status(run), wf.RunEnded, tc.want)
		})
	}
}

func TestRequerySpawnsNewKeysOnly(t *testing.T) {
	src := wf.FromObservation(wf.Observe[[]chItem]("kv", "items", nil))
	d := fanDef("requery", src, 2, wf.AllOK(), func(m *wf.Map[chFan, chItem, string, chRes]) { m.Requery() })
	w := newWorld()
	w.set("kv", "items", nil, []chItem{{N: 1}})
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	ins := h.next(run)
	if got := childIDs(ins); len(got) != 1 || got[0] != "chFan[1]" {
		t.Fatalf("first round = %v", got)
	}
	w.set("kv", "items", nil, []chItem{{N: 1}, {N: 2}})
	h.submit(run, ins[0], `{"OK":true}`)
	ins = h.next(run)
	if got := childIDs(ins); len(got) != 1 || got[0] != "chFan[2]" {
		t.Fatalf("after the re-query = %v, want only the new key", got)
	}
	h.submit(run, ins[0], `{"OK":true}`)
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
	if n := len(h.kind(run, wf.JournalChildSpawned)); n != 2 {
		t.Fatalf("child_spawned records = %d, want 2 (key 1 is never respawned)", n)
	}
}

func TestFromValuesAndFromComputeSources(t *testing.T) {
	d1 := fanDef("v1", chItems(1, 2), 2, wf.AllOK())
	d2 := fanDef("v1", chItems(1, 3), 2, wf.AllOK())
	if out := d1.Render(); !strings.Contains(out, `values [{"N":1,"Files":null},{"N":2,"Files":null}]`) {
		t.Fatalf("render lacks the declared values:\n%s", out)
	}
	if d1.Hash() == d2.Hash() {
		t.Fatal("different static values hash the same")
	}
	if ps := d1.Lint(); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}

	d := wf.New("from-compute")
	seed := wf.NewCompute[chSeed, chList](d, func(wf.Ctx) (chList, error) { return chList{Items: []chItem{{N: 7}, {N: 8}}}, nil })
	m := wf.NewMap[chFan, chItem, string, chRes](d, wf.FromCompute(seed, func(l chList) []chItem { return l.Items }),
		func(i chItem) string { return fmt.Sprint(i.N) }, chChild())
	mid := wf.NewTerminal[chMerged](d, wf.Success)
	part := wf.NewTerminal[chPartial](d, wf.Stopped)
	toFan := seed.To(m, "seeded")
	seed.Route(func(chList, wf.Ctx) wf.Edge[chSeed] { return toFan }, toFan)
	m.Join(wf.AllOK(), m.To(mid, "met"), m.To(part, "partial"))
	if out := d.Render(); !strings.Contains(out, "computed value of chSeed") {
		t.Fatalf("render lacks the compute source:\n%s", out)
	}
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if got := childIDs(ins); len(got) != 2 || got[0] != "chFan[7]" || got[1] != "chFan[8]" {
		t.Fatalf("children = %v", got)
	}
	answerAll(h, run, okOut)
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
}

// concurrentWriters reports whether a Map whose two items write a and b runs
// both children at once (the fences found no overlap).
func concurrentWriters(t *testing.T, a, b string) bool {
	t.Helper()
	src := wf.FromValues(chItem{N: 1, Files: []string{a}}, chItem{N: 2, Files: []string{b}})
	d := fanDef("globs", src, 2, wf.AllSettled(), func(m *wf.Map[chFan, chItem, string, chRes]) {
		m.ItemWrites(func(i chItem) []string { return i.Files })
	})
	h := newHarness(t, d, nil)
	return len(h.next(h.run(d, nil))) == 2
}

func TestGlobsOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"src/**", "src/x/y.go", true},
		{"src/", "src/a.go", true},
		{"src", "src/a.go", true},
		{"src/*.go", "src/a*", true},
		{"docs/**", "docs/a/b.md", true},
		{"**", "a/b/c", true},
		{"a/**/z.go", "a/b/c/z.go", true},
		{"a.go", "a.go", true},
		{"?.go", "a.go", true},
		{"a/*.go", "b/*.go", false},
		{"src/*.go", "src/x/y.go", false},
		{"src/**", "lib/x.go", false},
		{"*.go", "*.md", false},
		{"a.go", "b.go", false},
	} {
		if got := concurrentWriters(t, c.a, c.b); got == c.want {
			t.Errorf("%q and %q: children concurrent = %v, want overlap = %v", c.a, c.b, got, c.want)
		}
		if got := concurrentWriters(t, c.b, c.a); got == c.want {
			t.Errorf("%q and %q (swapped): children concurrent = %v, want overlap = %v", c.b, c.a, got, c.want)
		}
	}
}

func TestWriteSetsKeepGlobChildrenApart(t *testing.T) {
	src := wf.FromValues(chItem{N: 1, Files: []string{"src/**"}}, chItem{N: 2, Files: []string{"src/a.go"}})
	d := fanDef("glob-writes", src, 2, wf.AllSettled(), func(m *wf.Map[chFan, chItem, string, chRes]) {
		m.ItemWrites(func(i chItem) []string { return i.Files })
	})
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	for i, want := range []string{"chFan[1]", "chFan[2]"} {
		ins := h.next(run)
		if got := childIDs(ins); len(got) != 1 || got[0] != want {
			t.Fatalf("round %d: active children = %v, want only %s", i, got, want)
		}
		h.submit(run, ins[0], `{"OK":true}`)
	}
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
}

// holdChild is a child whose start state holds the "entry" lock.
func holdChild() wf.Child[chItem, chRes] {
	d := wf.New("hold-entry")
	lock := d.Lock("entry", wf.LockRun)
	a := wf.NewAgent[chWork, chOut](d)
	ok := wf.NewTerminal[chOK](d, wf.Success)
	bad := wf.NewTerminal[chBad](d, wf.Stopped)
	wf.Hold(a, lock)
	a.Spec(wf.PromptText("a"), wf.Retry[chWork](wf.Const(1), a.To(bad, "gave up")))
	done := a.To(ok, "done")
	a.Route(func(chOut, wf.Ctx) wf.Edge[chWork] { return done }, done)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	return wf.Typed[chItem, chRes](d)
}

func TestHoldEnforcedOnChildEntry(t *testing.T) {
	pd := wf.New("hold-entry-parent")
	m := wf.NewMap[chFan, chItem, string, chRes](pd, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, holdChild())
	mid := wf.NewTerminal[chMerged](pd, wf.Success)
	part := wf.NewTerminal[chPartial](pd, wf.Stopped)
	m.Parallel(2).Join(wf.AllOK(), m.To(mid, "met"), m.To(part, "partial"))
	h := newHarness(t, pd, nil)
	run := h.run(pd, nil)
	ins := h.next(run)
	if got := childIDs(ins); len(got) != 1 || got[0] != "chFan[1]" {
		t.Fatalf("first instructions = %v, want only chFan[1]: the second child would enter a held state", got)
	}
	h.submit(run, ins[0], `{}`)
	ins = h.next(run)
	if got := childIDs(ins); len(got) != 1 || got[0] != "chFan[2]" {
		t.Fatalf("after the holder left: %v, want chFan[2]", got)
	}
	h.submit(run, ins[0], `{}`)
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
}

// subDef is a Sub over child with exits for chOK only.
func subDef(name string, child wf.Child[chItem, chRes]) *wf.Def {
	d := wf.New(name)
	sub := wf.NewSub[chSubRun, chItem, chRes](d, child, func(wf.Ctx) chItem { return chItem{} })
	fin := wf.NewTerminal[chFin](d, wf.Success)
	sub.Exits(wf.OnExit[chOK](sub.To(fin, "ok")))
	return d
}

// twoStepChild is A then B, ending at chOK; the edge A to B carries effects.
func twoStepChild(effects ...wf.Effect) wf.Child[chItem, chRes] {
	d := wf.New("two-step")
	a := wf.NewAgent[chWork, chOut](d)
	b := wf.NewAgent[chStep, chOut](d)
	ok := wf.NewTerminal[chOK](d, wf.Success)
	bad := wf.NewTerminal[chBad](d, wf.Stopped)
	a.Spec(wf.PromptText("a"), wf.Retry[chWork](wf.Const(1), a.To(bad, "gave up")))
	b.Spec(wf.PromptText("b"), wf.Retry[chStep](wf.Const(1), b.To(bad, "gave up")))
	toB, done := a.To(b, "to b"), b.To(ok, "done")
	for _, e := range effects {
		toB = toB.With(e)
	}
	a.Route(func(chOut, wf.Ctx) wf.Edge[chWork] { return toB }, toB)
	b.Route(func(chOut, wf.Ctx) wf.Edge[chStep] { return done }, done)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	return wf.Typed[chItem, chRes](d)
}

func TestSubChildHaltBlocksParent(t *testing.T) {
	fail := true
	d := haltSubDef(&fail)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	// The child's compute fails: a bare halt, which the Sub settles.
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none", ins)
	}
	settled := h.kind(run, wf.JournalChildSettled)
	if len(settled) != 1 || settled[0].Class != wf.Blocked {
		t.Fatalf("child_settled = %+v, want one Blocked record", settled)
	}
	st := h.status(run)
	if st.Phase != wf.RunBlocked {
		t.Fatalf("run phase = %s, want blocked", st.Phase)
	}
	// The settled child has no terminal: the parent reports that.
	var found bool
	for _, r := range h.kind(run, wf.JournalRefused) {
		if strings.Contains(string(r.Data), "did not reach a terminal") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no refusal naming a child without a terminal: %v", kinds(h.records(run)))
	}
}

func TestSubChildBudgetStopStopsParent(t *testing.T) {
	d := subDef("sub-budget", twoStepChild())
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1}})
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Child != "chSubRun#1" {
		t.Fatalf("first instructions = %+v, want the child's first step", ins)
	}
	h.submit(run, ins[0], `{}`)
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none: the budget is spent", ins)
	}
	var who []string
	for _, r := range h.kind(run, wf.JournalBudgetStopped) {
		who = append(who, r.Instance)
	}
	if len(who) != 2 || who[0] != "chSubRun#1" || who[1] != "" {
		t.Fatalf("budget_stopped instances = %q, want the child and then the parent", who)
	}
	if st := h.status(run); st.Phase == wf.RunEnded || !st.Stopped || st.BudgetStop == "" {
		t.Fatalf("status = %+v, want the run stopped on its budget, not ended", st)
	}
}

func TestSubChildNoExitIsBlocked(t *testing.T) {
	d := subDef("sub-noexit", chChild())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("instructions = %+v, want the child's step", ins)
	}
	h.submit(run, ins[0], `{"OK":false}`)
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none", ins)
	}
	if st := h.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("run phase = %s, want blocked: chBad has no exit", st.Phase)
	}
	var found bool
	for _, r := range h.kind(run, wf.JournalRefused) {
		if strings.Contains(string(r.Data), "which has no exit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no refusal naming the missing exit: %v", kinds(h.records(run)))
	}
}

func TestLockHeldThroughOpenEdgeEffect(t *testing.T) {
	// Child: A takes an edge carrying a non-idempotent effect into B, which
	// holds the "entry" lock.
	cd := wf.New("lock-edge")
	lock := cd.Lock("entry", wf.LockRun)
	a := wf.NewAgent[chWork, chOut](cd)
	b := wf.NewAgent[chStep, chOut](cd)
	ok := wf.NewTerminal[chOK](cd, wf.Success)
	bad := wf.NewTerminal[chBad](cd, wf.Stopped)
	wf.Hold(b, lock)
	a.Spec(wf.PromptText("a"), wf.Retry[chWork](wf.Const(1), a.To(bad, "gave up")))
	b.Spec(wf.PromptText("b"), wf.Retry[chStep](wf.Const(1), b.To(bad, "gave up")))
	toB, done := a.To(b, "to b").With(mergeEffect(wf.NonIdempotent("pushes"))), b.To(ok, "done")
	a.Route(func(chOut, wf.Ctx) wf.Edge[chWork] { return toB }, toB)
	b.Route(func(chOut, wf.Ctx) wf.Edge[chStep] { return done }, done)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)

	pd := wf.New("lock-edge-parent")
	m := wf.NewMap[chFan, chItem, string, chRes](pd, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, wf.Typed[chItem, chRes](cd))
	mid := wf.NewTerminal[chMerged](pd, wf.Success)
	part := wf.NewTerminal[chPartial](pd, wf.Stopped)
	m.Parallel(2).Join(wf.AllOK(), m.To(mid, "met"), m.To(part, "partial"))

	c := newCrashHarness(t, pd, newWorld(), wf.JournalEffectResult)
	run := c.run(pd, nil)
	c.crashAt(run, `{}`) // chFan[1]: effect applied, its result not journaled
	var verify, second wf.Instruction
	for _, in := range c.next(run) {
		switch {
		case in.Kind == wf.InstructionManualVerification:
			verify = in
		case in.Child == "chFan[2]":
			second = in
		}
	}
	if verify.Effect == 0 || second.Child == "" {
		t.Fatalf("want a manual verification of chFan[1] and chFan[2] at A")
	}
	c.submit(run, second, `{}`)
	for _, in := range c.next(run) {
		if in.State == "lock-edge/chStep" {
			t.Fatalf("instruction %+v: chFan[2] entered B while chFan[1]'s open effect holds the lock", in)
		}
	}
	// Once chFan[1] settles its effect and reaches B, it holds the lock.
	if err := settleAs(c.e, run, verify.Effect, true); err != nil {
		t.Fatal(err)
	}
	ins := c.next(run)
	if len(ins) != 1 || ins[0].Child != "chFan[1]" || ins[0].State != "lock-edge/chStep" {
		t.Fatalf("after settling: %+v, want only chFan[1] at B", ins)
	}
}

func srcDef(name string) *wf.Def {
	src := wf.FromObservation(wf.Observe[[]chItem]("kv", "items", nil))
	return fanDef(name, src, 2, wf.AllOK(), func(m *wf.Map[chFan, chItem, string, chRes]) { m.Requery() })
}

func joinOf(t *testing.T, h *harness, run wf.RunID) wf.MapResult[string, chRes] {
	t.Helper()
	var res wf.MapResult[string, chRes]
	if err := json.Unmarshal(acceptedOf(h, run, "chFan"), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestMapSourceFailureTakesPartial(t *testing.T) {
	d := srcDef("src-fail")
	w := newWorld()
	w.down["kv.items"] = true
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %v, want none", childIDs(ins))
	}
	statusOf(t, h.status(run), wf.RunEnded, "chPartial")
	if n := len(h.kind(run, wf.JournalChildSpawned)); n != 0 {
		t.Fatalf("child_spawned = %d, want 0", n)
	}
	res := joinOf(t, h, run)
	if !strings.Contains(res.SourceError, "down") || len(res.Items) != 0 {
		t.Fatalf("join = %+v, want the source error and no items", res)
	}
}

// A Map key names its child, so a key holding a control byte (here a NUL
// and a line break, from the item) is refused where it enters: no child is
// spawned and the refusal says why.
func TestMapRefusesControlByteKey(t *testing.T) {
	for _, key := range []string{"a\x00b", "a\nb"} {
		d := wf.New("ctl-key")
		m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1), func(chItem) string { return key }, chChild())
		merged := wf.NewTerminal[chMerged](d, wf.Success)
		partial := wf.NewTerminal[chPartial](d, wf.Stopped)
		m.Parallel(1).Join(wf.AllOK(), m.To(merged, "met"), m.To(partial, "partial"))
		h := newHarness(t, d, newWorld())
		run := h.run(d, nil)
		if ins := h.next(run); len(ins) != 0 {
			t.Fatalf("key %q: instructions = %v, want none", key, childIDs(ins))
		}
		if n := len(h.kind(run, wf.JournalChildSpawned)); n != 0 {
			t.Fatalf("key %q: child_spawned = %d, want 0", key, n)
		}
		st := h.status(run)
		if !strings.Contains(fmt.Sprint(st), "control character") {
			t.Fatalf("key %q: status does not name the refusal: %+v", key, st)
		}
	}
}

func TestMapRequeryFailureWaitsForActiveThenPartial(t *testing.T) {
	d := srcDef("requery-fail")
	w := newWorld()
	w.set("kv", "items", nil, []chItem{{N: 1}, {N: 2}})
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 2 {
		t.Fatalf("first round = %v", childIDs(ins))
	}
	w.down["kv.items"] = true
	h.submit(run, ins[0], `{"OK":true}`)
	ins = h.next(run)
	if got := childIDs(ins); len(got) != 1 || got[0] != "chFan[2]" {
		t.Fatalf("after the failed re-query = %v, want the active child to wait", got)
	}
	if st := h.status(run); st.Phase == wf.RunEnded {
		t.Fatalf("run ended with a child still active: %+v", st)
	}
	h.submit(run, ins[0], `{"OK":true}`)
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "chPartial")
	res := joinOf(t, h, run)
	if res.SourceError == "" || len(res.Items) != 2 {
		t.Fatalf("join = %+v, want the source error and both settled items", res)
	}
}

func TestMapRequeryRecovers(t *testing.T) {
	d := srcDef("requery-recover")
	w := newWorld()
	w.set("kv", "items", nil, []chItem{{N: 1}, {N: 2}})
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	ins := h.next(run)
	w.down["kv.items"] = true
	h.submit(run, ins[0], `{"OK":true}`)
	ins = h.next(run)
	if len(ins) != 1 {
		t.Fatalf("after the failed re-query = %v", childIDs(ins))
	}
	w.down["kv.items"] = false
	w.set("kv", "items", nil, []chItem{{N: 1}, {N: 2}, {N: 3}})
	h.submit(run, ins[0], `{"OK":true}`)
	ins = h.next(run)
	if got := childIDs(ins); len(got) != 1 || got[0] != "chFan[3]" {
		t.Fatalf("after the recovered read = %v, want the new key", got)
	}
	h.submit(run, ins[0], `{"OK":true}`)
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
	if res := joinOf(t, h, run); res.SourceError != "" {
		t.Fatalf("recovered join carries SourceError %q", res.SourceError)
	}
}

// B3: a quorum met with a seat missing proceeds and records which seat, and
// with what class, in a degraded record of kind join short.
func TestQuorumMetShortRecordsTheMissingSeat(t *testing.T) {
	h, run := tournamentRun(t)
	ins := h.next(run)
	for {
		if _, err := h.e.Fail(run, ins[0].Token, "gave up"); err != nil {
			t.Fatal(err)
		}
		again := h.next(run)
		if len(again) == 0 || again[0].Child != ins[0].Child {
			break
		}
		ins[0] = again[0]
	}
	h.submit(run, ins[1], `{"Candidate":"c2","Score":2}`)
	h.submit(run, ins[2], `{"Candidate":"c3","Score":3}`)
	statusOf(t, h.status(run), wf.RunEnded, "Decided")
	var join []wf.DegradedData
	for _, d := range degradedOf(t, h, run) {
		if d.Kind == wf.DegradedJoin {
			join = append(join, d)
		}
	}
	if len(join) != 1 || !strings.Contains(join[0].Cause, "2 of 3 seats") || !strings.Contains(join[0].Cause, "Judges[c1]") {
		t.Fatalf("join records = %+v, want one naming the missing seat c1", join)
	}
}
