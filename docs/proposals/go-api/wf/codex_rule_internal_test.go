package wf

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The preflight's Codex rule check asks about the spawn command exactly as
// next prints it: the unchained form passes with the rule setup-codex-rules
// writes, and a form the rule would not match (a `cd <ws> &&` before it,
// which Codex splits into two commands) fails it, so the preflight cannot
// pass while the real command runs sandboxed.
func TestCodexRuleCheckTakesThePrintedForm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake codex is a shell script")
	}
	root := t.TempDir()
	t.Chdir(root) // no project rules: only the Codex home's count
	home, bin := filepath.Join(root, "codex"), filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A fake `codex execpolicy check --rules F... CMD...` that decides as
	// Codex does for one command: allow when a rule's two-word pattern is
	// CMD's first two words.
	fake := `#!/bin/sh
shift 2
rules=""
while [ "$1" = --rules ]; do rules="$rules $2"; shift 2; done
if cat $rules | grep -qF "pattern = [\"$1\",\"$2\"]"; then echo '{"decision":"allow"}'; else echo '{"matchedRules":[]}'; fi
`
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	env := map[string]string{"CODEX_HOME": home}
	c := &cli{env: func(k string) string { return env[k] }, stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	if code := c.setupCodexRules(true); code != exitOK {
		t.Fatalf("setup-codex-rules --write exited %d", code)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(root, "ws")
	printed := multiVerbLine(exe, filepath.Join(root, "state"), "r1", "spawn", "--claim", "<claim>")
	if why, fix := c.codexRuleCheck(printed); why != "" {
		t.Fatalf("the printed spawn command failed the rule check: %s (fix: %s)", why, fix)
	}
	for _, altered := range []string{"cd " + ws + " && " + printed, "env " + printed} {
		if why, _ := c.codexRuleCheck(altered); !strings.Contains(why, "no Codex rule lets the spawn verb run outside") {
			t.Errorf("a form the rule does not match passed the check (why %q): %s", why, altered)
		}
	}
}

// shellWords splits the forms sink.Quote writes, keeping an operator a word
// of its own.
func TestShellWords(t *testing.T) {
	for in, want := range map[string][]string{
		`a 'b c' d\ e`:              {"a", "b c", "d e"},
		`cd '/w s' && /bin/x spawn`: {"cd", "/w s", "&&", "/bin/x", "spawn"},
		`'' x`:                      {"", "x"},
		`  `:                        nil,
		`a 'open`:                   {"a"},
	} {
		if got := shellWords(in); strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("shellWords(%q) = %q, want %q", in, got, want)
		}
	}
}
