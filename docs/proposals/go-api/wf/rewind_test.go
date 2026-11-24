package wf_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	rwAsk      struct{}
	rwDesign   struct{}
	rwGate     struct{}
	rwPass     struct{}
	rwStopped  struct{}
	rwFin      struct{}
	rwStopRoot struct{}
	rwGateOut  struct{ Refused bool }
)

// rewindDef is an agent, then a Sub whose child is one gate (a compute that
// refuses while *refuse holds) ending at rwStopped or rwPass; the root stops
// at rwStopRoot on rwStopped, rung 3's shape.
func rewindDef(refuse *bool) *wf.Def {
	kid := wf.New("rw-design")
	gate := wf.NewCompute[rwGate, rwGateOut](kid, func(wf.Ctx) (rwGateOut, error) { return rwGateOut{Refused: *refuse}, nil })
	pass := wf.NewTerminal[rwPass](kid, wf.Success)
	stopped := wf.NewTerminal[rwStopped](kid, wf.Stopped)
	toPass, toStop := gate.To(pass, "tree sound"), gate.To(stopped, "refused")
	gate.Route(func(o rwGateOut, _ wf.Ctx) wf.Edge[rwGate] {
		if o.Refused {
			return toStop
		}
		return toPass
	}, toPass, toStop)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(pass, res)
	wf.Yield(stopped, res)

	d := wf.New("rw")
	ask := wf.NewAgent[rwAsk, eOut](d)
	sub := wf.NewSub[rwDesign, chItem, chRes](d, wf.Typed[chItem, chRes](kid), func(wf.Ctx) chItem { return chItem{} })
	fin := wf.NewTerminal[rwFin](d, wf.Success)
	stop := wf.NewTerminal[rwStopRoot](d, wf.Stopped)
	ask.Spec(wf.PromptText("ask"), wf.Retry[rwAsk](wf.Const(1), ask.To(stop, "gave up")))
	next := ask.To(sub, "go")
	ask.Route(func(eOut, wf.Ctx) wf.Edge[rwAsk] { return next }, next)
	sub.Exits(wf.OnExit[rwPass](sub.To(fin, "designed")), wf.OnExit[rwStopped](sub.To(stop, "design stopped")))
	return d
}

// E2: rewind --to died forks a run that ended at a terminal a defect reached
// (rung 3: a gate refused inside the design child, so the run stopped). The
// fork's journal is the prefix up to the gate's arrival plus a rewound record;
// the source is untouched; on the fixed binary the fork re-enters the gate,
// passes and finishes, and the agent before it is not done again.
func TestRewindToDiedForksAtTheStateThatEndedTheRun(t *testing.T) {
	refuse := true
	d := rewindDef(&refuse)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	st := h.drive(run, map[string][]string{"rwAsk": {`{}`}})
	if st.Phase != wf.RunEnded || st.Terminal != "rwStopRoot" {
		t.Fatalf("source = %+v, want ended at rwStopRoot", st)
	}
	before := len(h.records(run))
	e := h.e.(*wf.Engine)
	rep, err := e.Rewind(run, "died")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Run == run || rep.Data.From != run || !strings.Contains(rep.Data.State, "rwGate") || !strings.HasPrefix(rep.Data.State, "rwDesign") {
		t.Fatalf("rewind = %+v, want a fork of %s at rwDesign / rwGate", rep, run)
	}
	if n := len(h.records(run)); n != before {
		t.Fatalf("the source journal changed: %d records, was %d", n, before)
	}
	fork := h.records(rep.Run)
	if last := fork[len(fork)-1]; last.Kind != wf.JournalRewound {
		t.Fatalf("the fork's last record is %s, want rewound", last.Kind)
	}
	for _, r := range fork[:len(fork)-1] {
		if r.Instance != "" && r.State == "rwGate" {
			t.Fatalf("the fork kept the gate's work: %s seq %d", r.Kind, r.Seq)
		}
	}
	refuse = false // the fixed binary
	ins, err := e.Next(rep.Run, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].Kind != wf.InstructionEnded {
		t.Fatalf("fork next = %+v, want the run ended", ins)
	}
	got, _ := e.Status(rep.Run)
	if got.Terminal != "rwFin" {
		t.Fatalf("fork ended at %q, want rwFin", got.Terminal)
	}
	if n := len(h.kind(rep.Run, wf.JournalInstructionIssued)); n != 1 {
		t.Fatalf("the fork issued %d agent instructions, want the one from before the point", n)
	}
	if again, _ := e.Status(run); again.Terminal != "rwStopRoot" {
		t.Fatalf("the source now ends at %q", again.Terminal)
	}
	// A second rewind makes a second fork; a run that has not died is refused.
	if rep2, err := e.Rewind(run, "died"); err != nil || rep2.Run == rep.Run {
		t.Fatalf("second rewind = %+v, %v; want another fork", rep2, err)
	}
	h2 := newHarness(t, d, nil)
	run2 := h2.run(d, nil)
	if _, err := h2.e.(*wf.Engine).Rewind(run2, "died"); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "has not died") {
		t.Fatalf("rewind of a live run = %v, want refused", err)
	}
	var rd wf.RewoundData
	_ = json.Unmarshal(fork[len(fork)-1].Data, &rd)
	if rd.Seq != fork[len(fork)-2].Seq {
		t.Fatalf("rewound seq %d, want the fork's last kept seq %d", rd.Seq, fork[len(fork)-2].Seq)
	}
}

