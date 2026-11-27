package wf_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	rjDraft  struct{}
	rjOther  struct{}
	rjVerify struct{}
	rjReview struct{}
	rjDone   struct{}
	rjStop   struct{}
	rjFell   struct{}
	rjCapped struct{}
)

// rjOpts are the variations of rjDef.
type rjOpts struct {
	returns  *int // Def.Returns
	two      bool // the compute also reads a second agent's output
	of       bool // with two, the compute names the producer (RejectOf)
	explicit bool // the compute declares a SendBack edge to the drafter (bound 2)
	capDraft bool // the drafter's visits are capped at 1
	check    bool // a reviewer's check rejects instead of a compute
}

// rjDef is a drafter whose output a compute (or a reviewer's check)
// rejects when it is "bad". None of its prompts binds wf.Returned().
func rjDef(o rjOpts) *wf.Def {
	d := wf.New("reject")
	if o.returns != nil {
		d.Returns(wf.Const(*o.returns))
	}
	draft := wf.NewAgent[rjDraft, string](d)
	done := wf.NewTerminal[rjDone](d, wf.Success)
	stop := wf.NewTerminal[rjStop](d, wf.Stopped)
	fell := wf.NewTerminal[rjFell](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	draft.Spec(wf.PromptText("Draft."), wf.Retry(wf.Const(2), draft.To(stop, "draft gave up")))
	if o.capDraft {
		wf.MaxVisits(wf.Const(1), draft.To(wf.NewTerminal[rjCapped](d, wf.Stopped), "draft cap").Ends(wf.Stopped, "one draft only"))
	}
	bad := func(s string) bool { return s == "bad" }
	if o.check {
		review := wf.NewAgent[rjReview, string](d)
		draft.Then(draft.To(review, "drafted"))
		review.Spec(wf.PromptText("Review {{d}}", wf.As("d", wf.Out(draft))), wf.Retry(wf.Const(2), review.To(stop, "review gave up")),
			wf.Checkf("draft is usable", func(_ string, c wf.Ctx) error {
				if bad(wf.OutputOf(c, draft)) {
					return wf.Reject("the draft is bad")
				}
				return nil
			}))
		review.Then(review.To(done, "reviewed"))
		return d
	}
	var other *wf.Agent[rjOther, string]
	verify := wf.NewCompute[rjVerify, string](d, func(c wf.Ctx) (string, error) {
		s := wf.OutputOf(c, draft)
		if o.two {
			_ = wf.OutputOf(c, other)
		}
		if bad(s) {
			if o.of {
				return "", wf.RejectOf(draft, "the draft is bad")
			}
			return "", wf.Reject("the draft is bad")
		}
		return s, nil
	})
	if o.two {
		other = wf.NewAgent[rjOther, string](d)
		other.Spec(wf.PromptText("Other."), wf.Retry(wf.Const(1), other.To(stop, "other gave up")))
		draft.Then(draft.To(other, "drafted"))
		other.Then(other.To(verify, "both"))
	} else {
		draft.Then(draft.To(verify, "drafted"))
	}
	verify.Retry(wf.Retry[rjVerify](wf.Const(1), verify.To(fell, "unverified").Ends(wf.Stopped, "the draft stayed bad")))
	ok := verify.To(done, "verified")
	if o.explicit {
		back := verify.SendBack(draft, "revise", func(string) []string { return nil }, wf.Const(2),
			verify.To(stop, "not fixed").Ends(wf.Stopped, "the draft was not fixed"))
		verify.Route(func(string, wf.Ctx) wf.Edge[rjVerify] { return ok }, ok, back)
	} else {
		verify.Then(ok)
	}
	return d
}

func intp(n int) *int { return &n }

// rjDrive submits each answer to the next agent unit in turn.
func rjDrive(t *testing.T, h *harness, run wf.RunID, answers ...[2]string) []wf.Instruction {
	t.Helper()
	var issued []wf.Instruction
	for _, a := range answers {
		in := sbOnly(t, h.next(run), a[0])
		issued = append(issued, in)
		if res := h.submit(run, in, a[1]); res.Class != wf.Accepted {
			t.Fatalf("%s %s: %+v", a[0], a[1], res)
		}
	}
	return issued
}

func rjPlans(h *harness, run wf.RunID) []wf.EdgeStep {
	var out []wf.EdgeStep
	for _, r := range h.records(run) {
		var p struct{ Plan *wf.EdgePlan }
		if json.Unmarshal(r.Data, &p) == nil && p.Plan != nil {
			out = append(out, p.Plan.Steps...)
		}
	}
	return out
}

func TestRejectReturnsTheWorkToTheProducerOnce(t *testing.T) {
	d := rjDef(rjOpts{})
	if ps := append(d.Lint(), d.LintSound()...); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	rjDrive(t, h, run, [2]string{"rjDraft", `"bad"`})
	again := sbOnly(t, h.next(run), "rjDraft")
	want := "Your earlier output was returned by rjVerify (round 1 of 1): the draft is bad. Your prior output and files follow for revision."
	if !strings.Contains(again.Prompt, want) || !strings.Contains(again.Prompt, "### returned") || again.Inputs["returned"] == "" {
		t.Fatalf("returned prompt lacks the note or the block:\n%s", again.Prompt)
	}
	if !strings.Contains(again.Inputs["returned"], `"prior":"bad"`) || !strings.Contains(again.Inputs["returned"], `"auto":true`) {
		t.Errorf("returned input = %s", again.Inputs["returned"])
	}
	var auto []wf.EdgeStep
	for _, s := range rjPlans(h, run) {
		if s.Auto {
			auto = append(auto, s)
		}
	}
	if len(auto) != 1 || auto[0].From != "rjVerify" || auto[0].To != "rjDraft" || auto[0].Index != -1 || auto[0].SendBack.Reasons[0] != "the draft is bad" {
		t.Fatalf("auto steps = %+v", auto)
	}
	if st := h.status(run); st.Instances[0].Returned == nil || !st.Instances[0].Returned.Auto {
		t.Errorf("status = %+v", st.Instances)
	}
	// The return spent none of the compute's one attempt.
	for _, r := range h.kind(run, wf.JournalRefused) {
		var rd wf.RefusedData
		if json.Unmarshal(r.Data, &rd) == nil && rd.Spent {
			t.Errorf("the return spent an attempt: %+v", rd)
		}
	}
	h.submit(run, again, `"good"`)
	statusOf(t, h.status(run), wf.RunEnded, "rjDone")
}

// Once the returns are spent, the Reject fails the compute as a plain error
// would: here its retry is exhausted and it takes its fallback.
func TestRejectFailsOverOnceSpent(t *testing.T) {
	d := rjDef(rjOpts{})
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	rjDrive(t, h, run, [2]string{"rjDraft", `"bad"`}, [2]string{"rjDraft", `"bad"`})
	statusOf(t, h.status(run), wf.RunEnded, "rjFell")
}

func TestRejectReturnsCanBeSwitchedOff(t *testing.T) {
	d := rjDef(rjOpts{returns: intp(0)})
	if !strings.Contains(d.Render(), "returns: ≤0") {
		t.Errorf("render lacks the bound:\n%s", d.Render())
	}
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	rjDrive(t, h, run, [2]string{"rjDraft", `"bad"`})
	statusOf(t, h.status(run), wf.RunEnded, "rjFell")
}

// A call that read two agents' outputs cannot say whose was wrong: no
// return, a degraded note in status, and the plain failure. RejectOf names
// the producer.
func TestRejectWithTwoReadsNeedsRejectOf(t *testing.T) {
	d := rjDef(rjOpts{two: true})
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	rjDrive(t, h, run, [2]string{"rjDraft", `"bad"`}, [2]string{"rjOther", `"x"`})
	st := h.status(run)
	statusOf(t, st, wf.RunEnded, "rjFell")
	if out := wf.RenderStatus(st); !strings.Contains(out, "reject unattributed") || !strings.Contains(out, "rjDraft and rjOther") || !strings.Contains(out, "wf.RejectOf") {
		t.Errorf("status lacks the unattributed note:\n%s", out)
	}

	d = rjDef(rjOpts{two: true, of: true})
	h = newHarness(t, d, nil)
	run = h.run(d, nil)
	rjDrive(t, h, run, [2]string{"rjDraft", `"bad"`}, [2]string{"rjOther", `"x"`}, [2]string{"rjDraft", `"good"`}, [2]string{"rjOther", `"x"`})
	statusOf(t, h.status(run), wf.RunEnded, "rjDone")
}

// A check's Reject accepts the checked submission and returns the work to
// the upstream producer.
func TestRejectFromACheckReturnsUpstream(t *testing.T) {
	d := rjDef(rjOpts{check: true})
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	rjDrive(t, h, run, [2]string{"rjDraft", `"bad"`}, [2]string{"rjReview", `"looks off"`})
	again := sbOnly(t, h.next(run), "rjDraft")
	if !strings.Contains(again.Prompt, "returned by rjReview (round 1 of 1): the draft is bad") {
		t.Fatalf("prompt:\n%s", again.Prompt)
	}
	if n := len(h.kind(run, wf.JournalAccepted)); n != 2 {
		t.Errorf("%d accepted records, want the draft and the review", n)
	}
	h.submit(run, again, `"good"`)
	rjDrive(t, h, run, [2]string{"rjReview", `"fine"`})
	statusOf(t, h.status(run), wf.RunEnded, "rjDone")
}

// An explicit SendBack edge to the producer takes precedence: its label and
// bound, then its fallback.
func TestRejectTakesAnExplicitSendBack(t *testing.T) {
	d := rjDef(rjOpts{explicit: true})
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	rjDrive(t, h, run, [2]string{"rjDraft", `"bad"`}, [2]string{"rjDraft", `"bad"`}, [2]string{"rjDraft", `"bad"`})
	statusOf(t, h.status(run), wf.RunEnded, "rjStop")
	var labels []string
	for _, s := range rjPlans(h, run) {
		if s.SendBack != nil {
			labels = append(labels, s.Label)
		}
	}
	if strings.Join(labels, ",") != "revise,revise" {
		t.Errorf("returns over %v, want revise twice", labels)
	}
}

// The producer's visit cap is checked on the arrival a return causes.
func TestRejectArrivalCountsAgainstTheProducersVisitCap(t *testing.T) {
	d := rjDef(rjOpts{capDraft: true})
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	rjDrive(t, h, run, [2]string{"rjDraft", `"bad"`})
	statusOf(t, h.status(run), wf.RunEnded, "rjCapped")
}
