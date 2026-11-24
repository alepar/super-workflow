package wf_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/router"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types for the engine tests.
type (
	eA         struct{}
	eB         struct{}
	eC         struct{}
	eDone      struct{}
	eBad       struct{}
	eOther     struct{}
	eStale     struct{}
	eRecovered struct{}
	gDraft     struct{}
	gVet       struct{}
	gExpand    struct{}
	gDone      struct{}
	gSchemaBad struct{}
	gVetBad    struct{}
)

type eOut struct {
	V   string
	ID  string
	Fix struct{ ID string }
}

type eRes struct{ OK bool }

type eItem struct{ Lang string }

// twoTerminals declares the Done and Bad terminals every test routes to.
func twoTerminals(d *wf.Def) (*wf.Terminal[eDone], *wf.Terminal[eBad]) {
	return wf.NewTerminal[eDone](d, wf.Success), wf.NewTerminal[eBad](d, wf.Stopped)
}

// simple builds A -> Done (Bad when attempts run out) with the given prompt.
func simple(name string, prompt wf.Prompt) (*wf.Def, *wf.Agent[eA, eOut]) {
	d := wf.New(name)
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	a.Spec(prompt, wf.Retry[eA](wf.Const(2), a.To(bad, "gave up")))
	next := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
	return d, a
}

func statusOf(t *testing.T, st wf.RunStatus, phase string, terminal string) {
	t.Helper()
	if st.Phase != phase || st.Terminal != terminal {
		t.Fatalf("status = %s/%s (%+v), want %s/%s", st.Phase, st.Terminal, st.Instances, phase, terminal)
	}
}

func TestRouterShape(t *testing.T) {
	for kind, want := range map[string]string{"bug": "Bug", "feature": "Feature", "question": "Question"} {
		d := router.Definition()
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		st := h.drive(run, map[string][]string{"Classify": {`{"Kind":"` + kind + `","Reason":"r"}`}})
		statusOf(t, st, wf.RunEnded, want)
	}
	d := router.Definition()
	h := newHarness(t, d, nil)
	st := h.drive(h.run(d, nil), map[string][]string{"Classify": {`{"Kind":"weird"}`, `{"Kind":"weird"}`}})
	if st.Outcome != wf.Stopped {
		t.Fatalf("outcome = %s, want stopped", st.Outcome)
	}
}

type (
	gVetExit int
	gOutline struct{ Path string }
	gSchema  struct{ OK bool }
	gVetRes  struct{ Exit gVetExit }
)

// gatedChain is the gatedchain shape without Command: an agent drafts, a
// compute reads the validator, cases route, a second agent expands.
func gatedChain() *wf.Def {
	d := wf.New("gated")
	exits := wf.NewEnum(d, "vet-exit", gVetExit(0), gVetExit(1))
	schemaObs := wf.Observe[gSchema]("fs", "schema", map[string]any{"path": "outline.json"})
	vetObs := wf.Observe[gVetRes]("vet", "run", map[string]any{"path": "outline.json"})

	draft := wf.NewAgent[gDraft, gOutline](d)
	vet := wf.NewCompute[gVet, gVetRes](d, func(c wf.Ctx) (gVetRes, error) { return wf.Observed(c, vetObs) }).Observes(vetObs)
	expand := wf.NewAgent[gExpand, gOutline](d)
	done := wf.NewTerminal[gDone](d, wf.Success)
	schemaBad := wf.NewTerminal[gSchemaBad](d, wf.Stopped)
	vetBad := wf.NewTerminal[gVetBad](d, wf.Stopped)

	draft.Spec(wf.PromptText("Draft an outline."),
		wf.Retry[gDraft](wf.Const(2), draft.To(schemaBad, "schema failed twice")),
		wf.Checkf("matches schema", func(o gOutline, c wf.Ctx) error {
			r, err := wf.Observed(c, wf.Observe[gSchema]("fs", "schema", map[string]any{"path": o.Path}))
			if err != nil {
				return err
			}
			if !r.OK {
				return errors.New("outline fails schema")
			}
			return nil
		}).Observes(schemaObs))
	toVet := draft.To(vet, "accepted")
	draft.Route(func(gOutline, wf.Ctx) wf.Edge[gDraft] { return toVet }, toVet)

	vet.RouteCases(wf.Cases[gVet, gVetRes](func(r gVetRes) gVetExit { return r.Exit }, exits,
		wf.On(gVetExit(0), vet.To(expand, "vet passed")),
		wf.On(gVetExit(1), vet.To(vetBad, "vet failed"))))

	expand.Spec(wf.PromptText("Expand the vetted outline: {{out}}", wf.Out(draft)),
		wf.Retry[gExpand](wf.Const(2), expand.To(vetBad, "expansion refused")))
	toDone := expand.To(done, "expanded")
	expand.Route(func(gOutline, wf.Ctx) wf.Edge[gExpand] { return toDone }, toDone)
	return d
}

