package wftest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventedStartedCannotBypassChildPrompt(t *testing.T) {
	for _, h := range []string{"claude", "codex", "hermes"} {
		for _, dev := range []bool{false, true} {
			t.Run(h+map[bool]string{true: "/dev", false: "/legacy"}[dev], func(t *testing.T) {
				c, err := newProtocolCase(t.TempDir(), h)
				if err != nil {
					t.Fatal(err)
				}
				p := filepath.Join(c.state, "runs/protocol-run/journal.jsonl")
				before, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"started", "protocol-run", c.instruction.Name, "--harness", h, "--session", "coord-session-1", "--agent", "coord-agent-1"}
				if dev {
					args = append([]string{"dev"}, args...)
				}
				code, out, stderr := c.call(c.coordinatorEnv, args...)
				if code != 4 || !strings.Contains(stderr, "start a child agent") {
					t.Errorf("exit%d: %s%s", code, out, stderr)
				}
				after, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("invented metadata mutated journal")
				}
			})
		}
	}
}

func TestHermesTruncatedBriefIsAccepted(t *testing.T) {
	report := &ChildProtocolReport{}
	checkHermesBriefProtocol(t.TempDir(), report)
	if len(report.Problems) != 0 || len(report.FixtureGaps) != 0 || report.Cases != 1 {
		t.Fatal(report)
	}
}

func TestOperatorStartedRemainsExplicit(t *testing.T) {
	c, err := newProtocolCase(t.TempDir(), "hermes")
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := c.call(map[string]string{}, "dev", "started", "protocol-run", c.instruction.Name, "--session", "operator-recorded-session", "--agent", "operator-recorded-agent")
	if code != 0 {
		t.Fatalf("operator exit%d: %s%s", code, out, stderr)
	}
}
