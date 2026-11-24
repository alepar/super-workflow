package wf

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type projectionItem struct{ ID, Path string }
type projectionMap struct{}
type projectionDone struct{}
type projectionPartial struct{}

func projectionDefinition(source Source[projectionItem]) *Def {
	child := New("projection-child")
	terminal := NewTerminal[projectionDone](child, Success)
	Yield(terminal, func(Ctx) bool { return true })
	d := New("projection-map")
	m := NewMap[projectionMap](d, source, func(i projectionItem) string { return i.ID }, Typed[projectionItem, bool](child))
	done := NewTerminal[projectionDone](d, Success)
	partial := NewTerminal[projectionPartial](d, Stopped)
	m.Join(AllSettled(), m.To(done, "done"), m.To(partial, "partial"))
	return d
}

func TestSourceProjectionPreservesObservationAndProjectsJSON(t *testing.T) {
	original := FromObservation(Observe[[]projectionItem]("test", "items", map[string]any{"filter": "ready"}))
	projected := original.Project("portable paths", func(i projectionItem) projectionItem { i.Path = PathTemplate(i.Path); return i })
	if projected.s.obs != original.s.obs || projected.s.from != original.s.from || projected.s.comp != original.s.comp || projected.s.static != original.s.static {
		t.Fatal("projection replaced source provenance")
	}
	raw := json.RawMessage(`{"ID":"one","Path":"docs/input"}`)
	result, err := projected.s.projectItem(raw)
	if err != nil {
		t.Fatal(err)
	}
	var item projectionItem
	if err := json.Unmarshal(result, &item); err != nil {
		t.Fatal(err)
	}
	if item.ID != "one" || item.Path != PathTemplate("docs/input") {
		t.Fatalf("projected item: %+v", item)
	}
	if _, err := projected.s.projectItem(json.RawMessage(`invalid`)); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	if got, err := original.s.projectItem(raw); err != nil || string(got) != string(raw) {
		t.Fatalf("unprojected source changed: %s %v", got, err)
	}
	before, after := Inspect(projectionDefinition(original)), Inspect(projectionDefinition(projected))
	if !reflect.DeepEqual(before.States[0].Observes, after.States[0].Observes) {
		t.Fatal("projection lost observation descriptor")
	}
}

func TestSourceProjectionPreservesOtherSourceKinds(t *testing.T) {
	d := New("producers")
	agent := NewAgent[runPathAgent, []projectionItem](d)
	compute := NewCompute[runPathSetup, []projectionItem](d, func(Ctx) ([]projectionItem, error) { return nil, nil })
	for _, source := range []Source[projectionItem]{FromValues(projectionItem{ID: "one"}), FromOutput(agent, func(items []projectionItem) []projectionItem { return items }), FromCompute(compute, func(items []projectionItem) []projectionItem { return items })} {
		projected := source.Project("identity", func(i projectionItem) projectionItem { return i })
		if projected.s.from != source.s.from || projected.s.comp != source.s.comp || projected.s.static != source.s.static || !reflect.DeepEqual(projected.s.values, source.s.values) || (projected.s.items == nil) != (source.s.items == nil) {
			t.Fatal("source kind/data changed")
		}
	}
}

func TestSourceProjectionIdentityLintAndBackwardHash(t *testing.T) {
	source := FromValues(projectionItem{ID: "one"})
	baseline := projectionDefinition(source)
	view := Inspect(baseline)
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), "Projection") {
		t.Fatal("unprojected definition gained identity fields")
	}
	projected := projectionDefinition(source.Project("first", func(i projectionItem) projectionItem { return i }))
	renamed := projectionDefinition(source.Project("second", func(i projectionItem) projectionItem { return i }))
	if baseline.Hash() == projected.Hash() || projected.Hash() == renamed.Hash() {
		t.Fatal("projection identity missing from hash")
	}
	if Inspect(projected).States[0].Projection != "first" || !strings.Contains(projected.Render(), "first") {
		t.Fatal("projection name missing from inspect/render")
	}
	deleteProjection := Inspect(projected)
	deleteProjection.States[0].Projection = ""
	deleteProjection.States[0].Source = view.States[0].Source
	if hashView(deleteProjection) != baseline.Hash() {
		t.Fatal("projection changed unrelated definition identity")
	}
	for _, invalid := range []Source[projectionItem]{source.Project("", func(i projectionItem) projectionItem { return i }), source.Project("  ", func(i projectionItem) projectionItem { return i }), source.Project("nil", nil), source.Project("one", func(i projectionItem) projectionItem { return i }).Project("two", func(i projectionItem) projectionItem { return i })} {
		if _, err := invalid.s.projectItem(json.RawMessage(`{}`)); err == nil {
			t.Error("invalid projection executed")
		}
		found := false
		for _, problem := range projectionDefinition(invalid).Lint() {
			if problem.Rule == RuleBuild && strings.Contains(problem.Msg, "projection") {
				found = true
			}
		}
		if !found {
			t.Error("invalid projection declaration not linted")
		}
	}
}
