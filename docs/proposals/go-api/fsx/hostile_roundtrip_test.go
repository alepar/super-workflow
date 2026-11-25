package fsx_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

func hostileJSON(v string) string {
	if utf8.ValidString(v) {
		return v
	}
	b, _ := json.Marshal(v)
	var out string
	_ = json.Unmarshal(b, &out)
	return out
}

// A member named by a hostile key is read back through its RFC 6901 pointer.
func TestHostileReadJSONPointerRoundTrip(t *testing.T) {
	ws := t.TempDir()
	rt := fsx.Runtime(fsx.Config{})
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			key := hostileJSON(in.Value)
			doc, _ := json.Marshal(map[string]any{key: map[string]string{key: "found"}})
			if err := os.WriteFile(filepath.Join(ws, "doc.json"), doc, 0o644); err != nil {
				t.Fatal(err)
			}
			tok := strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
			raw, err := rt.Read(ws, fsx.ReadJSON[string]("doc.json", "/"+tok+"/"+tok).Desc())
			if err != nil {
				t.Fatalf("pointer to member %q: %v", key, err)
			}
			if string(raw) != `"found"` {
				t.Errorf("pointer to member %q read %s", key, raw)
			}
		})
	}
}

// Glob lists a file under its own name and path.
func TestHostileGlobNamesRoundTrip(t *testing.T) {
	rt := fsx.Runtime(fsx.Config{})
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			v := in.Value
			if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/\x00") {
				t.Skip("not a file name")
			}
			ws := t.TempDir()
			if err := os.MkdirAll(filepath.Join(ws, "d"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, "d", v), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			raw, err := rt.Read(ws, fsx.Glob("d", "*").Desc())
			if err != nil {
				t.Fatal(err)
			}
			var files []fsx.File
			_ = json.Unmarshal(raw, &files)
			want := hostileJSON(v)
			if len(files) != 1 || files[0].Name != want || files[0].Path != filepath.Join("d", want) {
				t.Errorf("glob of file %q = %+v", want, files)
			}
		})
	}
}

// A relative path from the definition resolves against the workspace as
// written, .. included: a definition's paths are its author's choice, and
// values from model output go through sink.Under before they become paths.
// The probe file sits where the path lands, inside the test's own directory.
func TestHostileRelativePathsResolveAsWritten(t *testing.T) {
	rt := fsx.Runtime(fsx.Config{})
	base := t.TempDir()
	ws := filepath.Join(base, "w1", "w2")
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			v := hostileJSON(in.Value)
			if v == "" || strings.ContainsRune(v, 0) {
				t.Skip("names no file")
			}
			target := filepath.Join(ws, v)
			if !strings.HasPrefix(target, base+"/") {
				t.Fatalf("%q would leave the test's own directory", v)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
				t.Skipf("cannot place a probe file there: %v", err)
			}
			raw, err := rt.Read(ws, fsx.Exists(v).Desc())
			if err != nil || string(raw) != "true" {
				t.Errorf("Exists(%q) = %s, %v; want true for %q", v, raw, err, target)
			}
		})
	}
}
