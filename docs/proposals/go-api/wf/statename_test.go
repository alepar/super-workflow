package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	snMark[S any]    struct{}
	snOther[S any]   struct{}
	snPair[A, B any] struct{}
	snEnd            struct{}
)

// snDef is one agent whose marker is a generic type instantiated with a
// function-local type (the fork's codeSnapshotBefore[S] shape), then a
// terminal. both adds a second generic marker over the same local type.
func snDef(both bool) *wf.Def {
	type drained struct{}
	d := wf.New("statename")
	a := wf.NewAgent[snMark[drained], eOut](d)
	end := wf.NewTerminal[snEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	if both {
		b := wf.NewAgent[snOther[drained], eOut](d)
		b.Spec(wf.PromptText("other"), wf.Retry[snOther[drained]](wf.Const(1), b.To(end, "x")))
		nb := b.To(end, "done")
		b.Route(func(eOut, wf.Ctx) wf.Edge[snOther[drained]] { return nb }, nb)
	}
	a.Spec(wf.PromptText("work"), wf.Retry[snMark[drained]](wf.Const(1), a.To(end, "x")))
	next := a.To(end, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[snMark[drained]] { return next }, next)
	return d
}

// A generic marker's state is named by its base name and cleaned type
// arguments, never by the compiler's string for a function-local type
// ("snMark[github.com/.../wf_test.drained·1]" cut after its last '.').
func TestGenericMarkerStateNamesAreStable(t *testing.T) {
	type local struct{}
	names := map[string]string{
		wf.StateNameOf[snMark[local]]():                "snMark[local]",
		wf.StateNameOf[snPair[local, snMark[snEnd]]](): "snPair[local,snMark[snEnd]]",
		wf.StateNameOf[snEnd]():                        "snEnd",
		wf.StateNameOf[snMark[*local]]():               "snMark[*local]",
		wf.StateNameOf[snMark[[]local]]():              "snMark[[]local]",
	}
	for got, want := range names {
		if got != want {
			t.Errorf("state name %q, want %q", got, want)
		}
	}
	for _, p := range snDef(false).Lint() {
		t.Errorf("lint: %v", p)
	}
}

// Two marker types that clean to the same name (local types of the same name
// in different functions) are refused at definition time, by name.
func TestCollidingCleanedStateNamesAreRejected(t *testing.T) {
	d := wf.New("collide")
	end := wf.NewTerminal[snEnd](d, wf.Success)
	add := func() {
		type same struct{}
		a := wf.NewAgent[snMark[same], eOut](d)
		a.Spec(wf.PromptText("x"), wf.Retry[snMark[same]](wf.Const(1), a.To(end, "x")))
		n := a.To(end, "done")
		a.Route(func(eOut, wf.Ctx) wf.Edge[snMark[same]] { return n }, n)
	}
	add()
	func() {
		type same struct{}
		a := wf.NewAgent[snMark[same], eOut](d)
		a.Spec(wf.PromptText("y"), wf.Retry[snMark[same]](wf.Const(1), a.To(end, "x")))
		n := a.To(end, "done")
		a.Route(func(eOut, wf.Ctx) wf.Edge[snMark[same]] { return n }, n)
	}()
	found := false
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleDuplicateName && strings.Contains(p.Msg, "snMark[same]") {
			found = true
		}
	}
	if !found {
		t.Fatalf("two markers named snMark[same] were not refused: %v", d.Lint())
	}
}

