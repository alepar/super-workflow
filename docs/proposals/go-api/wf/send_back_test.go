package wf_test

import (
	"encoding/json"

	ha "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	sbDraft   struct{}
	sbReview  struct{}
	sbkDone   struct{}
	sbkStop   struct{}
	sbkCapped struct{}
)

type sbText struct{ Body string }

type sbVerdict struct {
	OK       bool
	Problems []string
}

// sbDef is a drafter whose reviewer returns its draft with the problems it
// found, at most max times, then stops. The drafter writes doc.md; seen
// records what ReturnedOf showed each accepted draft. capDraft caps the
// drafter's visits at 2 with an over edge of its own.
func sbDef(max int, capDraft bool, seen *[]wf.Return) *wf.Def {
	return sbBuild("sendback", max, capDraft, seen, false)
}

// sbBuild is sbDef named name; child yields the terminals' results so it can
// run as a Map child.
func sbBuild(name string, max int, capDraft bool, seen *[]wf.Return, child bool) *wf.Def {
	d := wf.New(name)
	draft := wf.NewAgent[sbDraft, sbText](d)
	review := wf.NewAgent[sbReview, sbVerdict](d)
	done := wf.NewTerminal[sbkDone](d, wf.Success)
	stop := wf.NewTerminal[sbkStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	wf.NewArtifact[string](draft, "doc.md")
	draft.Spec(wf.PromptText("Draft the text.\n{{returned}}", wf.As("returned", wf.Returned())),
		wf.Retry[sbDraft](wf.Const(2), draft.To(stop, "draft gave up")),
		wf.Checkf("note return", func(_ sbText, c wf.Ctx) error {
			if seen != nil {
				*seen = append(*seen, wf.ReturnedOf(c, draft))
			}
			return nil
		}))
	draft.Then(draft.To(review, "drafted"))
	if capDraft {
		capped := wf.NewTerminal[sbkCapped](d, wf.Stopped)
		wf.MaxVisits(wf.Const(2), draft.To(capped, "draft cap").Ends(wf.Stopped, "the drafter had its rounds"))
	}
	review.Spec(wf.PromptText("Review {{draft}}", wf.As("draft", wf.Out(draft))),
		wf.Retry[sbReview](wf.Const(2), review.To(stop, "review gave up")))
	ok := review.To(done, "approved")
	back := review.SendBack(draft, "revise", func(v sbVerdict) []string { return v.Problems },
		wf.Const(max), review.To(stop, "not fixed").Ends(wf.Stopped, "the drafter did not fix the problems"))
	review.Route(func(v sbVerdict, _ wf.Ctx) wf.Edge[sbReview] {
		if v.OK {
			return ok
		}
		return back
	}, ok, back)
	if child {
		wf.Yield(done, func(wf.Ctx) bool { return true })
		wf.Yield(stop, func(wf.Ctx) bool { return false })
	}
	return d
}

func sbRunDir(t *testing.T) wf.EngineOptions {
	dir := t.TempDir()
	return wf.EngineOptions{RunDir: func(wf.RunID) string { return dir }}
}

func sbDocPath(t *testing.T, prompt string) string {
	t.Helper()
	_, after, _ := strings.Cut(prompt, "## Artifacts")
	for _, f := range strings.Fields(after) {
		if strings.HasSuffix(f, "doc.md") {
			return f
		}
	}
	t.Fatalf("no allocated doc.md in:\n%s", prompt)
	return ""
}

func sbOnly(t *testing.T, ins []wf.Instruction, state string) wf.Instruction {
	t.Helper()
	if len(ins) != 1 || ins[0].State != state {
		t.Fatalf("instructions %+v, want one %s", ins, state)
	}
	return ins[0]
}

// sbReturned decodes the returned block from a Draft prompt.
func sbReturned(t *testing.T, prompt string) map[string]any {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, "Draft the text.\n")
	if !ok {
		t.Fatalf("prompt has no draft text:\n%s", prompt)
	}
	line, _, _ := strings.Cut(rest, "\n")
	if strings.TrimSpace(line) == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("returned block %q: %v", line, err)
	}
	return m
}

