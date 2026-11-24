package wf

import (
	"path/filepath"
	"testing"
)

type pathMapSeed struct{}
type pathMapFan struct{}
type pathMapWork struct{}
type pathMapEnd struct{}
type pathMapItem struct {
	ID  string
	Ref Ref[string]
}

func TestPathMapPredicateReceivesResolvedResult(t *testing.T) {
	workspace, runDir := t.TempDir(), t.TempDir()
	want := filepath.Join(runDir, "records", "result.md")
	child := New("path-map-result")
	end := NewTerminal[pathMapEnd](child, Success)
	Yield(end, func(Ctx) Ref[string] { return Ref[string]{Path: want} })
	d := New("path-map-predicate")
	m := NewMap[pathMapFan](d, FromValues("one"), func(s string) string { return s }, Typed[string, Ref[string]](child))
	finish := NewTerminal[pathMapEnd](d, Success)
	seen := false
	m.Join(JoinWhen("resolved result", func(result MapResult[string, Ref[string]]) bool {
		if item, ok := result.Items["one"]; ok {
			seen = true
			if item.Result.Path != want {
				t.Errorf("predicate path = %q, want %q", item.Result.Path, want)
			}
			return true
		}
		return false
	}), m.To(finish, "met"), m.To(finish, "partial"))
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{RunID: "r", Workspace: workspace, RunDir: func(RunID) string { return runDir }})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("predicate never received the settled child result")
	}
}

func pathMapActiveEngine(t *testing.T, parallel int, lock bool) (*Engine, RunID) {
	t.Helper()
	workspace, runDir := t.TempDir(), t.TempDir()
	child := New("path-map-worker")
	work := NewAgent[pathMapWork, string](child)
	end := NewTerminal[pathMapEnd](child, Success)
	done := work.To(end, "done")
	work.Spec(PromptText("Work."), Retry(Const(1), done))
	work.Route(func(string, Ctx) Edge[pathMapWork] { return done }, done)
	Yield(end, func(Ctx) string { return "done" })
	d := New("path-map-active")
	seed := NewCompute[pathMapSeed, []pathMapItem](d, func(Ctx) ([]pathMapItem, error) {
		return []pathMapItem{{ID: "one", Ref: Ref[string]{Path: filepath.Join(runDir, "records", "one")}}, {ID: "two", Ref: Ref[string]{Path: filepath.Join(runDir, "records", "two")}}}, nil
	})
	m := NewMap[pathMapFan](d, FromCompute(seed, func(items []pathMapItem) []pathMapItem { return items }), func(item pathMapItem) string { return item.ID }, Typed[pathMapItem, string](child))
	finish := NewTerminal[pathMapEnd](d, Success)
	m.Parallel(parallel).Join(AllOK(), m.To(finish, "met"), m.To(finish, "partial"))
	if lock {
		m.ItemWrites(func(pathMapItem) []string { return []string{"shared/output"} })
	}
	ready := seed.To(m, "ready")
	seed.Route(func([]pathMapItem, Ctx) Edge[pathMapSeed] { return ready }, ready)
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{RunID: "r", Workspace: workspace, RunDir: func(RunID) string { return runDir }}).(*Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return e, run
}

func TestPathMapItemWritesSurvivesTypedInput(t *testing.T) {
	e, run := pathMapActiveEngine(t, 2, true)
	if _, err := e.HandOut(run, 2); err != nil {
		t.Fatal(err)
	}
	if err := e.withRun(run, func(c *cycle) error {
		root := c.st.insts[""]
		kids := c.entryKids(root, root.node())
		if len(kids) != 1 || kids[0].Key != "one" {
			t.Fatalf("spawned children = %+v; want only one holding ItemWrites", kids)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPathMapStatusIncludesHeldTypedItem(t *testing.T) {
	e, run := pathMapActiveEngine(t, 1, false)
	if _, err := e.HandOut(run, 2); err != nil {
		t.Fatal(err)
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
		t.Fatalf("held rows = %d; want one: %+v", held, status.Units)
	}
}
