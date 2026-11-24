package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A halted descendant the run is held at blocks it; one its Map absorbed does
// not, and still shows on its unit line and its instance entry.
func TestHaltedDescendantBlocked(t *testing.T) {
	fail := true
	d := retryMapDef(&fail)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.next(run) // item 1 halts and its Map absorbs it; item 2 waits at its agent
	st := h.status(run)
	if st.Phase != wf.RunRunning || strings.HasPrefix(st.NothingReadyReason, "blocked:") {
		t.Fatalf("status = %s / %q, want running with no blocked: reason for an absorbed halt", st.Phase, st.NothingReadyReason)
	}
	item := kids(st)["RrFan[1]"]
	if !strings.Contains(item.Reason, "item 1 broke") {
		t.Fatalf("item 1 = %+v, want its halt reason on the instance entry", item)
	}

	// A run held at its root is blocked.
	rh, rrun := eiHaltWorld(t, nil)
	rh.next(rrun)
	if st := rh.status(rrun); st.Phase != wf.RunBlocked || !strings.HasPrefix(st.NothingReadyReason, "blocked: cRun ") {
		t.Fatalf("status = %s / %q, want blocked at cRun", st.Phase, st.NothingReadyReason)
	}
}

// An agent unit whose halt its Map absorbed shows failed with the halt reason,
// in the human view and in the JSON units; no blocked: reason appears.
func TestAbsorbedAgentHaltShowsOnUnitLine(t *testing.T) {
	cd := wf.New("ab-item")
	a := wf.NewAgent[RrWork, chOut](cd)
	ok := wf.NewTerminal[RrOK](cd, wf.Success)
	a.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.RetryPolicy[RrWork]{})
	toOK := a.To(ok, "ok")
	a.Route(func(chOut, wf.Ctx) wf.Edge[RrWork] { return toOK }, toOK)
	wf.Yield(ok, func(wf.Ctx) chRes { return chRes{} })
	d := wf.New("ab-map")
	m := wf.NewMap[RrFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, wf.Typed[chItem, chRes](cd))
	merged := wf.NewTerminal[RrMerge](d, wf.Success)
	partial := wf.NewTerminal[RrPart](d, wf.Stopped)
	m.Parallel(2).OnSiblingFailure(wf.ContinueSiblings).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	var tok string
	for _, in := range h.next(run) {
		if in.Child == "RrFan[1]" {
			tok = in.Token
		}
	}
	if tok == "" {
		t.Fatal("no instruction for item 1")
	}
	if _, err := h.e.Fail(run, tok, "no"); err != nil {
		t.Fatal(err)
	}
	st := h.status(run)
	if st.Phase == wf.RunBlocked || strings.HasPrefix(st.NothingReadyReason, "blocked:") {
		t.Fatalf("status = %s / %q: an absorbed halt must not block", st.Phase, st.NothingReadyReason)
	}
	var row *wf.UnitStatus
	for i := range st.Units {
		if st.Units[i].Name == "RrFan/1/RrWork#1" {
			row = &st.Units[i]
		}
	}
	if row == nil || row.State != "failed" || !strings.Contains(row.Note, "abandoned: no") {
		t.Fatalf("units = %+v, want item 1's unit failed with the halt reason as its note", st.Units)
	}
	if view := wf.RenderStatus(st); !strings.Contains(view, "failed") || !strings.Contains(view, row.Note) {
		t.Fatalf("view lacks the failed unit and its reason %q:\n%s", row.Note, view)
	}
	raw, err := json.Marshal(st)
	if err != nil || !strings.Contains(string(raw), row.Note) {
		t.Fatalf("json lacks the halt reason %q: %v\n%s", row.Note, err, raw)
	}
}

func TestStatusNamesDueTimes(t *testing.T) {
	d := engBackoffDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	start := h.now
	h.submit(run, h.next(run)[0], `{"Down":true}`)
	st := h.status(run)
	if len(st.Due) != 1 || st.Due[0].Kind != "backoff" || !st.Due[0].Due.Equal(start.Add(10*time.Second)) {
		t.Fatalf("Due = %+v, want one backoff at start+10s", st.Due)
	}

	fan := fanDef("due-fan", chItems(1, 2), 2, wf.AllSettled())
	h = newHarness(t, fan, nil)
	run = h.run(fan, nil)
	if got := h.handOut(run, 1); len(got) != 1 {
		t.Fatalf("HandOut(1) = %+v", got)
	}
	st = h.status(run)
	var handed []wf.DueTime
	for _, dt := range st.Due {
		if dt.Kind == "hand-out" {
			handed = append(handed, dt)
		}
	}
	if len(handed) != 1 || handed[0].Unit == "" || !handed[0].Due.After(h.now) {
		t.Fatalf("Due = %+v, want one hand-out for the handed unit, after now", st.Due)
	}
	if view := wf.RenderStatus(st); !strings.Contains(view, "due: hand-out "+handed[0].Unit+" ") {
		t.Fatalf("view lacks the due line:\n%s", view)
	}
}

