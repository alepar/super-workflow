package wf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunPathEncodingAndValidation(t *testing.T) {
	for _, invalid := range []string{"", ".", "..", "../outside", "/absolute", "a/../b", "a//b", "a/", "a\\b", "a\x00b"} {
		if _, err := json.Marshal(RunPath(invalid)); err == nil {
			t.Errorf("marshaled invalid path %q", invalid)
		}
	}
	value := RunPath("worktrees/integration")
	raw, err := json.Marshal(value)
	if err != nil || string(raw) != `{"$wf.runPath":"worktrees/integration"}` {
		t.Fatalf("marshal: %s %v", raw, err)
	}
	var restored RunPath
	if err := json.Unmarshal(raw, &restored); err != nil || restored != value {
		t.Fatalf("round trip: %q %v", restored, err)
	}
	for _, invalid := range []string{`"worktrees/integration"`, `{"$wf.runPath":"../bad"}`, `{"$wf.runPath":"ok","extra":1}`, `null`} {
		if err := json.Unmarshal([]byte(invalid), &restored); err == nil {
			t.Errorf("decoded invalid marker %s", invalid)
		}
	}
}

func TestRunPathBindingPreservesEffectIntentAndResolvesRuntimeInputs(t *testing.T) {
	dir := t.TempDir()
	s := recordsScope("run", func(RunID) string { return dir })
	s.in.Input = json.RawMessage(`{"Workdir":{"$wf.runPath":"worktrees/integration"},"Other":"/source/ordinary"}`)
	before := string(s.in.Input)
	want := filepath.Join(dir, "worktrees/integration")
	if got, ok := s.lookup("workdir"); !ok || got != want {
		t.Fatalf("lookup: %q %v", got, ok)
	}
	args := map[string]any{"path": "{workdir}", "direct": RunPath("records/log"), "ordinary": "{other}"}
	effect, err := s.bindEffect(EffectData{Kind: "external", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(effect.Args)
	if strings.Contains(string(raw), dir) || !strings.Contains(string(raw), "$wf.runPath") {
		t.Fatalf("intent lost markers: %s", raw)
	}
	resolved, err := resolveRunPathValue(effect.Args, dir)
	if err != nil {
		t.Fatal(err)
	}
	operational, err := s.bindArgs(args)
	if err != nil || !reflect.DeepEqual(resolved, operational) {
		t.Fatalf("args: %#v vs %#v: %v", resolved, operational, err)
	}
	if operational["path"] != want || operational["ordinary"] != "/source/ordinary" {
		t.Fatalf("operational args: %#v", operational)
	}
	inputs, err := s.inputValues([]Input{ChildInput()})
	if err != nil || len(inputs) != 1 || strings.Contains(inputs[0].value, "$wf.runPath") || !strings.Contains(inputs[0].value, want) {
		t.Fatalf("structured prompt input: %+v %v", inputs, err)
	}
	if string(s.in.Input) != before {
		t.Fatal("binding mutated historical document")
	}
	again, _ := json.Marshal(effect.Args)
	if string(again) != string(raw) {
		t.Fatal("runtime resolution mutated intent")
	}
}

type runPathRuntime struct{ checked, applied map[string]any }

func (r *runPathRuntime) Ops() []string                                         { return []string{"path.write"} }
func (r *runPathRuntime) Read(string, ObservationDesc) (json.RawMessage, error) { return nil, nil }
func (r *runPathRuntime) Check(_ string, e ExternalEffect) (EffectStatus, error) {
	r.checked = e.Args
	return EffectNotDone, nil
}
func (r *runPathRuntime) Apply(_ string, e ExternalEffect) (json.RawMessage, error) {
	r.applied = e.Args
	return nil, nil
}

func TestRunPathRecoveredIntentUsesCurrentRunDirectory(t *testing.T) {
	source, fork := t.TempDir(), t.TempDir()
	original := EffectData{Kind: "external", Adapter: "path", Op: "write", Key: "write", Args: map[string]any{"path": RunPath("worktrees/integration"), "ordinary": filepath.Join(source, "opaque")}}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a process dying after effect_intent, before effect_result. Recovery
	// decodes precisely the durable marker into a fresh engine using the fork root.
	var intent EffectData
	if err := json.Unmarshal(raw, &intent); err != nil {
		t.Fatal(err)
	}
	rt := &runPathRuntime{}
	reg := NewAdapters()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	in := newInstance("", "", New("runpath-recovery"))
	in.Cur = "Write"
	in.Pending = &pendingEdge{Open: 1, OpenEffect: intent}
	c := &cycle{run: "run", dry: true, e: &Engine{reg: reg, opts: EngineOptions{RunDir: func(RunID) string { return fork }, Clock: func() time.Time { return time.Time{} }}}, st: &runState{started: &RunStartedData{}, insts: map[string]*instance{"": in}}, recs: []JournalRecord{{Kind: JournalEffectIntent, Seq: 1}}}
	resolved, err := c.recoverEdgeIntent(in, 1, 0, intent)
	if err != nil || !resolved {
		t.Fatalf("recover: %v %v", resolved, err)
	}
	for _, args := range []map[string]any{rt.checked, rt.applied} {
		if args["path"] != filepath.Join(fork, "worktrees/integration") || args["ordinary"] != filepath.Join(source, "opaque") {
			t.Fatalf("runtime args: %#v", args)
		}
	}
	after, _ := json.Marshal(intent)
	if string(after) != string(raw) {
		t.Fatalf("recovery changed intent: %s -> %s", raw, after)
	}
}

func TestRunPathOfAndSchema(t *testing.T) {
	root := t.TempDir()
	c := recordsScope("run", func(RunID) string { return root })
	ctx := &runCtx{c: c.c, in: c.in}
	got, err := RunPathOf(ctx, filepath.Join(root, "records/spec.md"))
	if err != nil || got != RunPath("records/spec.md") {
		t.Fatalf("RunPathOf: %q %v", got, err)
	}
	for _, bad := range []string{root, filepath.Join(filepath.Dir(root), "outside"), "relative"} {
		if _, err := RunPathOf(ctx, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	schema := outputSchema(reflect.TypeFor[RunPath](), nil)
	if !strings.Contains(schema, `"type": "object"`) || !strings.Contains(schema, runPathMarker) {
		t.Fatalf("schema: %s", schema)
	}
	if !strings.Contains(typeShape(reflect.TypeFor[RunPath]()), runPathMarker) {
		t.Fatal("typeShape omitted wire marker")
	}
}

type runPathSetup struct{}
type runPathAgent struct{}
type runPathDone struct{}
type runPathLayout struct {
	Workdir RunPath
	Nested  []RunPath
}

func TestRunPathDefinitionResolvesStructuredComputedPromptInput(t *testing.T) {
	d := New("runpath-prompt")
	compute := NewCompute[runPathSetup, runPathLayout](d, func(Ctx) (runPathLayout, error) {
		return runPathLayout{RunPath("worktrees/integration"), []RunPath{RunPath("records/spec.md")}}, nil
	})
	agent := NewAgent[runPathAgent, struct{}](d)
	done := NewTerminal[runPathDone](d, Success)
	edge := compute.To(agent, "ready")
	compute.Route(func(runPathLayout, Ctx) Edge[runPathSetup] { return edge }, edge)
	agent.Spec(PromptText("Inspect {{layout}}", As("layout", Out(compute))), Retry(Const(1), agent.To(done, "exhausted")))
	finish := agent.To(done, "done")
	agent.Route(func(struct{}, Ctx) Edge[runPathAgent] { return finish }, finish)
	root := t.TempDir()
	j := MemJournal()
	e := NewEngine(j, NewAdapters(), EngineOptions{RunDir: func(RunID) string { return root }})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("next: %+v %v", ins, err)
	}
	if strings.Contains(ins[0].Prompt, runPathMarker) || !strings.Contains(ins[0].Prompt, filepath.Join(root, "records/spec.md")) || ins[0].Workspace != filepath.Join(root, "worktrees/integration") {
		t.Fatalf("prompt leaked marker or wrong root: %+v", ins[0])
	}
	unlock, err := j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	records, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range records {
		if rec.Kind == JournalAccepted {
			if strings.Contains(string(rec.Data), root) {
				t.Fatalf("computed history resolved prematurely: %s", rec.Data)
			}
			found = strings.Contains(string(rec.Data), runPathMarker)
		}
	}
	if !found {
		t.Fatal("computed history lost RunPath markers")
	}
}

func TestRunPathOfRecordedContextReplaysWithoutLivePaths(t *testing.T) {
	dir := t.TempDir()
	s := recordsScope("run", func(RunID) string { return dir })
	s.in.def = New("record-runpath")
	recorded := RecordedCheck{Name: "path"}
	ctx := &recordingCtx{live: &runCtx{c: s.c, in: s.in}, record: &recorded}
	absolute := filepath.Join(dir, "records/spec.md")
	got, err := RunPathOf(ctx, absolute)
	if err != nil || got != RunPath("records/spec.md") {
		t.Fatalf("record: %q %v", got, err)
	}
	if len(recorded.Incomplete) != 0 || len(recorded.Reads) != 1 {
		t.Fatalf("context: %+v", recorded)
	}
	contexts := &CheckContexts{Version: 1, Checks: []RecordedCheck{recorded}}
	resolved, err := resolveCheckContexts(contexts, JournalRecord{Run: "run", Seq: 1}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	probe, err := newRecordedProbeCtx(s.in.def, resolved.Checks[0].Reads, nil)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := RunPathOf(probe, absolute)
	if err != nil || replayed != got {
		t.Fatalf("replay: %q %v", replayed, err)
	}
}

func TestRunPathEscapedMarkerInStructuredPrompt(t *testing.T) {
	dir := t.TempDir()
	s := recordsScope("run", func(RunID) string { return dir })
	s.in.Input = json.RawMessage(`{"nested":[{"\u0024wf.runPath":"records/spec.md"}],"ordinary":"/source/opaque"}`)
	before := string(s.in.Input)
	inputs, err := s.inputValues([]Input{ChildInput()})
	if err != nil || len(inputs) != 1 {
		t.Fatalf("prompt inputs: %+v %v", inputs, err)
	}
	var rendered struct {
		Nested   []string
		Ordinary string
	}
	if err := json.Unmarshal([]byte(inputs[0].value), &rendered); err != nil {
		t.Fatalf("prompt kept marker object: %s: %v", inputs[0].value, err)
	}
	if len(rendered.Nested) != 1 || rendered.Nested[0] != filepath.Join(dir, "records/spec.md") || rendered.Ordinary != "/source/opaque" {
		t.Fatalf("resolved prompt: %+v", rendered)
	}
	if string(s.in.Input) != before {
		t.Fatal("prompt rendering mutated historical bytes")
	}
	ordinary := json.RawMessage("{ \"ordinary\" : 12 }")
	if got, err := s.promptDocument(ordinary, "prompt input"); err != nil || got != string(ordinary) {
		t.Fatalf("ordinary document changed: %q %v", got, err)
	}
}

func TestRunPathRejectsFilesystemEscapes(t *testing.T) {
	for _, mode := range []string{"ancestor", "target", "root"} {
		t.Run(mode, func(t *testing.T) {
			base, source := t.TempDir(), t.TempDir()
			original := filepath.Join(source, "source.txt")
			if err := os.WriteFile(original, []byte("source unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(base, "run")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			relative := RunPath("escape/source.txt")
			switch mode {
			case "ancestor":
				if err := os.Symlink(source, filepath.Join(root, "escape")); err != nil {
					t.Fatal(err)
				}
			case "target":
				relative = "source.txt"
				if err := os.Symlink(original, filepath.Join(root, "source.txt")); err != nil {
					t.Fatal(err)
				}
			case "root":
				root = filepath.Join(base, "linked-run")
				relative = "source.txt"
				if err := os.Symlink(source, root); err != nil {
					t.Fatal(err)
				}

			}
			if got, err := resolveRunPathValue(relative, root); err == nil {
				t.Errorf("resolved escaping path: %v", got)
			}
			s := recordsScope("run", func(RunID) string { return root })
			if got, err := RunPathOf(&runCtx{c: s.c, in: s.in}, filepath.Join(root, string(relative))); err == nil {
				t.Errorf("marked escaping path: %q", got)
			}
			if data, err := os.ReadFile(original); err != nil || string(data) != "source unchanged" {
				t.Fatalf("source changed: %q %v", data, err)
			}
		})
	}
}

func TestRunPathAllowsSafeMissingTailAndInternalLink(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "actual"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("actual", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []RunPath{"new/dir/file", "alias/new/file"} {
		got, err := resolveRunPathValue(relative, root)
		if err != nil || got != filepath.Join(root, string(relative)) {
			t.Fatalf("safe tail: %v %v", got, err)
		}
	}
	missingRoot := filepath.Join(root, "new-run")
	if _, err := resolveRunPathValue(RunPath("records/file"), missingRoot); err != nil {
		t.Fatalf("safe missing root: %v", err)
	}
}

func TestRunPathAllowsParentAlias(t *testing.T) {
	actual, base := t.TempDir(), t.TempDir()
	alias := filepath.Join(base, "parent")
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(alias, "new-run")
	if got, err := resolveRunPathValue(RunPath("records/new-file"), root); err != nil || got != filepath.Join(root, "records/new-file") {
		t.Fatalf("parent alias: %v %v", got, err)
	}
}
