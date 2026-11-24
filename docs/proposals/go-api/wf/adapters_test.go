package wf_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// fakeRuntime answers every call with its fields and records the workspace.
type fakeRuntime struct {
	ops       []string
	value     json.RawMessage
	err       error
	status    wf.EffectStatus
	workspace string
}

func (f *fakeRuntime) Ops() []string { return f.ops }
func (f *fakeRuntime) Read(ws string, _ wf.ObservationDesc) (json.RawMessage, error) {
	f.workspace = ws
	return f.value, f.err
}
func (f *fakeRuntime) Apply(ws string, _ wf.ExternalEffect) (json.RawMessage, error) {
	f.workspace = ws
	return f.value, f.err
}
func (f *fakeRuntime) Check(ws string, _ wf.ExternalEffect) (wf.EffectStatus, error) {
	f.workspace = ws
	return f.status, f.err
}

func registry(t *testing.T, rts ...wf.AdapterRuntime) *wf.Adapters {
	t.Helper()
	a := wf.NewAdapters()
	for _, rt := range rts {
		if err := a.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

var (
	gitHead  = wf.ObservationDesc{Adapter: "git", Op: "head"}
	bdCreate = wf.ExternalEffect{Adapter: "bd", Op: "create", Key: wf.KeyOf("fix", "1")}
)

func TestClassifiesOkUnavailableAndFailed(t *testing.T) {
	boom := errors.New("boom")
	notFound := exec.Command("wf-no-such-binary-on-path").Run()
	for _, tc := range []struct {
		name        string
		err         error
		unavailable bool
		failed      bool
	}{
		{name: "ok"},
		{name: "runtime error", err: boom, failed: true},
		{name: "RequireCLI", err: wf.RequireCLI("wf-no-such-binary-on-path"), unavailable: true},
		{name: "exec not found", err: notFound, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRuntime{ops: []string{"git.head", "bd.create"}, value: json.RawMessage(`"abc"`), err: tc.err, status: wf.EffectDone}
			a := registry(t, rt)
			_, readErr := a.Read("/ws", gitHead)
			_, applyErr := a.Apply("/ws", bdCreate)
			_, checkErr := a.Check("/ws", bdCreate)
			for call, err := range map[string]error{"Read": readErr, "Apply": applyErr, "Check": checkErr} {
				if got := errors.Is(err, wf.ErrUnavailable); got != tc.unavailable {
					t.Errorf("%s: unavailable = %v (%v)", call, got, err)
				}
				if got := err != nil && !errors.Is(err, wf.ErrUnavailable); got != tc.failed {
					t.Errorf("%s: failed = %v (%v)", call, got, err)
				}
			}
			if tc.failed && !errors.Is(readErr, boom) {
				t.Errorf("the runtime's error is not kept: %v", readErr)
			}
		})
	}
}

func TestUnregisteredOpIsUnavailable(t *testing.T) {
	a := registry(t, &fakeRuntime{ops: []string{"git.head"}})
	if _, err := a.Read("/ws", wf.ObservationDesc{Adapter: "git", Op: "nope"}); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("Read = %v", err)
	}
	if _, err := a.Apply("/ws", bdCreate); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("Apply = %v", err)
	}
	st, err := a.Check("/ws", bdCreate)
	if !errors.Is(err, wf.ErrUnavailable) || st != wf.EffectUnknown {
		t.Errorf("Check = %v, %v", st, err)
	}
}

func TestRequireCLI(t *testing.T) {
	if err := wf.RequireCLI("wf-no-such-binary-on-path"); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("missing CLI: %v", err)
	}
	if err := wf.RequireCLI("sh"); err != nil {
		t.Errorf("sh: %v", err)
	}
}

