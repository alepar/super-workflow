package wftest

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// Every registered harness has a protocol environment whose forged-marker
// attack carries its own marker and whose stripped-env attack carries none:
// an attack that strips or forges nothing would pass vacuously.
func TestProtocolAttacksAreNotVacuous(t *testing.T) {
	for _, h := range harness.Names() {
		t.Run(h, func(t *testing.T) {
			coordinator, child := protocolEnvironments(h, t.TempDir())
			if coordinator == nil || child == nil {
				t.Fatalf("no protocol environment for %s", h)
			}
			lookup := func(env map[string]string) harness.Env {
				return func(k string) string { return env[k] }
			}
			if got := harness.Detect(lookup(child)); got != h {
				t.Errorf("the forged-marker environment is detected as %q, want %q", got, h)
			}
			stripped := protocolStripped(child)
			if m := harness.Markers(lookup(stripped)); len(m) != 0 {
				t.Errorf("the stripped environment still carries markers %v", m)
			}
			if got := harness.Detect(lookup(stripped)); got != "generic" {
				t.Errorf("the stripped environment is detected as %q, want generic", got)
			}
			if h == "agy" && stripped["ANTIGRAVITY_APP_DATA_DIR"] == "" {
				t.Error("the agy stripped environment lost its records root: the attack would not reach the records")
			}
		})
	}
}

// The agy matrix: an agy records tree built by agytest, a positive control for
// each verb, and the six cases, all through the real CLI.
func TestChildProtocolAgy(t *testing.T) {
	report := &ChildProtocolReport{}
	checkChildProtocolHarness(report, t.TempDir(), "agy")
	t.Log(report)
	if report.PositiveControls != 4 {
		t.Errorf("positive controls=%d, want 4 (prompt, validate, done, failed)", report.PositiveControls)
	}
	if report.Cases != 24 {
		t.Errorf("cases=%d, want 24 (6 cases x 4 verbs)", report.Cases)
	}
	if len(report.BlockedEarlier) != 0 {
		t.Errorf("blocked earlier: %v", report.BlockedEarlier)
	}
	if len(report.FixtureGaps) != 0 || len(report.Problems) != 0 {
		t.Errorf("want no gaps or problems:\n%s", strings.TrimSpace(report.String()))
	}
}

// The opencode matrix: no records at all, so each verb's positive control and
// the six cases are settled by the claim token alone, through the real CLI.
func TestChildProtocolOpencode(t *testing.T) {
	report := &ChildProtocolReport{}
	checkChildProtocolHarness(report, t.TempDir(), "opencode")
	t.Log(report)
	if report.PositiveControls != 4 || report.Cases != 24 || len(report.BlockedEarlier) != 0 {
		t.Errorf("controls=%d cases=%d blocked=%v, want 4, 24, none", report.PositiveControls, report.Cases, report.BlockedEarlier)
	}
	if len(report.FixtureGaps) != 0 || len(report.Problems) != 0 {
		t.Errorf("want no gaps or problems:\n%s", strings.TrimSpace(report.String()))
	}
}
