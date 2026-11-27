package wf_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	ruDraft  struct{}
	ruReview struct{}
	ruDone   struct{}
	ruStop   struct{}
)

// ruKnobs are the ways ruDef's versions differ.
type ruKnobs struct {
	reviewPrompt string
	approveLabel string
	acceptBad    bool   // the route's body: a closure-only change
	needsParam   string // the review prompt binds this param
}

// ruDef is a drafter and a reviewer whose route approves "good" (and "bad"
// too when acceptBad) and stops on anything else.
func ruDef(k ruKnobs) *wf.Def {
	if k.reviewPrompt == "" {
		k.reviewPrompt = "review {{d}}"
	}
	if k.approveLabel == "" {
		k.approveLabel = "approved"
	}
	d := wf.New("lcp")
	draft := wf.NewAgent[ruDraft, string](d)
	review := wf.NewAgent[ruReview, string](d)
	done := wf.NewTerminal[ruDone](d, wf.Success)
	stop := wf.NewTerminal[ruStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	draft.Spec(wf.PromptText("draft"), wf.Retry(wf.Const(1), draft.To(stop, "draft gave up")))
	draft.Then(draft.To(review, "drafted"))
	inputs := []wf.Input{wf.As("d", wf.Out(draft))}
	prompt := k.reviewPrompt
	if k.needsParam != "" {
		inputs = append(inputs, wf.As("extra", d.Param(k.needsParam)))
		prompt += " {{extra}}"
	}
	review.Spec(wf.PromptText(prompt, inputs...), wf.Retry(wf.Const(1), review.To(stop, "review gave up")))
	ok := review.To(done, k.approveLabel)
	no := review.To(stop, "rejected").Ends(wf.Stopped, "the reviewer rejects the draft")
	accept := k.acceptBad
	review.Route(func(s string, _ wf.Ctx) wf.Edge[ruReview] {
		if s == "good" || (accept && s == "bad") {
			return ok
		}
		return no
	}, ok, no)
	return d
}

// ruStopped runs ruDef(v1) to its stop: the draft is "bad" and the reviewer
// sees it as such.
func ruStopped(t *testing.T, v1 *wf.Def) *harness {
	t.Helper()
	h := newHarness(t, v1, nil)
	run := h.run(v1, nil)
	h.submit(run, sbOnly(t, h.next(run), "ruDraft"), `"draft"`)
	h.submit(run, sbOnly(t, h.next(run), "ruReview"), `"bad"`)
	statusOf(t, h.status(run), wf.RunEnded, "ruStop")
	return h
}

func ruRunID(t *testing.T, h *harness) wf.RunID {
	t.Helper()
	runs, err := h.j.Runs()
	if err != nil || len(runs) == 0 {
		t.Fatalf("runs %v %v", runs, err)
	}
	return runs[0]
}

// ruEngine is an engine with v2 over h's journal; adopt sets EngineOptions.Adopt.
func ruEngine(h *harness, v2 *wf.Def, adopt bool) *wf.Engine {
	return wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{v2}, Adopt: adopt, Clock: func() time.Time { return h.now }}).(*wf.Engine)
}

