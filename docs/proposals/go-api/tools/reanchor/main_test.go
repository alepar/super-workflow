package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/tools/excerptcheck"
)

const pkg = "pkg"

// fixture writes pkg/a.go (with the given source) and a document holding one
// excerpt of Keep and one of Edit, anchored to the original line numbers.
func fixture(t *testing.T, source string) (root, doc string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, pkg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, pkg, "a.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	doc = filepath.Join(root, "DOC.md")
	body := "Lines 3-5: Keep.\n\n<!-- src: pkg/a.go#L3-L5 -->\n```go\nfunc Keep() int {\n\treturn 1\n}\n```\n\n" +
		"Lines 7-9: Edit.\n\n<!-- src: pkg/a.go#L7-L9 -->\n```go\nfunc Edit() int {\n\treturn 2\n}\n```\n"
	if err := os.WriteFile(doc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, doc
}

const original = "package pkg\n\nfunc Keep() int {\n\treturn 1\n}\n\nfunc Edit() int {\n\treturn 2\n}\n"

func readDoc(t *testing.T, doc string) string {
	t.Helper()
	b, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUnchangedSourceIsClean(t *testing.T) {
	root, doc := fixture(t, original)
	var out bytes.Buffer
	n, err := run(&out, root, doc, pkg, true, true)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v out=%s", n, err, out.String())
	}
	if got := readDoc(t, doc); !strings.Contains(got, "pkg/a.go#L3-L5") || !strings.Contains(got, "pkg/a.go#L7-L9") {
		t.Fatalf("anchors changed:\n%s", got)
	}
}

func TestMovedBlockIsReanchored(t *testing.T) {
	// Two comment lines inserted above Keep: both blocks shift by 2.
	root, doc := fixture(t, "package pkg\n\n// a\n// b\nfunc Keep() int {\n\treturn 1\n}\n\nfunc Edit() int {\n\treturn 2\n}\n")
	var out bytes.Buffer
	n, err := run(&out, root, doc, pkg, true, true)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v out=%s", n, err, out.String())
	}
	got := readDoc(t, doc)
	for _, want := range []string{"pkg/a.go#L5-L7 -->", "pkg/a.go#L9-L11 -->", "Lines 5-7: Keep.", "Lines 9-11: Edit."} {
		if !strings.Contains(got, want) {
			t.Errorf("document lacks %q:\n%s", want, got)
		}
	}
	if ms := excerptcheck.CheckDoc(root, doc, nil); len(ms) != 0 {
		t.Errorf("rewritten document still mismatches: %+v", ms)
	}
}

func TestMovedBlockWithoutWriteIsOnlyReported(t *testing.T) {
	root, doc := fixture(t, "package pkg\n\n// a\nfunc Keep() int {\n\treturn 1\n}\n\nfunc Edit() int {\n\treturn 2\n}\n")
	before := readDoc(t, doc)
	var out bytes.Buffer
	n, err := run(&out, root, doc, pkg, false, false)
	if err != nil || n == 0 {
		t.Fatalf("a pending move must count as a problem: n=%d err=%v", n, err)
	}
	if !strings.Contains(out.String(), "moved") || readDoc(t, doc) != before {
		t.Fatalf("expected a report only, out=%s", out.String())
	}
}

func TestChangedBlockIsListedStale(t *testing.T) {
	root, doc := fixture(t, strings.Replace(original, "return 2", "return 3", 1))
	var out bytes.Buffer
	n, err := run(&out, root, doc, pkg, true, false)
	if err != nil || n != 1 {
		t.Fatalf("want exactly the changed block, n=%d err=%v out=%s", n, err, out.String())
	}
	if !strings.Contains(out.String(), "stale: pkg/a.go#L7-L9") {
		t.Errorf("stale block not named:\n%s", out.String())
	}
	if !strings.Contains(readDoc(t, doc), "pkg/a.go#L3-L5") {
		t.Errorf("the unchanged block must keep its anchor")
	}
	if strings.Contains(readDoc(t, doc), "return 3") {
		t.Errorf("a stale block must not be rewritten automatically")
	}
}

func TestUncoveredDeclarationIsReported(t *testing.T) {
	root, doc := fixture(t, original+"\nfunc Added() {}\n")
	var out bytes.Buffer
	n, err := run(&out, root, doc, pkg, false, true)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v out=%s", n, err, out.String())
	}
	if !strings.Contains(out.String(), "uncovered: a.go:Added") {
		t.Errorf("uncovered declaration not named:\n%s", out.String())
	}
}

// TestPkgIsRequired runs the command itself: without --pkg it prints the usage
// and exits 2, whatever else is given.
func TestPkgIsRequired(t *testing.T) {
	if os.Getenv("REANCHOR_RUN_MAIN") == "1" {
		os.Args = []string{"reanchor", os.Getenv("REANCHOR_DOC")}
		main()
		return
	}
	_, doc := fixture(t, original)
	cmd := exec.Command(os.Args[0], "-test.run=^TestPkgIsRequired$")
	cmd.Env = append(os.Environ(), "REANCHOR_RUN_MAIN=1", "REANCHOR_DOC="+doc)
	out, err := cmd.CombinedOutput()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "--pkg dir") {
		t.Fatalf("reanchor without --pkg = %v, %q; want exit 2 and the usage naming --pkg", err, out)
	}
}
