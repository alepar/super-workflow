package wf_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The an earlier revision audit: a stalled unit is not "running" for status to wait on.
// Only next asks the coordinator whether its child is still running, and the
// run moves only once that is answered, so status sends the caller to next.
func TestStalledUnitReasonSaysRunNext(t *testing.T) {
	d := lsDef("lv-a", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, 0))
	run, ins := handOneUnit(t, h, d)
	lvRunning(t, h, m, run, ins)
	h.now = h.now.Add(20*time.Minute + time.Second)
	h.handOut(run, 0)
	if n := countStatus(heartbeats(t, h, run), "stalled"); n != 1 {
		t.Fatalf("stalled heartbeats = %d, want the unit stalled", n)
	}
	got := h.status(run).NothingReadyReason
	if want := "unit " + ins.Name + " is stalled: run next, which asks whether its child is still running"; got != want {
		t.Errorf("status reason = %q, want %q", got, want)
	}
	noPersonOrSettle(t, got)
}

// The an earlier revision audit: an open intent the next call checks again (a keyed
// command, or a keyed edge effect) is not yet the coordinator's to settle:
// next checks it first and settles it by the check when it can. Only an
// effect with no check (non-idempotent) is settled on sight.
func TestRecheckedEffectReasonSaysRunNext(t *testing.T) {
	t.Run("keyed command", func(t *testing.T) {
		c, run := crashedCommand(t, newWorld(), nil, keyedBuild)
		seq := c.kind(run, wf.JournalEffectIntent)[0].Seq
		got := c.status(run).NothingReadyReason
		if want := recoveredReason(seq, false); got != want {
			t.Errorf("status reason = %q, want %q", got, want)
		}
		noPersonOrSettle(t, got)
	})
	t.Run("keyed edge effect", func(t *testing.T) {
		c, run, seq := uncertainRun(t, mergeEffect(wf.KeyOf("merge", "feat")))
		got := c.status(run).NothingReadyReason
		if want := recoveredReason(seq, false); got != want {
			t.Errorf("status reason = %q, want %q", got, want)
		}
		noPersonOrSettle(t, got)
	})
	t.Run("keyed outside effect", func(t *testing.T) {
		// Review round 1 (2): its check answers unknown, so next asks a
		// person. Status names that outcome and does not send a caller who
		// has next's answer back to next.
		c, run, seq := uncertainRun(t, wf.Outside(mergeEffect(wf.KeyOf("merge", "feat"))))
		if ins := c.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification || !ins[0].Person {
			t.Fatalf("instructions = %+v, want a person's manual verification", ins)
		}
		for i := 0; i < 2; i++ {
			if got, want := c.status(run).NothingReadyReason, recoveredReason(seq, true); got != want {
				t.Errorf("status %d: reason = %q, want %q", i, got, want)
			}
		}
	})
	t.Run("idempotent inline command", func(t *testing.T) {
		// Review round 1 (2): an Idempotent command is run again with no check.
		c, run := crashedCommand(t, newWorld(), nil, func(c *wf.Command[cRun]) { c.Idempotent() })
		seq := c.kind(run, wf.JournalEffectIntent)[0].Seq
		got := c.status(run).NothingReadyReason
		if want := recoveredReason(seq, false); got != want {
			t.Errorf("status reason = %q, want %q", got, want)
		}
		if !strings.Contains(got, "runs it again") {
			t.Errorf("status reason = %q does not say it may run it again", got)
		}
	})
	t.Run("detached command past its start grace", func(t *testing.T) {
		// Review round 1 (1): no proc record and no result file past the
		// grace: next starts it again, with no settlement.
		fr, w := newDetRunner(), newWorld()
		d := detDef(idempotentDetached(time.Minute))
		h := newHarnessOpts(t, d, w, detOpts(t, fr))
		withDroppedStarted(t, h, fr, d)
		run := h.run(d, map[string]string{"topic": "build"})
		if _, err := h.e.Next(run, ""); err == nil {
			t.Fatal("Next succeeded, want the lost effect_started append to fail it")
		}
		seq := lastIntent(t, h, run)
		fr.setHideProc(seq, true)
		h.now = h.now.Add(10 * time.Second)
		got := h.status(run).NothingReadyReason
		if want := recoveredReason(seq, false); got != want {
			t.Errorf("status reason = %q, want %q", got, want)
		}
		noPersonOrSettle(t, got)
	})
	t.Run("unkeyed command is settled", func(t *testing.T) {
		c, run := crashedCommand(t, newWorld(), nil, nil)
		if got := c.status(run).NothingReadyReason; !strings.HasPrefix(got, "waiting on the coordinator: settle effect") {
			t.Errorf("status reason = %q, want the coordinator's settle", got)
		}
	})
	t.Run("non-idempotent edge effect is settled", func(t *testing.T) {
		c, run, _ := uncertainRun(t, mergeEffect(wf.NonIdempotent("writes a ledger")))
		if got := c.status(run).NothingReadyReason; !strings.HasPrefix(got, "waiting on the coordinator: settle effect") {
			t.Errorf("status reason = %q, want the coordinator's settle", got)
		}
	})
}