func TestSendBackReturnsTheWorkToItsProducerWithTheReasons(t *testing.T) {
	var seen []wf.Return
	d := sbDef(2, false, &seen)
	if ps := append(d.Lint(), d.LintSound()...); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}
	if r := d.Render(); !strings.Contains(r, "[send back ≤2, then not fixed]") {
		t.Errorf("render lacks the send-back:\n%s", r)
	}
	h := newHarnessOpts(t, d, nil, sbRunDir(t))
	run := h.run(d, nil)

	first := sbOnly(t, h.next(run), "sbDraft")
	if m := sbReturned(t, first.Prompt); m != nil {
		t.Fatalf("a first visit shows a return: %v", m)
	}
	p1 := sbDocPath(t, first.Prompt)
	if err := os.WriteFile(p1, []byte("first draft"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := h.submit(run, first, `{"Body":"v1"}`); res.Class != wf.Accepted {
		t.Fatalf("draft 1: %+v", res)
	}
	rev := sbOnly(t, h.next(run), "sbReview")
	if res := h.submit(run, rev, `{"OK":false,"Problems":["cite the source","shorter"]}`); res.Class != wf.Accepted {
		t.Fatalf("review 1: %+v", res)
	}

	second := sbOnly(t, h.next(run), "sbDraft")
	if st := h.status(run); len(st.Instances) == 0 || st.Instances[0].Returned == nil || st.Instances[0].Returned.Round != 1 {
		t.Errorf("status instances = %+v, want the return on the run", st.Instances)
	}
	if out := wf.RenderStatus(h.status(run)); !strings.Contains(out, `instance run at sbDraft: returned by sbReview ("revise", round 1 of 2), 2 reason(s)`) {
		t.Errorf("status lacks the return:\n%s", out)
	}
	m := sbReturned(t, second.Prompt)
	if m["from"] != "sbReview" || m["label"] != "revise" || m["round"] != 1.0 || m["of"] != 2.0 {
		t.Errorf("returned block = %v", m)
	}
	if rs, _ := m["reasons"].([]any); len(rs) != 2 || rs[0] != "cite the source" {
		t.Errorf("reasons = %v", m["reasons"])
	}
	if prior, _ := m["prior"].(map[string]any); prior["Body"] != "v1" {
		t.Errorf("prior = %v", m["prior"])
	}
	p2 := sbDocPath(t, second.Prompt)
	arts, _ := m["artifacts"].(map[string]any)
	doc, _ := arts["doc.md"].(map[string]any)
	if doc["prior"] != p1 || doc["path"] != p2 || p1 == p2 {
		t.Errorf("artifacts = %v, want prior %s copied to %s", arts, p1, p2)
	}
	if b, err := os.ReadFile(p2); err != nil || string(b) != "first draft" {
		t.Fatalf("returned visit's doc.md = %q, %v; want the prior draft copied to edit", b, err)
	}
	if b, _ := os.ReadFile(p1); string(b) != "first draft" {
		t.Errorf("prior doc.md changed: %q", b)
	}
	if err := os.WriteFile(p2, []byte("second draft"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := h.submit(run, second, `{"Body":"v2"}`); res.Class != wf.Accepted {
		t.Fatalf("draft 2: %+v", res)
	}
	if b, _ := os.ReadFile(p2); string(b) != "second draft" {
		t.Errorf("an issue after the edit overwrote it: %q", b)
	}
	if len(seen) != 2 || seen[0].From != "" || seen[1].From != "sbReview" || seen[1].Round != 1 || len(seen[1].Reasons) != 2 {
		t.Errorf("ReturnedOf per draft = %+v", seen)
	}
	rev2 := sbOnly(t, h.next(run), "sbReview")
	if res := h.submit(run, rev2, `{"OK":true}`); res.Class != wf.Accepted {
		t.Fatalf("review 2: %+v", res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "sbkDone")
}

// The returns over one edge are bounded: the (max+1)th choice of the edge
// takes its fallback, journaled as such.
func TestSendBackTakesItsFallbackOnceSpent(t *testing.T) {
	d := sbDef(1, false, nil)
	h := newHarnessOpts(t, d, nil, sbRunDir(t))
	run := h.run(d, nil)
	for round := 0; round < 2; round++ {
		dr := sbOnly(t, h.next(run), "sbDraft")
		if err := os.WriteFile(sbDocPath(t, dr.Prompt), []byte("draft"), 0o644); err != nil {
			t.Fatal(err)
		}
		h.submit(run, dr, `{"Body":"v"}`)
		h.submit(run, sbOnly(t, h.next(run), "sbReview"), `{"OK":false,"Problems":["again"]}`)
	}
	statusOf(t, h.status(run), wf.RunEnded, "sbkStop")
	var steps []wf.EdgeStep
	for _, r := range h.records(run) {
		var p struct{ Plan *wf.EdgePlan }
		if json.Unmarshal(r.Data, &p) == nil && p.Plan != nil {
			steps = append(steps, p.Plan.Steps...)
		}
	}
	var returned, spent int
	for _, s := range steps {
		if s.SendBack != nil {
			returned++
		}
		if s.SendBackSpent == "revise" && s.Label == "not fixed" {
			spent++
		}
	}
	if returned != 1 || spent != 1 {
		t.Errorf("plans: %d returns, %d spent fallbacks; want 1 and 1: %+v", returned, spent, steps)
	}
}

// A resumed engine rebuilds the returned unit from the journal alone.
func TestSendBackResumesToTheSameReturnedUnit(t *testing.T) {
	d := sbDef(2, false, nil)
	runDir := t.TempDir()
	opts := wf.EngineOptions{RunDir: func(wf.RunID) string { return runDir }}
	h := newHarnessOpts(t, d, nil, opts)
	run := h.run(d, nil)
	dr := sbOnly(t, h.next(run), "sbDraft")
	if err := os.WriteFile(sbDocPath(t, dr.Prompt), []byte("draft"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.submit(run, dr, `{"Body":"v1"}`)
	h.submit(run, sbOnly(t, h.next(run), "sbReview"), `{"OK":false,"Problems":["fix it"]}`)
	before := sbOnly(t, h.next(run), "sbDraft")

	opts.Clock = func() time.Time { return h.now }
	opts.Defs = []*wf.Def{d}
	again := wf.NewEngine(h.j, h.reg, opts)
	ins, err := again.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	after := sbOnly(t, ins, "sbDraft")
	if sbReturned(t, after.Prompt)["reasons"] == nil || after.Prompt != before.Prompt {
		t.Errorf("resumed prompt differs:\n%s\nwant\n%s", after.Prompt, before.Prompt)
	}
}

// The producer's own visit cap is checked on the arrival a return causes.
func TestSendBackArrivalCountsAgainstTheProducersVisitCap(t *testing.T) {
	d := sbDef(5, true, nil)
	h := newHarnessOpts(t, d, nil, sbRunDir(t))
	run := h.run(d, nil)
	for round := 0; round < 2; round++ {
		dr := sbOnly(t, h.next(run), "sbDraft")
		if err := os.WriteFile(sbDocPath(t, dr.Prompt), []byte("draft"), 0o644); err != nil {
			t.Fatal(err)
		}
		h.submit(run, dr, `{"Body":"v"}`)
		h.submit(run, sbOnly(t, h.next(run), "sbReview"), `{"OK":false,"Problems":["again"]}`)
	}
	statusOf(t, h.status(run), wf.RunEnded, "sbkCapped")
}

type (
	sblA    struct{}
	sblB    struct{}
	sblDone struct{}
	sblStop struct{}
)

// Each lint rule over send-back edges, one broken definition per case.
func TestSendBackLint(t *testing.T) {
	reasons := func(v sbVerdict) []string { return v.Problems }
	build := func(name string, wire func(d *wf.Def, a *wf.Agent[sblA, sbText], b *wf.Agent[sblB, sbVerdict], done *wf.Terminal[sblDone], stop *wf.Terminal[sblStop])) *wf.Def {
		d := wf.New(name)
		a := wf.NewAgent[sblA, sbText](d)
		b := wf.NewAgent[sblB, sbVerdict](d)
		done := wf.NewTerminal[sblDone](d, wf.Success)
		stop := wf.NewTerminal[sblStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		a.Spec(wf.PromptText("a {{r}}", wf.As("r", wf.Returned())), wf.Retry[sblA](wf.Const(1), a.To(stop, "a gave up")))
		wire(d, a, b, done, stop)
		return d
	}
	cases := []struct {
		name string
		rule wf.RuleID
		def  *wf.Def
	}{
		{"not upstream", wf.RuleSendBackTarget, build("sbl-upstream", func(d *wf.Def, a *wf.Agent[sblA, sbText], b *wf.Agent[sblB, sbVerdict], done *wf.Terminal[sblDone], stop *wf.Terminal[sblStop]) {
			// b runs first and sends back to a, which only runs after it.
			d2 := b.SendBack(a, "back", reasons, wf.Const(1), b.To(stop, "nf").Ends(wf.Stopped, "not fixed"))
			a.Then(a.To(done, "done"))
			b.Spec(wf.PromptText("b"), wf.Retry[sblB](wf.Const(1), b.To(stop, "b gave up")))
			b.Then(d2)
		})},
		{"used as a fallback", wf.RuleSendBackTarget, build("sbl-fallback-use", func(d *wf.Def, a *wf.Agent[sblA, sbText], b *wf.Agent[sblB, sbVerdict], done *wf.Terminal[sblDone], stop *wf.Terminal[sblStop]) {
			a.Then(a.To(b, "drafted"))
			back := b.SendBack(a, "back", reasons, wf.Const(1), b.To(stop, "nf").Ends(wf.Stopped, "not fixed"))
			b.Spec(wf.PromptText("b"), wf.Retry[sblB](wf.Const(1), back))
			b.Then(b.To(done, "ok"))
		})},
		{"fallback is a send-back", wf.RuleSendBackFallback, build("sbl-chain", func(d *wf.Def, a *wf.Agent[sblA, sbText], b *wf.Agent[sblB, sbVerdict], done *wf.Terminal[sblDone], stop *wf.Terminal[sblStop]) {
			a.Then(a.To(b, "drafted"))
			inner := b.SendBack(a, "inner", reasons, wf.Const(1), b.To(stop, "nf").Ends(wf.Stopped, "not fixed"))
			outer := b.SendBack(a, "outer", reasons, wf.Const(1), inner)
			ok := b.To(done, "ok")
			b.Spec(wf.PromptText("b"), wf.Retry[sblB](wf.Const(1), b.To(stop, "b gave up")))
			b.Route(func(v sbVerdict, _ wf.Ctx) wf.Edge[sblB] {
				if v.OK {
					return ok
				}
				return outer
			}, ok, outer, inner)
		})},
	}
	for _, c := range cases {
		var got []string
		for _, p := range c.def.Lint() {
			if p.Rule == c.rule {
				got = append(got, p.String())
			}
		}
		if len(got) == 0 {
			t.Errorf("%s: no %s problem in %v", c.name, c.rule, c.def.Lint())
		}
	}
}

type sbFan struct{}

// Each Map child counts its own returns: one child spending its budget
// leaves its sibling's untouched.
func TestSendBackCountsPerMapChild(t *testing.T) {
	d := wf.New("sendback-fan")
	m := wf.NewMap[sbFan, string, string, bool](d, wf.FromValues("a", "b"), func(s string) string { return s }, wf.Typed[string, bool](sbBuild("sendback-item", 1, false, nil, true)))
	m.Parallel(1).Join(wf.AllSettled(), m.To(wf.NewTerminal[sbkDone](d, wf.Success), "met"), m.To(wf.NewTerminal[sbkStop](d, wf.Stopped), "partial").Ends(wf.Stopped, "an item stopped"))
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}
	h := newHarnessOpts(t, d, nil, sbRunDir(t))
	run := h.run(d, nil)
	step := func(state, out string) wf.Instruction {
		t.Helper()
		ins := h.next(run)
		if len(ins) != 1 || !strings.HasSuffix(ins[0].State, state) {
			t.Fatalf("instructions %+v, want one %s", ins, state)
		}
		if strings.HasSuffix(state, "sbDraft") {
			if err := os.WriteFile(sbDocPath(t, ins[0].Prompt), []byte("draft"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if res := h.submit(run, ins[0], out); res.Class != wf.Accepted {
			t.Fatalf("%s: %+v", state, res)
		}
		return ins[0]
	}
	fail := `{"OK":false,"Problems":["no"]}`
	// Child a: one return, then its budget of 1 is spent and it stops.
	step("sbDraft", `{"Body":"a1"}`)
	step("sbReview", fail)
	step("sbDraft", `{"Body":"a2"}`)
	step("sbReview", fail)
	// Child b: its own first return is round 1 of 1, not spent.
	first := step("sbDraft", `{"Body":"b1"}`)
	if first.Child != "sbFan[b]" {
		t.Fatalf("third draft is %s's, want sbFan[b]", first.Child)
	}
	step("sbReview", fail)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Child != "sbFan[b]" || !strings.HasSuffix(ins[0].State, "sbDraft") {
		t.Fatalf("child b after its first return: %+v, want its draft returned", ins)
	}
	if m := sbReturned(t, ins[0].Prompt); m["round"] != 1.0 || m["of"] != 1.0 {
		t.Errorf("child b's return = %v, want round 1 of 1", m)
	}
}

// The budget is folded from the journal: a run cut before a return has it
// back, a run cut after it does not.
func TestSendBackBudgetFollowsTheJournalCut(t *testing.T) {
	d := sbDef(1, false, nil)
	opts := sbRunDir(t)
	h := newHarnessOpts(t, d, nil, opts)
	run := h.run(d, nil)
	dr := sbOnly(t, h.next(run), "sbDraft")
	if err := os.WriteFile(sbDocPath(t, dr.Prompt), []byte("draft"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.submit(run, dr, `{"Body":"v1"}`)
	h.submit(run, sbOnly(t, h.next(run), "sbReview"), `{"OK":false,"Problems":["no"]}`)
	recs := h.records(run)
	// cut is the review's submission whose decision took the send-back.
	cut, submitted := int64(0), int64(0)
	for _, r := range recs {
		if r.Kind == wf.JournalSubmitted {
			submitted = r.Seq
		}
		var p struct{ Plan *wf.EdgePlan }
		if json.Unmarshal(r.Data, &p) == nil && p.Plan != nil && p.Plan.Steps[0].SendBack != nil {
			cut = submitted
		}
	}
	if cut == 0 {
		t.Fatal("no send-back plan journaled")
	}
	fork := func(keep func(wf.JournalRecord) bool) (*harness, wf.Instruction) {
		t.Helper()
		f := newHarnessOpts(t, d, nil, opts)
		unlock, err := f.j.Lock(run)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if keep(r) {
				if err := f.j.Append(r); err != nil {
					t.Fatal(err)
				}
			}
		}
		unlock()
		ins := f.next(run)
		if len(ins) != 1 {
			t.Fatalf("fork instructions %+v", ins)
		}
		return f, ins[0]
	}
	// Cut before the return: the review is open again and its fail returns.
	before, rev := fork(func(r wf.JournalRecord) bool { return r.Seq < cut })
	if rev.State != "sbReview" {
		t.Fatalf("fork before the return is at %s, want sbReview", rev.State)
	}
	before.submit(run, rev, `{"OK":false,"Problems":["still"]}`)
	if m := sbReturned(t, sbOnly(t, before.next(run), "sbDraft").Prompt); m["round"] != 1.0 {
		t.Errorf("return after the cut = %v, want round 1 again", m)
	}
	// Kept: the return stands, so the next fail spends the budget.
	after, dr2 := fork(func(wf.JournalRecord) bool { return true })
	if dr2.State != "sbDraft" {
		t.Fatalf("full copy is at %s, want the returned sbDraft", dr2.State)
	}
	after.submit(run, dr2, `{"Body":"v2"}`)
	after.submit(run, sbOnly(t, after.next(run), "sbReview"), `{"OK":false,"Problems":["still"]}`)
	statusOf(t, after.status(run), wf.RunEnded, "sbkStop")
}

// A long return travels as any long input does: under paged delivery it is
// an indexed reference whose original holds every reason, with no inline
// limit of its own.
func TestSendBackReasonsUsePagedDelivery(t *testing.T) {
	d := wf.New("sendback-paged")
	draft := wf.NewAgent[sbDraft, sbText](d)
	review := wf.NewAgent[sbReview, sbVerdict](d)
	done := wf.NewTerminal[sbkDone](d, wf.Success)
	stop := wf.NewTerminal[sbkStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	draft.Spec(wf.PromptText("Draft the text.\n{{returned}}", wf.As("returned", wf.Returned())), wf.Retry[sbDraft](wf.Const(2), draft.To(stop, "draft gave up")))
	draft.Then(draft.To(review, "drafted"))
	review.Spec(wf.PromptText("Review {{draft}}", wf.As("draft", wf.Out(draft))), wf.Retry[sbReview](wf.Const(2), review.To(stop, "review gave up")))
	ok := review.To(done, "approved")
	back := review.SendBack(draft, "revise", func(v sbVerdict) []string { return v.Problems }, wf.Const(2),
		review.To(stop, "not fixed").Ends(wf.Stopped, "not fixed"))
	review.Route(func(v sbVerdict, _ wf.Ctx) wf.Edge[sbReview] {
		if v.OK {
			return ok
		}
		return back
	}, ok, back)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	e := h.e.(*wf.Engine)
	run := h.run(d, nil)
	finish := func(out string) {
		t.Helper()
		ins := h.handOut(run, 1)
		if len(ins) != 1 {
			t.Fatalf("hand-out %+v", ins)
		}
		if err := h.e.Started(run, ins[0].Name, claim("child")); err != nil {
			t.Fatal(err)
		}
		if res, err := e.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(out)}); err != nil || res.Class != wf.Accepted {
			t.Fatalf("done %s = %+v, %v", ins[0].State, res, err)
		}
	}
	finish(`{"Body":"v1"}`)
	long := strings.Repeat("the third paragraph contradicts the summary; ", 600) + "LAST REASON"
	verdict, _ := json.Marshal(sbVerdict{Problems: []string{long, "second"}})
	finish(string(verdict))

	ins := h.handOut(run, 1)
	if len(ins) != 1 || ins[0].State != "sbDraft" {
		t.Fatalf("after the return: %+v", ins)
	}
	if err := h.e.Started(run, ins[0].Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PromptPage(run, ins[0].Name, "", ha.Caller{}, wf.PromptPageRequest{Page: 1}); err != nil {
		t.Fatal(err)
	}
	fetched := h.kind(run, wf.JournalPromptFetched)
	var record wf.PromptFetchedData
	if err := json.Unmarshal(fetched[len(fetched)-1].Data, &record); err != nil {
		t.Fatal(err)
	}
	if got := record.Packet.Originals["returned"]; !strings.Contains(got, "LAST REASON") || !strings.Contains(got, `"second"`) {
		t.Fatalf("returned original lost reasons: %d bytes", len(got))
	}
	if len(record.Packet.References) == 0 {
		t.Fatal("a long return was inlined instead of indexed as a reference")
	}
}

// The dead-end report lists a send-back's fallback like any other route
// into a stop.
func TestSendBackFallbackIsADeadEnd(t *testing.T) {
	ds, err := sbDef(2, false, nil).DeadEnds(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range ds {
		if de.Trigger == `send-back "revise" spent (≤2)` {
			if de.Edge != "not fixed" || de.To != "sbkStop" || de.Single != "no" || de.Ends == "" {
				t.Errorf("fallback dead end = %+v", de)
			}
			return
		}
	}
	t.Fatalf("no send-back fallback in:\n%s", wf.FormatDeadEnds("sendback", ds))
}

type sbJudge struct{}

// A compute can send work back too: its result carries the reasons.
func TestSendBackFromACompute(t *testing.T) {
	d := wf.New("sendback-compute")
	draft := wf.NewAgent[sbDraft, sbText](d)
	done := wf.NewTerminal[sbkDone](d, wf.Success)
	stop := wf.NewTerminal[sbkStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	judge := wf.NewCompute[sbJudge, sbVerdict](d, func(c wf.Ctx) (sbVerdict, error) {
		if wf.OutputOf(c, draft).Body == "bad" {
			return sbVerdict{Problems: []string{"body is bad"}}, nil
		}
		return sbVerdict{OK: true}, nil
	})
	draft.Spec(wf.PromptText("Draft the text.\n{{returned}}", wf.As("returned", wf.Returned())), wf.Retry[sbDraft](wf.Const(2), draft.To(stop, "draft gave up")))
	draft.Then(draft.To(judge, "drafted"))
	ok := judge.To(done, "fine")
	back := judge.SendBack(draft, "redo", func(v sbVerdict) []string { return v.Problems }, wf.Const(1),
		judge.To(stop, "still bad").Ends(wf.Stopped, "the draft stayed bad"))
	judge.Route(func(v sbVerdict, _ wf.Ctx) wf.Edge[sbJudge] {
		if v.OK {
			return ok
		}
		return back
	}, ok, back)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}
	h := newHarnessOpts(t, d, nil, sbRunDir(t))
	run := h.run(d, nil)
	h.submit(run, sbOnly(t, h.next(run), "sbDraft"), `{"Body":"bad"}`)
	again := sbOnly(t, h.next(run), "sbDraft")
	if m := sbReturned(t, again.Prompt); m["from"] != "sbJudge" || m["label"] != "redo" {
		t.Fatalf("returned block = %v", m)
	}
	h.submit(run, again, `{"Body":"good"}`)
	statusOf(t, h.status(run), wf.RunEnded, "sbkDone")
}
