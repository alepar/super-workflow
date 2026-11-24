package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// tieredFan is fanDef whose child agent is hinted with tier.
func tieredFan(name string, par int, tier wf.Tier, items ...int) *wf.Def {
	d := wf.New(name)
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(items...), func(i chItem) string { return fmt.Sprint(i.N) }, chChild(wf.ModelTier(tier)))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(par).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// boundaryEngine is an engine on h's journal whose resume, rewind or fork
// journals modelMap on the boundary it opens.
func boundaryEngine(h *harness, d *wf.Def, modelMap map[string]string) *wf.Engine {
	return wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }, BoundaryModelMap: modelMap}).(*wf.Engine)
}

// handedModels are the models the run's handed_out records journaled, by unit.
func handedModels(t *testing.T, h *harness, run wf.RunID) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, r := range h.kind(run, wf.JournalHandedOut) {
		var d wf.HandedOutData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		m := "<none>"
		if d.Spawn != nil {
			m = d.Spawn.Model
		}
		out[d.Name] = append(out[d.Name], m)
	}
	return out
}

// an earlier revision acceptance: all=gpt-6.1-terra yields only terra spawns across a full
// run, whatever tier each state asks for.
func TestAllPinsEveryTierAcrossARun(t *testing.T) {
	d := tieredFan("all-terra", 2, wf.TierTop, 1, 2, 3)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex", ModelMap: map[string]string{"all": "gpt-6.1-terra"}})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	for i := 0; i < 3; i++ {
		u := runUnit(t, e, run, "codex", "c-coord")
		if u.Dispatch.Model != "gpt-6.1-terra" || u.Dispatch.Effort != "xhigh" {
			t.Fatalf("unit %s spawns %q at %q, want gpt-6.1-terra at the top tier's xhigh", u.Name, u.Dispatch.Model, u.Dispatch.Effort)
		}
	}
	end, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "c-coord"})
	if err != nil || len(end) != 1 || end[0].Kind != wf.InstructionEnded {
		t.Fatalf("after three units = %+v, %v; want the run ended", end, err)
	}
	for name, ms := range handedModels(t, h, run) {
		for _, m := range ms {
			if m != "gpt-6.1-terra" {
				t.Fatalf("handed_out %s journaled model %q", name, m)
			}
		}
	}
}

// an earlier revision acceptance: a resume with a new map changes spawns from the next
// hand-out; a unit claimed before the boundary keeps its recorded model, and
// an offer no child claimed is offered again under the new map. Status and the
// journal show both maps.
func TestResumeWithANewMapChangesTheNextHandOut(t *testing.T) {
	d := tieredFan("resume-map", 2, wf.TierStrong, 1, 2, 3)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "codex", Session: "c1"})
	if err != nil || len(agentsOf(ins)) != 2 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	claimed, offered := agentsOf(ins)[0], agentsOf(ins)[1]
	if claimed.Dispatch.Model != "gpt-6.2-astra" || offered.Dispatch.Model != "gpt-6.2-astra" {
		t.Fatalf("first segment spawns %q, %q; want the codex table's gpt-6.2-astra", claimed.Dispatch.Model, offered.Dispatch.Model)
	}
	if _, err := e.PromptFetchAs(run, claimed.Name, claimToken(t, claimed), segChild("codex", "c1", "kid-a")); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(run, "cheaper model"); err != nil {
		t.Fatal(err)
	}
	if err := boundaryEngine(h, d, map[string]string{"strong": "gpt-6.1-terra"}).Resume(run); err != nil {
		t.Fatal(err)
	}
	again, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "codex", Session: "c2"})
	if err != nil || len(agentsOf(again)) != 1 || agentsOf(again)[0].Name != offered.Name {
		t.Fatalf("after the resume = %+v, %v; want %s offered again", again, err, offered.Name)
	}
	if m := agentsOf(again)[0].Dispatch.Model; m != "gpt-6.1-terra" {
		t.Fatalf("the re-offer spawns %q, want the new map's gpt-6.1-terra", m)
	}
	if _, err := e.Done(run, claimed.Name, wf.Submission{Output: json.RawMessage(`{"N":1}`)}); err != nil {
		t.Fatalf("the claimed child of the first segment could not finish: %v", err)
	}
	third, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "codex", Session: "c2"})
	if err != nil || len(agentsOf(third)) != 1 || agentsOf(third)[0].Dispatch.Model != "gpt-6.1-terra" {
		t.Fatalf("the next new unit = %+v, %v; want it on gpt-6.1-terra", third, err)
	}
	hm := handedModels(t, h, run)
	if got := hm[claimed.Name]; len(got) != 1 || got[0] != "gpt-6.2-astra" {
		t.Fatalf("claimed unit's recorded models = %v, want only gpt-6.2-astra", got)
	}
	if got := hm[offered.Name]; len(got) != 2 || got[0] != "gpt-6.2-astra" || got[1] != "gpt-6.1-terra" {
		t.Fatalf("re-offered unit's recorded models = %v, want gpt-6.2-astra then gpt-6.1-terra", got)
	}
	var resumed wf.ResumedData
	if err := json.Unmarshal(h.kind(run, wf.JournalResumed)[0].Data, &resumed); err != nil || resumed.ModelMap["strong"] != "gpt-6.1-terra" {
		t.Fatalf("resumed record = %+v, %v; want the given map", resumed, err)
	}
	st, err := e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Segments) != 2 || st.Segments[0].Models["strong"] != "gpt-6.2-astra" || st.Segments[1].Models["strong"] != "gpt-6.1-terra" || st.Segments[1].ModelsFrom != wf.ModelsFromMap {
		t.Fatalf("segments = %+v", st.Segments)
	}
	if st.Models == nil || st.Models.Models["strong"] != "gpt-6.1-terra" {
		t.Fatalf("status models = %+v", st.Models)
	}
	view := wf.RenderStatus(st)
	for _, want := range []string{"models: strong=gpt-6.1-terra (--model-map strong=gpt-6.1-terra)", "codex 1-", "strong=gpt-6.2-astra", "resume, strong=gpt-6.1-terra"} {
		if !strings.Contains(view, want) {
			t.Errorf("status lacks %q:\n%s", want, view)
		}
	}
}

