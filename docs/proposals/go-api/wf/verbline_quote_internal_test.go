package wf

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLineArgQuotesValuesNotSlots(t *testing.T) {
	for in, want := range map[string]string{
		"<n>":        "<n>",
		"<file>":     "<file>",
		"a b":        "'a b'",
		"clAsk#1":    "clAsk#1",
		"#x":         "'#x'",
		"x'y":        `'x'\''y'`,
		"a;b":        "'a;b'",
		"<N>":        "'<N>'",
		"<a b>":      "'<a b>'",
		"<>":         "'<>'",
		"--output":   "--output",
		"tokens=<n>": "tokens=<n>",
		"a b=<n>":    "'a b=<n>'",
		"k=<N>":      "'k=<N>'",
	} {
		if got := lineArg(in); got != want {
			t.Errorf("lineArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerbLinesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "argv.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nfor a; do printf '%s\\n' \"$a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := "sw/a b;$(touch PWNED)#1"
	session := name
	lines := map[string]string{
		"wf":    wfLine(script, dir, "run-1", "started", name, "--session", session),
		"multi": multiVerbLine(script, dir, "run-1", "done", name, "--session", session),
	}
	for label, line := range lines {
		cmd := exec.Command("sh", "-c", line)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", label, err, out)
		}
		argv := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		var gotName, gotSession bool
		for i, a := range argv {
			if a == name {
				gotName = true
			}
			if a == session && i > 0 && argv[i-1] == "--session" {
				gotSession = true
			}
		}
		if !gotName || !gotSession {
			t.Errorf("%s: name/session not verbatim single argv elements: %q\nline: %s", label, argv, line)
		}
		if _, err := os.Stat(filepath.Join(dir, "PWNED")); err == nil {
			t.Fatalf("%s: line executed an injected command: %s", label, line)
		}
	}
}

func TestVerbLinePlainNamesUnchanged(t *testing.T) {
	got := multiVerbLine("/bin/w", "/s", "r", "prompt", "clAsk#1")
	if want := "/bin/w prompt r clAsk#1 --state-dir /s"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
