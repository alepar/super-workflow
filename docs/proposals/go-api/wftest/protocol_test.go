package wftest_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func TestChildProtocolMissingFixtureRootHasOneDiagnostic(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("WF_HARNESS_FIXTURES_DIR", "missing")
	root := filepath.Join(dir, "missing")

	protocol := wftest.CheckChildProtocol()
	if len(protocol.FixtureGaps) != 1 || protocol.Cases != 0 {
		t.Fatalf("want one fixture root diagnostic before exercising cases, got %s", protocol)
	}
	diagnostic := protocol.FixtureGaps[0]
	if !strings.Contains(diagnostic, root) || !strings.Contains(diagnostic, "WF_HARNESS_FIXTURES_DIR") {
		t.Fatalf("fixture diagnostic does not locate the root or name the remedy: %s", diagnostic)
	}

	factory := func() *wftest.Sim {
		d := wf.New("missing-fixtures")
		wf.NewTerminal[protocolEarlyDone](d, wf.Success)
		return wftest.New(d)
	}
	report := wftest.Check(factory, wftest.CheckConfig{ChildProtocol: true, MaxRuns: 1})
	if !slices.Contains(report.Problems, diagnostic) {
		t.Fatal("missing fixture root did not fail the checker")
	}
	if want := 1; strings.Count(report.String(), diagnostic) != want {
		t.Fatalf("want one fixture root diagnostic in the checker output, got %s", report)
	}
}

func TestChildProtocolExercisesRealCLIAndReportsCurrentGaps(t *testing.T) {
	before := os.Environ()
	report := wftest.CheckChildProtocol()
	if !slices.Equal(before, os.Environ()) {
		t.Fatal("protocol checker mutated process environment")
	}
	t.Log(report)
	if report.PositiveControls != 28 {
		t.Fatalf("positive controls=%d, want all 28: %s", report.PositiveControls, report)
	}
	if report.Cases+len(report.BlockedEarlier) != 143 {
		t.Fatalf("executed=%d blocked earlier=%d, want 143 total: %s", report.Cases, len(report.BlockedEarlier), report)
	}
	if !wf.TrustSessionlessCallers && len(report.BlockedEarlier) != 4 {
		t.Fatalf("want4 clean early refusals: %s", report)
	}
	for _, site := range report.BlockedEarlier {
		if !strings.HasPrefix(site, "claude/") || !strings.HasSuffix(site, "/sessionless-first-coordinator") {
			t.Errorf("unexpected early block: %s", site)
		}
	}
	if len(report.FixtureGaps) != 0 {
		t.Fatalf("fixture gaps: %s", report)
	}
	// A call holding its unit's claim token is accepted whatever the
	// harness's records show; the coordinator's own first fetch and a token
	// never offered are refused.
	if len(report.Problems) != 0 {
		t.Fatalf("problems: %s", report)
	}
}

type protocolEarlyWork struct{}
type protocolEarlyDone struct{}
type protocolEarlyStopped struct{}

// Exercise the actual early guard without the wf package's permissive TestMain.
func TestSessionlessProtocolAttackBlockedAtNextWithoutWrites(t *testing.T) {
	if wf.TrustSessionlessCallers {
		t.Fatal("requires production sessionless policy")
	}
	root := t.TempDir()
	d := wf.New("early-block")
	a := wf.NewAgent[protocolEarlyWork, bool](d)
	done := wf.NewTerminal[protocolEarlyDone](d, wf.Success)
	stopped := wf.NewTerminal[protocolEarlyStopped](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stopped)
	a.Spec(wf.PromptText("Return true."), wf.Retry[protocolEarlyWork](wf.Const(1), a.To(done, "exhausted")))
	a.Then(a.To(done, "accepted"))
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}}
	state := filepath.Join(root, "state")
	env := map[string]string{"CLAUDECODE": "1", "CLAUDE_CONFIG_DIR": filepath.Join(root, "records")}
	call := func(args ...string) (int, string) {
		var out, stderr bytes.Buffer
		code := wf.CLIWithEnv(cfg, append(args, "--state-dir", state), &out, &stderr, harness.Env(func(k string) string { return env[k] }))
		return code, out.String() + stderr.String()
	}
	if code, out := call("run", "early-block", "--run-id", "early", "--workspace", root); code != 0 {
		t.Fatalf("setup exit%d: %s", code, out)
	}
	journal := filepath.Join(state, "runs", "early", "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	code, out := call("next", "early", "1", "--json")
	after, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if code != 4 || !bytes.Equal(before, after) {
		t.Fatalf("early refusal exit%d journal unchanged=%t: %s", code, bytes.Equal(before, after), out)
	}
	// The refusal says what is missing, by name; it prints no value of the
	// caller's environment.
	for _, want := range []string{"names no session", "CLAUDE_CODE_SESSION_ID"} {
		if !strings.Contains(out, want) {
			t.Errorf("early refusal does not say %q: %s", want, out)
		}
	}
	if leak := env["CLAUDE_CONFIG_DIR"]; strings.Contains(out, leak) {
		t.Errorf("early refusal disclosed the records root %q: %s", leak, out)
	}
}

func TestChildProtocolRejectsTokenlessSiblingSubstitution(t *testing.T) {
	r := wftest.CheckChildProtocol()
	if len(r.FixtureGaps) > 0 {
		t.Fatal(r)
	}
	for _, p := range r.Problems {
		if strings.Contains(p, "sibling-") {
			t.Error(p)
		}
	}
}