// snLegacyJournal writes run's journal again under id legacy as an engine
// before stable names recorded it: every name becomes its old last-dot form
// ("drained·7]") and the definition hash an older one's.
func snLegacyJournal(t *testing.T, h *harness, run wf.RunID, legacy wf.RunID, names map[string]string) {
	t.Helper()
	unlock, err := h.j.Lock(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, r := range h.records(run) {
		raw, state := string(r.Data), r.State
		for now, old := range names {
			raw = strings.ReplaceAll(raw, now, old)
			if state == now {
				state = old
			}
		}
		r.Data, r.State, r.Run, r.Seq, r.DefHash = json.RawMessage(raw), state, legacy, 0, "sha256:legacy-build"
		if err := h.j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
}

// A journal recorded with legacy names is carried onto the stable names by
// an explicit adoption: the rename is in the migrated record, and the run
// goes on to its end. Without the adoption, a replay or a rewind of it is
// refused with what to do, not a confusing state-not-found.
func TestAdoptionMapsLegacyStateNames(t *testing.T) {
	d := snDef(false)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if _, err := h.e.(*wf.Engine).HandOutAs(run, 0, wf.Coordinator{}); err != nil {
		t.Fatal(err)
	}
	legacy := wf.RunID(string(run) + "-legacy")
	snLegacyJournal(t, h, run, legacy, map[string]string{"snMark[drained]": "drained·7]"})
	e := h.e.(*wf.Engine)
	if _, err := e.Rewind(legacy, "died"); err == nil || !strings.Contains(err.Error(), "recorded with legacy state names") {
		t.Fatalf("rewind of a legacy journal = %v, want the adopt-first refusal", err)
	}
	if _, err := e.PlanFanout(legacy, "distinct"); err == nil || !strings.Contains(err.Error(), "recorded with legacy state names") {
		t.Fatalf("fanout planning of a legacy journal = %v, want the adopt-first refusal", err)
	}
	if _, err := wf.ReplayWith(h.records(legacy), d, "", wf.ReplayOptions{}); err == nil || !strings.Contains(err.Error(), "recorded with legacy state names") {
		t.Fatalf("replay of a legacy journal = %v, want the adopt-first refusal", err)
	}
	adopter := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Adopt: true}).(*wf.Engine)
	adopted, _, err := adopter.AdoptOnly(legacy)
	if err != nil || !adopted {
		t.Fatalf("adopting the legacy journal = %v, %v", adopted, err)
	}
	var m wf.MigratedData
	migrated := h.kind(legacy, wf.JournalMigrated)
	if len(migrated) != 1 || json.Unmarshal(migrated[0].Data, &m) != nil || m.States["drained·7]"] != "snMark[drained]" {
		t.Fatalf("migrated = %+v; want the rename drained·7] -> snMark[drained]", m)
	}
	if _, err := e.Rewind(legacy, "died"); err != nil && strings.Contains(err.Error(), "legacy state names") {
		t.Fatalf("rewind after the adoption still refused as legacy: %v", err)
	}
	st := h.drive(legacy, map[string][]string{"snMark[drained]": {`{}`}})
	if st.Phase != wf.RunEnded {
		t.Fatalf("after the adoption the run = %+v, want ended", st)
	}
}