func TestGatedChainShape(t *testing.T) {
	schema := map[string]any{"path": "outline.json"}
	cases := []struct {
		name     string
		ok       bool
		exit     int
		draft    []string
		terminal string
	}{
		{"passes", true, 0, []string{`{"Path":"outline.json"}`}, "gDone"},
		{"vet fails", true, 1, []string{`{"Path":"outline.json"}`}, "gVetBad"},
		{"schema fails twice", false, 0, []string{`{"Path":"outline.json"}`, `{"Path":"outline.json"}`}, "gSchemaBad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeRuntime()
			rt.set("fs", "schema", schema, gSchema{OK: tc.ok})
			rt.set("vet", "run", schema, gVetRes{Exit: gVetExit(tc.exit)})
			d := gatedChain()
			h := newHarness(t, d, rt)
			st := h.drive(h.run(d, nil), map[string][]string{"gDraft": tc.draft, "gExpand": {`{"Path":"x"}`}})
			statusOf(t, st, wf.RunEnded, tc.terminal)
		})
	}
}

func TestRouteOffCandidatesBlocked(t *testing.T) {
	d := wf.New("off")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	other := wf.NewTerminal[eOther](d, wf.Stopped)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(2), a.To(bad, "x")))
	listed := a.To(done, "listed")
	unlisted := a.To(other, "unlisted")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return unlisted }, listed)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	st := h.drive(run, map[string][]string{"eA": {`{}`}})
	if st.Phase != wf.RunBlocked {
		t.Fatalf("status = %+v, want blocked", st)
	}
	if h.results[0].Class != wf.Blocked {
		t.Fatalf("class = %s, want blocked", h.results[0].Class)
	}
	recs := h.kind(run, wf.JournalRouteOffCandidates)
	if len(recs) != 1 || recs[0].Class != wf.Blocked {
		t.Fatalf("route_off_candidates records = %+v", recs)
	}
	var data wf.RouteOffCandidatesData
	if err := json.Unmarshal(recs[0].Data, &data); err != nil || !strings.Contains(data.Returned, "unlisted") || len(data.Candidates) != 1 {
		t.Fatalf("payload = %+v, %v", data, err)
	}
}

func TestWithCopyIsACandidate(t *testing.T) {
	d := wf.New("with")
	n := d.Counter("n")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(2), a.To(bad, "x")))
	cand := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return cand.With(n.Inc()) }, cand)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	st := h.drive(run, map[string][]string{"eA": {`{}`}})
	statusOf(t, st, wf.RunEnded, "eDone")
	if got := st.Instances[0].Counters["n"]; got != 1 {
		t.Fatalf("counter n = %d, want 1 (the With copy's effect ran)", got)
	}
}

func TestPromptOffCandidatesBlocked(t *testing.T) {
	d := wf.New("poff")
	wf.Typed[eItem, eRes](d)
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptFor(func(eItem) string { return "prompts/nope.md" }, []string{"prompts/a.md", "prompts/b.md"}),
		wf.Retry[eA](wf.Const(2), a.To(bad, "x")))
	next := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none", ins)
	}
	if st := h.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("status = %+v, want blocked", st)
	}
	if recs := h.kind(run, wf.JournalPromptOffCandidates); len(recs) != 1 || recs[0].Class != wf.Blocked {
		t.Fatalf("prompt_off_candidates records = %+v", recs)
	}
}

func TestInstructionHintsAndPromptFor(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "prompts", "a.md"), []byte("Review the {{topic}} work."), 0o644); err != nil {
		t.Fatal(err)
	}
	d := wf.New("hints")
	wf.Typed[eItem, eRes](d)
	topic := d.Param("topic")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptFor(func(eItem) string { return "prompts/a.md" }, []string{"prompts/a.md", "prompts/b.md"}, topic),
		wf.Retry[eA](wf.Const(2), a.To(bad, "x")))
	a.Dispatch(wf.Model("opus"), wf.Effort("high"), wf.AgentType("reviewer"))
	next := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: ws})
	run := h.run(d, map[string]string{"topic": "parser"})
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("instructions = %+v", ins)
	}
	if want := (wf.DispatchHints{Model: "opus", Effort: "high", AgentType: "reviewer"}); ins[0].Dispatch != want {
		t.Fatalf("Dispatch = %+v, want %+v", ins[0].Dispatch, want)
	}
	if !strings.Contains(ins[0].Prompt, "Review the parser work.") {
		t.Fatalf("prompt does not carry the PromptFor text:\n%s", ins[0].Prompt)
	}
	if ins[0].Kind != wf.InstructionAgent || ins[0].State != "eA" {
		t.Fatalf("instruction = %+v", ins[0])
	}
}

func TestInstructionsCarryWorkspace(t *testing.T) {
	d, _ := simple("ws", wf.PromptText("p"))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: "/work/space"})
	ins := h.next(h.run(d, nil))
	if ins[0].Workspace != "/work/space" || !strings.Contains(ins[0].Prompt, "Work in /work/space.") {
		t.Fatalf("instruction = %+v", ins[0])
	}
}

