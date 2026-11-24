package wf

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

type projectedSourceItem struct {
	ID      string
	Path    string
	Write   string
	Wait    bool
	Checked string `pattern:"^safe$"`
}

func (i projectedSourceItem) WaitingOnDeps() bool { return i.Wait }

type projectedSourceRuntime struct{ items []projectedSourceItem }

func (*projectedSourceRuntime) Ops() []string { return []string{"kv.items"} }
func (s *projectedSourceRuntime) Read(string, ObservationDesc) (json.RawMessage, error) {
	return json.Marshal(s.items)
}
func (*projectedSourceRuntime) Apply(string, ExternalEffect) (json.RawMessage, error) {
	panic("unexpected effect")
}
func (*projectedSourceRuntime) Check(string, ExternalEffect) (EffectStatus, error) {
	panic("unexpected check")
}

func projectedRuntimeDef(project func(projectedSourceItem) projectedSourceItem) *Def {
	kid := New("projected-kid")
	child := Typed[projectedSourceItem, rqRes](kid)
	work := NewAgent[rqWork, rqOut](kid)
	end := NewTerminal[rqKEnd](kid, Success)
	edge := work.To(end, "done")
	work.Spec(PromptText("Work"), Retry(Const(1), edge))
	work.Route(func(rqOut, Ctx) Edge[rqWork] { return edge }, edge)
	d := New("projected-source")
	source := FromObservation(Observe[[]projectedSourceItem]("kv", "items", nil)).Project("portable", project)
	fan := NewMap[rqFan, projectedSourceItem, string, rqRes](d, source, func(i projectedSourceItem) string { return i.ID }, child).Requery()
	terminal := NewTerminal[rqEnd](d, Success)
	d.OnFailure(Refused, terminal)
	fan.Parallel(5).ItemWrites(func(i projectedSourceItem) []string { return []string{i.Write} }).Join(AllOK(), fan.To(terminal, "met"), fan.To(terminal, "partial"))
	return d
}