// The fork's shape (codeSnapshotBefore[S] over a local type per path): one
// generic marker over several local types, each legacy name carried to its
// own state, one to one.
func TestAdoptionMapsEachLegacyPathOneToOne(t *testing.T) {
	type first struct{}
	type second struct{}
	build := func() *wf.Def {
		d := wf.New("statename-paths")
		a := wf.NewAgent[snMark[first], eOut](d)
		b := wf.NewAgent[snMark[second], eOut](d)
		end := wf.NewTerminal[snEnd](d, wf.Success)
		d.OnFailure(wf.Blocked, end)
		a.Spec(wf.PromptText("a"), wf.Retry[snMark[first]](wf.Const(1), a.To(end, "x")))
		na := a.To(b, "next")
		a.Route(func(eOut, wf.Ctx) wf.Edge[snMark[first]] { return na }, na)
		b.Spec(wf.PromptText("b"), wf.Retry[snMark[second]](wf.Const(1), b.To(end, "x")))
		nb := b.To(end, "done")
		b.Route(func(eOut, wf.Ctx) wf.Edge[snMark[second]] { return nb }, nb)
		return d
	}
	d := build()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{}`) // first done; the run now waits at second
	if at := h.next(run); len(at) != 1 || at[0].State != "snMark[second]" {
		t.Fatalf("the run is at %+v, want snMark[second]", at)
	}
	legacy := wf.RunID(string(run) + "-legacy")
	snLegacyJournal(t, h, run, legacy, map[string]string{"snMark[first]": "first·3]", "snMark[second]": "second·4]"})
	adopter := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Adopt: true}).(*wf.Engine)
	if adopted, _, err := adopter.AdoptOnly(legacy); err != nil || !adopted {
		t.Fatalf("adopting = %v, %v", adopted, err)
	}
	var m wf.MigratedData
	if recs := h.kind(legacy, wf.JournalMigrated); len(recs) != 1 || json.Unmarshal(recs[0].Data, &m) != nil || m.States["first·3]"] != "snMark[first]" || m.States["second·4]"] != "snMark[second]" {
		t.Fatalf("renames = %v; want each legacy path to its own state", m.States)
	}
}

// An adoption refuses a legacy name that maps to no state, or to several,
// naming the old state and the candidates.
func TestAdoptionRefusesAmbiguousOrMissingLegacyNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		now  *wf.Def
		want string
	}{
		{"ambiguous", snDef(true), "snOther[drained]"},
		{"missing", func() *wf.Def {
			d := wf.New("statename")
			a := wf.NewAgent[snMark[snEnd], eOut](d)
			end := wf.NewTerminal[snEnd](d, wf.Success)
			a.Spec(wf.PromptText("work"), wf.Retry[snMark[snEnd]](wf.Const(1), a.To(end, "x")))
			n := a.To(end, "done")
			a.Route(func(eOut, wf.Ctx) wf.Edge[snMark[snEnd]] { return n }, n)
			return d
		}(), "no state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := snDef(false)
			h := newHarness(t, d, nil)
			run := h.run(d, nil)
			if _, err := h.e.(*wf.Engine).HandOutAs(run, 0, wf.Coordinator{}); err != nil {
				t.Fatal(err)
			}
			legacy := wf.RunID(string(run) + "-legacy")
			snLegacyJournal(t, h, run, legacy, map[string]string{"snMark[drained]": "drained·7]"})
			adopter := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{tc.now}, Adopt: true}).(*wf.Engine)
			if _, _, err := adopter.AdoptOnly(legacy); err == nil || !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "drained·7]") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("adoption = %v; want refused naming drained·7] and %q", err, tc.want)
			}
		})
	}
}

// A generic marker over a package-level type was recorded as its type
// argument's name and the closing bracket ("snEnd]"): it maps too.
func TestAdoptionMapsAPackageLevelLegacyName(t *testing.T) {
	d := wf.New("statename-pkg")
	a := wf.NewAgent[snMark[snEnd], eOut](d)
	end := wf.NewTerminal[snEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	a.Spec(wf.PromptText("work"), wf.Retry[snMark[snEnd]](wf.Const(1), a.To(end, "x")))
	n := a.To(end, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[snMark[snEnd]] { return n }, n)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if _, err := h.e.(*wf.Engine).HandOutAs(run, 0, wf.Coordinator{}); err != nil {
		t.Fatal(err)
	}
	legacy := wf.RunID(string(run) + "-legacy")
	snLegacyJournal(t, h, run, legacy, map[string]string{"snMark[snEnd]": "snEnd]"})
	adopter := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Adopt: true}).(*wf.Engine)
	if adopted, _, err := adopter.AdoptOnly(legacy); err != nil || !adopted {
		t.Fatalf("adopting = %v, %v", adopted, err)
	}
	var m wf.MigratedData
	if recs := h.kind(legacy, wf.JournalMigrated); len(recs) != 1 || json.Unmarshal(recs[0].Data, &m) != nil || m.States["snEnd]"] != "snMark[snEnd]" {
		t.Fatalf("renames = %v; want snEnd] -> snMark[snEnd]", m.States)
	}
}

// snChain is first -> second over generic markers of two local types.
type snFirst struct{}
type snSecond struct{}

func snChain(name string) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[snMark[snFirst], eOut](d)
	b := wf.NewAgent[snMark[snSecond], eOut](d)
	end := wf.NewTerminal[snEnd](d, wf.Stopped)
	d.OnFailure(wf.Blocked, end)
	a.Spec(wf.PromptText("a"), wf.Retry[snMark[snFirst]](wf.Const(1), a.To(end, "x")))
	na := a.To(b, "next")
	a.Route(func(eOut, wf.Ctx) wf.Edge[snMark[snFirst]] { return na }, na)
	b.Spec(wf.PromptText("b"), wf.Retry[snMark[snSecond]](wf.Const(1), b.To(end, "x")))
	nb := b.To(end, "done")
	b.Route(func(eOut, wf.Ctx) wf.Edge[snMark[snSecond]] { return nb }, nb)
	return d
}

var snChainLegacy = map[string]string{"snMark[snFirst]": "snFirst]", "snMark[snSecond]": "snSecond]"}

// adoptLegacy builds the legacy copy of run and adopts it with def.
func adoptLegacy(t *testing.T, h *harness, run wf.RunID, def *wf.Def, names map[string]string, keep func([]wf.JournalRecord) []wf.JournalRecord) (wf.RunID, error) {
	t.Helper()
	legacy := wf.RunID(string(run) + "-legacy")
	src := h.records(run)
	if keep != nil {
		src = keep(src)
	}
	unlock, err := h.j.Lock(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range src {
		raw, state := string(r.Data), r.State
		for now, old := range names {
			raw = strings.ReplaceAll(raw, now, old)
			if state == now {
				state = old
			}
		}
		r.Data, r.State, r.Run, r.Seq, r.DefHash = json.RawMessage(raw), state, legacy, 0, "sha256:legacy-build"
		if err := h.j.Append(r); err != nil {
			unlock()
			t.Fatal(err)
		}
	}
	unlock()
	_, _, err = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{def}, Adopt: true}).(*wf.Engine).AdoptOnly(legacy)
	return legacy, err
}

// Round 2, #1: a renamed state keeps its shape: an agent recorded under the
// legacy name is not adopted as a compute of the new name.
func TestALegacyRenameKeepsItsShape(t *testing.T) {
	d := snDef(false)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if _, err := h.e.(*wf.Engine).HandOutAs(run, 0, wf.Coordinator{}); err != nil {
		t.Fatal(err)
	}
	type drained struct{}
	_ = drained{}
	now := func() *wf.Def {
		type drained struct{}
		d := wf.New("statename")
		c := wf.NewCompute[snMark[drained], eOut](d, func(wf.Ctx) (eOut, error) { return eOut{}, nil })
		end := wf.NewTerminal[snEnd](d, wf.Success)
		n := c.To(end, "done")
		c.Route(func(eOut, wf.Ctx) wf.Edge[snMark[drained]] { return n }, n)
		return d
	}()
	if _, err := adoptLegacy(t, h, run, now, map[string]string{"snMark[drained]": "drained·7]"}, nil); !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "drained·7]") {
		t.Fatalf("adopting an agent as a compute = %v, want refused", err)
	}
}

// Round 2, #2: two recorded legacy states that would both map to the one new
// state are refused, not merged.
func TestTwoLegacyStatesDoNotMapToOne(t *testing.T) {
	d := snChain("statename-many")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{}`)
	h.next(run)
	type Item struct{}
	now := func() *wf.Def {
		d := wf.New("statename-many")
		a := wf.NewAgent[snMark[Item], eOut](d)
		end := wf.NewTerminal[snEnd](d, wf.Stopped)
		d.OnFailure(wf.Blocked, end)
		a.Spec(wf.PromptText("a"), wf.Retry[snMark[Item]](wf.Const(1), a.To(end, "x")))
		n := a.To(end, "done")
		a.Route(func(eOut, wf.Ctx) wf.Edge[snMark[Item]] { return n }, n)
		return d
	}()
	if _, err := adoptLegacy(t, h, run, now, map[string]string{"snMark[snFirst]": "Item·1]", "snMark[snSecond]": "Item·2]"}, nil); !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "Item·1]") || !strings.Contains(err.Error(), "Item·2]") {
		t.Fatalf("two legacy states onto one = %v, want refused naming both", err)
	}
}