func TestStaleAndSupersededTokensRejected(t *testing.T) {
	d, _ := simple("tok", wf.PromptText("p"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	first := h.next(run)[0]

	snapshot := func() string {
		b, _ := json.Marshal([]any{h.status(run), h.next(run)})
		return string(b)
	}
	before := snapshot()
	res := h.submit(run, wf.Instruction{Token: "nonsense"}, `{}`)
	if res.Class != wf.Rejected || !strings.Contains(res.Refusal[0], "unknown token") {
		t.Fatalf("unknown token result = %+v", res)
	}
	if snapshot() != before {
		t.Fatal("a rejected token changed the run")
	}

	if _, err := h.e.Fail(run, first.Token, "gave up"); err != nil {
		t.Fatal(err)
	}
	second := h.next(run)[0]
	if second.Token == first.Token || second.Attempt != 2 {
		t.Fatalf("re-issued instruction = %+v, want a fresh token for attempt 2", second)
	}
	before = snapshot()
	res = h.submit(run, first, `{}`)
	if res.Class != wf.Rejected || !strings.Contains(res.Refusal[0], "superseded") {
		t.Fatalf("superseded result = %+v", res)
	}
	if snapshot() != before {
		t.Fatal("a superseded token changed the run")
	}

	if res = h.submit(run, second, `{}`); res.Class != wf.Accepted {
		t.Fatalf("current token result = %+v", res)
	}
	before = snapshot()
	res = h.submit(run, second, `{}`)
	if res.Class != wf.Rejected || !strings.Contains(res.Refusal[0], "stale") {
		t.Fatalf("stale result = %+v", res)
	}
	if snapshot() != before {
		t.Fatal("a stale token changed the run")
	}
}

func TestSupersededVersusStale(t *testing.T) {
	obs := wf.Observe[eRes]("x", "y", map[string]any{"k": "v"})
	build := func(route string) *wf.Def {
		d := wf.New("stale")
		a := wf.NewAgent[eA, eOut](d)
		done, bad := twoTerminals(d)
		stale := wf.NewTerminal[eStale](d, wf.Stopped)
		retry := wf.Retry[eA](wf.Const(2), a.To(bad, "x"))
		if route == "own" {
			retry = retry.OnClass(wf.Stale, a.To(stale, "stale"))
		}
		if route == "default" {
			d.OnFailure(wf.Stale, stale)
		}
		a.Spec(wf.PromptText("p"), retry, wf.Checkf("reads", func(o eOut, c wf.Ctx) error {
			_, err := wf.Observed(c, obs)
			return err
		}).Observes(obs))
		next := a.To(done, "done")
		a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
		return d
	}
	for _, route := range []string{"own", "default", "none"} {
		rt := newFakeRuntime()
		rt.set("x", "y", map[string]any{"k": "v"}, eRes{OK: false})
		d := build(route)
		h := newHarness(t, d, rt)
		run := h.run(d, nil)
		in := h.next(run)[0]
		rt.set("x", "y", map[string]any{"k": "v"}, eRes{OK: true})
		res := h.submit(run, in, `{}`)
		st := h.status(run)
		if route != "none" {
			if res.Class != wf.Stale {
				t.Fatalf("class = %s, want stale", res.Class)
			}
			statusOf(t, st, wf.RunEnded, "eStale")
		} else {
			if res.Class != wf.Accepted {
				t.Fatalf("without a Stale route the re-read is just the checks' value: class = %s", res.Class)
			}
			statusOf(t, st, wf.RunEnded, "eDone")
		}
	}
}

func TestUndeclaredObservedBlocked(t *testing.T) {
	obs := wf.Observe[eRes]("x", "y", nil)
	rt := newFakeRuntime()
	rt.set("x", "y", nil, eRes{OK: true})

	t.Run("compute", func(t *testing.T) {
		d := wf.New("undeclared-compute")
		c := wf.NewCompute[eC, eOut](d, func(c wf.Ctx) (eOut, error) {
			_, err := wf.Observed(c, obs)
			return eOut{}, err
		})
		done, _ := twoTerminals(d)
		next := c.To(done, "done")
		c.Route(func(eOut, wf.Ctx) wf.Edge[eC] { return next }, next)
		h := newHarness(t, d, rt)
		run := h.run(d, nil)
		h.next(run)
		st := h.status(run)
		if st.Phase != wf.RunBlocked || !strings.Contains(st.Instances[0].Reason, "not declared") {
			t.Fatalf("status = %+v, want blocked on the undeclared read", st)
		}
	})
	t.Run("check", func(t *testing.T) {
		d2 := wf.New("undeclared-check")
		a2 := wf.NewAgent[eA, eOut](d2)
		done, bad := twoTerminals(d2)
		a2.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(2), a2.To(bad, "x")),
			wf.Checkf("sneaky", func(eOut, wf.Ctx) error { return nil }),
			wf.Checkf("reads", func(o eOut, c wf.Ctx) error { _, err := wf.Observed(c, obs); return err }))
		next := a2.To(done, "done")
		a2.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
		h := newHarness(t, d2, rt)
		run := h.run(d2, nil)
		res := h.submit(run, h.next(run)[0], `{}`)
		if res.Class != wf.Blocked {
			t.Fatalf("class = %s, want blocked", res.Class)
		}
		if st := h.status(run); st.Phase != wf.RunBlocked {
			t.Fatalf("status = %+v", st)
		}
	})
}

