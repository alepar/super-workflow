package wf

import (
	"strings"
	"testing"
)

func TestModelTiersCheckIsInertAndNamed(t *testing.T) {
	c := modelTiersCheck(nil)
	if c.Name != "model-tiers" {
		t.Errorf("Name = %q", c.Name)
	}
	if r := c.Run(PreflightContext{Workspace: "/ws", Harness: "claude"}); r != nil {
		t.Errorf("inert check refused: %+v", r)
	}
}

func TestPreflightChecksOrderModelTiersFirst(t *testing.T) {
	reg := &Registration{Def: New("order"), Preflight: []PreflightCheck{{Name: "a"}, {Name: "b"}}}
	var names []string
	for _, c := range preflightChecks(reg) {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "model-tiers,a,b" {
		t.Errorf("order = %s, want model-tiers,a,b", got)
	}
}

// A warning is printed and the run goes on: the checks after it still run, and
// only a refusal stops it (an earlier revision follow-up: a host fact that may or may not
// matter, such as a read-only Go cache, never blocks a run).
func TestPreflightWarningPrintsAndContinues(t *testing.T) {
	ran := false
	reg := &Registration{Def: New("warn"), Preflight: []PreflightCheck{
		{Name: "soft", Run: func(PreflightContext) *Refusal {
			return &Refusal{Cause: "GOCACHE /ro is not writable", Remedy: "point GOCACHE inside the roots", Warn: true}
		}},
		{Name: "after", Run: func(PreflightContext) *Refusal { ran = true; return nil }},
	}}
	var out strings.Builder
	if code := runPreflight(reg, PreflightContext{Workspace: "/ws", Harness: "claude"}, &out); code != exitOK {
		t.Fatalf("exit = %d, want ok: a warning never refuses", code)
	}
	if !ran {
		t.Error("the check after a warning did not run")
	}
	if want := "warning: soft: GOCACHE /ro is not writable\nfix: point GOCACHE inside the roots\n"; out.String() != want {
		t.Errorf("stderr = %q, want %q", out.String(), want)
	}
	reg.Preflight = append(reg.Preflight, PreflightCheck{Name: "hard", Run: func(PreflightContext) *Refusal {
		return &Refusal{Cause: "TMPDIR /x is not writable", Remedy: "fix it"}
	}})
	out.Reset()
	if code := runPreflight(reg, PreflightContext{Workspace: "/ws", Harness: "claude"}, &out); code != exitUsage {
		t.Fatalf("exit = %d, want the refusal's", code)
	}
	if !strings.Contains(out.String(), "refused: hard: TMPDIR /x is not writable\n") {
		t.Errorf("stderr = %q, want the refusal after the warning", out.String())
	}
}
