package wftest

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/alepar/super-workflow/docs/proposals/go-api/internal/harnessfixture"
)

// The old coordinator override accepted invented metadata without a child
// prompt fetch. Exercise both spellings, including the explicit operator one.
func checkInventedStarted(root string, report *ChildProtocolReport) {
	for _, h := range []string{"claude", "codex", "hermes"} {
		for _, dev := range []bool{false, true} {
			site := h + "/started/invented-metadata"
			if dev {
				site = h + "/dev-started/invented-metadata"
			}
			c, err := newProtocolCase(filepath.Join(root, site), h)
			if err != nil {
				report.FixtureGaps = append(report.FixtureGaps, site+": "+err.Error())
				continue
			}
			const session, agent = "coord-session-1", "coord-agent-1"
			args := []string{"started", "protocol-run", c.instruction.Name, "--harness", h, "--session", session, "--agent", agent}
			if dev {
				args = append([]string{"dev"}, args...)
			}
			checkProtocolRefusal(c, site, args, c.coordinatorEnv, report, c.startedProtected(session, agent)...)
		}
	}
}

func checkProtocolRefusal(c *protocolCase, site string, args []string, env map[string]string, report *ChildProtocolReport, protected ...protectedValue) {
	p := filepath.Join(c.state, "runs/protocol-run/journal.jsonl")
	before, err := os.ReadFile(p)
	if err != nil {
		report.FixtureGaps = append(report.FixtureGaps, site+": "+err.Error())
		return
	}
	report.Cases++
	code, out, stderr := c.call(env, args...)
	if code != 4 {
		report.Problems = append(report.Problems, fmt.Sprintf("refusal-required %s: exit%d: %s%s", site, code, out, stderr))
	}
	after, err := os.ReadFile(p)
	if err != nil {
		report.FixtureGaps = append(report.FixtureGaps, site+": "+err.Error())
	} else if err := refusalWrite(c, protocolVerb(args), stderr, before, after); err != nil {
		report.Problems = append(report.Problems, "unauthorized-write "+site+": "+err.Error())
	}
	for _, leak := range protocolDisclosures(out+stderr, protected...) {
		report.Problems = append(report.Problems, "refusal-disclosure "+site+": "+leak)
	}
}

// The fixture models a kickoff preview clipped before the claim.
// The child's prompt fetch is accepted on the token alone.
func checkHermesBriefProtocol(root string, report *ChildProtocolReport) {
	raw, err := harnessfixture.ReadFile("started/" + "hermes-context-truncated.log")
	if err != nil {
		report.FixtureGaps = append(report.FixtureGaps, err.Error())
		return
	}
	site := "hermes/prompt/truncated"
	c, err := newProtocolCase(filepath.Join(root, site), "hermes")
	if err != nil {
		report.FixtureGaps = append(report.FixtureGaps, site+": "+err.Error())
		return
	}
	path := filepath.Join(c.records, "cache/delegation/live/deleg_protocol/task-0.log")
	if err = os.MkdirAll(filepath.Dir(path), 0700); err == nil {
		err = os.WriteFile(path, raw, 0600)
	}
	if err != nil {
		report.FixtureGaps = append(report.FixtureGaps, err.Error())
		return
	}
	report.Cases++
	if code, out, stderr := c.invoke("prompt", c.childEnv); code != 0 {
		report.Problems = append(report.Problems, fmt.Sprintf("claim-refused %s: exit%d: %s%s", site, code, out, stderr))
	}
}

// protocolVerb is the verb of a probe call's arguments ("dev" verbs too).
func protocolVerb(args []string) string {
	if len(args) > 1 && args[0] == "dev" {
		return args[1]
	}
	if len(args) > 0 {
		return args[0]
	}
	return ""
}
