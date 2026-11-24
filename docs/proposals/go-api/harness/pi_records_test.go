package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPiRootIsSharedWithPiFiles(t *testing.T) {
	home := t.TempDir()
	env := envMap(map[string]string{"HOME": home, "PI_CODING_AGENT_DIR": "~/agent", "PI_SESSION_ID": "s1"})
	dir := filepath.Join(home, "agent", "sessions", "--w--")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "2026_s1.jsonl")
	if err := os.WriteFile(file, []byte(`{"type":"session","id":"s1","cwd":"/w"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := piFiles(For("pi").Caller(env).Root, "s1")
	if len(got) != 1 || got[0] != file {
		t.Fatalf("piFiles = %v, want [%s]", got, file)
	}
}
