package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// slowWorld is clWorld whose shell "sleep N" effect takes N seconds to apply,
// so a done drives a long chain under the run lock.
type slowWorld struct{ clWorld }

func (w slowWorld) Apply(op string, x wf.ExternalEffect) (json.RawMessage, error) {
	if argv, _ := x.Args["argv"].([]any); len(argv) >= 2 && argv[0] == "sleep" {
		if d, err := time.ParseDuration(argv[1].(string) + "s"); err == nil {
			time.Sleep(d)
		}
	}
	return w.clWorld.Apply(op, x)
}

// busyDef fans out two agent units; the first unit's accepted result takes
// an edge whose effect runs for the given seconds, under its done's lock.
func busyDef(seconds string) *wf.Def {
	child := wf.New("busy-item")
	w := wf.NewAgent[chWork, chOut](child)
	ok := wf.NewTerminal[chOK](child, wf.Success)
	bad := wf.NewTerminal[chBad](child, wf.Stopped)
	child.OnFailure(wf.Blocked, bad)
	w.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[chWork](wf.Const(1), w.To(bad, "gave up")))
	toOK := w.To(ok, "ok").With(shell.Effect(wf.KeyOf("post-chain"), shell.Run("sleep", seconds)))
	toBad := w.To(bad, "bad")
	w.Route(func(o chOut, _ wf.Ctx) wf.Edge[chWork] {
		if o.OK {
			return toOK
		}
		return toBad
	}, toOK, toBad)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)

	d := wf.New("busy-lock")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return string(rune('0' + i.N)) }, wf.Typed[chItem, chRes](child))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	return d
}

// A busy lock is never a child's problem: one unit's done holds the
// run for a 30 s chain, longer than the old 10 s lock wait, and the other
// unit's prompt, from a caller whose harness reads no tool limit (as Codex),
// waits it out and succeeds.
func TestPromptWaitsOutADoneHoldingTheRun(t *testing.T) {
	if testing.Short() {
		t.Skip("holds the run for 30 s")
	}
	for _, k := range wf.HarnessEnvVars() {
		t.Setenv(k, "")
	}
	e := newCLIEnv(t)
	d := busyDef("30")
	reg := wf.NewAdapters()
	if err := reg.Register(slowWorld{}); err != nil {
		t.Fatal(err)
	}
	cfg := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: d}}}
	call := func(args ...string) (int, string, string) {
		var out, er bytes.Buffer
		code := fixtureCLI(cfg, args, &out, &er)
		return code, out.String(), er.String()
	}
	must := func(args ...string) string {
		t.Helper()
		code, out, er := call(args...)
		if code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, er)
		}
		return out
	}
	run := strings.TrimSpace(must("run", d.Name(), "--run-id", "busy", "--state-dir", e.state, "--workspace", e.ws))
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(must("next", run, "2", "--state-dir", e.state, "--json")), &ins); err != nil || len(ins) != 2 {
		t.Fatalf("next: %v: %+v", err, ins)
	}
	first, second := ins[0], ins[1]
	for _, in := range ins {
		must("dev", "started", run, in.Name, "--session", "s", "--agent", in.Name, "--state-dir", e.state)
	}
	withBody(t, must("prompt", run, first.Name, "--state-dir", e.state))
	writeAllocated(t, first.Output, `{"OK":true}`)

	var wg sync.WaitGroup
	wg.Add(1)
	var doneCode int
	var doneErr string
	go func() {
		defer wg.Done()
		doneCode, _, doneErr = call("done", run, first.Name, "--state-dir", e.state)
	}()
	// Let done take the lock and start its chain.
	lockFile := e.state + "/runs/" + run + "/lock"
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		if b, _ := os.ReadFile(lockFile); bytes.Contains(b, []byte(`"verb":"done"`)) {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("done never took the run lock")
		}
	}
	start := time.Now()
	code, out, er := call("prompt", run, second.Name, "--state-dir", e.state)
	waited := time.Since(start)
	wg.Wait()
	if doneCode != 0 {
		t.Fatalf("done exited %d: %s", doneCode, doneErr)
	}
	if code != 0 {
		t.Fatalf("prompt under a done's 30 s chain exited %d after %v: %s", code, waited, er)
	}
	if !strings.Contains(withBody(t, out), "do ") {
		t.Errorf("prompt printed no prompt: %q", out)
	}
	if waited < 20*time.Second {
		t.Errorf("prompt took %v; the test meant it to wait out the chain", waited)
	}
	// The holder is the kernel's pid when /proc/locks shows one; a drive
	// between two steps may have just let the lock go when the line is
	// written, and then the holder is unknown. Either way the line names
	// the last writer.
	if !strings.Contains(er, "waiting for the run: held by ") || !strings.Contains(er, "(last written by done for unit "+first.Name) {
		t.Errorf("prompt's wait printed no progress line naming the holder: %q", er)
	}
}

// busyEnv makes the test's CLI calls a person's shell (no harness) whose
// lock wait is short.
func busyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "BASH_DEFAULT_TIMEOUT_MS", "HERMES_SESSION_ID", "TERMINAL_TIMEOUT", "CODEX_THREAD_ID", "CODEX_SESSION_ID"} {
		t.Setenv(k, "")
	}
	t.Cleanup(wf.SetFileLockWait(300 * time.Millisecond))
}