func TestBlockedHaltsInstance(t *testing.T) {
	obs := wf.Observe[eRes]("x", "y", nil)
	build := func(route bool) *wf.Def {
		d := wf.New("blocked")
		c := wf.NewCompute[eC, eOut](d, func(c wf.Ctx) (eOut, error) {
			_, err := wf.Observed(c, obs)
			return eOut{}, err
		})
		done, bad := twoTerminals(d)
		rec := wf.NewTerminal[eRecovered](d, wf.Stopped)
		retry := wf.Retry[eC](wf.Const(2), c.To(bad, "x"))
		if route {
			retry = retry.OnClass(wf.Blocked, c.To(rec, "recovered"))
		}
		c.Retry(retry)
		next := c.To(done, "done")
		c.Route(func(eOut, wf.Ctx) wf.Edge[eC] { return next }, next)
		return d
	}
	d := build(false)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v", ins)
	}
	st := h.status(run)
	if st.Phase != wf.RunBlocked || st.Instances[0].Phase != "blocked" {
		t.Fatalf("status = %+v, want a blocked halt", st)
	}
	// A3: an undeclared read is a definition bug, so even a declared Blocked
	// route is not taken: the unit halts with the cause named.
	d = build(true)
	h = newHarness(t, d, nil)
	run = h.run(d, nil)
	h.next(run)
	st = h.status(run)
	if st.Phase != wf.RunBlocked || !strings.Contains(st.Instances[0].Reason, "definition error: ") {
		t.Fatalf("status = %+v, want a definition-error halt, not the Blocked route", st)
	}
}

func TestEveryJournalKindHasPayload(t *testing.T) {
	last := wf.JournalCoordinatorStopStalled
	for k := wf.JournalRunStarted; k <= last; k++ {
		if strings.HasPrefix(k.String(), "JournalKind(") {
			t.Errorf("kind %d has no name", int(k))
		}
		ty, ok := wf.JournalPayloads[k]
		if !ok || ty.Kind().String() != "struct" {
			t.Errorf("kind %s has no struct payload in JournalPayloads", k)
		}
	}
	if !strings.HasPrefix((last + 1).String(), "JournalKind(") {
		t.Errorf("a kind after %s exists: update this test", last)
	}
	if len(wf.JournalPayloads) != int(last) {
		t.Errorf("JournalPayloads has %d entries, want %d", len(wf.JournalPayloads), int(last))
	}
}

func TestRunIdentity(t *testing.T) {
	build := func(keyed bool) *wf.Def {
		d, _ := simple("ident", wf.PromptText("p"))
		topic := d.Param("topic")
		if keyed {
			d.RunKey(topic)
		}
		return d
	}
	d := build(true)
	h := newHarness(t, d, nil)
	a, b, c := h.run(d, map[string]string{"topic": "x"}), h.run(d, map[string]string{"topic": "x"}), h.run(d, map[string]string{"topic": "y"})
	if a != b || a == c {
		t.Fatalf("run ids = %s %s %s, want the same key to resume and a different key to differ", a, b, c)
	}
	if n := len(h.kind(a, wf.JournalRunStarted)); n != 1 {
		t.Fatalf("run_started count = %d after a resume", n)
	}

	d = build(false)
	h = newHarness(t, d, nil)
	if x, y := h.run(d, nil), h.run(d, nil); x == y {
		t.Fatalf("without a run key both runs got %s", x)
	}

	h = newHarnessOpts(t, d, nil, wf.EngineOptions{RunID: "fixed"})
	if x, y := h.run(d, nil), h.run(d, nil); x != "fixed" || y != "fixed" || len(h.kind("fixed", wf.JournalRunStarted)) != 1 {
		t.Fatalf("supplied id: %s %s", x, y)
	}

	d = build(true)
	h = newHarness(t, d, nil)
	var wg sync.WaitGroup
	ids := make([]wf.RunID, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i] = h.run(d, map[string]string{"topic": "same"})
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent runs got different ids: %v", ids)
		}
	}
	if n := len(h.kind(ids[0], wf.JournalRunStarted)); n != 1 {
		t.Fatalf("concurrent Run calls wrote %d run_started records, want 1", n)
	}
}

