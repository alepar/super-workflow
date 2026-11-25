package shell

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

func hostileJSON(v string) string {
	if utf8.ValidString(v) {
		return v
	}
	b, _ := json.Marshal(v)
	var out string
	_ = json.Unmarshal(b, &out)
	return out
}

// hostilePrinter is a command that prints its arguments NUL-separated.
func hostilePrinter(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "printargv")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\0' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func hostileRead(t *testing.T, ws string, i Invocation) (wf.RunResult, error) {
	t.Helper()
	raw, err := Runtime(Config{}).Read(ws, Observe(i).Desc())
	if err != nil {
		return wf.RunResult{}, err
	}
	var r wf.RunResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return r, nil
}

// An argv element and the stdin text reach the command as given.
func TestHostileShellRunArgvAndStdin(t *testing.T) {
	printer := hostilePrinter(t)
	ws := t.TempDir()
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			want := hostileJSON(in.Value)
			r, err := hostileRead(t, ws, Run(printer, in.Value, "end"))
			switch {
			case strings.ContainsRune(in.Value, 0):
				if err == nil && r.Exit == 0 {
					t.Errorf("argv with NUL ran: %q", r.Stdout)
				}
			case err != nil:
				t.Errorf("argv refused: %v", err)
			case r.Stdout != want+"\x00end\x00":
				t.Errorf("argv %q arrived as %q", want, r.Stdout)
			}
			if in.Value == "" {
				return // empty stdin means no input
			}
			r, err = hostileRead(t, ws, Run("cat").WithStdin(in.Value))
			if err != nil {
				t.Errorf("stdin refused: %v", err)
			} else if r.Stdout != want {
				t.Errorf("stdin %q arrived as %q", want, r.Stdout)
			}
		})
	}
}

// A relative Dir from the definition resolves against the workspace as
// written, .. included: a definition's paths are its author's choice, and
// values from model output go through sink.Under before they become paths.
func TestHostileShellRunDirResolvesAsWritten(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "w1", "w2")
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			v := hostileJSON(in.Value)
			if strings.ContainsRune(v, 0) {
				t.Skip("no directory name holds NUL")
			}
			target := filepath.Join(ws, v)
			if !strings.HasPrefix(target, base+"/") {
				t.Fatalf("%q would leave the test's own directory", v)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			r, err := hostileRead(t, ws, Run("pwd").In(in.Value))
			if err != nil {
				t.Skipf("refused: %v", err)
			}
			if cwd := strings.TrimSuffix(r.Stdout, "\n"); cwd != target {
				t.Errorf("Dir %q ran the command in %q, want %q", v, cwd, target)
			}
		})
	}
}

// The detached runner hands an argv element to the command as given, on
// either launch mode.
func TestHostileDetachedArgvRoundTrip(t *testing.T) {
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	printer := hostilePrinter(t)
	for _, mode := range []string{wf.DetachProcess, wf.DetachSystemd} {
		t.Run(mode, func(t *testing.T) {
			if mode == wf.DetachSystemd {
				skipWithoutSystemd(t)
				withLauncher(t, true)
			}
			r := DetachedRunner(testBin(t))
			for _, in := range hostile.Corpus() {
				t.Run(in.Name, func(t *testing.T) {
					if strings.ContainsRune(in.Value, 0) {
						t.Skip("argv cannot carry NUL")
					}
					req := reqIn(t, t.TempDir(), printer, in.Value, "end")
					req.Mode = mode
					p, err := r.Start(req)
					if err != nil {
						t.Skipf("refused: %v", err)
					}
					t.Cleanup(func() { syscall.Kill(-p.Pgid, syscall.SIGKILL) })
					got := waitStatus(t, r, p, req, wf.DetachedPresent)
					if want := hostileJSON(in.Value) + "\x00end\x00"; got.Stdout != want {
						t.Errorf("argv %q arrived as %q (exit %d, stderr %q)", in.Value, got.Stdout, got.Exit, got.Stderr)
					}
				})
			}
		})
	}
}

// A run directory whose name carries hostile text still launches: the
// result, proc and log paths reach the runner and systemd as given.
func TestHostileDetachedRunDirRoundTrip(t *testing.T) {
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	for _, mode := range []string{wf.DetachProcess, wf.DetachSystemd} {
		t.Run(mode, func(t *testing.T) {
			if mode == wf.DetachSystemd {
				skipWithoutSystemd(t)
				withLauncher(t, true)
			}
			r := DetachedRunner(testBin(t))
			for _, in := range hostile.Corpus() {
				t.Run(in.Name, func(t *testing.T) {
					v := in.Value
					if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/\x00") {
						t.Skip("not a directory name")
					}
					req := reqIn(t, filepath.Join(t.TempDir(), v), "sh", "-c", "echo hi")
					req.Mode = mode
					p, err := r.Start(req)
					if errors.Is(err, wf.ErrNotLaunched) {
						t.Skipf("refused, nothing launched (the ladder falls back): %v", err)
					} else if err != nil {
						t.Errorf("run dir %q: launch failed: %v", v, err)
						return
					}
					t.Cleanup(func() { syscall.Kill(-p.Pgid, syscall.SIGKILL) })
					if got := waitStatus(t, r, p, req, wf.DetachedPresent); got.Stdout != "hi\n" {
						t.Errorf("run dir %q: result %+v", v, got)
					}
					if b, err := os.ReadFile(req.LogPath); err != nil || !strings.Contains(string(b), launchLine) {
						t.Errorf("run dir %q: log %q not written where asked (%v)", v, req.LogPath, err)
					}
				})
			}
		})
	}
}
