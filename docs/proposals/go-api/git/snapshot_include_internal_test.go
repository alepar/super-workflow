package git

import (
	"os"
	"path/filepath"
	"testing"
)

// The restore writes a declared file only through real directories: a
// parent that is a link (here into .git, which os.Root alone would follow,
// since it resolves inside the workspace) is refused and nothing is written
// through it; an existing file is replaced whole, never truncated in place.
func TestWriteIncludedRefusesLinkedParents(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(".git", "hooks"), filepath.Join(dir, ".superpowers")); err != nil {
		t.Fatal(err)
	}
	if err := writeIncluded(dir, filepath.Join(".superpowers", "install-manifest.json"), []byte("x"), false); err == nil {
		t.Error("a declared file was written through a linked parent")
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, ".git", "hooks")); len(ents) != 0 {
		t.Errorf("the write reached .git/hooks: %v", ents)
	}

	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, ".superpowers"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(other, "kept")
	if err := os.WriteFile(keep, []byte("hard-linked elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(other, ".superpowers", "install-manifest.json")
	if err := os.Link(keep, target); err != nil {
		t.Fatal(err)
	}
	if err := writeIncluded(other, filepath.Join(".superpowers", "install-manifest.json"), []byte("restored"), true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "restored" {
		t.Errorf("target holds %q", got)
	}
	if got, _ := os.ReadFile(keep); string(got) != "hard-linked elsewhere" {
		t.Errorf("the old destination was truncated in place: the other link now reads %q", got)
	}
	if info, _ := os.Stat(target); info == nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode not applied: %v", info)
	}
}

// The capture's read refuses a declared file reached through a linked parent
// that resolves inside the workspace (os.Root alone would follow it).
func TestReadIncludedRefusesLinkedParents(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "install-manifest.json"), []byte("in .git"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git", filepath.Join(dir, ".superpowers")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := readIncluded(dir, filepath.Join(".superpowers", "install-manifest.json")); err != nil || ok {
		t.Errorf("read through a linked parent: ok %v, err %v", ok, err)
	}
}