func TestUndecodableSubmissionRefused(t *testing.T) {
	d, _ := simple("decode", wf.PromptText("p"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	first := h.next(run)[0]
	res := h.submit(run, first, `"not an object"`)
	if res.Class != wf.Refused || len(res.Refusal) == 0 {
		t.Fatalf("result = %+v, want refused, spending the attempt", res)
	}
	if n := len(h.kind(run, wf.JournalRefused)); n != 1 {
		t.Fatalf("refused records = %d", n)
	}
	second := h.next(run)[0]
	if second.Attempt != 2 || second.Token == first.Token {
		t.Fatalf("re-issued = %+v, want attempt 2 with a fresh token", second)
	}
	h.submit(run, second, `[1]`)
	statusOf(t, h.status(run), wf.RunEnded, "eBad")
}

func TestFailSpendsBudgetAsRefused(t *testing.T) {
	d, _ := simple("fail", wf.PromptText("p"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	res, err := h.e.Fail(run, h.next(run)[0].Token, "cannot do it")
	if err != nil || res.Class != wf.Refused {
		t.Fatalf("Fail = %+v, %v", res, err)
	}
	if h.status(run).Phase != wf.RunRunning {
		t.Fatal("one failed attempt of two should re-issue")
	}
	if _, err := h.e.Fail(run, h.next(run)[0].Token, "still cannot"); err != nil {
		t.Fatal(err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eBad")
}

func TestBindingIntoIntentAndObserved(t *testing.T) {
	d := wf.New("bind")
	topic := d.Param("topic")
	_ = topic
	stat := wf.Observe[eRes]("fs", "stat", map[string]any{"path": "{run}/{topic}.txt"})
	a := wf.NewAgent[eA, eOut](d)
	c := wf.NewCompute[eC, eOut](d, func(c wf.Ctx) (eOut, error) {
		r, err := wf.Observed(c, stat)
		return eOut{V: map[bool]string{true: "ok", false: "no"}[r.OK]}, err
	}).Observes(stat)
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(2), a.To(bad, "x")))
	toC := a.To(c, "go").With(wf.ExternalEffect{
		Adapter: "bd", Op: "create",
		Args: map[string]any{"title": "T {topic}", "fix": "{fix.id}", "n": 3},
		Key:  wf.KeyOf("fix", "{id}"),
	})
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return toC }, toC)
	toDone := c.To(done, "done")
	c.Retry(wf.Retry[eC](wf.Const(1), c.To(bad, "x")))
	c.Route(func(eOut, wf.Ctx) wf.Edge[eC] { return toDone }, toDone)

	rt := newFakeRuntime()
	h := newHarness(t, d, rt)
	run := h.run(d, map[string]string{"topic": "x"})
	rt.set("fs", "stat", map[string]any{"path": string(run) + "/x.txt"}, eRes{OK: true})
	st := h.drive(run, map[string][]string{"eA": {`{"ID":"I1","Fix":{"ID":"F1"}}`}})
	statusOf(t, st, wf.RunEnded, "eDone")

	intents := h.kind(run, wf.JournalEffectIntent)
	if len(intents) != 1 {
		t.Fatalf("effect_intent records = %d", len(intents))
	}
	var intent wf.EffectIntentData
	_ = json.Unmarshal(intents[0].Data, &intent)
	if intent.Effect.Key != "fix/I1" || intent.Effect.Args["title"] != "T x" || intent.Effect.Args["fix"] != "F1" || intent.Effect.Args["n"] != float64(3) {
		t.Fatalf("effect_intent = %+v, want the resolved key and args", intent.Effect)
	}
	if len(rt.applied) != 1 || rt.applied[0].KeyID() != "fix/I1" || rt.applied[0].Args["title"] != "T x" {
		t.Fatalf("applied = %+v", rt.applied)
	}
	obs := h.kind(run, wf.JournalObserved)
	var od wf.ObservedData
	_ = json.Unmarshal(obs[0].Data, &od)
	if od.Desc.Args["path"] != string(run)+"/x.txt" {
		t.Fatalf("observed descriptor = %+v, want the resolved path", od.Desc)
	}
}

func TestUnresolvedPlaceholderBlocked(t *testing.T) {
	t.Run("effect args", func(t *testing.T) {
		d := wf.New("unresolved")
		a := wf.NewAgent[eA, eOut](d)
		done, bad := twoTerminals(d)
		a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(2), a.To(bad, "x")))
		next := a.To(done, "done").With(wf.ExternalEffect{Adapter: "bd", Op: "create", Args: map[string]any{"x": "{nosuch}"}, Key: wf.KeyOf("k")})
		a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
		rt := newFakeRuntime()
		h := newHarness(t, d, rt)
		st := h.drive(h.run(d, nil), map[string][]string{"eA": {`{}`}})
		if st.Phase != wf.RunBlocked || !strings.Contains(st.Instances[0].Reason, "nosuch") || len(rt.applied) != 0 {
			t.Fatalf("status = %+v applied = %v, want a blocked halt before any effect", st, rt.applied)
		}
	})
	t.Run("observation descriptor", func(t *testing.T) {
		d := wf.New("unresolved-desc")
		obs := wf.Observe[eRes]("fs", "stat", map[string]any{"path": "{nosuch}"})
		c := wf.NewCompute[eC, eOut](d, func(c wf.Ctx) (eOut, error) { return eOut{}, nil }).Observes(obs)
		done, _ := twoTerminals(d)
		next := c.To(done, "done")
		c.Route(func(eOut, wf.Ctx) wf.Edge[eC] { return next }, next)
		h := newHarness(t, d, newFakeRuntime())
		run := h.run(d, nil)
		h.next(run)
		if st := h.status(run); st.Phase != wf.RunBlocked {
			t.Fatalf("status = %+v", st)
		}
	})
}

