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

var autonomous = map[string]bool{"Autonomous": true}

// startAgent starts clAgentDef with options, hands its unit out and claims it.
func startAgent(t *testing.T, h *harness, d *wf.Def, opts any) (wf.RunID, wf.Instruction) {
	t.Helper()
	run, err := h.e.Run(d, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	ins := h.handOut(run, 1)
	if len(ins) != 1 {
		t.Fatalf("HandOut(1) = %+v, want one unit", ins)
	}
	if err := h.e.Started(run, ins[0].Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	return run, ins[0]
}

func TestParseFailureClass(t *testing.T) {
	for in, want := range map[string]wf.FailureClass{"": wf.FailTransient, "transient": wf.FailTransient, "blocked": wf.FailBlocked} {
		if got, err := wf.ParseFailureClass(in); err != nil || got != want {
			t.Errorf("ParseFailureClass(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	if _, err := wf.ParseFailureClass("Blocked"); err == nil {
		t.Error("ParseFailureClass accepted an unknown class")
	}
}

func TestTransientFailureQueuesNextAttempt(t *testing.T) {
	d := clAgentDef("fc-transient")
	h := newHarness(t, d, nil)
	run, unit := startAgent(t, h, d, nil)
	res, err := h.e.FailedAs(run, unit.Name, "flaked", wf.FailTransient)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failure == nil || res.Failure.Disposition != "queued" || res.Failure.Message != "attempt 2 queued" {
		t.Fatalf("Failure = %+v, want queued as 'attempt 2 queued'", res.Failure)
	}
	st := h.status(run)
	var queued bool
	for _, u := range st.Units {
		queued = queued || (u.State == "pending" && u.Attempt == 2)
	}
	if !queued || len(st.Held) != 0 {
		t.Fatalf("status = %+v, want attempt 2 pending and nothing held", st.Units)
	}
}

// waitingOf is the one waiting instruction next answers a run waiting on its
// holds with.
func waitingOf(t *testing.T, h *harness, run wf.RunID) wf.Instruction {
	t.Helper()
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionWaiting {
		t.Fatalf("next = %+v, want one waiting instruction", ins)
	}
	return ins[0]
}

// fbAsk's OnClass(Blocked) route takes a blocked failure to fbBlocker, a
// compute that reads the cause; the edge out of it files the cause through an
// effect that binds {blockedCause}.
type (
	fbAsk     struct{}
	fbBlocker struct{}
	fbDone    struct{}
	fbHeld    struct{}
	fbCause   struct{ Cause string }
)

func blockedRouteDef(name string) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[fbAsk, clAnswer](d)
	blk := wf.NewCompute[fbBlocker, fbCause](d, func(c wf.Ctx) (fbCause, error) { return fbCause{Cause: wf.BlockedCause(c)}, nil })
	done := wf.NewTerminal[fbDone](d, wf.Success)
	held := wf.NewTerminal[fbHeld](d, wf.Stopped)
	a.Spec(wf.PromptText("Go."), wf.Retry[fbAsk](wf.Const(2), a.To(held, "gave up")).OnClass(wf.Blocked, a.To(blk, "blocked")))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[fbAsk] { return next }, next)
	filed := blk.To(held, "filed").With(wf.ExternalEffect{Adapter: "bd", Op: "create", Args: map[string]any{"description": "cause: {blockedCause}"}, Key: wf.KeyOf("blocker")})
	blk.Route(func(fbCause, wf.Ctx) wf.Edge[fbBlocker] { return filed }, filed)
	return d
}

// A state that declares OnClass(Blocked) takes that edge on a blocked failure,
// with the cause bound as {blockedCause} (and read by wf.BlockedCause): no
// hold, no retry spent, in either run mode.
func TestBlockedFailureTakesBlockedRoute(t *testing.T) {
	for name, opts := range map[string]any{"interactive": nil, "autonomous": autonomous} {
		t.Run(name, func(t *testing.T) {
			d := blockedRouteDef("fc-route")
			w := newWorld()
			h := newHarness(t, d, w)
			run, unit := startAgent(t, h, d, opts)
			res, err := h.e.FailedAs(run, unit.Name, "BLOCKED_NETWORK: proxy.golang.org unreachable", wf.FailBlocked)
			if err != nil {
				t.Fatal(err)
			}
			if f := res.Failure; f == nil || f.Disposition != wf.DispositionRouted || f.Message != "attempt 1 blocked: BLOCKED_NETWORK: proxy.golang.org unreachable; next: fbHeld" || f.Escalation != "" {
				t.Fatalf("Failure = %+v, want routed through the Blocked edge", f)
			}
			if n := len(h.kind(run, wf.JournalUnitHeld)); n != 0 {
				t.Fatalf("%d unit_held records: a routed blocked failure holds nothing", n)
			}
			refused := h.kind(run, wf.JournalRefused)
			var rd wf.RefusedData
			if len(refused) != 1 || refused[0].Class != wf.Blocked || json.Unmarshal(refused[0].Data, &rd) != nil || rd.Plan == nil || rd.Spent {
				t.Fatalf("refused = %+v, want one Blocked record taking its route with nothing spent", refused)
			}
			statusOf(t, h.status(run), wf.RunEnded, "fbHeld")
			computed := h.kind(run, wf.JournalAccepted)
			if len(computed) != 1 || !strings.Contains(string(computed[0].Data), `"Cause":"BLOCKED_NETWORK: proxy.golang.org unreachable"`) {
				t.Fatalf("accepted = %+v, want the compute to read the cause", computed)
			}
			if len(w.applied) != 1 || w.applied[0].Args["description"] != "cause: BLOCKED_NETWORK: proxy.golang.org unreachable" {
				t.Fatalf("applied = %+v, want {blockedCause} bound in the edge effect", w.applied)
			}
		})
	}
}

func TestFailedCLIRendersJournaledOutcome(t *testing.T) {
	e := newCLIEnv(t)
	auto := clAgentDef("agent-auto")
	wf.Options[struct{ Autonomous bool }](auto)
	cfg := clConfig()
	cfg.Defs = append(cfg.Defs, wf.Registration{Def: auto})
	call := func(args ...string) (int, string, string) {
		var o, er bytes.Buffer
		code := fixtureCLI(cfg, append(args, "--state-dir", e.state), &o, &er)
		return code, o.String(), er.String()
	}
	optsFile := filepath.Join(e.ws, "opts.json")
	if err := os.WriteFile(optsFile, []byte(`{"Autonomous":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	claimUnit := func(run string) string {
		code, o, er := call("next", run, "1", "--json")
		var ins []wf.Instruction
		if err := json.Unmarshal([]byte(o), &ins); err != nil || code != 0 || len(ins) != 1 {
			t.Fatalf("next = %d %q (%v) %s", code, o, err, er)
		}
		if code, _, er := call("dev", "started", run, ins[0].Name, "--harness", "claude", "--session", "s1", "--agent", "a"); code != 0 {
			t.Fatalf("started = %d %s", code, er)
		}
		return ins[0].Name
	}
	status := func(run string) wf.RunStatus {
		code, o, er := call("status", run, "--json")
		var st wf.RunStatus
		if err := json.Unmarshal([]byte(o), &st); err != nil || code != 0 {
			t.Fatalf("status = %d %q (%v) %s", code, o, err, er)
		}
		return st
	}
	start := func(id string, extra ...string) string {
		if code, _, er := call(append([]string{"run", "agent-auto", "--workspace", e.ws, "--run-id", id}, extra...)...); code != 0 {
			t.Fatalf("run = %d %s", code, er)
		}
		return id
	}

	// Transient: exit 0, attempt 2 queued, status agrees.
	run := start("t1")
	name := claimUnit(run)
	code, _, er := call("failed", run, name, "--reason", "flaked")
	if code != 0 || !strings.Contains(er, "attempt 2 queued") || strings.Contains(er, "refused") {
		t.Fatalf("transient failed = %d %q, want 0 and 'attempt 2 queued'", code, er)
	}
	pending := 0
	for _, u := range status(run).Units {
		if u.State == "pending" && u.Attempt == 2 {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("status units = %+v, want attempt 2 pending", status(run).Units)
	}
	// A rejected verb is refused: exit 4.
	if code, _, er = call("failed", run, name, "--reason", "again"); code != 4 || !strings.Contains(er, "refused") {
		t.Fatalf("failed on a retired name = %d %q, want 4 and 'refused'", code, er)
	}
	if code, _, er = call("failed", run, "no/such#1"); code != 4 || !strings.Contains(er, "refused") {
		t.Fatalf("failed on an unknown unit = %d %q, want 4 and 'refused'", code, er)
	}
	// An unknown class is a usage error, not a refusal.
	if code, _, er = call("failed", run, "x", "--class", "fatal"); code != 2 || strings.Contains(er, "refused") {
		t.Fatalf("failed --class fatal = %d %q, want 2", code, er)
	}

	// Blocked: the definition's Blocked route (OnFailure) is taken and the
	// line names where the run went; nothing is held, in either run mode.
	for _, id := range []string{"b1", "i1"} {
		args := []string{}
		if id == "b1" {
			args = []string{"--options", optsFile}
		}
		run = start(id, args...)
		name = claimUnit(run)
		code, _, er = call("failed", run, name, "--class", "blocked", "--reason", "BLOCKED_AUTH")
		if code != 0 || er != "failed: attempt 1 blocked: BLOCKED_AUTH; next: clGaveUp\n" {
			t.Fatalf("%s blocked failed = %d %q, want 0 and the routed line", id, code, er)
		}
		if st := status(run); st.Phase != wf.RunEnded || len(st.Held) != 0 {
			t.Fatalf("%s status = %s held %+v, want ended on the Blocked route", id, st.Phase, st.Held)
		}
	}
}

// D1, D3: the refusal and escalation rules reach the child, who meets them,
// with its prompt (C2); none of them is a coordinator approval setting.
func TestChildPromptCarriesRefusalRules(t *testing.T) {
	d := clAgentDef("refusal-rules")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.handOut(run, 1)
	p, err := h.e.PromptClaim(run, "", claimToken(t, ins[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"is blocked: never work around it; finish with:", "A command that ran and failed",
		"When the sandbox blocks a command the task needs, request escalation",
		"--escalated '<command>: <reason>: <outcome>' to done or failed for each one, the outcome a short phrase without \": \"",
		"If escalation is refused, that is still blocked unless your coordinator can present the approval prompt you could not access.",
		"Check your result first (it checks exactly as done does and records nothing); fix what it names and run it again until it says ok:",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	for _, bad := range []string{"approval_policy", "approvals_reviewer =", "never prompt", "Never request sandbox escalation"} {
		if strings.Contains(p, bad) {
			t.Errorf("prompt carries %q", bad)
		}
	}
}

type (
	fcAfter struct{}
	fcOnce  struct{}
	fcEnd   struct{}
)

// {blockedCause} is empty in a state entered by an ordinary route, and clears
// after the next transition out of a state entered by a Blocked route.
func TestBlockedCauseOnlyOnBlockedEntry(t *testing.T) {
	build := func(name string) *wf.Def {
		d := wf.New(name)
		a := wf.NewAgent[fbAsk, clAnswer](d)
		blk := wf.NewCompute[fbBlocker, fbCause](d, func(c wf.Ctx) (fbCause, error) { return fbCause{Cause: wf.BlockedCause(c)}, nil })
		after := wf.NewCompute[fcAfter, fbCause](d, func(c wf.Ctx) (fbCause, error) { return fbCause{Cause: wf.BlockedCause(c)}, nil })
		done := wf.NewTerminal[fcEnd](d, wf.Success)
		held := wf.NewTerminal[fbHeld](d, wf.Stopped)
		a.Spec(wf.PromptText("Go."), wf.Retry[fbAsk](wf.Const(2), a.To(held, "gave up")).OnClass(wf.Blocked, a.To(blk, "blocked")))
		next := a.To(blk, "ok")
		a.Route(func(clAnswer, wf.Ctx) wf.Edge[fbAsk] { return next }, next)
		onward := blk.To(after, "onward")
		blk.Route(func(fbCause, wf.Ctx) wf.Edge[fbBlocker] { return onward }, onward)
		fin := after.To(done, "fin")
		after.Route(func(fbCause, wf.Ctx) wf.Edge[fcAfter] { return fin }, fin)
		return d
	}
	causes := func(h *harness, run wf.RunID) []string {
		var out []string
		for _, r := range h.kind(run, wf.JournalAccepted) {
			var c struct{ Output fbCause }
			if err := json.Unmarshal(r.Data, &c); err != nil {
				t.Fatal(err)
			}
			out = append(out, c.Output.Cause)
		}
		return out
	}
	t.Run("ordinary", func(t *testing.T) {
		d := build("fc-cause-ordinary")
		h := newHarness(t, d, newWorld())
		run, unit := startAgent(t, h, d, nil)
		if _, err := h.e.Done(run, unit.Name, wf.Submission{Output: []byte(`{"OK":true}`)}); err != nil {
			t.Fatal(err)
		}
		got := causes(h, run)
		if len(got) != 3 || got[1] != "" || got[2] != "" {
			t.Fatalf("causes = %q, want empty for both computes after an ordinary route", got)
		}
	})
	t.Run("blocked then cleared", func(t *testing.T) {
		d := build("fc-cause-blocked")
		h := newHarness(t, d, newWorld())
		run, unit := startAgent(t, h, d, nil)
		if _, err := h.e.FailedAs(run, unit.Name, "BLOCKED_X: no network", wf.FailBlocked); err != nil {
			t.Fatal(err)
		}
		got := causes(h, run)
		if len(got) != 2 || got[0] != "BLOCKED_X: no network" || got[1] != "" {
			t.Fatalf("causes = %q, want the cause on the entered state and cleared after the next transition", got)
		}
	})
}

// A Blocked route is taken even when retry budget remains, and spends none.
func TestBlockedRouteIgnoresRetryBudget(t *testing.T) {
	d := wf.New("fc-budget")
	a := wf.NewAgent[fbAsk, clAnswer](d)
	held := wf.NewTerminal[fbHeld](d, wf.Stopped)
	gaveUp := wf.NewTerminal[fcEnd](d, wf.Stopped)
	a.Spec(wf.PromptText("Go."), wf.Retry[fbAsk](wf.Const(5), a.To(gaveUp, "gave up")).OnClass(wf.Blocked, a.To(held, "blocked")))
	next := a.To(held, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[fbAsk] { return next }, next)
	h := newHarness(t, d, newWorld())
	run, unit := startAgent(t, h, d, nil)
	if _, err := h.e.FailedAs(run, unit.Name, "BLOCKED_X: no network", wf.FailBlocked); err != nil {
		t.Fatal(err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "fbHeld")
	var rd wf.RefusedData
	refused := h.kind(run, wf.JournalRefused)
	if len(refused) != 1 || json.Unmarshal(refused[0].Data, &rd) != nil || rd.Spent || rd.Plan == nil {
		t.Fatalf("refused = %+v, want one Blocked record routed with no retry spent", refused)
	}
	if n := len(h.kind(run, wf.JournalAttemptStarted)); n != 1 {
		t.Fatalf("%d attempts started, want only the first", n)
	}
}
