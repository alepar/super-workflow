package wf_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Child-settlement hygiene: a settled child is inert, a cancel checks the
// child's open intent first, a failing Map parent cancels its children, and
// the join does not depend on a crash.

var mergeKey = wf.KeyOf("merge", "feat")

// stMapDef is a Map over items 1 and 2 with child, both running at once.
func stMapDef(name string, child wf.Child[chItem, chRes], join wf.JoinPolicy) *wf.Def {
	d := wf.New(name)
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return string(rune('0' + i.N)) }, child)
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).OnSiblingFailure(wf.CancelSiblings).Join(join, m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// stWrapChild is a Map item that runs one Sub over the two-step child.
func stWrapChild() wf.Child[chItem, chRes] {
	d := wf.New("c-wrap")
	sub := wf.NewSub[chSubRun, chItem, chRes](d, twoStepChild(mergeEffect(mergeKey)), func(wf.Ctx) chItem { return chItem{} })
	fin := wf.NewTerminal[chFin](d, wf.Success)
	sub.Exits(wf.OnExit[chOK](sub.To(fin, "ok")))
	wf.Yield(fin, func(wf.Ctx) chRes { return chRes{} })
	return wf.Typed[chItem, chRes](d)
}

// failWork fails the agent instruction of child id until it has none left.
func failWork(t *testing.T, h *harness, run wf.RunID, id string) {
	t.Helper()
	for i := 0; i < 5; i++ {
		var tok string
		for _, in := range h.next(run) {
			if in.Child == id && in.Kind == wf.InstructionAgent {
				tok = in.Token
			}
		}
		if tok == "" {
			if slices.Contains(exhaustionHalted(h.status(run)), id) {
				// The spent child waits on its exhaustion halt: the human
				// takes its exhausted edge.
				if err := h.e.Retry(run, id, true); err != nil {
					t.Fatal(err)
				}
			}
			return
		}
		if _, err := h.e.Fail(run, tok, "no"); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("child %s still has work after 5 failures", id)
}

// cancelled lists the child_settled records that cancelled a child, by child.
func cancelled(t *testing.T, h *harness, run wf.RunID) map[string]int {
	t.Helper()
	out := map[string]int{}
	for i, r := range h.records(run) {
		if r.Kind != wf.JournalChildSettled {
			continue
		}
		var d wf.ChildSettledData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Cancelled {
			if d.Class != wf.Rejected {
				t.Fatalf("cancelled child %s class = %s, want Rejected", d.ID, d.Class)
			}
			out[d.ID] = i
		}
	}
	return out
}

// assertInert checks that Next offers nothing for the children and that
// settling the intent is rejected.
func assertInert(t *testing.T, h *harness, run wf.RunID, intent int64, ids ...string) {
	t.Helper()
	for _, in := range h.next(run) {
		if slices.Contains(ids, in.Child) {
			t.Fatalf("Next offers %+v for cancelled child %s", in, in.Child)
		}
	}
	err := settleAs(h.e, run, intent, true)
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "child is settled") {
		t.Fatalf("Settle of a cancelled child's intent = %v, want ErrRejected naming the settled child", err)
	}
}

// stCrashed runs def d until chFan[1]'s edge effect has an intent with no
// result (the first instruction is submitted and the process dies).
func stCrashed(t *testing.T, d *wf.Def, w *world) (*crashHarness, wf.RunID, int64) {
	t.Helper()
	c := newCrashHarness(t, d, w, wf.JournalEffectResult)
	run := c.run(d, nil)
	c.crashAt(run, `{}`)
	intents := c.kind(run, wf.JournalEffectIntent)
	if len(intents) != 1 || c.w.appliedCount() != 1 {
		t.Fatalf("intents = %d, applied = %d, want one applied intent with no result", len(intents), c.w.appliedCount())
	}
	return c, run, intents[0].Seq
}

func TestCancelledMapChildNeverRunsAgain(t *testing.T) {
	d := stMapDef("settle-map", twoStepChild(mergeEffect(mergeKey)), wf.AllSettled())
	c, run, intent := stCrashed(t, d, newWorld())

	// chFan[2] fails while chFan[1]'s effect is still unresolved.
	failWork(t, c.harness, run, "chFan[2]")

	if _, ok := cancelled(t, c.harness, run)["chFan[1]"]; !ok {
		t.Fatalf("chFan[1] was not cancelled: %v", kinds(c.records(run)))
	}
	assertInert(t, c.harness, run, intent, "chFan[1]")
	if n := c.w.appliedCount(); n != 1 {
		t.Fatalf("effect applied %d times, want only the pre-crash apply", n)
	}
	statusOf(t, c.status(run), wf.RunEnded, "chPartial")
	if got := kids(c.status(run))["chFan[1]"].Phase; got != "done" {
		t.Fatalf("cancelled child phase = %q, want done", got)
	}
	// The next engine over the same journal agrees.
	c.resume()
	assertInert(t, c.harness, run, intent, "chFan[1]")
}

func TestCancelledSubChildNeverRunsAgain(t *testing.T) {
	d := stMapDef("settle-sub", stWrapChild(), wf.AllSettled())
	c, run, intent := stCrashed(t, d, newWorld())

	// chFan[2]'s Sub child fails, which blocks chFan[2] and cancels chFan[1]
	// and, first, its Sub child that holds the open intent.
	failWork(t, c.harness, run, "chFan[2]/chSubRun#1")

	gone := cancelled(t, c.harness, run)
	sub, okSub := gone["chFan[1]/chSubRun#1"]
	top, okTop := gone["chFan[1]"]
	if !okSub || !okTop || sub > top {
		t.Fatalf("cancelled = %v, want the Sub child cancelled before chFan[1]", gone)
	}
	assertInert(t, c.harness, run, intent, "chFan[1]", "chFan[1]/chSubRun#1")
	if n := c.w.appliedCount(); n != 1 {
		t.Fatalf("effect applied %d times, want only the pre-crash apply", n)
	}
	for _, id := range []string{"chFan[1]", "chFan[1]/chSubRun#1"} {
		if got := kids(c.status(run))[id].Phase; got != "done" {
			t.Fatalf("%s phase = %q, want done", id, got)
		}
	}
}

func TestHaltedParentLeavesNoLiveChildren(t *testing.T) {
	d := srcDef("settle-halt")
	w := newWorld()
	w.set("kv", "items", nil, []chItem{{N: 1}, {N: 2}})
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 2 {
		t.Fatalf("first round = %v", childIDs(ins))
	}
	// The re-query after chFan[1] settles reads something that is no array.
	w.set("kv", "items", nil, "not an array")
	h.submit(run, ins[0], `{"OK":true}`)
	h.next(run)

	st := h.status(run)
	if st.Phase != wf.RunBlocked {
		t.Fatalf("run phase = %s, want blocked: %+v", st.Phase, st.Instances)
	}
	if _, ok := cancelled(t, h, run)["chFan[2]"]; !ok {
		t.Fatalf("active child chFan[2] was not cancelled: %v", kinds(h.records(run)))
	}
	for id, in := range kids(st) {
		if in.Phase != "done" {
			t.Errorf("child %s phase = %q after the parent halted, want settled", id, in.Phase)
		}
	}
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %v after the parent halted", childIDs(ins))
	}
}