func TestPromptBindingAndInputsBlock(t *testing.T) {
	d := wf.New("prompts")
	topic := d.Param("topic")
	a1 := wf.NewAgent[eA, eOut](d)
	a2 := wf.NewAgent[eB, eOut](d)
	a3 := wf.NewAgent[eC, eOut](d)
	done, bad := twoTerminals(d)
	a1.Spec(wf.PromptText("first"), wf.Retry[eA](wf.Const(2), a1.To(bad, "x")))
	a2.Spec(wf.PromptText("Topic {{topic}}: findings {{FINDINGS}}", topic, wf.As("FINDINGS", wf.Out(a1))), wf.Retry[eB](wf.Const(2), a2.To(bad, "x")))
	a3.Spec(wf.PromptText("Fix this: {{out}}", wf.Out(a1)), wf.Retry[eC](wf.Const(2), a3.To(bad, "x")))
	n1, n2, n3 := a1.To(a2, "n"), a2.To(a3, "n"), a3.To(done, "n")
	a1.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return n1 }, n1)
	a2.Route(func(eOut, wf.Ctx) wf.Edge[eB] { return n2 }, n2)
	a3.Route(func(eOut, wf.Ctx) wf.Edge[eC] { return n3 }, n3)
	h := newHarness(t, d, nil)
	run := h.run(d, map[string]string{"topic": "parser"})
	h.submit(run, h.next(run)[0], `{"V":"v1"}`)
	in2 := h.next(run)[0]
	h.submit(run, in2, `{}`)
	in3 := h.next(run)[0]
	for _, in := range []wf.Instruction{in2, in3} {
		if strings.Contains(in.Prompt, "{{") {
			t.Fatalf("unbound placeholder left in prompt:\n%s", in.Prompt)
		}
	}
	if !strings.Contains(in2.Prompt, `Topic parser: findings {"V":"v1"`) {
		t.Fatalf("alias and param not bound:\n%s", in2.Prompt)
	}
	// Inputs a token consumed are in the prompt once: the Inputs block lists
	// only the rest, and the instruction's Inputs map keeps every input.
	if strings.Count(in2.Prompt, `{"V":"v1"`) != 1 || strings.Contains(in2.Prompt, "### FINDINGS") || strings.Contains(in2.Prompt, "### topic") {
		t.Fatalf("a consumed input is repeated in the Inputs block:\n%s", in2.Prompt)
	}
	if in2.Inputs["FINDINGS"] == "" || in2.Inputs["topic"] != "parser" {
		t.Fatalf("instruction inputs %+v, want every input", in2.Inputs)
	}
	if !strings.Contains(in3.Prompt, `Fix this: {"V":"v1"`) || in3.Inputs["eA"] == "" {
		t.Fatalf("{{out}} not bound to the next Out input: %+v", in3)
	}
	// The positional fallback consumed eA as well: it is not listed again.
	if strings.Contains(in3.Prompt, "### eA") {
		t.Fatalf("an input bound by the positional fallback is repeated:\n%s", in3.Prompt)
	}
}

func TestFreshEngineResolvesDefByName(t *testing.T) {
	d, _ := simple("byname", wf.PromptText("p"))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: "/ws", ModelMap: map[string]string{"opus": "sonnet"}})
	run := h.run(d, map[string]string{"k": "v"})
	var started wf.RunStartedData
	_ = json.Unmarshal(h.kind(run, wf.JournalRunStarted)[0].Data, &started)
	if started.Def != "byname" || started.Workspace != "/ws" || started.Params["k"] != "v" || started.ModelMap["opus"] != "sonnet" {
		t.Fatalf("run_started = %+v", started)
	}
	fresh := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
	ins, err := fresh.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].Workspace != "/ws" {
		t.Fatalf("fresh engine Next = %+v, %v", ins, err)
	}
	blind := wf.NewEngine(h.j, h.reg, wf.EngineOptions{})
	if _, err := blind.Next(run, ""); !errors.Is(err, wf.ErrUnknownDef) {
		t.Fatalf("engine without the definition: %v", err)
	}
}

func TestHashMismatchIsAClearError(t *testing.T) {
	d1, _ := simple("versioned", wf.PromptText("p"))
	h := newHarnessOpts(t, d1, nil, wf.EngineOptions{RunID: "r1"})
	run := h.run(d1, nil)
	d2, _ := simple("versioned", wf.PromptText("p"))
	d2.Version(2)
	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d2}, RunID: "r1"})
	if _, err := e2.Next(run, ""); !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "Migration") {
		t.Fatalf("Next across a hash change: %v", err)
	}
	if _, err := e2.Run(d2, nil, nil); !errors.Is(err, wf.ErrDefChanged) {
		t.Fatalf("Run across a hash change: %v", err)
	}
}

func TestDoneWhenCompletesAgent(t *testing.T) {
	d, a := simple("donewhen", wf.PromptText("p"))
	wf.DoneWhen(a, wf.Observe[bool]("x", "done", nil), "already done")
	rt := newFakeRuntime()
	rt.set("x", "done", nil, true)
	h := newHarness(t, d, rt)
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none: the state is done", ins)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eDone")
	if len(h.kind(run, wf.JournalCompletedByObservation)) != 1 || len(h.kind(run, wf.JournalInstructionIssued)) != 0 {
		t.Fatalf("kinds = %v", kinds(h.records(run)))
	}

	rt.set("x", "done", nil, false)
	h = newHarness(t, d, rt)
	run = h.run(d, nil)
	if ins := h.next(run); len(ins) != 1 {
		t.Fatalf("with the condition unmet the agent is issued: %+v", ins)
	}
}

