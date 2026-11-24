package wf

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestResolvePathExplicitLiveContextAndErrors(t *testing.T) {
	s := pathTemplateScope(t)
	s.c.st.started.Params["integrationWorktree"] = "worktrees/integration"
	s.c.st.started.ParamPaths = map[string]PathBase{"integrationWorktree": WorkspaceRelative}
	ctx := &runCtx{c: s.c, in: s.in}
	got, err := ResolvePath(ctx, PathTemplate("{integrationWorktree}"))
	if err != nil || got != filepath.Join(s.c.workspace(), "worktrees/integration") {
		t.Fatalf("resolve: %q %v", got, err)
	}
	if got, err := ResolvePath(nil, ""); got != "" || err != nil {
		t.Fatalf("optional empty: %q %v", got, err)
	}
	for _, plain := range []string{"relative", "/absolute", "before " + PathTemplate("a")} {
		if _, err := ResolvePath(ctx, plain); err == nil {
			t.Errorf("unmarked value accepted: %q", plain)
		}
	}
	if _, err := ResolvePath(ctx, PathTemplate("{absent}")); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("binding error lost: %v", err)
	}
}

func TestResolvePathRecordedTemplatesRemainDistinct(t *testing.T) {
	s := pathTemplateScope(t)
	s.in.def = New("record-resolve")
	record := RecordedCheck{Name: "paths"}
	ctx := &recordingCtx{live: &runCtx{c: s.c, in: s.in}, record: &record}
	tokens := []string{PathTemplate("one/file"), PathTemplate("two/file#section")}
	wants := make([]string, len(tokens))
	for i, token := range tokens {
		got, err := ResolvePath(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		wants[i] = got
	}
	if len(record.Incomplete) != 0 || len(record.Reads) != 2 {
		t.Fatalf("recorded reads: %+v", record)
	}
	if record.Reads[0].Name == record.Reads[1].Name {
		t.Fatal("template identities collapsed")
	}
	context, err := resolveCheckContexts(&CheckContexts{Version: 1, Checks: []RecordedCheck{record}}, JournalRecord{Run: "run", Seq: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := newRecordedProbeCtx(s.in.def, context.Checks[0].Reads, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, token := range tokens {
		got, err := ResolvePath(replay, token)
		if err != nil || got != wants[i] {
			t.Fatalf("replay template %q: %q %v", token, got, err)
		}
	}
}

type resolvePathCompute struct{}
type resolvePathDone struct{}

func TestResolvePathInsideRealCompute(t *testing.T) {
	workspace := t.TempDir()
	d := New("resolve-path-compute")
	d.Param("integrationWorktree").Path(WorkspaceRelative)
	compute := NewCompute[resolvePathCompute, bool](d, func(c Ctx) (bool, error) {
		path, err := ResolvePath(c, PathTemplate("{integrationWorktree}"))
		return path == filepath.Join(workspace, "worktrees/integration"), err
	})
	done := NewTerminal[resolvePathDone](d, Success)
	edge := compute.To(done, "resolved")
	compute.Route(func(bool, Ctx) Edge[resolvePathCompute] { return edge }, edge)
	journal := MemJournal()
	e := NewEngine(journal, NewAdapters(), EngineOptions{Workspace: workspace})
	run, err := e.Run(d, map[string]string{"integrationWorktree": "worktrees/integration"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
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
	found := false
	for _, record := range records {
		if record.Kind == JournalAccepted {
			var accepted AcceptedData
			if err := json.Unmarshal(record.Data, &accepted); err != nil {
				t.Fatal(err)
			}
			found = string(accepted.Output) == "true"
		}
	}
	if !found {
		t.Fatal("compute did not resolve current workspace path")
	}
}