// carryOn adopts the fork onto v2 and finishes it with review answer.
func carryOn(t *testing.T, h *harness, v2 *wf.Def, fork wf.RunID, state, answer string) wf.RunStatus {
	t.Helper()
	e := ruEngine(h, v2, true)
	ins, err := e.Next(fork, "")
	if err != nil {
		t.Fatalf("adopting the fork: %v", err)
	}
	in := sbOnly(t, ins, state)
	if _, err := e.Submit(fork, wf.Submission{Token: in.Token, Output: json.RawMessage(answer)}); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status(fork)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRewindUnchangedCutsBeforeTheFirstChangedState(t *testing.T) {
	for _, c := range []struct {
		name string
		v2   ruKnobs
		why  string
	}{
		{"prompt edit", ruKnobs{reviewPrompt: "review carefully {{d}}"}, "prompt"},
		{"edge edit", ruKnobs{approveLabel: "accepted"}, "edges"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := ruStopped(t, ruDef(ruKnobs{}))
			run := ruRunID(t, h)
			v2 := ruDef(c.v2)
			rep, err := ruEngine(h, v2, false).RewindUnchanged(run, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			if rep.Diverged.State != "lcp/ruReview" || strings.Join(rep.Diverged.Why, ",") != c.why || rep.Diverged.Kind != wf.JournalInstructionIssued {
				t.Fatalf("diverged = %+v, want lcp/ruReview's issue, %s", rep.Diverged, c.why)
			}
			if rep.Cut >= rep.Diverged.Seq || rep.Caveat == "" || rep.Run == "" {
				t.Fatalf("report = %+v", rep)
			}
			// The draft is kept: the fork asks only for the review again,
			// under the new definition, and the source stays as it was.
			st := carryOn(t, h, v2, rep.Run, "ruReview", `"good"`)
			if st.Phase != wf.RunEnded || st.Terminal != "ruDone" {
				t.Fatalf("fork = %+v", st)
			}
			statusOf(t, h.status(run), wf.RunEnded, "ruStop")
		})
	}
}

// A fix inside a closure leaves every identity and the hash as they were:
// named with --changed, the cut lands before that state's first run; not
// named, the verb refuses rather than call the run unchanged when the
// binary differs, and says nothing changed when it does not.
func TestRewindUnchangedClosureOnlyFix(t *testing.T) {
	v1, v2 := ruDef(ruKnobs{}), ruDef(ruKnobs{acceptBad: true})
	if v1.Hash() != v2.Hash() {
		t.Fatal("a closure-only change moved the hash")
	}
	restore := wf.SetBuildInfo(stampInfo("aaaa"))
	h := ruStopped(t, v1)
	run := ruRunID(t, h)

	_, err := ruEngine(h, v2, false).RewindUnchanged(run, nil, "")
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "nothing to rewind") {
		t.Fatalf("same binary, nothing named: %v", err)
	}
	restore()
	defer wf.SetBuildInfo(stampInfo("bbbb"))()
	_, err = ruEngine(h, v2, false).RewindUnchanged(run, nil, "")
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "this binary differs") || !strings.Contains(err.Error(), "--changed") || !strings.Contains(err.Error(), "--to <seq>") {
		t.Fatalf("other binary, nothing named: %v", err)
	}
	if _, err := ruEngine(h, v2, false).RewindUnchanged(run, []string{"lcp/nosuch"}, ""); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("unknown --changed state: %v", err)
	}

	rep, err := ruEngine(h, v2, false).RewindUnchanged(run, []string{"lcp/ruReview"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Diverged.State != "lcp/ruReview" || rep.Diverged.Why[0] != "named by --changed" {
		t.Fatalf("diverged = %+v", rep.Diverged)
	}
	// The fixed route now accepts the same answer.
	st := carryOn(t, h, v2, rep.Run, "ruReview", `"bad"`)
	if st.Terminal != "ruDone" {
		t.Fatalf("fork = %+v", st)
	}
}

// A journal from before per-state identities is refused with the by-hand
// alternative.
func TestRewindUnchangedRefusesAnOldJournal(t *testing.T) {
	h := ruStopped(t, ruDef(ruKnobs{}))
	recs := h.records(ruRunID(t, h))
	var started map[string]json.RawMessage
	if err := json.Unmarshal(recs[0].Data, &started); err != nil {
		t.Fatal(err)
	}
	if started["StateIDs"] == nil {
		t.Fatal("run_started records no StateIDs")
	}
	delete(started, "StateIDs")
	old := newHarness(t, ruDef(ruKnobs{}), nil)
	unlock, err := old.j.Lock(recs[0].Run)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range recs {
		if i == 0 {
			r.Data, _ = json.Marshal(started)
		}
		if err := old.j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	unlock()
	_, err = ruEngine(old, ruDef(ruKnobs{reviewPrompt: "x {{d}}"}), false).RewindUnchanged(recs[0].Run, nil, "")
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "--to <seq>") {
		t.Fatalf("old journal: %v", err)
	}
}

// ruIntDef is ruDef whose reviewer answers with a number: a changed result
// type, which adoption refuses at a live reviewer.
func ruIntDef() *wf.Def {
	d := wf.New("lcp")
	draft := wf.NewAgent[ruDraft, string](d)
	review := wf.NewAgent[ruReview, int](d)
	done := wf.NewTerminal[ruDone](d, wf.Success)
	stop := wf.NewTerminal[ruStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	draft.Spec(wf.PromptText("draft"), wf.Retry(wf.Const(1), draft.To(stop, "draft gave up")))
	draft.Then(draft.To(review, "drafted"))
	review.Spec(wf.PromptText("score {{d}}", wf.As("d", wf.Out(draft))), wf.Retry(wf.Const(1), review.To(stop, "review gave up")))
	ok := review.To(done, "approved")
	no := review.To(stop, "rejected").Ends(wf.Stopped, "the reviewer rejects the draft")
	review.Route(func(n int, _ wf.Ctx) wf.Edge[ruReview] {
		if n > 5 {
			return ok
		}
		return no
	}, ok, no)
	return d
}

// A cut whose adoption is refused steps back to an earlier one: here the
// reviewer's result type changed, so the fork starts before the draft.
func TestRewindUnchangedStepsBackPastARefusedCut(t *testing.T) {
	h := ruStopped(t, ruDef(ruKnobs{}))
	run := ruRunID(t, h)
	v2 := ruIntDef()
	rep, err := ruEngine(h, v2, false).RewindUnchanged(run, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.SteppedBy) != 1 || !strings.Contains(rep.SteppedBy[0], "ruReview") || rep.Diverged.Why[0] != "shape" {
		t.Fatalf("report %+v", rep)
	}
	e := ruEngine(h, v2, true)
	ins, err := e.Next(rep.Run, "")
	if err != nil {
		t.Fatal(err)
	}
	sbOnly(t, ins, "ruDraft")
}

// A migration records the new definition's identities, so records after it
// are judged by them.
func TestMigrationRecordsStateIDs(t *testing.T) {
	v1 := ruDef(ruKnobs{})
	h := newHarness(t, v1, nil)
	run := h.run(v1, nil)
	h.submit(run, sbOnly(t, h.next(run), "ruDraft"), `"draft"`)
	v2 := ruDef(ruKnobs{reviewPrompt: "review again {{d}}"})
	e := ruEngine(h, v2, true)
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range h.kind(run, wf.JournalMigrated) {
		var m wf.MigratedData
		if err := json.Unmarshal(r.Data, &m); err != nil {
			t.Fatal(err)
		}
		found = m.StateIDs["lcp/ruReview"] != ""
	}
	if !found {
		t.Fatal("the adoption recorded no state identities")
	}
}

// A snapshot run is restored from the latest checkpoint before the first
// run of the named state, through the CLI; with the cause fixed, the fork
// carries on to success.
func TestRewindUnchangedRestoresASnapshotCheckpoint(t *testing.T) {
	m := failedMarkerRun(t)
	cp, _ := m.checkpointBefore(t, wf.JournalEffectIntent)
	out := filepath.Join(t.TempDir(), "fork")
	code, stdout, er := cliCall(m.cfg, "wf", "dev", "rewind", "--to", "unchanged", "--changed", "rewind-to/rwCheck", "--out", out,
		"--state-dir", m.state, "--workspace", m.ws, "--run-id", string(m.run))
	if code != 0 {
		t.Fatalf("rewind --to unchanged = %d %s %s", code, stdout, er)
	}
	if !strings.Contains(er, "first change: rewind-to/rwCheck") || !strings.Contains(er, "named by --changed") || !strings.Contains(er, "closure bodies") {
		t.Errorf("report:\n%s", er)
	}
	if err := os.WriteFile(m.marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ws, state := filepath.Join(out, "workspace"), filepath.Join(out, "state")
	e := wf.NewEngine(wf.FileJournal(state), m.cfg.Adapters, wf.EngineOptions{Defs: []*wf.Def{m.cfg.Defs[0].Def}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }})
	if _, err := e.Next(m.run, ""); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status(m.run)
	if err != nil || st.Terminal != "rwDone" {
		t.Fatalf("fork = %+v %v", st, err)
	}
	recs, err := wf.ReadJournal(state, m.run)
	if err != nil {
		t.Fatal(err)
	}
	if d := rewound(t, recs); d.To != "unchanged" || d.Seq != cp {
		t.Fatalf("rewound = %+v, want cut at checkpoint %d", d, cp)
	}
}