func kinds(recs []wf.JournalRecord) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Kind.String())
	}
	return slices.Clone(out)
}

type oeVerdict string

type oeOut struct{ Verdict oeVerdict }

// outOfEnumDef routes an agent through a Cases table over a registered Enum
// (open=false) or a CasesOpen table with Otherwise (open=true), retrying twice.
func outOfEnumDef(open bool) *wf.Def {
	d := wf.New("out-of-enum")
	a := wf.NewAgent[eA, oeOut](d)
	done, bad := twoTerminals(d)
	fallback := wf.NewTerminal[eC](d, wf.Stopped)
	d.OnFailure(wf.Blocked, fallback)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(2), a.To(fallback, "gave up")))
	field := func(o oeOut) oeVerdict { return o.Verdict }
	if open {
		a.RouteCases(wf.CasesOpen[eA, oeOut](field,
			wf.On(oeVerdict("pass"), a.To(done, "pass")),
		).Otherwise(a.To(bad, "other")))
	} else {
		verdicts := wf.NewEnum(d, "oe-verdict", oeVerdict("pass"), oeVerdict("fail"))
		a.RouteCases(wf.Cases[eA, oeOut](field, verdicts,
			wf.On(oeVerdict("pass"), a.To(done, "pass")),
			wf.On(oeVerdict("fail"), a.To(bad, "fail"))))
	}
	return d
}