// an earlier revision: with no new map, a resume on the same harness carries the
// segment's map; on another harness the old map is dropped, that harness's
// defaults apply, and the run says so.
func TestResumeWithoutAMapCarriesOrFallsBack(t *testing.T) {
	d := tieredFan("resume-carry", 1, wf.TierStrong, 1, 2, 3)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex", ModelMap: map[string]string{"all": "gpt-6.1-terra"}})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	runUnit(t, e, run, "codex", "c1")
	for _, step := range []struct{ h, session, want, from string }{
		{"codex", "c2", "gpt-6.1-terra", wf.ModelsFromCarried},
		{"claude", "cl", "opus", wf.ModelsFromDefaults},
	} {
		if err := e.Stop(run, "move"); err != nil {
			t.Fatal(err)
		}
		if err := e.Resume(run); err != nil {
			t.Fatal(err)
		}
		if u := runUnit(t, e, run, step.h, step.session); u.Dispatch.Model != step.want {
			t.Fatalf("%s after a resume with no map spawns %q, want %q", step.h, u.Dispatch.Model, step.want)
		}
		st, _ := e.Status(run)
		if from := st.Segments[len(st.Segments)-1].ModelsFrom; from != step.from {
			t.Fatalf("%s segment's models came from %q, want %q", step.h, from, step.from)
		}
	}
	st, _ := e.Status(run)
	said := false
	for _, dg := range st.Degraded {
		if dg.Kind == wf.DegradedTiers && strings.Contains(dg.Cause, "no --model-map") && strings.Contains(dg.Action, "strong=opus") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the harness change with no map is not reported: %+v", st.Degraded)
	}
}

// an earlier revision: the map changes only at a segment boundary: resume --adopt of a
// running run with a map is refused, and nothing is journaled.
func TestAMapIsRefusedMidSegment(t *testing.T) {
	d := tieredFan("mid-segment", 1, wf.TierStrong, 1)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	before := len(h.records(run))
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }, BoundaryModelMap: map[string]string{"all": "x"}, Adopt: true}).(*wf.Engine)
	err := e.Resume(run)
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "only at a segment boundary") {
		t.Fatalf("resume --adopt --model-map of a running run = %v, want refused", err)
	}
	if len(h.records(run)) != before {
		t.Fatal("a refused resume journaled something")
	}
}

// an earlier revision: an unknown key is refused at the boundary, naming the valid keys.
func TestABoundaryMapWithAnUnknownKeyIsRefused(t *testing.T) {
	d := tieredFan("bad-key", 1, wf.TierStrong, 1)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	if err := h.e.(*wf.Engine).Stop(run, "s"); err != nil {
		t.Fatal(err)
	}
	err := boundaryEngine(h, d, map[string]string{"lite": "x"}).Resume(run)
	if err == nil || !strings.Contains(err.Error(), `"lite"`) || !strings.Contains(err.Error(), "light, standard, strong, top, all") {
		t.Fatalf("resume with key lite = %v", err)
	}
}