func TestSourceProjectionControlsRequeryWritesWaitingAndChildInput(t *testing.T) {
	unsafe := filepath.Join(t.TempDir(), "source-only.md")
	rt := &projectedSourceRuntime{items: []projectedSourceItem{
		{ID: "a", Path: unsafe, Write: "shared", Wait: true, Checked: "safe"},
		{ID: "b", Path: unsafe, Write: "shared", Wait: false, Checked: "safe"},
		{ID: "d", Path: unsafe, Write: "shared", Wait: true, Checked: "safe"},
	}}
	initial, _ := json.Marshal(rt.items)
	d := projectedRuntimeDef(func(item projectedSourceItem) projectedSourceItem {
		item.ID = "projected-" + item.ID
		item.Path = ""
		item.Write = item.ID
		item.Wait = !item.Wait
		return item
	})
	reg := NewAdapters()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	journal := MemJournal()
	e := NewEngine(journal, reg, EngineOptions{Defs: []*Def{d}}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.HandOut(run, 5)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, in := range first {
		ids[in.Child] = true
	}
	if len(first) != 2 || !ids["rqFan[projected-a]"] || !ids["rqFan[projected-d]"] {
		t.Fatalf("projected keys/writes/readiness not used: %+v", first)
	}
	status, err := e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	if status.Progress.WaitingOnDeps != 1 {
		t.Fatalf("projected waiting: %+v", status.Progress)
	}
	records, err := projectionRuntimeRecords(e, run)
	if err != nil {
		t.Fatal(err)
	}
	observed := false
	for _, record := range records {
		if record.Kind == JournalObserved {
			var data ObservedData
			if err := json.Unmarshal(record.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Desc.Adapter == "kv" {
				observed = true
				if string(data.Value) != string(initial) {
					t.Fatalf("observation changed: %s", data.Value)
				}
			}
		}
		if record.Kind == JournalChildSpawned {
			var data ChildSpawnedData
			if err := json.Unmarshal(record.Data, &data); err != nil {
				t.Fatal(err)
			}
			var item projectedSourceItem
			if err := json.Unmarshal(data.Input, &item); err != nil {
				t.Fatal(err)
			}
			if item.Path != "" || item.Wait || item.Write != item.ID || !strings.HasPrefix(item.ID, "projected-") {
				t.Fatalf("unprojected child input: %s", data.Input)
			}
		}
	}
	if !observed {
		t.Fatal("missing observation receipt")
	}
	if current, _ := json.Marshal(rt.items); string(current) != string(initial) {
		t.Fatal("source mutated")
	}
	rt.items = []projectedSourceItem{{ID: "b", Path: unsafe, Write: "shared", Wait: true, Checked: "safe"}, {ID: "c", Path: unsafe, Write: "shared", Wait: true, Checked: "safe"}}
	second, err := e.HandOut(run, 5)
	if err != nil {
		t.Fatal(err)
	}
	ids = map[string]bool{}
	for _, in := range second {
		ids[in.Child] = true
	}
	if len(second) != 2 || !ids["rqFan[projected-b]"] || !ids["rqFan[projected-c]"] {
		t.Fatalf("requery projected keys: %+v", second)
	}
	status, err = e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	if status.Progress.WaitingOnDeps != 0 {
		t.Fatalf("stale dependency waits: %+v", status.Progress)
	}
	records, err = projectionRuntimeRecords(e, run)
	if err != nil {
		t.Fatal(err)
	}
	withdrawn := map[string]bool{}
	for _, record := range records {
		if record.Kind == JournalWithdrawn {
			withdrawn[record.Instance] = true
		}
	}
	if !withdrawn["rqFan[projected-a]"] || !withdrawn["rqFan[projected-d]"] || len(withdrawn) != 2 {
		t.Fatalf("withdrawn projected keys: %#v", withdrawn)
	}
	// Record the key's absence after cancellation, then return it. The
	// historical presence check must use projected keys too.
	if extra, err := e.HandOut(run, 5); err != nil || len(extra) != 0 {
		t.Fatalf("absent projected key: %+v %v", extra, err)
	}
	rt.items = append(rt.items, projectedSourceItem{ID: "a", Path: unsafe, Write: "shared", Wait: true, Checked: "safe"})
	returned, err := e.HandOut(run, 5)
	if err != nil || len(returned) != 1 || returned[0].Child != "rqFan[projected-a]~2" {
		t.Fatalf("returned projected generation: %+v %v", returned, err)
	}
	if extra, err := e.HandOut(run, 5); err != nil || len(extra) != 0 {
		t.Fatalf("stable projected source readmitted twice: %+v %v", extra, err)
	}
}

func TestSourceProjectionValidationFailureIsReported(t *testing.T) {
	d := projectedRuntimeDef(func(item projectedSourceItem) projectedSourceItem { item.Checked = "invalid"; return item })
	rt := &projectedSourceRuntime{items: []projectedSourceItem{{ID: "a", Checked: "safe"}}}
	reg := NewAdapters()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	journal := MemJournal()
	e := NewEngine(journal, reg, EngineOptions{Defs: []*Def{d}}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	records, err := projectionRuntimeRecords(e, run)
	if err != nil {
		t.Fatal(err)
	}
	refused := false
	for _, record := range records {
		if record.Kind == JournalChildSpawned {
			t.Fatalf("spawned invalid projected input: %s", record.Data)
		}
		if record.Kind == JournalRefused && strings.Contains(string(record.Data), "Checked") {
			refused = true
		}
	}
	if !refused {
		t.Fatal("projected validation failure not reported")
	}
}

func projectionRuntimeRecords(e *Engine, run RunID) ([]JournalRecord, error) {
	var records []JournalRecord
	err := e.withRecords(run, func(value []JournalRecord) { records = value })
	return records, err
}

func TestSourceProjectionTemplateWritesStayHeldAcrossPasses(t *testing.T) {
	d := projectedRuntimeDef(func(item projectedSourceItem) projectedSourceItem {
		item.Write = PathTemplate("{records}/shared.md")
		return item
	})
	rt := &projectedSourceRuntime{items: []projectedSourceItem{{ID: "a", Checked: "safe"}, {ID: "b", Checked: "safe"}}}
	reg := NewAdapters()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	e := NewEngine(MemJournal(), reg, EngineOptions{Defs: []*Def{d}, Workspace: t.TempDir(), RunDir: func(RunID) string { return root }}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.HandOut(run, 5)
	if err != nil || len(first) != 1 {
		t.Fatalf("first shared write: %+v %v", first, err)
	}
	second, err := e.HandOut(run, 5)
	if err != nil || len(second) != 0 {
		t.Fatalf("projected token bypassed active write conflict: %+v %v", second, err)
	}
	status, err := e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, unit := range status.Units {
		if unit.State == "waiting" && unit.Note == "held" {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("shared projected write not held: %+v", status.Units)
	}
}