// E2 through the CLI: dev rewind <run> --to died prints the fork's id, names
// the state that runs again and the carry-on line, and the fork hands that
// state out again while the source stays ended.
func TestDevRewindThroughTheCLI(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	in := e.next(run, "1")[0]
	e.must("dev", "started", string(run), in.Name, "--session", "s1", "--agent", "a1", "--state-dir", e.state)
	e.must("done", string(run), in.Name, "--output", writeAllocated(t, in.Output, `{"OK":true}`), "--state-dir", e.state)
	if code, _, er := e.cli("dev", "rewind", string(run), "--state-dir", e.state); code != 2 || !strings.Contains(er, "--to is required") {
		t.Fatalf("rewind without --to = %d %s, want a usage refusal", code, er)
	}
	code, out, er := e.cli("dev", "rewind", string(run), "--to", "died", "--state-dir", e.state)
	if code != 0 {
		t.Fatalf("dev rewind exited %d: %s", code, er)
	}
	fork := wf.RunID(strings.TrimSpace(out))
	if fork == run || !strings.Contains(er, "clAsk runs again in "+string(fork)) || !strings.Contains(er, "carry on: adopt the fork with:\n  cd ") || !strings.Contains(er, "\nthen the coordinator drives it with:\n  cd ") {
		t.Fatalf("dev rewind = %q / %q", out, er)
	}
	// Each command it prints is a line of its own, ending at its own last
	// argument: nothing before it but the indent, nothing after it.
	bin, _ := os.Executable()
	commands := 0
	for _, line := range strings.Split(er, "\n") {
		if !strings.Contains(line, bin) {
			continue
		}
		commands++
		if why := plainCommandProblem(line, bin); why != "" || !strings.HasSuffix(line, " --state-dir "+e.state) || !strings.Contains(line, " "+string(fork)+" ") {
			t.Errorf("dev rewind printed %q: %s, want the fork's command alone on its line", line, why)
		}
	}
	if commands != 2 {
		t.Errorf("dev rewind printed %d command lines, want 2 (adopt, then next):\n%s", commands, er)
	}
	again := e.next(fork, "1")
	if len(again) != 1 || again[0].State != "clAsk" {
		t.Fatalf("the fork handed out %+v, want clAsk again", again)
	}
	if st := e.status(run); st.Phase != wf.RunEnded {
		t.Fatalf("the source is %s, want still ended", st.Phase)
	}
}