// stJoinDrive answers one instruction at a time (item 2 fails, the rest
// succeed) until none is open, and returns the keyed join and the final status.
func stJoinDrive(t *testing.T, h *harness, run wf.RunID) (string, wf.RunStatus) {
	t.Helper()
	for i := 0; i < 50; i++ {
		ins := h.next(run)
		if len(ins) == 0 {
			break
		}
		out := `{"OK":true}`
		if strings.HasSuffix(ins[0].Child, "[2]") {
			out = `{"OK":false}`
		}
		h.submit(run, ins[0], out)
	}
	st := h.status(run)
	return string(acceptedOf(h, run, "chFan")), st
}

func TestJoinSameWithAndWithoutCrash(t *testing.T) {
	predicate := wf.JoinWhen("two settled", func(r wf.MapResult[string, chRes]) bool { return len(r.Items) >= 2 })
	for name, join := range map[string]wf.JoinPolicy{
		"all_settled": wf.AllSettled(), "all_ok": wf.AllOK(), "quorum": wf.Quorum(1), "predicate": predicate,
	} {
		t.Run(name, func(t *testing.T) {
			build := func() *wf.Def {
				d := wf.New("join-" + name)
				items := chItems(1, 2, 3)
				m := wf.NewMap[chFan, chItem, string, chRes](d, items, func(i chItem) string { return string(rune('0' + i.N)) }, chChild())
				merged := wf.NewTerminal[chMerged](d, wf.Success)
				partial := wf.NewTerminal[chPartial](d, wf.Stopped)
				m.Parallel(3).OnSiblingFailure(wf.CancelSiblings).Join(join, m.To(merged, "met"), m.To(partial, "partial"))
				return d
			}
			d := build()
			straight := newHarness(t, d, nil)
			run := straight.run(d, nil)
			wantJoin, wantSt := stJoinDrive(t, straight, run)
			recs := straight.records(run)
			nCancel := len(cancelled(t, straight, run))
			if wantSt.Phase != wf.RunEnded {
				t.Fatalf("straight run did not end: %+v", wantSt)
			}

			// A crash after any record, then a resume to the end.
			for n := 1; n <= len(recs); n++ {
				h2 := resumeFrom(t, straight, d, run, n)
				gotJoin, gotSt := stJoinDrive(t, h2, run)
				if gotJoin != wantJoin || gotSt.Terminal != wantSt.Terminal {
					t.Fatalf("crash after record %d (%s): join %s at %s, want %s at %s", n, recs[n-1].Kind, gotJoin, gotSt.Terminal, wantJoin, wantSt.Terminal)
				}
			}
			if name == "all_settled" && nCancel == 0 {
				t.Fatal("the scenario cancelled nothing, so the crash points proved nothing")
			}
		})
	}
}