func TestRegisterRejectsDuplicatesAtomically(t *testing.T) {
	a := registry(t, &fakeRuntime{ops: []string{"git.head"}})
	if err := a.Register(&fakeRuntime{ops: []string{"git.status", "git.head"}}); err == nil {
		t.Fatal("registering git.head twice succeeded")
	}
	if _, err := a.Read("/ws", wf.ObservationDesc{Adapter: "git", Op: "status"}); !errors.Is(err, wf.ErrUnavailable) {
		t.Error("a refused registration left git.status registered")
	}
	if err := wf.NewAdapters().Register(&fakeRuntime{ops: []string{"bd.show", "bd.show"}}); err == nil {
		t.Error("an op listed twice by one runtime succeeded")
	}
	for _, bad := range []string{"", "noop", ".x", "x."} {
		if err := wf.NewAdapters().Register(&fakeRuntime{ops: []string{bad}}); err == nil {
			t.Errorf("op %q registered", bad)
		}
	}
}

func TestCatchAllLosesToExactOp(t *testing.T) {
	exact := &fakeRuntime{ops: []string{"git.head"}, value: json.RawMessage(`"exact"`)}
	catchAll := &fakeRuntime{ops: []string{"*"}, value: json.RawMessage(`"any"`)}
	a := registry(t, catchAll, exact)
	if v, err := a.Read("/ws", gitHead); err != nil || string(v) != `"exact"` {
		t.Errorf("exact op: %s, %v", v, err)
	}
	if v, err := a.Read("/ws", wf.ObservationDesc{Adapter: "fsx", Op: "exists"}); err != nil || string(v) != `"any"` {
		t.Errorf("catch-all: %s, %v", v, err)
	}
	if err := a.Register(&fakeRuntime{ops: []string{"*"}}); err == nil {
		t.Error("a second catch-all registered")
	}
}

func TestEffectCheckStatuses(t *testing.T) {
	for _, st := range []wf.EffectStatus{wf.EffectDone, wf.EffectNotDone, wf.EffectUnknown} {
		a := registry(t, &fakeRuntime{ops: []string{"bd.create"}, status: st})
		if got, err := a.Check("/ws", bdCreate); err != nil || got != st {
			t.Errorf("Check = %v, %v; want %v", got, err, st)
		}
	}
	a := registry(t, &fakeRuntime{ops: []string{"bd.create"}, status: wf.EffectDone, err: errors.New("boom")})
	if got, err := a.Check("/ws", bdCreate); err == nil || got != wf.EffectUnknown {
		t.Errorf("a failed check = %v, %v; want unknown with the error", got, err)
	}
	a = registry(t, &fakeRuntime{ops: []string{"bd.create"}})
	if got, err := a.Check("/ws", bdCreate); err != nil || got != wf.EffectUnknown {
		t.Errorf("a runtime that answers nothing = %v, %v; want unknown", got, err)
	}
}

func TestEffectStatusNames(t *testing.T) {
	for st, want := range map[wf.EffectStatus]string{wf.EffectDone: "done", wf.EffectNotDone: "not done", wf.EffectUnknown: "unknown"} {
		if st.String() != want {
			t.Errorf("%d = %q", st, st)
		}
	}
}

func TestWorkspaceIsPerCall(t *testing.T) {
	rt := &fakeRuntime{ops: []string{"git.head"}}
	a := registry(t, rt)
	for _, ws := range []string{"/a", "/b"} {
		if _, err := a.Read(ws, gitHead); err != nil {
			t.Fatal(err)
		}
		if rt.workspace != ws {
			t.Errorf("workspace = %q, want %q", rt.workspace, ws)
		}
	}
}

func TestKeyID(t *testing.T) {
	for want, e := range map[string]wf.ExternalEffect{
		"fix/1": {Key: wf.KeyOf("fix", "1")},
		"":      {Key: wf.NonIdempotent("once only")},
	} {
		if got := e.KeyID(); got != want {
			t.Errorf("KeyID = %q, want %q", got, want)
		}
	}
	if got := (wf.ExternalEffect{}).KeyID(); got != "" {
		t.Errorf("nil key: %q", got)
	}
}

func hungScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hung")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nsleep 30 & wait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunToolTimesOutAsUnavailable(t *testing.T) {
	start := time.Now()
	_, err := wf.RunTool(wf.Tool{Bin: hungScript(t), Timeout: 200 * time.Millisecond})
	if !errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want it to say timed out", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v, the group kill did not release the call", d)
	}
}

func TestRunToolExitIsData(t *testing.T) {
	res, err := wf.RunTool(wf.Tool{Bin: "sh", Args: []string{"-c", "echo out; echo err >&2; exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exit != 3 || res.Stdout != "out\n" || res.Stderr != "err\n" {
		t.Errorf("res = %+v", res)
	}
}

func TestRunToolMissingIsUnavailable(t *testing.T) {
	if _, err := wf.RunTool(wf.Tool{Bin: "no-such-tool-xyzzy"}); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

// TestAdaptersCallOnlyRunTool holds the helper-only rule: adapter packages
// never start a process themselves.
func TestAdaptersCallOnlyRunTool(t *testing.T) {
	for _, dir := range []string{"../git", "../gh", "../bd"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %d files, %v", dir, len(files), err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{"exec.Command(", "exec.CommandContext("} {
				if strings.Contains(string(b), bad) {
					t.Errorf("%s calls %s; use wf.RunTool", f, bad)
				}
			}
		}
	}
}

func TestContextualApplyFallsBackToExistingRuntime(t *testing.T) {
	rt := &fakeRuntime{ops: []string{"bd.create"}, value: json.RawMessage(`{"ok":true}`), err: exec.ErrNotFound}
	a := registry(t, rt)
	value, err := a.ApplyAt("/workspace", wf.EffectContext{Run: "run", Instance: "child", State: "kid/Work"}, bdCreate)
	if rt.workspace != "/workspace" || string(value) != `{"ok":true}` || !errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("legacy runtime lost behavior: %q %s %v", rt.workspace, value, err)
	}
}

type contextualReadRuntime struct {
	fakeRuntime
	contexts    []wf.EffectContext
	descriptors []wf.ObservationDesc
}

func (r *contextualReadRuntime) ReadAt(ws string, at wf.EffectContext, d wf.ObservationDesc) (json.RawMessage, error) {
	r.contexts = append(r.contexts, at)
	r.descriptors = append(r.descriptors, d)
	r.workspace = ws
	return r.value, r.err
}

func TestContextualObservationRegistryAndLegacyParity(t *testing.T) {
	at := wf.EffectContext{Run: "run", Instance: "child#2", State: "child/Review", Purpose: "check"}
	boom := errors.New("boom")
	for _, cause := range []error{nil, boom, exec.ErrNotFound} {
		for _, contextual := range []bool{false, true} {
			base := fakeRuntime{ops: []string{"git.head"}, value: json.RawMessage(`"abc"`), err: cause}
			var rt wf.AdapterRuntime = &base
			owned := &contextualReadRuntime{fakeRuntime: base}
			if contextual {
				rt = owned
			}
			a := registry(t, rt)
			value, err := a.ReadAt("/workspace", at, gitHead)
			_, legacyErr := a.Read("/workspace", gitHead)
			if string(value) != `"abc"` || (err == nil) != (legacyErr == nil) || errors.Is(err, wf.ErrUnavailable) != errors.Is(legacyErr, wf.ErrUnavailable) || (cause == boom && !errors.Is(err, boom)) {
				t.Fatalf("read parity: %s %v / %v", value, err, legacyErr)
			}
			if contextual {
				if len(owned.contexts) != 1 || owned.contexts[0] != at || owned.descriptors[0].Adapter != "git" || owned.descriptors[0].Op != "head" || owned.workspace != "/workspace" {
					t.Fatalf("context lost: %+v", owned)
				}
			} else if base.workspace != "/workspace" {
				t.Fatal("legacy workspace lost")
			}
		}
	}
	if _, err := wf.NewAdapters().ReadAt("/workspace", at, gitHead); !errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("unregistered: %v", err)
	}
}
