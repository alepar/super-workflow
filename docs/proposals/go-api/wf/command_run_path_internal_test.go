package wf

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestCommandArgvPreservesOnlyWholeTypedArguments(t *testing.T) {
	root := t.TempDir()
	s := recordsScope("run", func(RunID) string { return root })
	s.in.Input = json.RawMessage(`{"Spec":{"$wf.runPath":"records/spec.md"},"Opaque":"/original/literal"}`)
	args, err := s.bindArgv([]string{"cat", "{spec}", "{opaque}", "--file={spec}"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var values []any
	if err = json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	marker, ok := values[1].(map[string]any)
	if !ok || marker[runPathMarker] != "records/spec.md" {
		t.Fatalf("typed argv lost: %s", raw)
	}
	if values[2] != "/original/literal" || values[3] != "--file="+filepath.Join(root, "records/spec.md") {
		t.Fatalf("ordinary argument semantics changed: %s", raw)
	}
}

type commandArtifactProducer struct{}

func TestCommandArtifactPathBindingIsScopedToRunOwnedWholeArguments(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	d := New("artifact-binding")
	producer := NewAgent[commandArtifactProducer, string](d)
	artifact := NewArtifact[string](producer, "goal_test.go")
	repo := NewRepoArtifact[string](producer, "repo_test.go", filepath.Join(outside, "repo_test.go"))
	s := recordsScope("run", func(RunID) string { return root })
	s.in.def = d
	path := filepath.Join(root, "records", "goal_test.go")
	s.in.Artifacts = map[string]ArtifactRef{"goal_test.go": {Path: path}}
	token := ArtifactPath(artifact)
	args, err := s.bindArgv([]string{token, "--file=" + token, ArtifactPath(repo)})
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != RunPath("records/goal_test.go") || args[1] != "--file="+path || args[2] != filepath.Join(outside, "repo_test.go") {
		t.Fatalf("argv: %#v", args)
	}
	effect, err := s.bindEffect(EffectData{Kind: "external", Args: map[string]any{"path": token}})
	if err != nil || effect.Args["path"] != path {
		t.Fatalf("external effect binding changed: %+v %v", effect, err)
	}
}