// Round 2, #3: a legacy name recorded only in run_started's shape (a run
// that never moved) or only as a transition's target (a crash right after
// it) is renamed too.
func TestALegacyNameOnlyInShapeOrTransitionIsRenamed(t *testing.T) {
	d := snChain("statename-shape")
	t.Run("only run_started", func(t *testing.T) {
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		legacy, err := adoptLegacy(t, h, run, d, snChainLegacy, func(r []wf.JournalRecord) []wf.JournalRecord { return r[:1] })
		if err != nil {
			t.Fatalf("adopting a run that never moved = %v", err)
		}
		var m wf.MigratedData
		if recs := h.kind(legacy, wf.JournalMigrated); len(recs) != 1 || json.Unmarshal(recs[0].Data, &m) != nil || m.States["snFirst]"] != "snMark[snFirst]" {
			t.Fatalf("renames = %v", m.States)
		}
	})
	t.Run("only a transition's target", func(t *testing.T) {
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		h.submit(run, h.next(run)[0], `{}`)
		_, err := adoptLegacy(t, h, run, d, snChainLegacy, func(r []wf.JournalRecord) []wf.JournalRecord {
			for i := len(r) - 1; i >= 0; i-- {
				if r[i].Kind == wf.JournalTransition {
					return r[:i+1] // the crash: nothing after the transition
				}
			}
			t.Fatal("no transition")
			return nil
		})
		if err != nil {
			t.Fatalf("adopting a run whose live state is only a transition's target = %v", err)
		}
	})
}

