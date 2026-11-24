package wf

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

type portableSeed struct{}
type portableSub struct{}
type portableMap struct{}
type portableEnd struct{}
type portableItem struct {
	Ref  Ref[string]
	File string
}

func TestPortablePathsThroughComputeSubAndMap(t *testing.T) {
	for _, mapChild := range []bool{false, true} {
		name := "sub"
		if mapChild {
			name = "map"
		}
		t.Run(name, func(t *testing.T) {
			workspace, state := t.TempDir(), t.TempDir()
			runDir := filepath.Join(state, "runs", "r")
			want := filepath.Join(runDir, "records", "spec.md")
			seen := 0
			child := New("portable-child")
			end := NewTerminal[portableEnd](child, Success)
			Yield(end, func(c Ctx) portableItem {
				item := InputOf[portableItem](c)
				if item.Ref.Path != want || item.File != want {
					t.Errorf("child input: %+v", item)
				}
				seen++
				item.File = PathTemplate(item.File)
				return item
			})
			d := New("portable-" + name)
			seed := NewCompute[portableSeed, []portableItem](d, func(Ctx) ([]portableItem, error) {
				return []portableItem{{Ref: Ref[string]{Path: want, Hash: "retained"}, File: PathTemplate(want)}}, nil
			})
			var next State
			if mapChild {
				source := FromCompute(seed, func(items []portableItem) []portableItem {
					for i := range items {
						items[i].File = PathTemplate(items[i].File)
					}
					return items
				})
				m := NewMap[portableMap, portableItem, string, portableItem](d, source, func(portableItem) string { return "one" }, Typed[portableItem, portableItem](child))
				finish := NewTerminal[portableEnd](d, Success)
				m.Join(AllOK(), m.To(finish, "met"), m.To(finish, "partial"))
				next = m
			} else {
				sub := NewSub[portableSub, portableItem, portableItem](d, Typed[portableItem, portableItem](child), func(c Ctx) portableItem {
					items := ComputedOf(c, seed)
					if len(items) != 1 {
						t.Fatalf("computed decode: %+v", items)
					}
					items[0].File = PathTemplate(items[0].File)
					return items[0]
				})
				finish := NewTerminal[portableEnd](d, Success)
				sub.Exits(OnExit[portableEnd](sub.To(finish, "done")))
				next = sub
			}
			edge := seed.To(next, "ready")
			seed.Route(func([]portableItem, Ctx) Edge[portableSeed] { return edge }, edge)
			journal := MemJournal()
			engine := NewEngine(journal, NewAdapters(), EngineOptions{RunID: "r", Workspace: workspace, StateRoot: state, RunDir: func(RunID) string { return runDir }})
			run, err := engine.Run(d, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Next(run, ""); err != nil {
				t.Fatal(err)
			}
			if seen != 1 {
				t.Fatalf("child yields=%d", seen)
			}
			unlock, err := journal.Lock(run)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			records, err := journal.Records(run)
			if err != nil {
				t.Fatal(err)
			}
			if err := snapshotRelocatable(records, PathRoots{Workspace: workspace, State: state}); err != nil {
				t.Fatal(err)
			}
			for _, rec := range records {
				if rec.Kind == JournalAccepted || rec.Kind == JournalChildSpawned || rec.Kind == JournalChildSettled {
					if strings.Contains(string(rec.Data), runDir) || strings.Contains(string(rec.Data), pathTemplatePrefix) {
						t.Fatalf("unportable record: %s", rec.Data)
					}
				}
			}
		})
	}
}

func TestPathTemplateBindingAndPromptProse(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	runDir := filepath.Join(state, "runs", "r")
	d := New("template-bind")
	c := &cycle{run: "r", e: &Engine{opts: EngineOptions{StateRoot: state, RunDir: func(RunID) string { return runDir }}}, st: &runState{started: &RunStartedData{Workspace: workspace, Params: map[string]string{"round": "2"}}}}
	s := c.scope(&instance{def: d})
	for _, prefix := range []string{"", "./", "././"} {
		got, err := s.bindString(PathTemplate(prefix + "docs/spec.md"))
		if err != nil || got != filepath.Join(workspace, "docs/spec.md") {
			t.Fatalf("relative root %q: %q %v", prefix, got, err)
		}
	}
	if _, err := s.bindString(PathTemplate("./../escape")); err == nil {
		t.Fatal("dot prefix hid parent traversal")
	}
	value := PathTemplate("{records}/review-{round}.json") + "#/lane"
	argv, err := s.bindArgv([]string{value, "ordinary"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(argv)
	if strings.Contains(string(raw), runDir) || !strings.Contains(string(raw), ownedPathMarker) {
		t.Fatalf("argv=%s", raw)
	}
	forkRun := t.TempDir()
	c.e.opts.RunDir = func(RunID) string { return forkRun }
	c.st.started.Params["round"] = "99"
	resolved, err := c.resolvePathValues(argv)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(forkRun, "records/review-2.json") + "#/lane"
	if resolved.([]any)[0] != want {
		t.Fatalf("rebound dynamic suffix: %v", resolved)
	}
	prose, err := s.inputValues([]Input{Bound("instructions", "Read "+PathTemplate("{records}/spec.md")+", then report."), Bound("location", PathTemplate("{workspace}/source")+", branch main")})
	if err != nil {
		t.Fatal(err)
	}
	if prose[0].value != "Read "+filepath.Join(forkRun, "records/spec.md")+", then report." || prose[1].value != filepath.Join(workspace, "source")+", branch main" {
		t.Fatalf("prose=%+v", prose)
	}
}
