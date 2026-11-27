// Package bad holds negative examples of the wf API. Every example lives in
// its own directory behind the badexample build tag, so go build, go vet and
// go test ./... never see it; this test builds each one explicitly.
package bad

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var wantRe = regexp.MustCompile(`(?m)^// want: (.+)$`)

// wantOf reads the expected error text from the example's `// want:` comment.
func wantOf(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(name, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := wantRe.FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s/main.go has no `// want:` comment", name)
	}
	return strings.TrimSpace(string(m[1]))
}

func goTool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	return p
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func TestCompileRejections(t *testing.T) {
	goBin := goTool(t)
	for _, name := range []string{"foreign_edge", "retry_no_fallback", "effect_no_key", "wrong_check_type", "cases_foreign_edge", "route_foreign_candidate", "option_field_misspelled", "option_field_wrong_type"} {
		t.Run(name, func(t *testing.T) {
			want := wantOf(t, name)
			cmd := exec.Command(goBin, "build", "-tags", "badexample", "-o", os.DevNull, "./bad/"+name)
			cmd.Dir = ".."
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("%s compiled; it must be rejected", name)
			}
			if !strings.Contains(string(out), want) {
				t.Errorf("%s: compiler output lacks %q:\n%s", name, want, out)
			}
		})
	}
}

// TestLintRejections builds each lint example and runs the binary (go run would
// flatten the exit status to 1): lint must exit 2 and name the rule id.
func TestLintRejections(t *testing.T) {
	goBin := goTool(t)
	for _, name := range []string{"lint_unwired_budget", "lint_reserved_policy", "lint_case_uncovered", "lint_sub_exit_unmapped", "lint_case_duplicate", "lint_sub_unyielded", "lint_route_no_candidates", "lint_send_back_target", "lint_send_back_fallback"} {
		t.Run(name, func(t *testing.T) {
			want := wantOf(t, name)
			bin := filepath.Join(t.TempDir(), name)
			build := exec.Command(goBin, "build", "-tags", "badexample", "-o", bin, "./bad/"+name)
			build.Dir = ".."
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("%s must compile: %v\n%s", name, err, out)
			}
			var stderr strings.Builder
			run := exec.Command(bin, "lint")
			run.Stderr = &stderr
			err := run.Run()
			if code := exitCode(err); code != 2 {
				t.Errorf("%s lint exit = %d (%v), want 2", name, code, err)
			}
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("%s: stderr lacks rule %q:\n%s", name, want, stderr.String())
			}
		})
	}
}