// Round 2, #6: a nested generic marker's legacy name keeps its nesting
// ("Item·7]]"), and maps to the nested state, not to a flat one over the same
// type ("Item]").
func TestANestedLegacyNameMapsToTheNestedState(t *testing.T) {
	type item struct{}
	build := func() *wf.Def {
		d := wf.New("statename-nested")
		a := wf.NewAgent[snMark[snMark[item]], eOut](d)
		b := wf.NewAgent[snOther[item], eOut](d)
		end := wf.NewTerminal[snEnd](d, wf.Stopped)
		d.OnFailure(wf.Blocked, end)
		a.Spec(wf.PromptText("a"), wf.Retry[snMark[snMark[item]]](wf.Const(1), a.To(end, "x")))
		na := a.To(b, "next")
		a.Route(func(eOut, wf.Ctx) wf.Edge[snMark[snMark[item]]] { return na }, na)
		b.Spec(wf.PromptText("b"), wf.Retry[snOther[item]](wf.Const(1), b.To(end, "x")))
		nb := b.To(end, "done")
		b.Route(func(eOut, wf.Ctx) wf.Edge[snOther[item]] { return nb }, nb)
		return d
	}
	d := build()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.next(run)
	legacy, err := adoptLegacy(t, h, run, d, map[string]string{"snMark[snMark[item]]": "item·7]]", "snOther[item]": "item·7]"}, nil)
	if err != nil {
		t.Fatalf("adopting = %v", err)
	}
	var m wf.MigratedData
	if recs := h.kind(legacy, wf.JournalMigrated); len(recs) != 1 || json.Unmarshal(recs[0].Data, &m) != nil || m.States["item·7]]"] != "snMark[snMark[item]]" {
		t.Fatalf("renames = %v; want item·7]] -> snMark[snMark[item]]", m.States)
	}
}

// Round 2, #4: an ended legacy run cannot be adopted, so it is rewound (not
// refused for its legacy names), and its fork is adopted and driven on.
func TestAnEndedLegacyRunIsRewoundThenItsForkAdopted(t *testing.T) {
	d := snChain("statename-ended")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if st := h.drive(run, map[string][]string{"snMark[snFirst]": {`{}`}, "snMark[snSecond]": {`{}`}}); st.Phase != wf.RunEnded {
		t.Fatalf("source = %+v, want ended", st)
	}
	legacy := wf.RunID(string(run) + "-legacy")
	snLegacyJournal(t, h, run, legacy, snChainLegacy)
	e := h.e.(*wf.Engine)
	rep, err := e.Rewind(legacy, "died")
	if err != nil {
		t.Fatalf("rewind of an ended legacy run = %v, want its fork", err)
	}
	adopter := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Adopt: true}).(*wf.Engine)
	if adopted, _, err := adopter.AdoptOnly(rep.Run); err != nil || !adopted {
		t.Fatalf("adopting the fork = %v, %v", adopted, err)
	}
	if st := h.drive(rep.Run, map[string][]string{"snMark[snSecond]": {`{}`}}); st.Phase != wf.RunEnded {
		t.Fatalf("the adopted fork = %+v, want it driven to its end", st)
	}
}