func TestBudgetStoppedChildDoesNotStallMap(t *testing.T) {
	d := fanDef("settle-budget", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1}})
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("instructions = %v, want one (the others are stopped on the budget)", childIDs(ins))
	}
	recs := h.records(run)
	stop := slices.IndexFunc(recs, func(r wf.JournalRecord) bool { return r.Kind == wf.JournalBudgetStopped })
	if stop < 0 {
		t.Fatal("no budget_stopped record")
	}
	// A crash right after the first budget_stopped, before its child_settled.
	h2 := resumeFrom(t, h, d, run, stop+1)
	if n := len(h2.kind(run, wf.JournalChildSettled)); n != 0 {
		t.Fatalf("setup: %d child_settled before the cut", n)
	}
	ins = h2.next(run)
	if len(ins) != 1 {
		t.Fatalf("after the resume instructions = %v, want one", childIDs(ins))
	}
	var unavailable int
	for _, r := range h2.kind(run, wf.JournalChildSettled) {
		if r.Class == wf.Unavailable {
			unavailable++
		}
	}
	if unavailable != 2 {
		t.Fatalf("Unavailable child_settled records = %d, want the two stopped children", unavailable)
	}
	h2.submit(run, ins[0], `{"OK":true}`)
	statusOf(t, h2.status(run), wf.RunEnded, "chPartial")
	if got := kids(h2.status(run))["chFan[3]"].Phase; got != "budget_stopped" {
		t.Fatalf("stopped child phase = %q, want it still reported budget_stopped", got)
	}
}

// settleHook sees every appended record before it is written.
type settleHook struct {
	wf.Journal
	before func(wf.JournalRecord)
}

func (j *settleHook) Append(r wf.JournalRecord) error {
	j.before(r)
	return j.Journal.Append(r)
}

func TestOpenIntentCheckedBeforeCancel(t *testing.T) {
	d := stMapDef("settle-check", twoStepChild(mergeEffect(mergeKey)), wf.AllSettled())
	w := newWorld() // the check answers unknown until chFan[2] has failed
	c, run, intent := stCrashed(t, d, w)
	c.e = wf.NewEngine(&settleHook{Journal: c.j, before: func(r wf.JournalRecord) {
		if r.Kind == wf.JournalChildSettled {
			w.mu.Lock()
			w.status["merge/feat"] = wf.EffectDone
			w.mu.Unlock()
		}
	}}, c.reg, wf.EngineOptions{Defs: []*wf.Def{d}})

	failWork(t, c.harness, run, "chFan[2]")

	recs := c.records(run)
	result := slices.IndexFunc(recs, func(r wf.JournalRecord) bool {
		var rd wf.EffectResultData
		return r.Kind == wf.JournalEffectResult && json.Unmarshal(r.Data, &rd) == nil && rd.Intent == intent
	})
	at, ok := cancelled(t, c.harness, run)["chFan[1]"]
	if !ok || result < 0 || result > at {
		t.Fatalf("effect_result at %d, cancel of chFan[1] at %d (%v): the done effect's result must come first", result, at, kinds(recs))
	}
	var rd wf.EffectResultData
	_ = json.Unmarshal(recs[result].Data, &rd)
	if !rd.OK || rd.Outcome != "done" {
		t.Fatalf("effect_result = %+v, want outcome done", rd)
	}
	if n := w.appliedCount(); n != 1 {
		t.Fatalf("effect applied %d times, want only the pre-crash apply", n)
	}
}