func TestStatusOpenIntents(t *testing.T) {
	d, _ := doneWhenCommand(false)
	w := newWorld()
	w.set("x", "done", nil, false)
	c := newCrashHarness(t, d, w, wf.JournalEffectResult)
	run := c.run(d, map[string]string{"topic": "t"})
	if st := c.status(run); len(st.OpenIntents) != 0 {
		t.Fatalf("OpenIntents before the command = %+v, want none", st.OpenIntents)
	}
	c.cj.armed = true
	if _, err := c.e.Next(run, ""); !errors.Is(err, errCrash) {
		t.Fatalf("Next error = %v, want the crash", err)
	}
	c.resume()
	st := c.status(run)
	if len(st.OpenIntents) != 1 {
		t.Fatalf("OpenIntents = %+v, want one", st.OpenIntents)
	}
	oi := st.OpenIntents[0]
	if oi.Seq == 0 || oi.Effect == "" || !strings.Contains(oi.Effect, ".") || oi.Detached {
		t.Fatalf("OpenIntent = %+v, want a seq and adapter.op, not detached", oi)
	}
	if view := wf.RenderStatus(st); !strings.Contains(view, "open effect "+strconv.FormatInt(oi.Seq, 10)+" "+oi.Effect+"\n") {
		t.Fatalf("view lacks the open effect line:\n%s", view)
	}
}

func TestStatusLockHolder(t *testing.T) {
	dir := t.TempDir()
	d := engBackoffDef()
	newEngine := func() wf.Executor {
		reg := wf.NewAdapters()
		return wf.NewEngine(wf.FileJournal(dir), reg, wf.EngineOptions{Defs: []*wf.Def{d}})
	}
	e := newEngine()
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A later CLI call is a fresh process: a new journal value finds the record
	// the earlier call left in the lock file.
	st, err := newEngine().Status(run)
	if err != nil {
		t.Fatal(err)
	}
	if st.LockHolder == nil || st.LockHolder.Pid != os.Getpid() || st.LockHolder.Since.IsZero() {
		t.Fatalf("LockHolder = %+v, want this pid with a start time", st.LockHolder)
	}
	if view := wf.RenderStatus(st); !strings.Contains(view, "lock: last held by pid ") {
		t.Fatalf("view lacks the lock line:\n%s", view)
	}
}

func TestNothingReadyReason(t *testing.T) {
	fan := fanDef("idle-fan", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, fan, nil)
	run := h.run(fan, nil)
	h.handOut(run, 0) // drive to the pending units without handing any out
	if st := h.status(run); st.NothingReadyReason != "" {
		t.Fatalf("reason = %q with two pending units, want none", st.NothingReadyReason)
	}
	if got := h.handOut(run, -1); len(got) != 2 {
		t.Fatalf("HandOut = %+v, want two", got)
	}
	st := h.status(run)
	if st.NothingReadyReason != "waiting on 2 running units" {
		t.Fatalf("reason = %q, want waiting on 2 running units", st.NothingReadyReason)
	}
	if view := wf.RenderStatus(st); !strings.Contains(view, "nothing ready: waiting on 2 running units\n") {
		t.Fatalf("view lacks the reason:\n%s", view)
	}

	bd := engBackoffDef()
	h = newHarness(t, bd, nil)
	run = h.run(bd, nil)
	due := h.now.Add(10 * time.Second)
	h.submit(run, h.next(run)[0], `{"Down":true}`)
	want := "waiting until " + due.UTC().Format(time.RFC3339) + " (backoff)"
	if got := h.status(run).NothingReadyReason; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}

	h = newHarness(t, bd, nil)
	run = h.run(bd, nil)
	h.submit(run, h.next(run)[0], `{"Down":false}`)
	if st := h.status(run); st.Phase != wf.RunEnded || st.NothingReadyReason != "" {
		t.Fatalf("ended run: phase %s reason %q, want ended and none", st.Phase, st.NothingReadyReason)
	}
}