// an earlier revision: a journal from before per-segment maps (a binding coordinator
// record with no Models) folds as it did: its units spawn with run_started's
// Models, even after the binary's table changed.
func TestALegacyBindingRecordUsesTheStartModels(t *testing.T) {
	d := tieredFan("legacy-bind", 1, wf.TierStrong, 1)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{})
	unlock, err := h.j.Lock("old")
	if err != nil {
		t.Fatal(err)
	}
	started, _ := json.Marshal(wf.RunStartedData{Def: d.Name(), Harness: "codex", Models: map[string]string{"strong": "gpt-6.2-astra"}, Efforts: map[string]string{"strong": "high"}})
	bind, _ := json.Marshal(wf.CoordinatorData{Harness: "codex", Session: "c-old", SegmentStart: 1})
	for _, r := range []wf.JournalRecord{
		{Kind: wf.JournalRunStarted, Run: "old", Seq: 1, DefHash: d.Hash(), At: h.now, Data: started},
		{Kind: wf.JournalCoordinator, Run: "old", Seq: 2, DefHash: d.Hash(), At: h.now, Data: bind},
	} {
		if err := h.j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	unlock()
	defer wf.SetHarnessTierModel("codex", wf.TierStrong, wf.TierModel{Model: "gpt-changed", Effort: "low"})()
	ins, err := h.e.(*wf.Engine).HandOutAs("old", 1, wf.Coordinator{Harness: "codex", Session: "c-old"})
	if err != nil || len(agentsOf(ins)) != 1 || agentsOf(ins)[0].Dispatch.Model != "gpt-6.2-astra" {
		t.Fatalf("legacy hand-out = %+v, %v; want gpt-6.2-astra", ins, err)
	}
}

