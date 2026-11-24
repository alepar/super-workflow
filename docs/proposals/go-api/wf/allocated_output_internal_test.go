package wf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllocatedUnitOutputEncoding(t *testing.T) {
	root := t.TempDir()
	c := &cycle{e: &Engine{opts: EngineOptions{RunDir: func(RunID) string { return root }}}}
	seen := map[string]string{}
	for _, name := range []string{"map/../work#1", "map/%2E%2E/work#1", "map/./work#1", "map/%2E/work#1", `map/..\work#1`, "map/a/b/work#1", "map/a%2Fb/work#1", "map//work#1", "map/%EMPTY/work#1", "map/\x00/work#1", "map/a b/work#1", "map/a b/work#2"} {
		p, err := c.legacyOutputPath(name)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(filepath.Join(root, "out"), p)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			t.Fatalf("escaped output root: %s", p)
		}
		if old, ok := seen[p]; ok {
			t.Fatalf("%q aliases %q at %s", name, old, p)
		}
		seen[p] = name
		again, err := c.legacyOutputPath(name)
		if err != nil || again != p {
			t.Fatalf("unstable path %q != %q: %v", again, p, err)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("placeholder for %s: %v", name, err)
		}
	}
}

// Catch oversized directory components and the final component's .md suffix,
// as well as compaction that discards a distinct suffix of the logical name.
func TestAllocatedOutputLongComponents(t *testing.T) {
	root := t.TempDir()
	c := &cycle{e: &Engine{opts: EngineOptions{RunDir: func(RunID) string { return root }}}}
	seen := map[string]string{}
	for _, name := range []string{
		"map/" + strings.Repeat(".", 86) + "/work#1",
		"map/" + strings.Repeat(".", 86) + "/work#2",
		"map/" + strings.Repeat("界", 30) + "/work#1",
		"map/" + strings.Repeat("界", 30) + "x/work#1",
		"map/" + strings.Repeat("%2E", 86) + "/work#1",
		"map/" + strings.Repeat("a", 255) + "/work#1",
		"map/" + strings.Repeat("a", 252),
		"map/" + strings.Repeat("a", 253),
		"map/" + strings.Repeat("a", 254),
		"map/%SHA256-ordinary/work#1",
	} {
		t.Run(name, func(t *testing.T) {
			p, err := c.legacyOutputPath(name)
			if err != nil {
				t.Fatal(err)
			}
			rel, err := filepath.Rel(filepath.Join(root, "out"), p)
			if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
				t.Fatalf("escaped output root: %s", p)
			}
			for _, part := range strings.Split(rel, string(filepath.Separator)) {
				if len(part) > 255 {
					t.Fatalf("component has %d bytes: %s", len(part), p)
				}
			}
			if old, ok := seen[p]; ok {
				t.Fatalf("%q aliases %q at %s", name, old, p)
			}
			seen[p] = name
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("unexpected result file: %v", err)
			}
			if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
				t.Fatal(err)
			}
			again, err := c.legacyOutputPath(name)
			if err != nil || again != p {
				t.Fatalf("unstable path %q != %q: %v", again, p, err)
			}
			raw, err := os.ReadFile(again)
			if err != nil || string(raw) != name {
				t.Fatalf("read allocated result: %q, %v", raw, err)
			}
			// Existing usable paths retain their spelling, including boundary sizes.
			if strings.Contains(name, strings.Repeat("a", 255)+"/") || name == "map/"+strings.Repeat("a", 252) {
				if p != filepath.Join(root, "out", strings.ReplaceAll(name, "#", "%23")+".md") {
					t.Fatalf("changed existing path: %s", p)
				}
			}
		})
	}
}

// A unit whose hand-out recorded no output path (a journal written before
// hand-outs recorded one) keeps the legacy spelling on resume; one that
// recorded a path resolves to it.
func TestOutputPathFollowsTheRecordedSpelling(t *testing.T) {
	root := t.TempDir()
	st := &runState{insts: map[string]*instance{}}
	st.initIndex()
	c := &cycle{e: &Engine{opts: EngineOptions{RunDir: func(RunID) string { return root }}}, st: st}
	add := func(id, name, output string) {
		st.insts[id] = &instance{ID: id, Unit: &unit{Name: name, State: UnitHanded, Output: output}}
		st.names[name] = unitRef{Instance: id}
	}
	add("a", "map/a.b/work#1", "")
	add("b", "map/c.d/work#1", slugOutput("map/c.d/work#1"))
	legacy, err := c.legacyOutputPath("map/a.b/work#1")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.outputPath("map/a.b/work#1"); err != nil || got != legacy || !strings.Contains(got, "%2E") {
		t.Errorf("legacy unit resolves to %q (%v), want its recorded-less legacy path %q", got, err, legacy)
	}
	want := filepath.Join(root, "out", slugOutput("map/c.d/work#1"))
	if got, err := c.outputPath("map/c.d/work#1"); err != nil || got != want {
		t.Errorf("recorded unit resolves to %q (%v), want %q", got, err, want)
	}
	if s := slugOutput("map/c.d/work#1"); !strings.HasPrefix(s, "map-c.d-work-1-") || strings.ContainsAny(s, "/#%") {
		t.Errorf("slug %q", s)
	}
}
