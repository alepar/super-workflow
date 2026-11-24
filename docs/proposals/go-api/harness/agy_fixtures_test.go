package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agyTableCells returns the cells of each markdown table row in text whose
// first cell, stripped of backticks, satisfies keep.
func agyTableCells(text string, keep func(first string) bool) [][]string {
	var rows [][]string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		var cells []string
		for _, c := range strings.Split(strings.Trim(line, "|"), "|") {
			cells = append(cells, strings.Trim(strings.TrimSpace(c), "`"))
		}
		if keep(cells[0]) {
			rows = append(rows, cells)
		}
	}
	return rows
}

// TestAgyFixtureManifest checks that the synthetic probe manifests every
// file and its digest, and that the edge README names each edge transcript.
func TestAgyFixtureManifest(t *testing.T) {
	probe := filepath.Join(agyFixture)
	readme, err := os.ReadFile(filepath.Join(probe, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]string{}
	for _, r := range agyTableCells(string(readme), func(f string) bool { return strings.HasPrefix(f, "brain/") }) {
		if len(r) != 2 {
			t.Errorf("manifest row needs a fixture path and SHA-256: %v", r)
			continue
		}
		if _, dup := listed[r[0]]; dup {
			t.Errorf("fixture %s listed twice", r[0])
		}
		listed[r[0]] = r[1]
	}
	if len(listed) == 0 {
		t.Fatal("probe/README.md lists no fixture")
	}
	for path, want := range listed {
		b, err := os.ReadFile(filepath.Join(probe, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("listed fixture %s is missing: %v", path, err)
			continue
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s: sha256 %s, README lists %s", path, got, want)
		}
	}
	err = filepath.WalkDir(probe, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(probe, p)
		rel = filepath.ToSlash(rel)
		if _, ok := listed[rel]; !ok && rel != "README.md" {
			t.Errorf("fixture %s has no manifest row", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	edge, err := os.ReadFile(filepath.Join(agyEdgeDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, r := range agyTableCells(string(edge), func(f string) bool { return strings.HasSuffix(f, ".jsonl") }) {
		named[r[0]] = true
	}
	files, err := filepath.Glob(filepath.Join(agyEdgeDir, "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no edge transcripts: %v", err)
	}
	for _, f := range files {
		if !named[filepath.Base(f)] {
			t.Errorf("edge transcript %s is not named in edge/README.md", filepath.Base(f))
		}
	}
	for n := range named {
		if _, err := os.Stat(filepath.Join(agyEdgeDir, n)); err != nil {
			t.Errorf("edge/README.md names %s: %v", n, err)
		}
	}
}
