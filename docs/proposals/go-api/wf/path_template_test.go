package wf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func pathTemplateScope(t *testing.T) scope {
	t.Helper()
	workspace, state := t.TempDir(), t.TempDir()
	s := recordsScope("run", func(RunID) string { return filepath.Join(state, "runs/run") })
	s.c.e.opts.StateRoot = state
	s.c.st.started.Workspace = workspace
	s.c.st.started.Params = map[string]string{"name": "one file"}
	return s
}

// A bound value holding # is part of the path: the fragment is the
// template's own, split before binding.
func TestPathTemplateBoundHashStaysInThePath(t *testing.T) {
	s := pathTemplateScope(t)
	s.c.st.started.Params = map[string]string{"name": "issue#12.md"}
	for _, test := range []struct{ template, path, fragment string }{
		{"docs/{name}", "docs/issue#12.md", ""},
		{"docs/{name}#L3", "docs/issue#12.md", "#L3"},
		{"docs/x.md#{name}", "docs/x.md", "#issue#12.md"},
	} {
		value, marked, err := s.bindPathTemplate(PathTemplate(test.template))
		if err != nil || !marked {
			t.Fatalf("bind %q: %v %v", test.template, marked, err)
		}
		p, _, err := markedOwnedPath(value)
		if err != nil || p.Path != test.path || p.Fragment != test.fragment {
			t.Errorf("%q: path %q fragment %q (%v), want %q %q", test.template, p.Path, p.Fragment, err, test.path, test.fragment)
		}
	}
}

func TestPathTemplateOwnershipFragmentsAndForkRoots(t *testing.T) {
	s := pathTemplateScope(t)
	outside := filepath.Join(t.TempDir(), "driver with spaces")
	for _, test := range []struct {
		template       string
		base           PathBase
		path, fragment string
	}{
		{"config/{name}.json#selected", WorkspaceRelative, "config/one file.json", "#selected"},
		{filepath.Join(s.c.resultDir(), "records", "{name}") + "#L10", RunRelative, "records/one file", "#L10"},
		{s.c.workspace(), WorkspaceRelative, ".", ""},
		{outside, externalPathBase, outside, ""},
	} {
		value, marked, err := s.bindPathTemplate(PathTemplate(test.template))
		if err != nil || !marked {
			t.Fatalf("bind %q: %v %v", test.template, marked, err)
		}
		p, marked, err := markedOwnedPath(value)
		if err != nil || !marked || p != (ownedPath{Base: test.base, Path: test.path, Fragment: test.fragment}) {
			t.Fatalf("marker: %+v %v %v", p, marked, err)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var durable any
		if err := json.Unmarshal(raw, &durable); err != nil {
			t.Fatal(err)
		}
		copied, _, err := markedOwnedPath(durable)
		if err != nil {
			t.Fatal(err)
		}
		fork := pathTemplateScope(t)
		root := fork.c.workspace()
		if test.base == RunRelative {
			root = fork.c.resultDir()
		}
		want := filepath.Join(root, test.path)
		if test.base == externalPathBase {
			want = test.path
		}
		want += test.fragment
		got, err := fork.c.resolveOwnedPath(copied)
		if err != nil || got != want {
			t.Fatalf("fork path: %q %v, want %q", got, err, want)
		}
		if !reflect.DeepEqual(value, p.value()) {
			t.Fatal("durable marker mutated")
		}
	}
}

func TestPathTemplateRejectsInvalidAndLeavesOrdinaryStrings(t *testing.T) {
	s := pathTemplateScope(t)
	if PathTemplate("") != "" {
		t.Fatal("empty optional template changed")
	}
	for _, token := range []string{pathTemplatePrefix, PathTemplate("../escape"), PathTemplate("a/../b"), PathTemplate("a//b"), PathTemplate(filepath.Join(s.c.workspace(), "a") + "/../b"), PathTemplate(s.c.e.opts.StateRoot + "/other/file"), "prefix " + PathTemplate("config"), PathTemplate(PathTemplate("config")), PathTemplate("config#" + PathTemplate("nested"))} {
		if _, _, err := s.bindPathTemplate(token); err == nil {
			t.Errorf("accepted %q", token)
		}
	}
	ordinary := filepath.Join(s.c.workspace(), "ordinary") + "#unchanged"
	if got, marked, err := s.bindPathTemplate(ordinary); err != nil || marked || got != ordinary {
		t.Fatalf("ordinary value changed: %v %v %v", got, marked, err)
	}
	token := strings.ReplaceAll(PathTemplate("config/{name}"), "config/", "docs/") + "#heading"
	value, _, err := s.bindPathTemplate(token)
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ := markedOwnedPath(value)
	if p.Path != "docs/one file" || p.Fragment != "#heading" {
		t.Fatalf("template manipulation lost meaning: %+v", p)
	}
	s.c.st.started.Params["name"] = "later"
	if p.Path != "docs/one file" {
		t.Fatal("dynamic input was not frozen")
	}
}

func TestOwnedPathMarkerValidation(t *testing.T) {
	for _, value := range []any{map[string]any{"$wf.path": "bad"}, map[string]any{"$wf.path": map[string]any{"base": "run", "path": "../bad"}}, map[string]any{"$wf.path": map[string]any{"base": "bad", "path": "a"}}, map[string]any{"$wf.path": map[string]any{"base": "run", "path": "a", "fragment": "missing-hash"}}, map[string]any{"$wf.path": map[string]any{"base": "run", "path": "a", "extra": true}}} {
		if _, marked, err := markedOwnedPath(value); !marked || err == nil {
			t.Errorf("invalid marker accepted: %v %v %v", value, marked, err)
		}
	}
	if _, marked, err := markedOwnedPath(map[string]any{"path": "ordinary"}); marked || err != nil {
		t.Fatal("ordinary map interpreted as marker")
	}
}

func TestOwnedPathResolutionRejectsExistingEscapeLink(t *testing.T) {
	s := pathTemplateScope(t)
	source := t.TempDir()
	if err := os.Symlink(source, filepath.Join(s.c.workspace(), "escape")); err != nil {
		t.Fatal(err)
	}
	value, _, err := s.bindPathTemplate(PathTemplate("escape/source.txt#section"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := markedOwnedPath(value)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.c.resolveOwnedPath(p); err == nil {
		t.Fatalf("delivered escaping path: %s", got)
	}
	safe, err := s.c.resolveOwnedPath(ownedPath{Base: WorkspaceRelative, Path: "missing/tail", Fragment: "#section"})
	if err != nil || safe != filepath.Join(s.c.workspace(), "missing/tail")+"#section" {
		t.Fatalf("safe missing tail: %q %v", safe, err)
	}
}

func TestPathTemplateDirectoryTrailingSeparator(t *testing.T) {
	s := pathTemplateScope(t)
	for _, template := range []string{"{records}/", s.c.workspace() + "/"} {
		value, marked, err := s.bindPathTemplate(PathTemplate(template))
		if err != nil || !marked {
			t.Fatalf("directory template %q: %v %v", template, marked, err)
		}
		p, _, err := markedOwnedPath(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.c.resolveOwnedPath(p)
		if err != nil {
			t.Fatal(err)
		}
		want := s.c.workspace()
		if template == "{records}/" {
			want = filepath.Join(s.c.resultDir(), "records")
		}
		if got != want {
			t.Fatalf("directory = %q, want %q", got, want)
		}
	}
}