// Round 2, #5: a replay of an adopted journal reads its pre-adoption records
// under the renamed states: the first state's recorded submission is found.
func TestAReplayReadsPreAdoptionRecordsUnderTheirNewNames(t *testing.T) {
	d := snChain("statename-replay")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{}`)
	h.next(run)
	legacy, err := adoptLegacy(t, h, run, d, snChainLegacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st := h.drive(legacy, map[string][]string{"snMark[snSecond]": {`{}`}}); st.Phase != wf.RunEnded {
		t.Fatalf("the adopted run = %+v, want ended", st)
	}
	rep, err := wf.ReplayWith(h.records(legacy), d, "", wf.ReplayOptions{})
	if err != nil || rep.Stop.Kind != "ended" || rep.Retraced < 2 {
		t.Fatalf("replay = %+v, %v; want it to retrace both transitions to the end", rep, err)
	}
}

// Round 3, (2): a second adoption reads the run's names through the renames
// the first one journaled; the live state, resolved then, is not re-inferred
// (and found ambiguous) from the raw legacy records.
func TestASecondAdoptionKeepsTheFirstOnesRenames(t *testing.T) {
	d := snDef(false)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if _, err := h.e.(*wf.Engine).HandOutAs(run, 0, wf.Coordinator{}); err != nil {
		t.Fatal(err)
	}
	legacy, err := adoptLegacy(t, h, run, d, map[string]string{"snMark[drained]": "drained·7]"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	both := snDef(true)
	adopted, _, err := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{both}, Adopt: true}).(*wf.Engine).AdoptOnly(legacy)
	if err != nil || !adopted {
		t.Fatalf("second adoption = %v, %v; want the first one's rename to stand", adopted, err)
	}
}

type snFinal struct{}

// Round 3, (1): a replay reads a record under its name after every migration
// in journal order: snFirst] -> snMark[snFirst] (adoption), then
// snMark[snFirst] -> snFinal (a declared Migration).
func TestAReplayComposesMigrationsInJournalOrder(t *testing.T) {
	v1 := snChain("statename-chain")
	h := newHarness(t, v1, nil)
	run := h.run(v1, nil)
	h.submit(run, h.next(run)[0], `{}`)
	h.next(run)
	legacy, err := adoptLegacy(t, h, run, v1, snChainLegacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	v2 := wf.New("statename-chain")
	a := wf.NewAgent[snFinal, eOut](v2)
	b := wf.NewAgent[snMark[snSecond], eOut](v2)
	end := wf.NewTerminal[snEnd](v2, wf.Stopped)
	v2.OnFailure(wf.Blocked, end)
	a.Spec(wf.PromptText("a"), wf.Retry[snFinal](wf.Const(1), a.To(end, "x")))
	na := a.To(b, "next")
	a.Route(func(eOut, wf.Ctx) wf.Edge[snFinal] { return na }, na)
	b.Spec(wf.PromptText("b"), wf.Retry[snMark[snSecond]](wf.Const(1), b.To(end, "x")))
	nb := b.To(end, "done")
	b.Route(func(eOut, wf.Ctx) wf.Edge[snMark[snSecond]] { return nb }, nb)
	v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash(), States: map[string]string{"snMark[snFirst]": "snFinal"}})
	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{v2}})
	if _, err := e2.Next(legacy, ""); err != nil {
		t.Fatalf("migrating to v2 = %v", err)
	}
	if n := len(h.kind(legacy, wf.JournalMigrated)); n != 2 {
		t.Fatalf("migrated records = %d, want the adoption and the migration", n)
	}
	rep, err := wf.ReplayWith(h.records(legacy), v2, "", wf.ReplayOptions{})
	if err != nil || rep.Retraced < 1 || rep.Stop.State == "snFinal" || rep.Stop.State == "snMark[snFirst]" {
		t.Fatalf("replay = %+v, %v; want the first state's recorded answer found under snFinal", rep, err)
	}
}

// A fanout plan selects an offer by its state's name as of now: after an
// adoption renamed it, the adopted name selects it and the legacy one no
// longer names a state.
func TestAFanoutPlanSelectsByTheAdoptedName(t *testing.T) {
	d := snChain("statename-fanout")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{}`)
	h.next(run) // offers second
	legacy, err := adoptLegacy(t, h, run, d, snChainLegacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := h.e.(*wf.Engine)
	plan, err := e.PlanFanout(legacy, "snMark[snSecond]")
	if err != nil || len(plan.Targets) != 1 || plan.Targets[0].State != "snMark[snSecond]" || plan.Targets[0].LocalState != "snSecond]" {
		t.Fatalf("planning by the adopted name = %+v, %v; want the offer, under its adopted name, its checkpoint under the recorded one", plan, err)
	}
	if _, err := e.PlanFanout(legacy, "snSecond]"); err == nil || !strings.Contains(err.Error(), "must name one unique") {
		t.Fatalf("planning by the legacy name = %v, want it no longer a state", err)
	}
}
