package excerptcheck

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const repoRoot = "../../../../.."

var sampleOutput = map[string]string{"lint": "lint x: ok"}

func kinds(ms []Mismatch) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Kind)
	}
	return out
}

func TestMatchingExcerptPasses(t *testing.T) {
	if ms := CheckDoc(repoRoot, "docs/proposals/go-api/tools/excerptcheck/testdata/match.md", sampleOutput); len(ms) != 0 {
		t.Fatalf("expected no mismatches, got %+v", ms)
	}
}

func TestMismatchedExcerptFails(t *testing.T) {
	ms := CheckDoc(repoRoot, "docs/proposals/go-api/tools/excerptcheck/testdata/mismatch.md", nil)
	if got := kinds(ms); len(got) != 1 || got[0] != "excerpt-mismatch" {
		t.Fatalf("want one excerpt-mismatch, got %+v", ms)
	}
	if ms[0].Line != 4 {
		t.Errorf("mismatch reported at line %d, want the fence line 4", ms[0].Line)
	}
}

func TestFenceWithoutSrcFails(t *testing.T) {
	ms := CheckDoc(repoRoot, "docs/proposals/go-api/tools/excerptcheck/testdata/nosrc.md", nil)
	if got := kinds(ms); len(got) != 1 || got[0] != "no-src" {
		t.Fatalf("want one no-src, got %+v", ms)
	}
}

func TestOutputBlocksCompared(t *testing.T) {
	doc := "docs/proposals/go-api/tools/excerptcheck/testdata/match.md"
	if got := kinds(CheckDoc(repoRoot, doc, map[string]string{"lint": "lint x: FAILED"})); len(got) != 1 || got[0] != "output-mismatch" {
		t.Errorf("want output-mismatch, got %v", got)
	}
	if got := kinds(CheckDoc(repoRoot, doc, map[string]string{"other": "x"})); len(got) != 1 || got[0] != "unknown-section" {
		t.Errorf("want unknown-section, got %v", got)
	}
}

func TestBadSrcRange(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "bad.md")
	body := "<!-- src: docs/proposals/go-api/tools/excerptcheck/testdata/sample.go#L4-L99 -->\n```go\nx\n```\n"
	if err := os.WriteFile(doc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := kinds(CheckDoc(repoRoot, doc, nil)); len(got) != 1 || got[0] != "bad-src" {
		t.Fatalf("want bad-src, got %v", got)
	}
}

func TestParseOutput(t *testing.T) {
	got := ParseOutput("#### lint\na\nb\n\n#### graph x\nc\n\n")
	if got["lint"] != "a\nb" || got["graph x"] != "c" || len(got) != 2 {
		t.Fatalf("unexpected sections: %q", got)
	}
}

func TestUncoveredReportsDeclarationsOutsideExcerpts(t *testing.T) {
	got := Uncovered(repoRoot, "docs/proposals/go-api/tools/excerptcheck/testdata/match.md", "docs/proposals/go-api/tools/excerptcheck/testdata")
	if len(got) != 1 || got[0] != "sample.go:Sub" {
		t.Fatalf("want [sample.go:Sub], got %v", got)
	}
}

// withoutSection drops the section headed "#### id", up to the next "#### "
// header or the end of text.
func withoutSection(text, id string) string {
	var out []string
	skip := false
	for _, l := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(l, "#### ") {
			skip = strings.TrimRight(l, "\n") == "#### "+id
		}
		if !skip {
			out = append(out, l)
		}
	}
	return strings.Join(out, "")
}

func TestWithoutSection(t *testing.T) {
	text := "#### lint\na\n\n#### go test\nok\n\n#### hash\nh\n\n"
	if got, want := withoutSection(text, "go test"), "#### lint\na\n\n#### hash\nh\n\n"; got != want {
		t.Errorf("middle section: got %q, want %q", got, want)
	}
	if got, want := withoutSection("#### lint\na\n\n#### go test\nok\n", "go test"), "#### lint\na\n\n"; got != want {
		t.Errorf("last section: got %q, want %q", got, want)
	}
	if got := withoutSection(text, "absent"); got != text {
		t.Errorf("absent id changed text: %q", got)
	}
}

func TestOutputGolden(t *testing.T) {
	cmd := exec.Command("bash", "../../scripts/regen-output.sh", "--stdout", "--no-go-test")
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOFLAGS=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "GOFLAGS=")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("regen-output.sh failed: %v\n%s", err, errb.String())
	}
	raw, err := os.ReadFile("../../OUTPUT.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(withoutSection(string(raw), "go test"))
	if bytes.Equal(out.Bytes(), want) {
		return
	}
	a, b := strings.Split(string(want), "\n"), strings.Split(out.String(), "\n")
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if i >= len(a) || i >= len(b) || x != y {
			t.Fatalf("OUTPUT.txt differs from a fresh regeneration at line %d\n- committed:   %q\n+ regenerated: %q\nrun scripts/regen-output.sh (the go test section is excluded from this check)", i+1, x, y)
		}
	}
}
