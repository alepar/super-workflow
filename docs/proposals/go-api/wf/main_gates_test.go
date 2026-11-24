package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// gatesCLI runs wf.CLI over the gate-test definitions, in this process.
func gatesCLI(args ...string) (int, string, string) {
	var o, er bytes.Buffer
	code := wf.CLI(wf.MainConfig{Defs: []wf.Registration{{Def: loopGateDef()}, {Def: policyDef(wf.PolicyAnswerable)}}}, args, &o, &er)
	return code, o.String(), er.String()
}

func gatesMust(t *testing.T, args ...string) string {
	t.Helper()
	code, out, er := gatesCLI(args...)
	if code != 0 && !(code == exitEndedCode && args[0] == "next") {
		t.Fatalf("%v exited %d: %s", args, code, er)
	}
	return out
}

func TestRunNeedsRunIDWithoutKey(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	code, _, er := gatesCLI("run", "loop-gate", "--state-dir", state)
	if code != 2 || !strings.Contains(er, "has no run key: pass --run-id") {
		t.Fatalf("run without a key or --run-id = %d %q, want exit 2 naming --run-id", code, er)
	}
	if code, out, er := gatesCLI("run", "loop-gate", "--state-dir", state, "--run-id", "lg-1"); code != 0 || strings.TrimSpace(out) != "lg-1" {
		t.Fatalf("run with --run-id = %d %q %q", code, out, er)
	}
}

func TestStatusShowsDelegations(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	gatesMust(t, "run", "loop-gate", "--state-dir", state, "--run-id", "lg-1", "--delegate", "lgAsk=done", "--delegate", "other/Gate=x")
	var st wf.RunStatus // a fresh CLI call: nothing is carried but the journal
	if err := json.Unmarshal([]byte(gatesMust(t, "status", "lg-1", "--json", "--state-dir", state)), &st); err != nil {
		t.Fatal(err)
	}
	if st.Delegations["lgAsk"] != "done" || st.Delegations["other/Gate"] != "x" || len(st.Delegations) != 2 {
		t.Fatalf("status --json delegations = %v", st.Delegations)
	}
	if human := gatesMust(t, "status", "lg-1", "--full", "--state-dir", state); !strings.Contains(human, "delegated: lgAsk=done, other/Gate=x\n") {
		t.Fatalf("human status lacks the delegated line:\n%s", human)
	}
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(gatesMust(t, "next", "lg-1", "all", "--state-dir", state, "--json")), &ins); err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || !ins[0].Delegated || ins[0].DelegatedAnswer != "done" {
		t.Fatalf("next = %+v, want the gate delegated with answer done", ins)
	}
	if code, _, er := gatesCLI("run", "loop-gate", "--state-dir", state, "--run-id", "lg-2", "--delegate", "nogate"); code != 2 || !strings.Contains(er, "--delegate") {
		t.Fatalf("a malformed --delegate = %d %q, want exit 2", code, er)
	}
}

func TestResumeInheritsOptions(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fast := write("fast.json", `{"Autonomous":true,"Mode":"fast"}`)
	slow := write("slow.json", `{"Autonomous":true,"Mode":"slow"}`)
	gatesMust(t, "run", "policy", "--state-dir", state, "--run-id", "x", "--options", fast)
	if got := gatesMust(t, "run", "policy", "--state-dir", state, "--run-id", "x"); strings.TrimSpace(got) != "x" {
		t.Fatalf("resume printed %q", got)
	}
	gatesMust(t, "next", "x", "all", "--state-dir", state, "--json") // drives: the fast-mode rule answers the gate
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(gatesMust(t, "status", "x", "--json", "--state-dir", state)), &st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(st.Options), `"fast"`) {
		t.Fatalf("after a resume with no options, status options = %s, want the journaled fast mode", st.Options)
	}
	if st.Phase != wf.RunEnded || st.Terminal != "pGo" {
		t.Fatalf("the journaled fast mode must drive the policy: phase %s terminal %s", st.Phase, st.Terminal)
	}
	if code, _, er := gatesCLI("run", "policy", "--state-dir", state, "--run-id", "x", "--options", slow); code != 6 || !strings.Contains(er, "different options") {
		t.Fatalf("resume with other options = %d %q, want exit 6", code, er)
	}
}

func TestAnswerVisitCLI(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	gatesMust(t, "run", "loop-gate", "--state-dir", state, "--run-id", "lg-1")
	next := func() wf.Instruction {
		var ins []wf.Instruction
		if err := json.Unmarshal([]byte(gatesMust(t, "next", "lg-1", "all", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
			t.Fatalf("next = %v %v", ins, err)
		}
		return ins[0]
	}
	g := next()
	gatesMust(t, "answer", "lg-1", "--state-dir", state, "--gate", g.State, "--child", g.Child, "--visit", "1", "--value", "again")
	if again := next(); again.Visit != 2 {
		t.Fatalf("visit after looping = %d, want 2", again.Visit)
	}
	if code, _, er := gatesCLI("answer", "lg-1", "--state-dir", state, "--gate", g.State, "--visit", "1", "--value", "done"); code != 4 || !strings.Contains(er, "stale visit") {
		t.Fatalf("a retried answer at visit 1 = %d %q, want exit 4", code, er)
	}
	if code, _, er := gatesCLI("answer", "lg-1", "--state-dir", state, "--gate", g.State, "--child", "nope", "--visit", "2", "--value", "done"); code != 4 {
		t.Fatalf("an answer for another child = %d %q, want exit 4", code, er)
	}
	gatesMust(t, "answer", "lg-1", "--state-dir", state, "--gate", g.State, "--visit", "2", "--value", "done")
}