// an earlier revision through the CLI: run creation and resume refuse an unknown
// --model-map key as a usage error naming the valid keys; the bootstrap and
// status echo the models; a resume's map reaches the next hand-out.
func TestModelMapThroughTheCLI(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	def := tierDef("p", wf.ModelTier(wf.TierStrong))
	state := t.TempDir() + "/state"
	if code, _, er := singleCLI(def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1", "--harness", "claude", "--model-map", "lite=haiku"); code != 2 || !strings.Contains(er, `key "lite"`) || !strings.Contains(er, "light, standard, strong, top, all") {
		t.Fatalf("run with key lite = %d %q, want a usage error naming the valid keys", code, er)
	}
	code, _, er := singleCLI(def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1", "--harness", "claude", "--model-map", "all=sonnet")
	if code != 0 || !strings.Contains(er, "models: strong=sonnet (--model-map all=sonnet)") {
		t.Fatalf("run = %d, bootstrap:\n%s\nwant the models line", code, er)
	}
	mustSingle(t, def, "wf", "stop", "--state-dir", state, "--reason", "operator")
	if code, _, er := singleCLI(def, "wf", "resume", "--state-dir", state, "--operator", "op", "--reason", "r", "--model-map", "strong.effort=huge"); code != 2 || !strings.Contains(er, "strong.effort") {
		t.Fatalf("resume with a bad effort = %d %q, want a usage error", code, er)
	}
	if code, _, er := singleCLI(def, "wf", "resume", "--state-dir", state, "--operator", "op", "--reason", "r", "--model-map", "strong=haiku"); code != 0 {
		t.Fatalf("resume --model-map = %d %s", code, er)
	}
	if st := mustSingle(t, def, "wf", "status", "--state-dir", state); !strings.Contains(st, "models: strong=haiku (--model-map strong=haiku)") {
		t.Fatalf("status after the resume:\n%s", st)
	}
	if in := handOne(t, def, state); in.Dispatch.Model != "haiku" {
		t.Fatalf("hand-out after the resume spawns %q, want haiku", in.Dispatch.Model)
	}
	if code, _, er := singleCLI(def, "wf", "resume", "--state-dir", state, "--operator", "op", "--reason", "r", "--adopt", "--model-map", "strong=opus"); code != 4 || !strings.Contains(er, "only at a segment boundary") {
		t.Fatalf("resume --adopt --model-map of a running run = %d %q, want refused", code, er)
	}
}

// modelsFan is tieredFan whose child prompt names {{resolvedModels}}.
func modelsFan(name string) *wf.Def {
	c := wf.New("c-models")
	w := wf.NewAgent[chWork, chOut](c)
	ok := wf.NewTerminal[chOK](c, wf.Success)
	bad := wf.NewTerminal[chBad](c, wf.Stopped)
	c.OnFailure(wf.Blocked, bad)
	w.Spec(wf.PromptText("m={{resolvedModels}} do {{item}}", wf.ChildInput()), wf.Retry[chWork](wf.Const(1), w.To(bad, "gave up")))
	toOK, toBad := w.To(ok, "ok"), w.To(bad, "bad")
	w.Route(func(o chOut, _ wf.Ctx) wf.Edge[chWork] {
		if o.OK {
			return toOK
		}
		return toBad
	}, toOK, toBad)
	w.Dispatch(wf.ModelTier(wf.TierStrong))
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)
	d := wf.New(name)
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, wf.Typed[chItem, chRes](c))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(1).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// an earlier revision round 2, #1: a replay never consults the live tier table for what
// the run recorded. A run whose first unit ran on the codex table and whose
// second ran after a resume with a new map replays with no stale prompt, even
// after the table changed; each issue gets its recorded segment's models.
func TestReplayFollowsRecordedSegmentModels(t *testing.T) {
	d := modelsFan("replay-models")
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "c1"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v %v", ins, err)
	}
	first := agentsOf(ins)[0]
	if _, err := e.PromptFetchAs(run, first.Name, claimToken(t, first), segChild("codex", "c1", "kid-1")); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(run, "cheaper"); err != nil {
		t.Fatal(err)
	}
	if err := boundaryEngine(h, d, map[string]string{"strong": "gpt-6.1-terra"}).Resume(run); err != nil {
		t.Fatal(err)
	}
	// The second unit is issued after the boundary, when the first settles.
	if _, err := e.Done(run, first.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	second := runUnit(t, e, run, "codex", "c2")
	if first.Dispatch.Model != "gpt-6.2-astra" || second.Dispatch.Model != "gpt-6.1-terra" {
		t.Fatalf("recorded spawns %q then %q", first.Dispatch.Model, second.Dispatch.Model)
	}
	recs := h.records(run)
	// The replayed journal's issued prompts and fetched prompt hashes, in
	// order.
	type replayed struct{ issued, fetched []string }
	var got replayed
	defer wf.ObserveReplay(func(rs []wf.JournalRecord) {
		got = replayed{}
		for _, r := range rs {
			switch r.Kind {
			case wf.JournalInstructionIssued:
				var d wf.InstructionIssuedData
				if json.Unmarshal(r.Data, &d) == nil {
					got.issued = append(got.issued, d.Prompt)
				}
			case wf.JournalPromptFetched:
				var d wf.PromptFetchedData
				if json.Unmarshal(r.Data, &d) == nil {
					// The shape hash: the full hash names the replay's own
					// temporary run directory.
					got.fetched = append(got.fetched, d.PromptShapeHash)
				}
			}
		}
	})()
	replay := func(recs []wf.JournalRecord) (wf.ReplayReport, replayed) {
		t.Helper()
		rep, err := wf.Replay(recs, d, "")
		if err != nil {
			t.Fatal(err)
		}
		if rep.Stop.Kind != "ended" || len(rep.Differences) != 0 {
			t.Fatalf("replay: %s", rep)
		}
		return rep, got
	}
	before, base := replay(recs)
	if len(base.issued) != 2 || !strings.Contains(base.issued[0], `"strong":"gpt-6.2-astra"`) || !strings.Contains(base.issued[1], `"strong":"gpt-6.1-terra"`) {
		t.Fatalf("replayed issues do not follow the recorded segments: %q", base.issued)
	}
	if len(base.fetched) != 2 {
		t.Fatalf("replayed fetches = %v, want 2", base.fetched)
	}
	// Only the live table changes: nothing replayed moves.
	restore := wf.SetHarnessTierModel("codex", wf.TierStrong, wf.TierModel{Model: "gpt-changed", Effort: "low"})
	after, table := replay(recs)
	restore()
	if len(after.Stale) != len(before.Stale) || !slices.Equal(table.fetched, base.fetched) || !slices.Equal(table.issued, base.issued) {
		t.Fatalf("replay moved with the live table:\nfetched %v -> %v", base.fetched, table.fetched)
	}
	// Only the second segment's binding changes (the reviewer's repro): the
	// first unit, issued, handed out and fetched before the resume, replays
	// exactly as before; the second's fetch follows the changed binding.
	changed := slices.Clone(recs)
	bindings := 0
	for i, r := range changed {
		var co wf.CoordinatorData
		if r.Kind != wf.JournalCoordinator || json.Unmarshal(r.Data, &co) != nil || co.SegmentStart == 0 || co.Models == nil {
			continue
		}
		if bindings++; bindings == 2 {
			co.Models.Models = map[string]string{"strong": "gpt-other"}
			changed[i].Data, _ = json.Marshal(co)
		}
	}
	if bindings != 2 {
		t.Fatalf("%d binding records, want 2", bindings)
	}
	_, other := replay(changed)
	if other.fetched[0] != base.fetched[0] || other.issued[0] != base.issued[0] {
		t.Fatalf("the first unit's replay moved with the second binding: fetched %s -> %s", base.fetched[0], other.fetched[0])
	}
	if other.fetched[1] == base.fetched[1] || !strings.Contains(other.issued[1], `"strong":"gpt-other"`) {
		t.Fatalf("the second unit's replay ignores its segment's binding: %v %q", other.fetched, other.issued[1])
	}
}