func recoveredReason(seq int64, outside bool) string {
	verdict := "asks for your verdict"
	if outside {
		verdict = "asks for a person's verdict"
	}
	return "effect " + strconv.FormatInt(seq, 10) + " is recovered by the next call, which checks it, runs it again, or " + verdict +
		": run next once and follow what it returns; a later status does not show what it decided"
}

// Review round 1 (3): a coordinator that cannot tell whether a stalled child
// runs answers neither way; next asking again at once, with a reminder to
// drive, sent it round the same question. It waits for fresh evidence.
func TestAnUnansweredStallWaitsForEvidence(t *testing.T) {
	text := wf.RenderNext(wf.NextResult{Stalled: []wf.StalledView{{Name: "u1", Since: time.Unix(0, 0)}}}, "claude",
		func(verb string, args ...string) string {
			return strings.Join(append([]string{"workflow", verb}, args...), " ")
		})
	if strings.Contains(text, "continue driving") {
		t.Errorf("a stall question ends with the drive reminder:\n%s", text)
	}
	if !strings.Contains(text, "wait for fresh evidence") {
		t.Errorf("a stall question does not say to wait for fresh evidence when the coordinator cannot tell:\n%s", text)
	}
}

// an earlier revision post-cap (1): a stall the coordinator cannot judge has a bounded
// recheck. Status lists it as due, and a blocking next that already asked
// waits for it (or for a sign of life) instead of asking again at once; past
// it, next asks again.
func TestAnUnansweredStallIsRecheckedOnASchedule(t *testing.T) {
	d := lsDef("lv-a", true)
	m := newTranscripts()
	h := newHarnessOpts(t, d, nil, lvOpts(m, 0))
	run, ins := handOneUnit(t, h, d)
	lvRunning(t, h, m, run, ins)
	h.now = h.now.Add(20*time.Minute + time.Second)
	co := wf.Coordinator{Harness: "claude", Session: "coord-1"}
	first, err := h.e.(*wf.Engine).NextWait(run, 1, co, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Stalled) != 1 || first.Stalled[0].Name != ins.Name {
		t.Fatalf("first next = %+v, want the stall question", first)
	}
	var recheck time.Time
	for _, due := range h.status(run).Due {
		if due.Kind == "stall recheck" && due.Unit == ins.Name {
			recheck = due.Due
		}
	}
	if !recheck.After(h.now) {
		t.Fatalf("status due = %+v, want a stall recheck for %s after now", h.status(run).Due, ins.Name)
	}
	if first.Stalled[0].Recheck != recheck {
		t.Errorf("stall view recheck = %v, want status's %v", first.Stalled[0].Recheck, recheck)
	}
	h.now = h.now.Add(time.Second) // a later call, the coordinator having looked
	again, err := h.e.(*wf.Engine).NextWait(run, 1, co, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Stalled) != 0 || !again.Timeout {
		t.Fatalf("next before the recheck = %+v, want it to wait, not ask again", again)
	}
	h.now = recheck.Add(time.Second)
	later, err := h.e.(*wf.Engine).NextWait(run, 1, co, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(later.Stalled) != 1 {
		t.Fatalf("next past the recheck = %+v, want the stall question again", later)
	}
	text := wf.RenderNext(later, "claude", func(verb string, args ...string) string {
		return strings.Join(append([]string{"workflow", verb}, args...), " ")
	})
	if !strings.Contains(text, "next waits for a sign of life or its recheck at") {
		t.Errorf("the stall question does not say next waits for the recheck:\n%s", text)
	}
}

// an earlier revision post-cap (2): a journaled launch whose runner cannot be read now is
// not the coordinator's to settle. Status sends the caller to next, and next's
// transient instruction carries no settle line.
func TestAnUnreadableRunnerIsNotSettled(t *testing.T) {
	fr, w := newDetRunner(), newWorld()
	h, run, intent := detStart(t, detDef(idempotentDetached(time.Minute)), fr, w)
	fr.statusErr = errors.New("proc table unreadable")
	got := h.status(run).NothingReadyReason
	if want := "effect " + strconv.FormatInt(intent, 10) + " cannot be read now: run next, which reads it again"; got != want {
		t.Errorf("status reason = %q, want %q", got, want)
	}
	noPersonOrSettle(t, got)
	ins := onlyInstruction(t, h.next(run), wf.InstructionManualVerification)
	if !ins.Transient {
		t.Fatalf("instruction = %+v, want a transient one", ins)
	}
	if ins.Submit != "" || strings.Contains(ins.Prompt, "settle") || strings.Contains(ins.Prompt, "--applied") {
		t.Errorf("a transient instruction carries a settle line: submit %q\n%s", ins.Submit, ins.Prompt)
	}
	if text := renderNext([]wf.Instruction{ins}); strings.Contains(text, "workflow settle") || strings.Contains(text, "--evidence") {
		t.Errorf("next prints a settle line for a transient check:\n%s", text)
	}
}