// harnessCall runs one CLI call as a Claude Code coordinator session whose
// Bash limit gives a 300 ms budget, so its refusal ends with its tail.
func harnessCall(cfg wf.MainConfig, args ...string) (int, string, string) {
	var out, er bytes.Buffer
	code := wf.CLIWithEnv(cfg, args, &out, &er, func(k string) string {
		switch k {
		case "CLAUDECODE":
			return "1"
		case "CLAUDE_CODE_SESSION_ID":
			return "s"
		case "BASH_DEFAULT_TIMEOUT_MS":
			return "600"
		}
		return os.Getenv(k)
	})
	return code, out.String(), er.String()
}

// holdFrom takes run's lock from another journal value and keeps it until
// the test ends.
func holdFrom(t *testing.T, state string, run wf.RunID) {
	t.Helper()
	unlock, err := wf.FileJournal(state).Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
}

// run's status read after its journal is durable reads lock-free:
// a lock held from then on does not stop run.
func TestRunFinishesWithItsRunHeldAfterItsJournalIsDurable(t *testing.T) {
	busyEnv(t)
	e := newCLIEnv(t)
	cfg := clConfig()
	defer wf.SetBetweenLocksHook(func(cmd string, run wf.RunID) {
		if cmd == "run" {
			holdFrom(t, e.state, run)
		}
	})()
	code, out, stderr := harnessCall(cfg, "run", "agent-cli", "--run-id", "partial", "--state-dir", e.state, "--workspace", e.ws)
	if code != 0 || strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]) != "partial" {
		t.Fatalf("run with its run held after its journal was durable: exit %d: %q %s", code, out, stderr)
	}
}

// A call that became busy after it had recorded something did not "change
// nothing": it says what it recorded in which run, prints that run's status
// command, and never says to run the same command again.
// dev started --escalated records the escalations under one lock and the
// start under the next.
func TestStartedBusyAfterItsEscalationsReportsWhatItRecorded(t *testing.T) {
	busyEnv(t)
	e := newCLIEnv(t)
	cfg := clConfig()
	call := func(args ...string) (int, string, string) {
		var out, er bytes.Buffer
		code := fixtureCLI(cfg, args, &out, &er)
		return code, out.String(), er.String()
	}
	if code, _, er := call("run", "agent-cli", "--run-id", "partial", "--state-dir", e.state, "--workspace", e.ws); code != 0 {
		t.Fatal(er)
	}
	var ins []wf.Instruction
	_, out, _ := call("next", "partial", "1", "--state-dir", e.state, "--json")
	if err := json.Unmarshal([]byte(out), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("next: %v %s", err, out)
	}
	defer wf.SetBetweenLocksHook(func(cmd string, run wf.RunID) {
		if cmd == "started" {
			holdFrom(t, e.state, run)
		}
	})()
	code, _, stderr := call("dev", "started", "partial", ins[0].Name, "--session", "s", "--agent", "a", "--escalated", "git push: network: refused by reviewer", "--state-dir", e.state)
	if code != 6 {
		t.Fatalf("started: exit %d, want 6: %s", code, stderr)
	}
	for _, want := range []string{
		"started: the run became busy before this call finished, held by ",
		"this call had already recorded degraded for run partial, so it did not finish",
		" status partial --state-dir " + e.state + "\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	for _, gone := range []string{"nothing was changed", wf.BusyRunTail} {
		if strings.Contains(stderr, gone) {
			t.Errorf("stderr says %q after recording its escalations:\n%s", gone, stderr)
		}
	}
}

// done records its --escalated reports and its result under one lock
// : busy before that lock, it records neither and exits 9,
// and the same command, --escalated included, then succeeds and records each
// report once.
func TestDoneEscalationsAndResultShareOneLock(t *testing.T) {
	busyEnv(t)
	e := newCLIEnv(t)
	cfg := clConfig()
	call := func(args ...string) (int, string, string) {
		var out, er bytes.Buffer
		code := fixtureCLI(cfg, args, &out, &er)
		return code, out.String(), er.String()
	}
	must := func(args ...string) string {
		t.Helper()
		code, out, er := call(args...)
		if code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, er)
		}
		return out
	}
	escalated := func() int {
		n := 0
		for _, line := range strings.Split(must("journal", "esc", "--state-dir", e.state), "\n") {
			if strings.Contains(line, `"Kind":"escalated"`) {
				n++
			}
		}
		return n
	}
	run := strings.TrimSpace(must("run", "agent-cli", "--run-id", "esc", "--state-dir", e.state, "--workspace", e.ws))
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(must("next", run, "1", "--state-dir", e.state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("next: %v %+v", err, ins)
	}
	in := ins[0]
	must("dev", "started", run, in.Name, "--session", "s", "--agent", "a", "--state-dir", e.state)
	withBody(t, must("prompt", run, in.Name, "--state-dir", e.state))
	writeAllocated(t, in.Output, `{"OK":true}`)
	var release func()
	restore := wf.SetBetweenLocksHook(func(cmd string, r wf.RunID) {
		if cmd == "done" {
			unlock, err := wf.FileJournal(e.state).Lock(r)
			if err != nil {
				t.Fatal(err)
			}
			release = unlock
		}
	})
	done := []string{"done", run, in.Name, "--state-dir", e.state, "--escalated", "git push: network: refused by reviewer"}
	code, _, er := call(done...)
	restore()
	release()
	if code != wf.ExitBusy || !strings.Contains(er, "done: the run is busy, held by ") || !strings.Contains(er, "; nothing was changed") {
		t.Fatalf("done on a busy run: exit %d, want %d: %s", code, wf.ExitBusy, er)
	}
	if n := escalated(); n != 0 {
		t.Fatalf("a busy done recorded %d escalations, want none", n)
	}
	must(done...)
	if n := escalated(); n != 1 {
		t.Errorf("done run again recorded %d escalations, want 1", n)
	}
}

