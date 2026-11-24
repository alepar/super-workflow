package wf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordsScope is a root-instance scope for run r with the given RunDir option.
func recordsScope(r RunID, runDir func(RunID) string) scope {
	c := &cycle{
		e:   &Engine{opts: EngineOptions{RunDir: runDir}},
		run: r,
		st:  &runState{started: &RunStartedData{}, insts: map[string]*instance{}},
	}
	return c.scope(&instance{})
}

func TestRecordsResolvesUnderRunDir(t *testing.T) {
	root := t.TempDir()
	s := recordsScope("r1", func(r RunID) string { return filepath.Join(root, "runs", string(r)) })
	want := filepath.Join(root, "runs", "r1", "records")
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("records dir exists before first use: %v", err)
	}

	got, err := s.bindString("{records}/run.md")
	if err != nil || got != want+"/run.md" {
		t.Fatalf("template: got %q, %v; want %q", got, err, want+"/run.md")
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("records dir not created on first use: %v", err)
	}
	argv, err := s.bindArgv([]string{"cat", "{records}/x.json"})
	if err != nil || argv[1] != want+"/x.json" {
		t.Fatalf("argv: got %q, %v", argv, err)
	}
	p, _, err := s.bindPrompt("write to {{records}}", nil)
	if err != nil || p != "write to "+want {
		t.Fatalf("prompt: got %q, %v", p, err)
	}
}

func TestRecordsRelativeRunDirIsMadeAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	s := recordsScope("r1", func(RunID) string { return "state/runs/r1" })
	got, err := s.bindString("{records}")
	if err != nil || !filepath.IsAbs(got) || !strings.HasSuffix(got, "state/runs/r1/records") {
		t.Fatalf("got %q, %v; want an absolute .../state/runs/r1/records", got, err)
	}
}

func TestRecordsWithoutRunDirIsBindError(t *testing.T) {
	for name, runDir := range map[string]func(RunID) string{
		"nil":   nil,
		"empty": func(RunID) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			s := recordsScope("r1", runDir)
			_, err := s.bindString("{records}/run.md")
			if !errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "RunDir") {
				t.Fatalf("template: want an unresolved error naming RunDir, got %v", err)
			}
			_, _, err = s.bindPrompt("{{records}}", nil)
			if !errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "RunDir") {
				t.Fatalf("prompt: want an unresolved error naming RunDir, got %v", err)
			}
		})
	}
}

func TestTemplateDelimiterBytesRemainData(t *testing.T) {
	s := recordsScope("run1", nil)
	const data = "\x1ewf.template:ordinary data\x1f"
	for _, literal := range []bool{false, true} {
		version := 0
		if literal {
			version = effectBindingVersion
		}
		in := EffectData{Kind: "external", BindingVersion: version, Literal: literal, Args: map[string]any{"stdin": data}, Key: data}
		got, err := s.bindEffect(in)
		if err != nil || got.Args["stdin"] != data || got.Key != data {
			t.Fatalf("literal=%v, got %+v, %v, want unchanged delimiter bytes", literal, got, err)
		}
	}
}

func TestRuntimeEffectPreservesPathMarkerBytes(t *testing.T) {
	root := t.TempDir()
	s := recordsScope("run1", func(r RunID) string { return filepath.Join(root, string(r)) })
	from, to := &node{name: "from"}, &node{name: "to"}
	edge := &edgeData{from: from, to: to}
	from.edges = []*edgeData{edge}
	for _, data := range []string{pathTemplatePrefix + "{records}/run.md", "model output: " + pathTemplatePrefix + "{run}"} {
		file := PathText("{records}/run.md")
		effect := ExternalEffect{Adapter: "shell", Op: "run", Args: map[string]any{"stdin": data, "file": file.Raw()}}
		effect = effect.WithTemplateArg(file, "file")
		step, err := s.c.stepOf(s.in, edgeUse{e: edge, effects: []Effect{runtimeEffect{effect}}})
		if err != nil {
			t.Fatalf("plan for %q: %v", data, err)
		}
		plan := step.Effects[0]
		if plan.BindingVersion != effectBindingVersion || plan.Args["stdin"] != data {
			t.Fatalf("plan for %q changed the literal: %+v", data, plan)
		}
		bound, err := s.bindEffect(plan)
		if err != nil || bound.Args["stdin"] != data {
			t.Fatalf("bound effect for %q: %+v, %v", data, bound, err)
		}
		_, marked, err := markedOwnedPath(bound.Args["file"])
		if err != nil || !marked {
			t.Fatalf("typed path did not retain ownership: %v, %v", bound.Args["file"], err)
		}
		got, err := s.c.resolvePathValues(bound.Args["file"])
		if want := filepath.Join(root, "run1", "records", "run.md"); err != nil || got != want {
			t.Fatalf("typed path resolved to %q, %v, want %q", got, err, want)
		}
	}
	legacy, err := s.bindEffect(EffectData{Kind: "external", Args: map[string]any{"file": PathTemplate("{records}/run.md")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, marked, err := markedOwnedPath(legacy.Args["file"]); err != nil || !marked {
		t.Fatalf("legacy plan did not retain path ownership: %v, %v", legacy.Args["file"], err)
	}
}

func TestRuntimeEffectBindsOnlyExplicitlyMarkedText(t *testing.T) {
	s := recordsScope("run1", nil)
	const model = "{run}, {os.Interrupt}, []int{1}"
	s.c.st.started.Params = map[string]string{"model": model}
	in := EffectData{
		Kind:           "external",
		BindingVersion: effectBindingVersion,
		Literal:        true,
		Args: map[string]any{
			"stdin": model,
			"argv":  []string{"{run}", "{model}", model},
		},
		ArgTemplates: []EffectArgTemplate{
			{Path: []string{"argv", "0"}, Spans: Template("{run}").spans},
			{Path: []string{"argv", "1"}, Spans: Template("{model}").spans},
		},
		Key:         "repair/{run}/" + model,
		KeyTemplate: TextOf("repair/", Template("{run}"), "/", model).spans,
	}
	got, err := s.bindEffect(in)
	if err != nil {
		t.Fatal(err)
	}
	argv := got.Args["argv"].([]any)
	if got.Args["stdin"] != model || argv[0] != "run1" || argv[1] != model || argv[2] != model ||
		got.Key != "repair/run1/"+model || got.Literal {
		t.Fatalf("bound effect %+v", got)
	}
	in.ArgTemplates[0].Spans = Template("{missing}").spans
	if _, err := s.bindEffect(in); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("unbound explicit template: %v", err)
	}
}

func TestRecordsWithoutRunDirNeverFallsBackToOutInput(t *testing.T) {
	s := recordsScope("r1", nil)
	ins := []promptInput{{name: "result", value: "OUT-VALUE", out: true}}
	got, _, err := s.bindPrompt("write to {{records}}", ins)
	if !errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "RunDir") {
		t.Fatalf("got %q, %v; want an unresolved error naming RunDir", got, err)
	}
}
