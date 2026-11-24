package wf

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

type pathEncodingCustom struct{ Ref Ref[string] }

func (v pathEncodingCustom) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"opaque": v.Ref.Path})
}

type pathEncodingEmbedded struct {
	Ref[string]
	Other string
}
type pathEncodingShadowed struct {
	Ref[string]
	Path string
}

func TestMarshalPathValuePreservesNestedRefsAndJSONSemantics(t *testing.T) {
	workspace, state, outside := t.TempDir(), t.TempDir(), t.TempDir()
	runDir := filepath.Join(state, "runs", "r")
	s := recordsScope("r", func(RunID) string { return runDir })
	s.c.e.opts.StateRoot = state
	s.c.st.started.Workspace = workspace
	path := filepath.Join(runDir, "records", "spec.md")
	ref := Ref[string]{Path: path, Hash: "abc"}
	input := struct {
		Nested   map[int][]*Ref[string] `json:"nested"`
		Relative Ref[string]
		External Ref[string]
		Empty    Ref[string]
		Plain    string
		Custom   pathEncodingCustom
		Embedded pathEncodingEmbedded
		Shadowed pathEncodingShadowed
		Omitted  *Ref[string] `json:"omitted,omitempty"`
		Ignored  Ref[string]  `json:"-"`
		Large    uint64
		Legacy   RunPath
	}{Nested: map[int][]*Ref[string]{7: {&ref, nil}}, Relative: Ref[string]{Path: "spec.md"}, External: Ref[string]{Path: filepath.Join(outside, "driver")}, Plain: path, Custom: pathEncodingCustom{ref}, Embedded: pathEncodingEmbedded{ref, "keep"}, Shadowed: pathEncodingShadowed{ref, "ordinary"}, Ignored: ref, Large: 9007199254740993, Legacy: RunPath("records/legacy")}
	raw, err := s.c.marshalPathValue(s.in, input)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["omitted"]; ok {
		t.Fatal("omitted field added")
	}
	if _, ok := wire["Ignored"]; ok {
		t.Fatal("ignored field added")
	}
	if string(wire["Large"]) != "9007199254740993" {
		t.Fatalf("numeric precision lost: %s", raw)
	}
	if !strings.Contains(string(wire["nested"]), `"$wf.path"`) || !strings.Contains(string(wire["Embedded"]), `"$wf.path"`) {
		t.Fatalf("missing ref markers: %s", raw)
	}
	if strings.Contains(string(wire["Shadowed"]), `"$wf.path"`) || strings.Contains(string(wire["Custom"]), `"$wf.path"`) {
		t.Fatalf("non-ref value annotated: %s", raw)
	}
	var empty Ref[string]
	if err := json.Unmarshal(wire["Empty"], &empty); err != nil || empty.Path != "" {
		t.Fatalf("empty ref: %+v %v", empty, err)
	}
	// Relocate only owned values; strings and external references stay pinned.
	forkWorkspace, forkState := t.TempDir(), t.TempDir()
	s.c.st.started.Workspace = forkWorkspace
	s.c.e.opts.StateRoot = forkState
	s.c.e.opts.RunDir = func(RunID) string { return filepath.Join(forkState, "runs", "r") }
	decoded, err := s.c.decodeOwnedPathValues(raw)
	if err != nil {
		t.Fatal(err)
	}
	restored := input
	restored.Nested = nil
	if err := json.Unmarshal(decoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Nested[7][0].Path != filepath.Join(forkState, "runs", "r", "records", "spec.md") || restored.Nested[7][0].Hash != "abc" || restored.Nested[7][1] != nil {
		t.Fatalf("nested ref: %+v", restored.Nested)
	}
	if restored.Relative.Path != filepath.Join(forkWorkspace, "spec.md") || restored.External.Path != input.External.Path || restored.Plain != path || restored.Legacy != input.Legacy {
		t.Fatalf("wrong resolution: %s", decoded)
	}
	if restored.Embedded.Path != restored.Nested[7][0].Path || restored.Shadowed.Path != "ordinary" {
		t.Fatalf("embedded refs: %s", decoded)
	}
	if input.Nested[7][0].Path != path {
		t.Fatal("source ref mutated")
	}
	ordinary, err := json.Marshal(ref)
	if err != nil || strings.Contains(string(ordinary), "$wf.path") {
		t.Fatalf("outside-engine Ref encoding changed: %s %v", ordinary, err)
	}
}

func TestMarshalPathValueBindsOnlyExplicitTemplates(t *testing.T) {
	runDir := t.TempDir()
	s := recordsScope("r", func(RunID) string { return runDir })
	s.c.st.started.Workspace = t.TempDir()
	s.in.Input = json.RawMessage(`{"Name":"frozen"}`)
	token := PathTemplate("{records}/{name}.md#review")
	input := map[string]any{"explicit": token, "ordinary": "{records}/{name}.md", "ref": Ref[string]{Path: token}, "numeric": json.Number("9007199254740993")}
	raw, err := s.c.marshalPathValue(s.in, input)
	if err != nil {
		t.Fatal(err)
	}
	s.in.Input = json.RawMessage(`{"Name":"changed"}`)
	forkDir := t.TempDir()
	s.c.e.opts.RunDir = func(RunID) string { return forkDir }
	decoded, err := s.c.decodeOwnedPathValues(raw)
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Explicit string
		Ordinary string
		Ref      Ref[string]
		Numeric  json.Number
	}
	if err := json.Unmarshal(decoded, &values); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(forkDir, "records", "frozen.md") + "#review"
	if values.Explicit != want || values.Ref.Path != want || values.Ordinary != "{records}/{name}.md" || values.Numeric.String() != "9007199254740993" {
		t.Fatalf("decoded: %s", decoded)
	}
	if input["explicit"] != token {
		t.Fatal("source token changed")
	}
}

