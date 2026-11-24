package wftest

import (
	"strings"
	"testing"
)

// The probe protects values, not words. A refusal may say what the caller
// lacks: the word "session", a harness's name, the name of an environment
// variable. It may not print the fixture's session ids, or a value of the
// case the refused caller must not be shown. A disclosure is reported under
// the value's label, so the report does not print the value either.
func TestProtocolRefusalDisclosureClassifier(t *testing.T) {
	secret := protectedValue{"the unit's claim token", "0123456789abcdef"}
	for _, text := range []string{
		"Start a child agent with the text between the lines.",
		"this command's environment is claude's but names no session",
		"from the coordinator's own session",
		"not set: CLAUDE_CODE_SESSION_ID",
		"load a plugin that exports OPENCODE_SESSION_ID",
		"set NEW_HARNESS_CHILD_CONTEXT",
		"This part of the run is driven from codex; this command's environment is hermes's",
	} {
		if got := protocolDisclosures(text, secret); len(got) != 0 {
			t.Errorf("guidance flagged as a disclosure: %q: %v", text, got)
		}
	}
	for text, want := range map[string]string{
		"caller " + protocolParent:                     "exposes the coordinator's session id",
		"the child " + protocolChild + " owns it":      "exposes the child's session id",
		"the unit's claim is 0123456789abcdef, use it": "exposes the unit's claim token",
	} {
		got := protocolDisclosures(text, secret)
		if len(got) != 1 || got[0] != want {
			t.Errorf("protocolDisclosures(%q) = %v, want [%s]", text, got, want)
		}
		for _, value := range []string{protocolParent, protocolChild, secret.value} {
			if strings.Contains(strings.Join(got, " "), value) {
				t.Errorf("the report of %q prints the value it guards: %v", text, got)
			}
		}
	}
}

// Each place the probe reads a refusal holds that refusal to the values of
// its own case: the harness's records root, the claim token the caller was
// not given, and the session and agent ids the caller supplied. A refusal
// that printed one is reported under its label; the same refusal with only
// words and variable names is not.
func TestProtocolSitesProtectTheirCasesValues(t *testing.T) {
	c := &protocolCase{records: "/tmp/probe-case/records-root", token: "aaaaaaaaaaaaaaaa", harness: "claude"}
	const words = "refused: this command's environment is claude's but names no session: not set: CLAUDE_CODE_SESSION_ID; the unit's claim token was not offered"
	for _, site := range []struct {
		name      string
		protected []protectedValue
		leaks     map[string]string // a value the refusal might print, and the label it is reported under
	}{
		{"an attack on a child verb (protocol.go)", c.attackProtected("bbbbbbbbbbbbbbbb"), map[string]string{
			c.records: "the records root", "bbbbbbbbbbbbbbbb": "the unit's claim token"}},
		{"a refused offer (protocol.go)", c.offerProtected(), map[string]string{
			c.records: "the records root"}},
		{"a started with invented metadata (protocol_start.go)", c.startedProtected("invented-session-9", "invented-agent-9"), map[string]string{
			c.records: "the records root", "invented-session-9": "the supplied session id", "invented-agent-9": "the supplied agent id"}},
		{"a tokenless sibling (protocol_sibling.go)", c.siblingProtected("sibling-session-9"), map[string]string{
			c.records: "the records root", c.token: "the unit's claim token", "sibling-session-9": "the supplied session id"}},
	} {
		if got := protocolDisclosures(words, site.protected...); len(got) != 0 {
			t.Errorf("%s: words and variable names reported: %v", site.name, got)
		}
		for value, label := range site.leaks {
			got := protocolDisclosures(words+" ("+value+")", site.protected...)
			if len(got) != 1 || got[0] != "exposes "+label {
				t.Errorf("%s: a refusal printing %s = %v, want [exposes %s]", site.name, label, got, label)
			}
			if strings.Contains(strings.Join(got, " "), value) {
				t.Errorf("%s: the report prints the value it guards: %v", site.name, got)
			}
		}
	}
	// A caller that presented the unit's own token is not held to it.
	if got := protocolDisclosures("claim "+c.token, c.attackProtected("")...); len(got) != 0 {
		t.Errorf("an attack that presented the token: %v", got)
	}
}

// End to end, over the actual CLI: no refusal the probe provokes prints a
// protected value, and none is reported for the words it now may use.
func TestChildProtocolRefusalsDiscloseNoValue(t *testing.T) {
	report := CheckChildProtocol()
	if len(report.FixtureGaps) > 0 {
		t.Fatal(report)
	}
	for _, problem := range report.Problems {
		if strings.Contains(problem, "refusal-disclosure") {
			t.Error(problem)
		}
	}
}

// In the unoffered-token attack the caller presents a claim the run never
// offered. A refusal that echoed only that submitted claim is a disclosure
// of a caller-passed value, reported by label, as one that printed the
// unit's real claim is; a refusal with neither is not reported.
func TestProtocolUnofferedTokenProtectsThePresentedClaim(t *testing.T) {
	c := &protocolCase{records: "/tmp/probe-case/records-root", token: "aaaaaaaaaaaaaaaa", harness: "claude"}
	real := c.token
	protected := c.presentUnofferedToken()
	presented := c.token
	if presented == real || presented == "" {
		t.Fatalf("the case presents %q, the unit's own claim is %q", presented, real)
	}
	const words = "refused: this claim token was never offered for the unit"
	if got := protocolDisclosures(words, protected...); len(got) != 0 {
		t.Errorf("words reported: %v", got)
	}
	for value, label := range map[string]string{presented: "the presented claim token", real: "the unit's claim token", "fixture attempt failed": "the supplied reason"} {
		got := protocolDisclosures(words+": "+value, protected...)
		if len(got) != 1 || got[0] != "exposes "+label {
			t.Errorf("a refusal echoing only %s = %v, want [exposes %s]", label, got, label)
		}
		if strings.Contains(strings.Join(got, " "), value) {
			t.Errorf("the report prints the value it guards: %v", got)
		}
	}
	// A caller presenting the unit's own token is not held to it.
	own := &protocolCase{records: c.records, token: real}
	if got := protocolDisclosures("claim "+real, own.attackProtected("")...); len(got) != 0 {
		t.Errorf("an attack that presented the unit's own token: %v", got)
	}
	// The tokenless sibling's failed supplies a reason of its own.
	if got := protocolDisclosures(words+": "+siblingReason, own.siblingProtected("sibling-session-9")...); len(got) != 1 || got[0] != "exposes the supplied reason" {
		t.Errorf("a sibling refusal echoing its reason = %v, want [exposes the supplied reason]", got)
	}
}