// E2 review: a Map with two children where the first fails (chBad) and the
// second finishes (chOK): the Map joins partial and the run stops. The fork
// keeps the finished child's work and runs only the failed child's state
// again; the kept records keep their seqs.
func TestRewindKeepsFinishedSiblingWork(t *testing.T) {
	d := fanDef("rw-fan", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	st := h.drive(run, map[string][]string{"c-item/chWork": {`{"OK":false}`, `{"OK":true}`}})
	if st.Terminal != "chPartial" {
		t.Fatalf("source ended at %q, want chPartial", st.Terminal)
	}
	e := h.e.(*wf.Engine)
	rep, err := e.Rewind(run, "died")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rep.Data.State, "chFan / chWork") {
		t.Fatalf("died at %q, want chFan / chWork of the failed child", rep.Data.State)
	}
	src, fork := h.records(run), h.records(rep.Run)
	for i, r := range fork[:len(fork)-1] {
		if r.Seq != src[i].Seq {
			t.Fatalf("fork record %d has seq %d, source %d", i, r.Seq, src[i].Seq)
		}
	}
	again, err := e.Next(rep.Run, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].State != "c-item/chWork" {
		t.Fatalf("fork offered %+v, want only the failed child's chWork", again)
	}
	if _, err := e.Submit(rep.Run, wf.Submission{Token: again[0].Token, Child: again[0].Child, Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	if fin, _ := e.Status(rep.Run); fin.Terminal != "chMerged" {
		t.Fatalf("fork ended at %q, want chMerged with the kept sibling", fin.Terminal)
	}
}

type (
	nsWork  struct{}
	nsOK    struct{}
	nsInner struct{}
	nsIDone struct{}
	nsIStop struct{}
	nsSub   struct{}
	nsFin   struct{}
	nsStop  struct{}
)

// nestedDef is the cross-review's repro: root Sub -> child (an Inner Sub)
// -> grandchild agent whose route goes off its declared candidates when off
// is set; Inner routes Blocked to its stop, and so does the root.
func nestedDef(off *bool) *wf.Def {
	gk := wf.New("ns-grand")
	w := wf.NewAgent[nsWork, eOut](gk)
	ok := wf.NewTerminal[nsOK](gk, wf.Success)
	w.Spec(wf.PromptText("w"), wf.Retry[nsWork](wf.Const(1), w.To(ok, "x")))
	toOK := w.To(ok, "done")
	w.Route(func(eOut, wf.Ctx) wf.Edge[nsWork] {
		if *off {
			return wf.Edge[nsWork]{} // not a declared candidate
		}
		return toOK
	}, toOK)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)

	ck := wf.New("ns-child")
	inner := wf.NewSub[nsInner, chItem, chRes](ck, wf.Typed[chItem, chRes](gk), func(wf.Ctx) chItem { return chItem{} })
	idone := wf.NewTerminal[nsIDone](ck, wf.Success)
	istop := wf.NewTerminal[nsIStop](ck, wf.Stopped)
	ck.OnFailure(wf.Blocked, istop)
	inner.Exits(wf.OnExit[nsOK](inner.To(idone, "ok")))
	wf.Yield(idone, res)
	wf.Yield(istop, res)

	d := wf.New("ns")
	sub := wf.NewSub[nsSub, chItem, chRes](d, wf.Typed[chItem, chRes](ck), func(wf.Ctx) chItem { return chItem{} })
	fin := wf.NewTerminal[nsFin](d, wf.Success)
	stop := wf.NewTerminal[nsStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	sub.Exits(wf.OnExit[nsIDone](sub.To(fin, "ok")), wf.OnExit[nsIStop](sub.To(stop, "inner stopped")))
	return d
}

// Cross-review 2: when the dying state is a Sub whose child settled with no
// terminal, the fork drops that state's child spawn and every record of the
// child's own descendants with it, so the fork folds and carries on: on the
// fixed binary the grandchild is spawned again and the run finishes.
func TestRewindDropsADroppedChildsDescendants(t *testing.T) {
	off := true
	d := nestedDef(&off)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	st := h.drive(run, map[string][]string{"ns-child/ns-grand/nsWork": {`{}`}})
	if st.Phase != wf.RunEnded {
		t.Fatalf("source = %+v, want ended", st)
	}
	e := h.e.(*wf.Engine)
	rep, err := e.Rewind(run, "died")
	if err != nil {
		t.Fatal(err)
	}
	off = false
	ins, err := e.Next(rep.Run, "")
	if err != nil {
		t.Fatalf("next on the fork (died at %q) = %v", rep.Data.State, err)
	}
	if len(ins) != 1 || ins[0].Kind != wf.InstructionAgent {
		t.Fatalf("fork offered %+v, want the grandchild's agent again", ins)
	}
	if _, err := e.Submit(rep.Run, wf.Submission{Token: ins[0].Token, Child: ins[0].Child, Output: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if fin, _ := e.Status(rep.Run); fin.Terminal != "nsFin" {
		t.Fatalf("fork ended at %q (%s), want nsFin", fin.Terminal, fin.Cause)
	}
}

// Rewind and the recovery offer (B2, F1): a run held only at a recovery
// offer, its answer journaled and not yet applied, has not died: resume
// applies it, so rewind refuses. A run that died through a recovery give-up
// forks at the state whose decision ended it: its offer, answer and degraded
// records go with the dying path, and the fork folds and re-enters the state.
func TestRewindAndRecovery(t *testing.T) {
	t.Run("a pending answer has not died", func(t *testing.T) {
		d := exDef("rw-recover", false)
		h := newHarness(t, d, nil)
		h.manualRecovery = true
		run := exRun(t, h, d, true)
		failWorkOnce(t, h, run, "a")
		failWorkOnce(t, h, run, "b")
		unit := recoveryOffers(t, h, run)[0].Unit
		cj := &crashJournal{Journal: h.j, kind: wf.JournalDegraded, armed: true}
		if err := wf.NewEngine(cj, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }}).Recover(run, unit, wf.RecoveryGiveUp); !errors.Is(err, errCrash) {
			t.Fatalf("Recover = %v, want the crash before the degraded record", err)
		}
		if len(h.kind(run, wf.JournalRecoveryAnswered)) != 1 {
			t.Fatal("the answer was not journaled")
		}
		if _, err := h.e.(*wf.Engine).Rewind(run, "died"); err == nil || !strings.Contains(err.Error(), "has not died") {
			t.Fatalf("Rewind of a run held at a pending answer = %v, want 'has not died'", err)
		}
	})
	t.Run("a run that died through a give-up", func(t *testing.T) {
		d := exDef("rw-gaveup", false)
		h := newHarness(t, d, nil)
		run := exRun(t, h, d, true)
		failWorkOnce(t, h, run, "a")
		failWorkOnce(t, h, run, "b")
		statusOf(t, h.status(run), wf.RunEnded, "exGaveUp") // the harness gave the offer up
		rep, err := h.e.(*wf.Engine).Rewind(run, "died")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range h.records(rep.Run) {
			switch r.Kind {
			case wf.JournalRecoveryOffered, wf.JournalRecoveryAnswered:
				t.Fatalf("the fork carries %s: it belongs to the dying path", r.Kind)
			case wf.JournalDegraded:
				var dd wf.DegradedData
				if json.Unmarshal(r.Data, &dd) == nil && dd.Kind == wf.DegradedRecovery {
					t.Fatal("the fork carries the give-up's recovery record")
				}
			case wf.JournalRefused:
				var rd wf.RefusedData
				if json.Unmarshal(r.Data, &rd) == nil && rd.Recovery {
					t.Fatal("the fork carries the recovery offer's halt")
				}
			}
		}
		ins := h.next(rep.Run)
		if len(ins) != 1 || ins[0].State != "exWork" {
			t.Fatalf("the fork's next = %+v, want exWork entered again", ins)
		}
	})
}
