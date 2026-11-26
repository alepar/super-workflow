package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Config configures the shell runtime.
type Config struct {
	// Timeout bounds one command; 10 minutes when zero.
	Timeout time.Duration
}

// Runtime returns the runtime serving shell.run for both Read and Apply. The
// command runs by argv, never through a shell, in the call's dir (relative to
// the workspace) or the workspace itself.
func Runtime(c Config) wf.AdapterRuntime {
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Minute
	}
	return runtime{timeout: c.Timeout}
}

type runtime struct{ timeout time.Duration }

func (runtime) Ops() []string { return []string{"shell.run"} }

func (r runtime) Read(workspace string, d wf.ObservationDesc) (json.RawMessage, error) {
	return r.run(workspace, d.Args, "")
}

func (r runtime) Apply(workspace string, e wf.ExternalEffect) (json.RawMessage, error) {
	return r.run(workspace, e.Args, "")
}

// ReadAt and ApplyAt run a sandboxed run's command with its bd shim first on
// PATH (wf.WriteSandboxShim), so a bd it calls, directly or from a script,
// runs with --sandbox.
func (r runtime) ReadAt(workspace string, at wf.EffectContext, d wf.ObservationDesc) (json.RawMessage, error) {
	bin, err := sandboxBin(at)
	if err != nil {
		return nil, err
	}
	return r.run(workspace, d.Args, bin)
}

func (r runtime) ApplyAt(workspace string, at wf.EffectContext, e wf.ExternalEffect) (json.RawMessage, error) {
	bin, err := sandboxBin(at)
	if err != nil {
		return nil, err
	}
	return r.run(workspace, e.Args, bin)
}

// sandboxBin is at's shim directory, "" for a run that is not sandboxed; a
// sandboxed context without one is refused.
func sandboxBin(at wf.EffectContext) (string, error) {
	if !at.Sandboxed {
		return "", nil
	}
	if at.SandboxBin == "" {
		return "", fmt.Errorf("shell.run: %w", wf.ErrNoSandboxShim)
	}
	return at.SandboxBin, nil
}

// CheckAt is Check: the answer depends only on the key, so a sandboxed run's
// check is answered the same way (and is not refused for want of a context).
func (r runtime) CheckAt(workspace string, _ wf.EffectContext, e wf.ExternalEffect) (wf.EffectStatus, error) {
	return r.Check(workspace, e)
}

// Check cannot tell whether a command ran. A keyed command is not done, so it
// re-runs (the caller claimed it safe to), per the comment on Effect; a
// NonIdempotent one is unknown and routes to manual verification.
func (runtime) Check(_ string, e wf.ExternalEffect) (wf.EffectStatus, error) {
	if e.KeyID() == "" {
		return wf.EffectUnknown, nil
	}
	return wf.EffectNotDone, nil
}

func (r runtime) run(workspace string, args map[string]any, sandboxBin string) (json.RawMessage, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var a struct {
		Argv  []string
		Dir   string
		Stdin string
		// Timeout bounds this call instead of the runtime's default: the max of
		// a .Detached() command run inline. Its expiry is a timed-out result
		// (exit 124, as timeout(1) and the detached runner report it), not an
		// error.
		Timeout string
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("shell.run: %w", err)
	}
	if len(a.Argv) == 0 {
		return nil, errors.New("shell.run: empty argv")
	}
	if sandboxBin != "" {
		a.Argv = append([]string{wf.SandboxArgv0(a.Argv[0], sandboxBin)}, a.Argv[1:]...)
	}
	if !strings.Contains(a.Argv[0], "/") {
		if err := wf.RequireCLI(a.Argv[0]); err != nil {
			return nil, err
		}
	}
	dir := workspace
	if a.Dir != "" && a.Dir != "." {
		dir = a.Dir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(workspace, dir)
		}
	}

	timeout, bounded := r.timeout, false
	if a.Timeout != "" {
		d, err := time.ParseDuration(a.Timeout)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("shell.run: Timeout %q: want a positive duration", a.Timeout)
		}
		timeout, bounded = d, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.Argv[0], a.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = commandEnv(sandboxBin)
	cmd.SysProcAttr = procAttr()
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if a.Stdin != "" {
		cmd.Stdin = strings.NewReader(a.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	res := wf.RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var ee *exec.ExitError
	switch {
	case runErr == nil, errors.Is(runErr, exec.ErrWaitDelay):
		// Exited 0. A descendant holding the pipes past WaitDelay does not change
		// how the command ended.
		return json.Marshal(res)
	case ctx.Err() != nil && bounded:
		res.Exit = 124
		res.Stderr += fmt.Sprintf("\ntimed out after %v", timeout)
		return json.Marshal(res)
	case ctx.Err() != nil:
		// Checked before ExitError: a killed command is an ExitError too.
		return nil, fmt.Errorf("shell.run %s: timed out after %v\nstdout: %s\nstderr: %s",
			a.Argv[0], timeout, tail(res.Stdout), tail(res.Stderr))
	case errors.As(runErr, &ee):
		res.Exit = ee.ExitCode()
		return json.Marshal(res)
	}
	return nil, fmt.Errorf("shell.run %s: %w", a.Argv[0], runErr)
}

// repoSelectors are the inherited variables that would point a command's git
// at a repository other than its workspace's: an engine started from a git
// hook, or with GIT_DIR exported for a worktree, inherits them. The workspace
// defines the repository; a command that wants another names it in its own
// argv (git -C). Identity, credential, transport and config variables
// (GIT_AUTHOR_*, GIT_COMMITTER_*, GIT_SSH*, GIT_ASKPASS, GIT_TERMINAL_PROMPT,
// GIT_CONFIG_*) pass through.
var repoSelectors = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true,
	"GIT_COMMON_DIR": true, "GIT_NAMESPACE": true, "GIT_PREFIX": true,
}

// commandEnv is the environment a workflow command runs with: this process's,
// with the bd shim first on PATH for a sandboxed run, less repoSelectors.
func commandEnv(sandboxBin string) []string {
	env := os.Environ()
	if sandboxBin != "" {
		env = wf.SandboxEnv(sandboxBin)
	}
	kept := env[:0:0]
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); !repoSelectors[name] {
			kept = append(kept, kv)
		}
	}
	return kept
}

// procAttr gives an inline command its own process group, so a timeout kills
// its children too, and, where the kernel allows, a parent-death signal, so it
// dies with its CLI call.
func procAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	setPdeathsig(attr)
	return attr
}

// tail keeps the last 4000 bytes, so the journal gets the failure's end.
func tail(s string) string {
	const max = 4000
	if len(s) > max {
		return s[len(s)-max:]
	}
	return s
}

// CheckCommandDirectory validates the real filesystem before a recovered
// Command is launched. Fake runtimes own their own directory semantics.
func (runtime) CheckCommandDirectory(workspace, dir string) error {
	if dir == "" || dir == "." {
		dir = workspace
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(workspace, dir)
	}
	if dir == "" {
		dir = "."
	}
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: command working directory %s does not exist", wf.ErrUnavailable, dir)
	case err == nil && !info.IsDir():
		return fmt.Errorf("%w: command working directory %s is not a directory", wf.ErrUnavailable, dir)
	case err != nil:
		return fmt.Errorf("%w: command working directory %s: %v", wf.ErrUnavailable, dir, err)
	}
	return nil
}
