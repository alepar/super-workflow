package wftest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/internal/harnessfixture"
)

// Exercises a sibling's substitution after the target fetches its prompt.
// The target token is rebound, while the attacker's recorded task stays distinct.
// A sibling that names the unit with no claim token is refused; one holding
// the unit's token is accepted, since the token is what binds a call.
func checkHermesSiblingProtocol(root string, report *ChildProtocolReport) {
	for _, verb := range []string{"prompt", "validate", "done", "failed"} {
		for _, tokened := range []bool{false, true} {
			site := fmt.Sprintf("hermes/%s/sibling-token-%t", verb, tokened)
			c, err := newProtocolCase(filepath.Join(root, site), "hermes")
			if err == nil {
				err = c.authorize("done")
			}
			if err == nil {
				err = os.RemoveAll(c.records)
			}
			if err == nil {
				for i, fixture := range []string{"first", "second"} {
					var data []byte
					data, err = harnessfixture.ReadFile("hermes/cross-task/" + fixture + ".log")
					if err != nil {
						break
					}
					data = []byte(strings.ReplaceAll(string(data), "3030303030303030", c.token))
					p := filepath.Join(c.records, "cache", "delegation", "live", "deleg_fixture", fmt.Sprintf("task-%d.log", i+1))
					if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
						break
					}
					if err = os.WriteFile(p, data, 0600); err != nil {
						break
					}
				}
			}
			if err != nil {
				report.FixtureGaps = append(report.FixtureGaps, site+": "+err.Error())
				continue
			}
			// Verify the correct child is still accepted with these records in place.
			if code, out, stderr := c.invoke("validate", c.childEnv); code != 0 {
				report.FixtureGaps = append(report.FixtureGaps, fmt.Sprintf("%s positive: exit%d %s%s", site, code, out, stderr))
				continue
			}
			report.PositiveControls++
			journal := filepath.Join(c.state, "runs", "protocol-run", "journal.jsonl")
			before, err := os.ReadFile(journal)
			if err != nil {
				report.FixtureGaps = append(report.FixtureGaps, err.Error())
				continue
			}
			const siblingSession = "fixture-first"
			env := map[string]string{"HERMES_SESSION_ID": siblingSession, "HERMES_HOME": c.records, "HERMES_DELEGATED_CHILD_CONTEXT": c.records}
			args := []string{verb, "protocol-run", c.instruction.Name}
			if tokened {
				args = append(args, "--claim", c.token)
			}
			if verb == "failed" {
				args = append(args, "--reason", siblingReason)
			}
			code, out, stderr := c.call(env, args...)
			report.Cases++
			if tokened {
				if code != 0 {
					report.Problems = append(report.Problems, fmt.Sprintf("claim-refused %s: exit%d %s%s", site, code, out, stderr))
				}
				continue
			}
			if code != 4 {
				report.Problems = append(report.Problems, fmt.Sprintf("sibling-substitution %s: exit%d %s%s", site, code, out, stderr))
			}
			after, err := os.ReadFile(journal)
			if err != nil {
				report.FixtureGaps = append(report.FixtureGaps, err.Error())
				continue
			}
			if err := refusalWrite(c, verb, stderr, before, after); err != nil {
				report.Problems = append(report.Problems, "unauthorized-write "+site+": "+err.Error())
			}
			// The tokenless caller was not given the unit's claim token: its
			// refusal must not hand it over.
			for _, leak := range protocolDisclosures(out+stderr, c.siblingProtected(siblingSession)...) {
				report.Problems = append(report.Problems, "refusal-disclosure "+site+": "+leak)
			}
		}
	}
}

// siblingReason is the --reason the tokenless sibling's failed supplies.
const siblingReason = "sibling substitution"
