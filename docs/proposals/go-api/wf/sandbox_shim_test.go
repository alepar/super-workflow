package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// fakeBD puts a bd first on PATH that records its arguments and its view of
// bd's storage overrides in the returned log, one call per line.
func fakeBD(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"args=$* db=${BEADS_DB:-} host=${BD_DOLT_HOST:-} actor=${BD_ACTOR:-} metrics=${BD_DISABLE_METRICS:-}\" >> " + log + "\necho '[]'\n"
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))
	return log
}

// A sandboxed run's bd shim runs the real bd, by absolute path, with
// --sandbox first, drops bd's storage overrides but not the actor name, and
// refuses the flags that move the database (bd config review round 1).
func TestTheSandboxShimRunsBdSandboxed(t *testing.T) {
	log := fakeBD(t)
	t.Setenv("BEADS_DB", "/elsewhere/beads.db")
	t.Setenv("BD_DOLT_HOST", "10.0.0.2")
	t.Setenv("BD_ACTOR", "fork-agent")
	bin, err := wf.WriteSandboxShim(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A fork's shim is found first, even with another fork's shim on PATH.
	t.Setenv("PATH", bin+string(filepath.ListSeparator)+os.Getenv("PATH"))
	if again, err := wf.WriteSandboxShim(t.TempDir()); err != nil {
		t.Fatal(err)
	} else if b, _ := os.ReadFile(filepath.Join(again, "bd")); strings.Contains(string(b), bin) {
		t.Fatalf("a shim resolved another shim as the real bd:\n%s", b)
	}
	run := func(script string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = wf.SandboxEnv(bin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("bd update x-1 --status open"); err != nil {
		t.Fatalf("bd through the shim: %v: %s", err, out)
	}
	calls, _ := os.ReadFile(log)
	if want := "args=--sandbox update x-1 --status open db= host= actor=fork-agent metrics=1\n"; string(calls) != want {
		t.Errorf("the real bd saw %q, want %q", calls, want)
	}
	for _, flag := range []string{"--db /elsewhere/beads.db", "--database=shared", "--global"} {
		os.WriteFile(log, nil, 0o644)
		out, err := run("bd list " + flag)
		if err == nil || !strings.Contains(out, "a sandboxed run refuses it") {
			t.Errorf("%s: %v: %s, want a refusal", flag, err, out)
		}
		if calls, _ := os.ReadFile(log); len(calls) != 0 {
			t.Errorf("%s reached the real bd: %s", flag, calls)
		}
	}
	for _, argv0 := range []string{"bd", "/usr/local/bin/bd", "./bin/bd"} {
		if got := wf.SandboxArgv0(argv0, bin); got != filepath.Join(bin, "bd") {
			t.Errorf("%s launches %q, want the shim", argv0, got)
		}
	}
	if got := wf.SandboxArgv0("/usr/bin/bdx", bin); got != "/usr/bin/bdx" {
		t.Errorf("bdx launches %q", got)
	}
}

// A shipped shell route (a script calling bd, as the fork's cursor_repair
// does) in a fork reaches bd with --sandbox and without the storage
// overrides; in its source it reaches bd as it is (bd config review round 1).
func TestAForksShellEffectsCallBdSandboxed(t *testing.T) {
	log := fakeBD(t)
	t.Setenv("BEADS_DB", "/elsewhere/beads.db")
	d := wf.New("sandbox-shell")
	a := wf.NewAgent[sbAsk, clAnswer](d)
	done := wf.NewTerminal[sbDone](d, wf.Success)
	stop := wf.NewTerminal[sbStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	a.Spec(wf.PromptText("Say hello."), wf.Retry[sbAsk](wf.Const(2), a.To(stop, "gave up")))
	abs, err := exec.LookPath("bd")
	if err != nil {
		t.Fatal(err)
	}
	next := a.To(done, "ok").With(
		wf.ExternalEffect{Adapter: "shell", Op: "run", Args: map[string]any{"Argv": []any{"sh", "-c", "bd update x-1 --status open"}}, Key: wf.KeyOf("repair")},
		// An installed bd named by its absolute path (review round 2).
		wf.ExternalEffect{Adapter: "shell", Op: "run", Args: map[string]any{"Argv": []any{abs, "close", "x-1"}}, Key: wf.KeyOf("close")})
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[sbAsk] { return next }, next)
	reg := wf.NewAdapters()
	if err := reg.Register(shell.Runtime(shell.Config{})); err != nil {
		t.Fatal(err)
	}
	cfg := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: d}}}
	e := newCLIEnv(t)
	must := func(args ...string) string {
		t.Helper()
		var o, er bytes.Buffer
		if code := fixtureCLI(cfg, args, &o, &er); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, er.String())
		}
		return o.String()
	}
	finish := func(run string) {
		t.Helper()
		var ins []wf.Instruction
		if err := json.Unmarshal([]byte(must("next", run, "1", "--state-dir", e.state, "--json")), &ins); err != nil || len(ins) != 1 {
			t.Fatalf("next %s: %v", run, err)
		}
		must("dev", "started", run, ins[0].Name, "--session", "s", "--agent", "a", "--state-dir", e.state)
		must("done", run, ins[0].Name, "--output", writeAllocated(t, ins[0].Output, `{"OK":true}`), "--state-dir", e.state)
	}
	must("run", d.Name(), "--run-id", "src", "--state-dir", e.state, "--workspace", e.ws)
	finish("src")
	fork := strings.TrimSpace(must("dev", "rewind", "src", "--to", "died", "--state-dir", e.state))
	finish(fork)
	calls, _ := os.ReadFile(log)
	want := "args=update x-1 --status open db=/elsewhere/beads.db host= actor= metrics=\n" +
		"args=close x-1 db=/elsewhere/beads.db host= actor= metrics=\n" +
		"args=--sandbox update x-1 --status open db= host= actor= metrics=1\n" +
		"args=--sandbox close x-1 db= host= actor= metrics=1\n"
	if string(calls) != want {
		t.Errorf("bd calls:\n%s\nwant the source's as is, then the fork's sandboxed:\n%s", calls, want)
	}
}