// A rewind's work after it published its fork only reads the fork, lock-free
// : a lock held on the fork from then on does not stop it.
func TestRewindFinishesWithItsForkHeld(t *testing.T) {
	busyEnv(t)
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	in := e.next(run, "1")[0]
	e.must("dev", "started", string(run), in.Name, "--session", "s1", "--agent", "a1", "--state-dir", e.state)
	e.must("done", string(run), in.Name, "--output", writeAllocated(t, in.Output, `{"OK":true}`), "--state-dir", e.state)
	var fork wf.RunID
	defer wf.SetBetweenLocksHook(func(cmd string, r wf.RunID) {
		if cmd == "dev rewind" {
			fork = r
			holdFrom(t, e.state, r)
		}
	})()
	code, _, er := e.cli("dev", "rewind", string(run), "--to", "died", "--state-dir", e.state)
	if code != 0 || fork == "" || !strings.Contains(er, "carry on: adopt the fork with:") {
		t.Fatalf("rewind with its fork held: exit %d, fork %q: %s", code, fork, er)
	}
}

// A call that changed nothing exits 9, distinct from exit 6, with the
// run-it-again tail; and a child is told so in both the fetched rules and
// the paged prompt's page, apart from the stop on other failures.
func TestBusyRunExitsNineAndChildRulesSayRunItAgain(t *testing.T) {
	busyEnv(t)
	e := newCLIEnv(t)
	cfg := clConfig()
	var out, er bytes.Buffer
	if code := fixtureCLI(cfg, []string{"run", "agent-cli", "--run-id", "nine", "--state-dir", e.state, "--workspace", e.ws}, &out, &er); code != 0 {
		t.Fatalf("run: %d %s", code, er.String())
	}
	var ins []wf.Instruction
	out.Reset()
	if code := fixtureCLI(cfg, []string{"next", "nine", "1", "--state-dir", e.state, "--json"}, &out, &er); code != 0 || json.Unmarshal(out.Bytes(), &ins) != nil || len(ins) != 1 {
		t.Fatalf("next: %d %s", code, er.String())
	}
	out.Reset()
	if code := fixtureCLI(cfg, []string{"prompt", "nine", ins[0].Name, "--state-dir", e.state}, &out, &er); code != 0 {
		t.Fatalf("prompt: %d %s", code, er.String())
	}
	// Both surfaces, each on its own: the page prompt prints, and the
	// finishing rules in the body file it names.
	page := out.String()
	body := strings.TrimPrefix(withBody(t, page), page)
	if body == "" {
		t.Fatalf("prompt names no body file: %s", page)
	}
	if want := "- Exit 9 (busy): rerun. Attempt spent: tell your coordinator. Else use failed, saying what you need."; !strings.Contains(page, want) {
		t.Errorf("the child page lacks the busy exception %q:\n%s", want, page)
	}
	if want := "fix your result until it says ok"; !strings.Contains(page, want) {
		t.Errorf("the child page lacks %q between validate attempts:\n%s", want, page)
	}
	if want := "When one exits 9 (the run was busy, nothing was changed), run the same command again. When done says this attempt is spent, the run has recorded it: do not run failed; tell your coordinator the output and end your task. On any other non-zero exit of done, run the failed command above with --reason saying what blocked you and what you need, so the run can route it; if failed itself exits non-zero, tell your coordinator its output."; !strings.Contains(body, want) {
		t.Errorf("the child's finishing rules lack the busy exception %q", want)
	}

	holdFrom(t, e.state, "nine")
	// retry writes the run, so it waits for the lock (status reads it
	// lock-free).
	code, _, stderr := harnessCall(cfg, "retry", "nine", "--state-dir", e.state)
	if code != wf.ExitBusy {
		t.Fatalf("retry of a busy run: exit %d, want %d: %s", code, wf.ExitBusy, stderr)
	}
	if !strings.Contains(stderr, "retry: the run is busy, held by ") || !strings.Contains(stderr, "; nothing was changed") || !strings.Contains(stderr, wf.BusyRunTail) || strings.Contains(stderr, "stop and report") {
		t.Errorf("busy retry: %s", stderr)
	}
}