// an earlier revision round 2, #2: rewinding one point twice with different maps makes two
// forks, each with its own map; the same map again is the same fork.
func TestRewindForksDifferByModelMap(t *testing.T) {
	d := tieredFan("rewind-maps", 1, wf.TierStrong, 1)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "c1"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v %v", ins, err)
	}
	u := agentsOf(ins)[0]
	if _, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), segChild("codex", "c1", "kid")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{"OK":false}`)}); err != nil {
		t.Fatal(err)
	}
	forks := map[string]wf.RunID{}
	for _, m := range []string{"alpha", "beta", "alpha"} {
		rep, err := boundaryEngine(h, d, map[string]string{"all": m}).Rewind(run, "died")
		if err != nil {
			t.Fatalf("rewind all=%s: %v", m, err)
		}
		if prev, ok := forks[m]; ok && prev != rep.Run {
			t.Fatalf("all=%s again made %s, want %s", m, rep.Run, prev)
		}
		forks[m] = rep.Run
		sm, err := e.ModelsFor(rep.Run, "codex")
		if err != nil || sm.Models["strong"] != m {
			t.Fatalf("fork %s for all=%s spawns %+v, %v", rep.Run, m, sm, err)
		}
	}
	if forks["alpha"] == forks["beta"] {
		t.Fatalf("all=alpha and all=beta share fork %s", forks["alpha"])
	}
}

// an earlier revision round 2, fork issue: a snapshot fork turns its source's coordinator
// records into notes, but keeps each binding's recorded models. A run started
// on codex with all=X that moved to claude with no map (claude defaults),
// forked later, still spawns the claude defaults, not X re-applied.
func TestASnapshotForkKeepsItsHistorysSegmentModels(t *testing.T) {
	d := tieredFan("fork-models", 1, wf.TierStrong, 1, 2, 3)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex", ModelMap: map[string]string{"all": "gpt-x"}})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	runUnit(t, e, run, "codex", "c1")
	if err := e.Stop(run, "move"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	if u := runUnit(t, e, run, "claude", "cl"); u.Dispatch.Model != "opus" {
		t.Fatalf("claude segment spawns %q, want opus", u.Dispatch.Model)
	}
	recs := h.records(run)
	unlock, err := h.j.Lock("fork")
	if err != nil {
		t.Fatal(err)
	}
	last := recs[len(recs)-1]
	for _, r := range recs {
		if r.Kind == wf.JournalCoordinator {
			r = wf.UninheritedCoordinator(r)
		}
		r.Run = "fork"
		if err := h.j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	rewound, _ := json.Marshal(wf.RewoundData{From: run, Seq: last.Seq, State: fmt.Sprintf("snapshot %d", last.Seq)})
	if err := h.j.Append(wf.JournalRecord{Run: "fork", Seq: last.Seq + 1, Kind: wf.JournalRewound, DefHash: last.DefHash, At: last.At, Data: rewound}); err != nil {
		t.Fatal(err)
	}
	unlock()
	sm, err := e.ModelsFor("fork", "claude")
	if err != nil || sm.Models["strong"] != "opus" || sm.From != wf.ModelsFromCarried {
		t.Fatalf("the fork on claude spawns %+v, %v; want the recorded claude defaults carried", sm, err)
	}
}

// an earlier revision round 2, #3: a bad fork map is refused before any side effect: no
// output directory is made, so a retry at the same path is not blocked; and
// fanout --dry-run refuses it too.
func TestABadForkMapIsRefusedBeforeAnySideEffect(t *testing.T) {
	d := tieredFan("fork-bad", 1, wf.TierStrong, 1)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	bad := boundaryEngine(h, d, map[string]string{"lite": "x"})
	out := filepath.Join(t.TempDir(), "fork")
	for name, call := range map[string]func() error{
		"snapshot fork": func() error { _, err := bad.ForkSnapshot(run, 1, out); return err },
		"rewind --out":  func() error { _, err := bad.RewindInto(run, "died", out); return err },
		"rewind":        func() error { _, err := bad.Rewind(run, "died"); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), `key "lite"`) {
			t.Fatalf("%s with key lite = %v", name, err)
		}
		if _, err := os.Lstat(out); !os.IsNotExist(err) {
			t.Fatalf("%s left %s behind", name, out)
		}
	}
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	def := tierDef("p", wf.ModelTier(wf.TierStrong))
	state := filepath.Join(t.TempDir(), "state")
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: def}}}
	if code, _, er := cliCall(cfg, "run", def.Name(), "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1", "--harness", "claude"); code != 0 {
		t.Fatalf("run = %d %s", code, er)
	}
	if code, _, er := cliCall(cfg, "dev", "fanout", "r1", "--state-dir", state, "--out", out, "--parallel", "1", "--coordinator-cmd", `["true"]`, "--dry-run", "--model-map", "lite=x"); code != 2 || !strings.Contains(er, `key "lite"`) {
		t.Fatalf("fanout --dry-run with key lite = %d %q, want a usage error", code, er)
	}
}

// an earlier revision round 2, #4: run --run-id on a live run never ignores a
// --model-map: a bad key is a usage error, another map is refused with the
// stop-and-resume way, and the map in force is accepted.
func TestRunAttachRefusesAModelMapChange(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	def := tierDef("p", wf.ModelTier(wf.TierStrong))
	state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
	run := func(m string) (int, string) {
		code, _, er := singleCLI(def, "run", "--state-dir", state, "--workspace", ws, "--run-id", "r1", "--harness", "claude", "--model-map", m)
		return code, er
	}
	if code, er := run("all=sonnet"); code != 0 {
		t.Fatalf("create = %d %s", code, er)
	}
	if code, er := run("typo=foo"); code != 2 || !strings.Contains(er, `key "typo"`) {
		t.Fatalf("attach with a typo = %d %q", code, er)
	}
	if code, er := run("all=opus"); code != 2 || !strings.Contains(er, "changes only at a segment boundary") || !strings.Contains(er, "resume it with --model-map") {
		t.Fatalf("attach with another map = %d %q", code, er)
	}
	if code, er := run("all=sonnet"); code != 0 {
		t.Fatalf("attach with the map in force = %d %q", code, er)
	}
}

// an earlier revision round 3, #2 (the reviewer's repro): a run started under codex with
// all=chosen whose first coordinator binds claude, forked at its first world
// snapshot, before any hand-out: the fork's history keeps the claude harness
// with its models, so the fork on claude still spawns chosen.
func TestAFirstCheckpointForkKeepsItsHistorysHarnessAndModels(t *testing.T) {
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	d := tieredFan("snap-models", 1, wf.TierStrong, 1, 2)
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunID: "src",
		Harness: "codex", ModelMap: map[string]string{"all": "chosen"},
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "cl"})
	if err != nil || len(agentsOf(ins)) != 1 || agentsOf(ins)[0].Dispatch.Model != "chosen" {
		t.Fatalf("claude hand-out = %+v, %v; want chosen", ins, err)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var first int64
	for _, r := range recs {
		if r.Kind == wf.JournalHandedOut && first == 0 {
			t.Fatal("no world snapshot before the first hand-out")
		}
		if r.Kind == wf.JournalWorldSnapshot {
			first = r.Seq
			break
		}
	}
	fork, err := e.ForkSnapshot(run, first, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	restored := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: fork.Workspace, StateRoot: fork.StateDir, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }}).(*wf.Engine)
	sm, err := restored.ModelsFor(run, "claude")
	if err != nil || sm.Models["strong"] != "chosen" || sm.Harness != "claude" {
		t.Fatalf("the fork on claude spawns %+v, %v; want chosen, carried from its claude history", sm, err)
	}
}

// an earlier revision round 3, #3 (the reviewer's repro): a binding from before
// per-segment maps on another harness than the start's spawns with the
// recorded start Models and Efforts, never the live table.
func TestALegacyCrossHarnessBindingUsesTheRecordedStartModels(t *testing.T) {
	d := tieredFan("legacy-cross", 1, wf.TierStrong, 1)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{})
	unlock, err := h.j.Lock("old")
	if err != nil {
		t.Fatal(err)
	}
	started, _ := json.Marshal(wf.RunStartedData{Def: d.Name(), Harness: "claude", Models: map[string]string{"strong": "old-recorded"}, Efforts: map[string]string{"strong": "high"}})
	bind, _ := json.Marshal(wf.CoordinatorData{Harness: "codex", Session: "c-old", SegmentStart: 1})
	for _, r := range []wf.JournalRecord{
		{Kind: wf.JournalRunStarted, Run: "old", Seq: 1, DefHash: d.Hash(), At: h.now, Data: started},
		{Kind: wf.JournalCoordinator, Run: "old", Seq: 2, DefHash: d.Hash(), At: h.now, Data: bind},
	} {
		if err := h.j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	unlock()
	defer wf.SetHarnessTierModel("codex", wf.TierStrong, wf.TierModel{Model: "gpt-changed", Effort: "low"})()
	ins, err := h.e.(*wf.Engine).HandOutAs("old", 1, wf.Coordinator{Harness: "codex", Session: "c-old"})
	if err != nil || len(agentsOf(ins)) != 1 || agentsOf(ins)[0].Dispatch.Model != "old-recorded" || agentsOf(ins)[0].Dispatch.Effort != "high" {
		t.Fatalf("legacy cross-harness hand-out = %+v, %v; want old-recorded at high", ins, err)
	}
}

type smLoopA struct{}
type smLoopB struct{}
type smLoopOut struct{ Again bool }

// smLoopDef is A -> B -> A (when B says Again) -> B -> end, both strong,
// each prompt naming {{resolvedModels}}.
func smLoopDef(name string) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[smLoopA, smLoopOut](d)
	b := wf.NewAgent[smLoopB, smLoopOut](d)
	end := wf.NewTerminal[dpEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	toB := a.To(b, "next")
	a.Spec(wf.PromptText("A m={{resolvedModels}}"), wf.Retry(wf.Const(1), toB))
	a.Then(toB)
	toA, toEnd := b.To(a, "again"), b.To(end, "done")
	b.Spec(wf.PromptText("B m={{resolvedModels}}"), wf.Retry(wf.Const(1), toEnd))
	b.Route(func(o smLoopOut, _ wf.Ctx) wf.Edge[smLoopB] {
		if o.Again {
			return toA
		}
		return toEnd
	}, toA, toEnd)
	a.Dispatch(wf.ModelTier(wf.TierStrong))
	b.Dispatch(wf.ModelTier(wf.TierStrong))
	return d
}

// replayedOf replays recs and returns the replayed journal's records of kind.
func replayedOf(t *testing.T, recs []wf.JournalRecord, d *wf.Def, kind wf.JournalKind) []wf.JournalRecord {
	t.Helper()
	var got []wf.JournalRecord
	restore := wf.ObserveReplay(func(rs []wf.JournalRecord) {
		got = nil
		for _, r := range rs {
			if r.Kind == kind {
				got = append(got, r)
			}
		}
	})
	defer restore()
	rep, err := wf.Replay(recs, d, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stop.Kind != "ended" || len(rep.Differences) != 0 {
		t.Fatalf("replay: %s", rep)
	}
	return got
}

// withBinding is recs with the n-th binding record's strong model set to m.
func withBinding(t *testing.T, recs []wf.JournalRecord, n int, m string) []wf.JournalRecord {
	t.Helper()
	out := slices.Clone(recs)
	seen := 0
	for i, r := range out {
		var co wf.CoordinatorData
		if r.Kind != wf.JournalCoordinator || json.Unmarshal(r.Data, &co) != nil || co.SegmentStart == 0 || co.Models == nil {
			continue
		}
		if seen++; seen == n {
			co.Models.Models = map[string]string{"strong": m}
			out[i].Data, _ = json.Marshal(co)
			return out
		}
	}
	t.Fatalf("no binding record %d", n)
	return nil
}

// an earlier revision final round, #1 (the reviewer's repro): A is fetched on its first
// visit, the run is stopped and resumed with another model while B runs, and
// A is fetched again on its second visit (attempt numbering restarts per
// visit). Each fetch replays with its own visit's segment: changing only
// binding 2 moves the second A fetch, not the first.
func TestReplayKeysFetchesByTheirVisit(t *testing.T) {
	d := smLoopDef("replay-loop")
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	unit := func(session string, out string, finish bool) wf.Instruction {
		t.Helper()
		ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: session})
		if err != nil || len(agentsOf(ins)) != 1 {
			t.Fatalf("hand-out %+v %v", ins, err)
		}
		u := agentsOf(ins)[0]
		if _, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), segChild("codex", session, "kid-"+u.Name)); err != nil {
			t.Fatal(err)
		}
		if finish {
			if _, err := e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(out)}); err != nil {
				t.Fatal(err)
			}
		}
		return u
	}
	unit("c1", `{"Again":false}`, true) // A, visit 1
	b := unit("c1", "", false)          // B claimed and running
	if err := e.Stop(run, "cheaper"); err != nil {
		t.Fatal(err)
	}
	if err := boundaryEngine(h, d, map[string]string{"strong": "gpt-6.1-terra"}).Resume(run); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Done(run, b.Name, wf.Submission{Output: json.RawMessage(`{"Again":true}`)}); err != nil {
		t.Fatal(err)
	}
	unit("c2", `{"Again":false}`, true) // A, visit 2
	unit("c2", `{"Again":false}`, true) // B, visit 2: done
	recs := h.records(run)
	shapes := func(rs []wf.JournalRecord) (a []string) {
		for _, r := range rs {
			var f wf.PromptFetchedData
			if r.State == "smLoopA" && json.Unmarshal(r.Data, &f) == nil {
				a = append(a, f.PromptShapeHash)
			}
		}
		return a
	}
	base := shapes(replayedOf(t, recs, d, wf.JournalPromptFetched))
	other := shapes(replayedOf(t, withBinding(t, recs, 2, "gpt-other"), d, wf.JournalPromptFetched))
	if len(base) != 2 || len(other) != 2 {
		t.Fatalf("A fetches replayed: %v and %v, want 2 each", base, other)
	}
	if base[0] != other[0] {
		t.Fatalf("A's first-visit fetch moved with binding 2: %s -> %s", base[0], other[0])
	}
	if base[1] == other[1] {
		t.Fatalf("A's second-visit fetch ignores binding 2: %s", base[1])
	}
}

// an earlier revision final round, #2 (the reviewer's repro): an offer made on codex
// strong, stopped unclaimed, resumed with strong=second and offered again,
// then claimed and finished, replays its hand-out with the model the claimed
// hand-out spawned with; the issued record keeps the issue's dispatch.
func TestReplayHandsOutAsTheClaimedHandOutDid(t *testing.T) {
	d := tieredFan("replay-reoffer", 1, wf.TierStrong, 1)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	if ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "c1"}); err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("first offer %+v %v", ins, err)
	}
	if err := e.Stop(run, "move"); err != nil {
		t.Fatal(err)
	}
	if err := boundaryEngine(h, d, map[string]string{"strong": "second"}).Resume(run); err != nil {
		t.Fatal(err)
	}
	if u := runUnit(t, e, run, "codex", "c2"); u.Dispatch.Model != "second" {
		t.Fatalf("the re-offer spawns %q, want second", u.Dispatch.Model)
	}
	handed := replayedOf(t, h.records(run), d, wf.JournalHandedOut)
	if len(handed) == 0 {
		t.Fatal("the replay handed nothing out")
	}
	var last wf.HandedOutData
	if err := json.Unmarshal(handed[len(handed)-1].Data, &last); err != nil || last.Spawn == nil || last.Spawn.Model != "second" {
		t.Fatalf("replayed hand-out = %+v, %v; want model second", last, err)
	}
	issued := replayedOf(t, h.records(run), d, wf.JournalInstructionIssued)
	var is wf.InstructionIssuedData
	if len(issued) != 1 || json.Unmarshal(issued[0].Data, &is) != nil || is.Dispatch.Model != "gpt-6.2-astra" {
		t.Fatalf("replayed issue dispatch = %+v, want the issue's gpt-6.2-astra", is.Dispatch)
	}
}

// an earlier revision landing review (the reviewer's repro): a journal from before
// per-segment maps, whose binding and hand-out records carry no models,
// replays its hand-out with the issued dispatch, not an empty one.
func TestReplayOfALegacyHandOutKeepsTheIssuedDispatch(t *testing.T) {
	d := tieredFan("replay-legacy", 1, wf.TierStrong, 1)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	runUnit(t, h.e.(*wf.Engine), run, "codex", "c1")
	legacy := slices.Clone(h.records(run))
	for i, r := range legacy {
		switch r.Kind {
		case wf.JournalCoordinator:
			var co wf.CoordinatorData
			if json.Unmarshal(r.Data, &co) == nil {
				co.Models = nil
				legacy[i].Data, _ = json.Marshal(co)
			}
		case wf.JournalHandedOut:
			var ho wf.HandedOutData
			if json.Unmarshal(r.Data, &ho) == nil {
				ho.Spawn = nil
				legacy[i].Data, _ = json.Marshal(ho)
			}
		}
	}
	handed := replayedOf(t, legacy, d, wf.JournalHandedOut)
	var ho wf.HandedOutData
	if len(handed) != 1 || json.Unmarshal(handed[0].Data, &ho) != nil || ho.Spawn == nil || ho.Spawn.Model != "gpt-6.2-astra" || ho.Spawn.Effort != "high" {
		t.Fatalf("replayed legacy hand-out = %+v, want the issued gpt-6.2-astra at high", ho.Spawn)
	}
}