func TestMarshalPathValuePreservesJSONErrorsAndEmptyDecode(t *testing.T) {
	s := recordsScope("r", func(RunID) string { return t.TempDir() })
	if _, err := s.c.marshalPathValue(s.in, make(chan int)); err == nil {
		t.Fatal("unsupported value accepted")
	}
	var cyclic []any
	cyclic = append(cyclic, nil)
	cyclic[0] = cyclic
	if _, err := s.c.marshalPathValue(s.in, cyclic); err == nil {
		t.Fatal("cycle accepted")
	}
	if raw, err := s.c.decodeOwnedPathValues(nil); err != nil || raw != nil {
		t.Fatalf("nil decode: %s %v", raw, err)
	}
}

func TestMarshalPathValueReferenceFragmentsAndTraversal(t *testing.T) {
	root := t.TempDir()
	s := recordsScope("r", func(RunID) string { return root })
	s.c.st.started.Workspace = t.TempDir()
	for _, path := range []string{"../escape.md", "sub/../../escape.md", "sub/../spec.md"} {
		if _, err := s.c.marshalPathValue(s.in, Ref[string]{Path: path}); err == nil {
			t.Errorf("unclean relative ref accepted: %s", path)
		}
	}
	for _, path := range []string{"spec.json#/items/0", filepath.Join(root, "records", "spec.json") + "#/items/0", filepath.Join(t.TempDir(), "external.json") + "#"} {
		raw, err := s.c.marshalPathValue(s.in, Ref[string]{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		owned, ok, err := markedOwnedPath(value["Path"])
		if err != nil || !ok || strings.Contains(owned.Path, "#") || !strings.HasPrefix(owned.Fragment, "#") {
			t.Fatalf("fragment ownership: %s %v", raw, err)
		}
		decoded, err := s.c.decodeOwnedPathValues(raw)
		if err != nil {
			t.Fatal(err)
		}
		var ref Ref[string]
		if err := json.Unmarshal(decoded, &ref); err != nil {
			t.Fatal(err)
		}
		want := path
		if !filepath.IsAbs(want) {
			want = filepath.Join(s.c.workspace(), want)
		}
		if ref.Path != want {
			t.Fatalf("ref = %q, want %q", ref.Path, want)
		}
	}
}

type pathEncodingPointerCustom struct{ R Ref[string] }

func (v *pathEncodingPointerCustom) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"opaque": v.R.Path})
}

func TestMarshalPathValueMatchesPointerMarshalerAddressability(t *testing.T) {
	root := t.TempDir()
	s := recordsScope("r", func(RunID) string { return root })
	s.c.st.started.Workspace = t.TempDir()
	wrapper := pathEncodingPointerCustom{R: Ref[string]{Path: filepath.Join(root, "records", "spec.md")}}
	for _, test := range []struct {
		name   string
		input  any
		marked bool
	}{
		{"value", wrapper, true},
		{"map-value", map[string]pathEncodingPointerCustom{"key": wrapper}, true},
		{"pointer", &wrapper, false},
		{"slice-value", []pathEncodingPointerCustom{wrapper}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := s.c.marshalPathValue(s.in, test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(raw), `"$wf.path"`); got != test.marked {
				t.Fatalf("marked=%v want %v: %s", got, test.marked, raw)
			}
		})
	}
}

func TestDecodeOwnedPathValuesPreservesUnmarkedJSONBytes(t *testing.T) {
	s := recordsScope("r", func(RunID) string { return t.TempDir() })
	raw := json.RawMessage("{\n  \"Z\": 1, \"A\": {\"$wf.runPath\": \"records/spec.md\"}\n}")
	decoded, err := s.c.decodeOwnedPathValues(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(raw) {
		t.Fatalf("unmarked JSON changed:\n%s\nwant:\n%s", decoded, raw)
	}
}