func TestOutOfEnumOutputSpendsRetry(t *testing.T) {
	d := outOfEnumDef(false)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	in := h.next(run)[0]
	res := h.submit(run, in, `{"Verdict":"maybe"}`)
	if res.Class != wf.Refused || len(res.Refusal) != 1 || res.Refusal[0] != `output.Verdict: value maybe is outside enum oe-verdict (pass, fail)` {
		t.Fatalf("result = %+v, want Refused naming the value and the set", res)
	}
	again := h.next(run)
	if len(again) != 1 || again[0].Attempt != 2 {
		t.Fatalf("after the refusal = %+v, want attempt 2 issued", again)
	}
	if res := h.submit(run, again[0], `{"Verdict":"nope"}`); res.Class != wf.Refused {
		t.Fatalf("second result = %+v, want Refused", res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eC")
}

func TestMissingEnumFieldSpendsRetry(t *testing.T) {
	d := outOfEnumDef(false)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	res := h.submit(run, h.next(run)[0], `{}`)
	if res.Class != wf.Refused || len(res.Refusal) != 1 || res.Refusal[0] != "output.Verdict: value  is outside enum oe-verdict (pass, fail)" {
		t.Fatalf("result = %+v, want Refused saying the value is missing", res)
	}
	if again := h.next(run); len(again) != 1 || again[0].Attempt != 2 {
		t.Fatalf("after the refusal = %+v, want attempt 2 issued", again)
	}
}

func TestOtherwiseStillCatches(t *testing.T) {
	d := outOfEnumDef(true)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	res := h.submit(run, h.next(run)[0], `{"Verdict":"weird"}`)
	if res.Class != wf.Accepted {
		t.Fatalf("result = %+v, want Accepted through Otherwise", res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eBad")
}

func TestComputeOutOfCasesStaysBlocked(t *testing.T) {
	d := wf.New("compute-out-of-cases")
	c := wf.NewCompute[eC, oeOut](d, func(wf.Ctx) (oeOut, error) { return oeOut{Verdict: "maybe"}, nil })
	done, bad := twoTerminals(d)
	verdicts := wf.NewEnum(d, "oe-verdict", oeVerdict("pass"), oeVerdict("fail"))
	c.RouteCases(wf.Cases[eC, oeOut](func(o oeOut) oeVerdict { return o.Verdict }, verdicts,
		wf.On(oeVerdict("pass"), c.To(done, "pass")),
		wf.On(oeVerdict("fail"), c.To(bad, "fail"))))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.next(run)
	st := h.status(run)
	if st.Phase != wf.RunBlocked || !strings.Contains(st.Instances[0].Reason, "no case for value") {
		t.Fatalf("status = %+v, want a Blocked halt", st)
	}
}

func TestPromptRefRecordsWithoutRunDirBlocksNamingRunDir(t *testing.T) {
	d := wf.New("precs")
	wf.Typed[eItem, eRes](d)
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	ref := "{records}/prompt.md"
	a.Spec(wf.PromptFor(func(eItem) string { return ref }, []string{ref}),
		wf.Retry[eA](wf.Const(2), a.To(bad, "x")))
	next := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: t.TempDir()})
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 0 {
		t.Fatalf("instructions = %+v, want none", ins)
	}
	if st := h.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("status = %+v, want blocked", st)
	}
	recs := h.kind(run, wf.JournalRefused)
	if len(recs) != 1 || recs[0].Class != wf.Blocked {
		t.Fatalf("refused records = %+v", recs)
	}
	var rd wf.RefusedData
	if err := json.Unmarshal(recs[0].Data, &rd); err != nil {
		t.Fatal(err)
	}
	if len(rd.Reasons) != 1 || !strings.Contains(rd.Reasons[0], "RunDir") || !strings.Contains(rd.Reasons[0], "{records}") {
		t.Fatalf("reasons = %q, want one naming {records} and RunDir", rd.Reasons)
	}
}

// inputDef is one agent whose input observation is x.y; withStale adds an
// OnClass(Stale) route to eStale.
func inputDef(withStale bool) (*wf.Def, wf.Observation[eRes]) {
	obs := wf.Observe[eRes]("x", "y", map[string]any{"k": "v"})
	d := wf.New("input-stale")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	stale := wf.NewTerminal[eStale](d, wf.Stopped)
	d.OnFailure(wf.Blocked, bad)
	retry := wf.Retry[eA](wf.Const(2), a.To(bad, "x")).NoRecovery()
	if withStale {
		retry = retry.OnClass(wf.Stale, a.To(stale, "stale"))
	}
	a.Spec(wf.PromptText("p"), retry)
	a.Observes(obs)
	next := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
	return d, obs
}

// Agent.Observes: a world that changes under a live attempt re-issues the
// step with nothing spent; the re-issued attempt is taken once the input is
// unchanged. A Stale route overrides the re-issue. Inputs are in the view.
func TestInputObservationChangeReissues(t *testing.T) {
	t.Run("re-issue", func(t *testing.T) {
		d, _ := inputDef(false)
		rt := newFakeRuntime()
		rt.set("x", "y", map[string]any{"k": "v"}, eRes{OK: false})
		h := newHarness(t, d, rt)
		run := h.run(d, nil)
		in := h.next(run)[0]
		rt.set("x", "y", map[string]any{"k": "v"}, eRes{OK: true}) // the world moves under the live attempt
		if res := h.submit(run, in, `{}`); res.Class != wf.Stale {
			t.Fatalf("submit after the input changed = %+v, want stale", res)
		}
		again := h.next(run)
		if len(again) != 1 || again[0].State != "eA" || again[0].Attempt != 2 {
			t.Fatalf("next = %+v, want eA re-issued at attempt 2", again)
		}
		if rd := lastRefused(t, h, run); rd.Spent {
			t.Fatalf("refused = %+v, want nothing spent", rd)
		}
		if res := h.submit(run, again[0], `{}`); res.Class != wf.Accepted {
			t.Fatalf("re-issued attempt = %+v, want accepted on an unchanged input", res)
		}
		statusOf(t, h.status(run), wf.RunEnded, "eDone")
	})
	t.Run("stale route", func(t *testing.T) {
		d, _ := inputDef(true)
		rt := newFakeRuntime()
		rt.set("x", "y", map[string]any{"k": "v"}, eRes{OK: false})
		h := newHarness(t, d, rt)
		run := h.run(d, nil)
		in := h.next(run)[0]
		rt.set("x", "y", map[string]any{"k": "v"}, eRes{OK: true})
		h.submit(run, in, `{}`)
		statusOf(t, h.status(run), wf.RunEnded, "eStale")
	})
	t.Run("a world that keeps changing spends attempts", func(t *testing.T) {
		d, _ := inputDef(false)
		rt := newFakeRuntime()
		h := newHarness(t, d, rt)
		run := h.run(d, nil)
		for i := 0; i < 10; i++ {
			rt.set("x", "y", map[string]any{"k": "v"}, eRes{OK: i%2 == 0})
			ins := h.next(run)
			if len(ins) == 0 {
				break
			}
			rt.set("x", "y", map[string]any{"k": "v"}, eRes{OK: i%2 != 0})
			h.submit(run, ins[0], `{}`)
		}
		if n := issuedAt(h, run, "eA"); n != wf.StaleReissues+2 {
			t.Fatalf("eA issued %d times, want %d: %d free re-issues, then the budget of 2", n, wf.StaleReissues+2, wf.StaleReissues)
		}
		statusOf(t, h.status(run), wf.RunEnded, "eBad")
	})
	for _, sv := range wf.Inspect(func() *wf.Def { d, _ := inputDef(false); return d }()).States {
		if sv.Name == "eA" && !slices.Contains(sv.Observes, `input x.y k="v"`) {
			t.Fatalf("eA observes %v, want the input listed", sv.Observes)
		}
	}
}

// An outage of an input observation is not a change: it is Unavailable and
// backs off, never Stale.
func TestInputOutageIsNotStale(t *testing.T) {
	d, obs := inputDef(false)
	rt := newFakeRuntime()
	rt.set("x", "y", map[string]any{"k": "v"}, eRes{OK: true})
	h := newHarness(t, d, rt)
	run := h.run(d, nil)
	in := h.next(run)[0]
	_ = obs
	rt.down["x.y"] = true
	if res := h.submit(run, in, `{}`); res.Class != wf.Unavailable {
		t.Fatalf("submit during an input outage = %+v, want unavailable", res)
	}
}
