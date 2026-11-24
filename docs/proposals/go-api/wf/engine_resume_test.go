package wf_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types for the accounting-on-resume tests.
type (
	rsFirst  struct{}
	rsSecond struct{}
	rsDone   struct{}
	rsBad    struct{}
)

type rsOut struct{ V string }

// rsChainDef is First -> Second -> Done; First refuses V "bad" (two attempts).
func rsChainDef() *wf.Def {
	d := wf.New("resume-chain")
	a := wf.NewAgent[rsFirst, rsOut](d)
	b := wf.NewAgent[rsSecond, rsOut](d)
	done := wf.NewTerminal[rsDone](d, wf.Success)
	bad := wf.NewTerminal[rsBad](d, wf.Stopped)
	a.Spec(wf.PromptText("First."), wf.Retry[rsFirst](wf.Const(2), a.To(bad, "gave up")),
		wf.Checkf("not-bad", func(o rsOut, _ wf.Ctx) error {
			if o.V == "bad" {
				return errors.New("V is bad")
			}
			return nil
		}))
	b.Spec(wf.PromptText("Second."), wf.Retry[rsSecond](wf.Const(2), b.To(bad, "gave up")))
	toB := a.To(b, "to second")
	a.Route(func(rsOut, wf.Ctx) wf.Edge[rsFirst] { return toB }, toB)
	toDone := b.To(done, "done")
	b.Route(func(rsOut, wf.Ctx) wf.Edge[rsSecond] { return toDone }, toDone)
	return d
}

// resumeFrom copies the first n records of run into a fresh MemJournal under a
// new engine with the same definition: a process that died after record n.
func resumeFrom(t *testing.T, h *harness, d *wf.Def, run wf.RunID, n int) *harness {
	t.Helper()
	recs := h.records(run)
	if n > len(recs) {
		t.Fatalf("journal has %d records, want at least %d", len(recs), n)
	}
	h2 := newHarness(t, d, nil)
	h2.now = h.now
	h2.j = wf.MemJournal()
	unlock, err := h2.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, r := range recs[:n] {
		if err := h2.j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	h2.e = wf.NewEngine(h2.j, h2.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h2.now }})
	return h2
}

// afterSubmitted is the record count up to and including the first submitted.
func afterSubmitted(t *testing.T, h *harness, run wf.RunID) int {
	t.Helper()
	i := slices.IndexFunc(h.records(run), func(r wf.JournalRecord) bool { return r.Kind == wf.JournalSubmitted })
	if i < 0 {
		t.Fatal("no submitted record")
	}
	return i + 1
}

func TestBudgetSpentBlocksBackoffReissue(t *testing.T) {
	d := engBackoffDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1}})
	run := h.run(d, nil)
	in := h.next(run)[0]
	h.submit(run, in, `{"Down":true}`)
	if got := len(h.kind(run, wf.JournalBackoffScheduled)); got != 1 {
		t.Fatalf("backoff_scheduled = %d, want 1", got)
	}
	h.now = h.now.Add(time.Minute) // past the 10s backoff
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none: the turn budget is spent", ins)
	}
	if got := len(h.kind(run, wf.JournalInstructionIssued)); got != 1 {
		t.Fatalf("instruction_issued = %d, want 1: no reissue after the backoff", got)
	}
	if got := len(h.kind(run, wf.JournalBudgetStopped)); got != 1 {
		t.Fatalf("budget_stopped = %d, want 1", got)
	}
}

func TestRootBudgetStopWritesRunEnded(t *testing.T) {
	d := rsChainDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1}})
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{"V":"ok"}`)
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none: the budget is spent", ins)
	}
	recs := h.records(run)
	k := kinds(recs)
	n := len(k)
	if k[n-2] != "budget_stopped" || k[n-1] != "run_ended" {
		t.Fatalf("journal tail = %q, want budget_stopped then run_ended", k[max(0, n-2):])
	}
	var d2 wf.RunEndedData
	if err := json.Unmarshal(recs[n-1].Data, &d2); err != nil {
		t.Fatal(err)
	}
	if d2.Outcome != wf.Stopped {
		t.Fatalf("run_ended outcome = %v, want Stopped", d2.Outcome)
	}
	if st := h.status(run); st.Phase != wf.RunEnded || st.Outcome != wf.Stopped {
		t.Fatalf("status = %+v, want RunEnded/Stopped", st)
	}
}

func TestOldStoppedJournalGainsRunEnded(t *testing.T) {
	d := rsChainDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1}})
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{"V":"ok"}`)
	h.next(run)
	stop := slices.IndexFunc(h.records(run), func(r wf.JournalRecord) bool { return r.Kind == wf.JournalBudgetStopped })
	if stop < 0 {
		t.Fatal("no budget_stopped record")
	}
	// An old journal: the stop, and no run_ended after it.
	h2 := resumeFrom(t, h, d, run, stop+1)
	h2.e = wf.NewEngine(h2.j, h2.reg, wf.EngineOptions{Defs: []*wf.Def{d}, RunBudget: wf.RunBudget{Turns: 1}, Clock: func() time.Time { return h2.now }})
	if got := len(h2.kind(run, wf.JournalRunEnded)); got != 0 {
		t.Fatalf("run_ended before resume = %d, want 0", got)
	}
	h2.next(run)
	if got := len(h2.kind(run, wf.JournalRunEnded)); got != 1 {
		t.Fatalf("run_ended after one Next = %d, want 1", got)
	}
	before := len(h2.records(run))
	h2.next(run)
	if after := len(h2.records(run)); after != before {
		t.Fatalf("second Next appended %d records, want 0", after-before)
	}
}

func TestLostDecisionReplayed(t *testing.T) {
	d := rsChainDef()

	t.Run("next decides from the journaled output", func(t *testing.T) {
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		first := h.next(run)[0]
		h.submit(run, first, `{"V":"ok"}`)
		h2 := resumeFrom(t, h, d, run, afterSubmitted(t, h, run))
		if got := len(h2.kind(run, wf.JournalAccepted)); got != 0 {
			t.Fatalf("accepted before resume = %d, want 0", got)
		}
		ins := h2.next(run)
		if got := len(h2.kind(run, wf.JournalSubmitted)); got != 1 {
			t.Fatalf("submitted = %d, want 1: no second submission", got)
		}
		if got := len(h2.kind(run, wf.JournalAccepted)); got != 1 {
			t.Fatalf("accepted = %d, want 1", got)
		}
		if len(ins) != 1 || ins[0].State != "rsSecond" || ins[0].Token == first.Token {
			t.Fatalf("instructions = %+v, want only the next state's", ins)
		}
	})

	t.Run("submit replays the journaled decision", func(t *testing.T) {
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		first := h.next(run)[0]
		h.submit(run, first, `{"V":"journaled"}`)
		h2 := resumeFrom(t, h, d, run, afterSubmitted(t, h, run))
		res := h2.submit(run, first, `{"V":"different"}`)
		if res.Class != wf.Accepted {
			t.Fatalf("class = %v, want Accepted", res.Class)
		}
		if got := len(h2.kind(run, wf.JournalSubmitted)); got != 1 {
			t.Fatalf("submitted = %d, want 1: no second submission", got)
		}
		acc := h2.kind(run, wf.JournalAccepted)
		if len(acc) != 1 || !strings.Contains(string(acc[0].Data), "journaled") || strings.Contains(string(acc[0].Data), "different") {
			t.Fatalf("accepted = %+v, want the journaled output", acc)
		}
	})

	t.Run("a refused submission is replayed as refused", func(t *testing.T) {
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		first := h.next(run)[0]
		want := h.submit(run, first, `{"V":"bad"}`)
		if want.Class != wf.Refused || len(want.Refusal) == 0 {
			t.Fatalf("original = %+v, want a refusal", want)
		}
		h2 := resumeFrom(t, h, d, run, afterSubmitted(t, h, run))
		res := h2.submit(run, first, `{"V":"ok"}`)
		if res.Class != wf.Refused || !slices.Equal(res.Refusal, want.Refusal) {
			t.Fatalf("replay = %+v, want %+v", res, want)
		}
		if got := len(h2.kind(run, wf.JournalSubmitted)); got != 1 {
			t.Fatalf("submitted = %d, want 1", got)
		}
		if got := len(h2.kind(run, wf.JournalRefused)); got != 1 {
			t.Fatalf("refused = %d, want 1: the refusal spends the attempt", got)
		}
	})
}
