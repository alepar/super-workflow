package wf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ErrUnavailable marks an adapter call that could not run: its op has no
// runtime or its tool is missing. Any other non-nil error from Adapters means
// the op ran and failed. Callers tell them apart with errors.Is.
var ErrUnavailable = errors.New("adapter unavailable")

// RequireCLI reports a CLI missing from PATH as unavailable. Adapter packages
// call it before invoking their tool.
func RequireCLI(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%w: %s not on PATH", ErrUnavailable, name)
	}
	return nil
}

// ToolTimeout bounds one adapter CLI call when its runtime sets no timeout.
const ToolTimeout = 2 * time.Minute

// Tool is one invocation of an adapter's CLI.
type Tool struct {
	// Env adds per-invocation overrides to the inherited environment.
	Env []string
	// UnsetEnv removes inherited names before applying Env overrides.
	UnsetEnv []string
	Bin      string
	Args     []string
	Dir      string
	Timeout  time.Duration // 0 means ToolTimeout
}

// ToolResult is what the tool printed and its exit code.
type ToolResult struct {
	Stdout, Stderr string
	Exit           int
}

// RunTool runs an adapter's CLI for the Engine.
//
// Owns: the exec.CommandContext timeout, running the tool in its own process
// group (a timeout kills the tool and its children), and reporting a missing
// tool or a timeout as ErrUnavailable.
// Does not: decode output or treat a non-zero exit as a failure: Exit is data,
// and each adapter decides what it means. It bounds only the calls adapters
// make for the Engine; agents' own tool calls never pass through it, and
// nothing here constrains them.
// Extend by: a new adapter calls RunTool; adapters never call exec.Command.
func RunTool(t Tool) (ToolResult, error) {
	if err := RequireCLI(t.Bin); err != nil {
		return ToolResult{}, err
	}
	timeout := t.Timeout
	if timeout == 0 {
		timeout = ToolTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.Bin, t.Args...)
	cmd.Dir = t.Dir
	if len(t.Env) > 0 || len(t.UnsetEnv) > 0 {
		removed := make(map[string]bool, len(t.UnsetEnv))
		for _, name := range t.UnsetEnv {
			removed[name] = true
		}
		cmd.Env = []string{}
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			if !removed[name] {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, t.Env...)
	}
	// Own process group, so a timeout kills the tool's children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	res := ToolResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var ee *exec.ExitError
	switch {
	case runErr == nil, errors.Is(runErr, exec.ErrWaitDelay):
		// Exited 0. A descendant holding the pipes past WaitDelay does not change
		// how the tool ended.
		return res, nil
	case ctx.Err() != nil:
		// Checked before ExitError: a killed tool is an ExitError too.
		return res, fmt.Errorf("%w: %s %s: timed out after %v", ErrUnavailable, t.Bin, strings.Join(t.Args, " "), timeout)
	case errors.As(runErr, &ee):
		res.Exit = ee.ExitCode()
		return res, nil
	case errors.Is(runErr, exec.ErrNotFound):
		return res, fmt.Errorf("%w: %w", ErrUnavailable, runErr)
	}
	return res, fmt.Errorf("%s %s: %w", t.Bin, strings.Join(t.Args, " "), runErr)
}

// EffectStatus is what a check found about an effect's prior completion.
type EffectStatus int

// Effect check answers. Unknown means completion cannot be established; the
// Engine classes the effect Uncertain and never re-applies it.
const (
	EffectDone EffectStatus = iota + 1
	EffectNotDone
	EffectUnknown
)

// String returns "done", "not done" or "unknown".
func (s EffectStatus) String() string {
	switch s {
	case EffectDone:
		return "done"
	case EffectNotDone:
		return "not done"
	case EffectUnknown:
		return "unknown"
	}
	return fmt.Sprintf("EffectStatus(%d)", int(s))
}

// KeyID is the effect's resolved idempotency key, "" for a NonIdempotent or nil
// key. Runtimes use it to check prior completion.
func (e ExternalEffect) KeyID() string {
	if e.Key == nil {
		return ""
	}
	return e.Key.key()
}

// AdapterRuntime performs one external tool's declared ops.
//
// Owns: the tool's invocation, argv templating and result decoding, in the
// workspace directory it is handed on each call. Results are JSON because the
// Engine journals them before acting on them. A non-zero exit of a command is
// data in the marshalled RunResult; a read or effect that ran and failed is an
// error.
// Does not: journal, retry or route: the Engine does that around each call.
// Extend by: a new tool is a new runtime in its adapter package plus one
// Register call in the composition root; the Engine never calls a CLI itself.
type AdapterRuntime interface {
	// Ops lists the "adapter.op" names this runtime serves. "*" serves every op
	// no runtime names exactly; it is for a fake world keyed by descriptor.
	Ops() []string
	// Read performs an observation.
	Read(workspace string, d ObservationDesc) (json.RawMessage, error)
	// Apply performs an effect.
	Apply(workspace string, e ExternalEffect) (json.RawMessage, error)
	// Check looks for the effect's prior completion by its key.
	Check(workspace string, e ExternalEffect) (EffectStatus, error)
}

// EffectContext identifies the state instance that owns an adapter invocation.
// State is qualified by child definition names, as in Instruction.State;
// Instance distinguishes concurrent entries of the same child definition.
type EffectContext struct {
	Purpose  string // observation purpose; empty for effects
	Run      RunID
	Instance string
	State    string
	// Sandboxed: the run was made by a snapshot restore or a rewind (a
	// fork); its adapters must not reach a real remote. bd runs every call
	// with --sandbox.
	Sandboxed bool `json:",omitempty"`
	// SandboxBin is the directory of a sandboxed run's bd shim, which its
	// subprocesses find first on PATH (WriteSandboxShim); "" when the run is
	// not sandboxed.
	SandboxBin string `json:",omitempty"`
}

// ContextualObservationRuntime optionally receives observation ownership and
// purpose from the engine. Existing runtimes need only Read.
type ContextualObservationRuntime interface {
	ReadAt(workspace string, at EffectContext, d ObservationDesc) (json.RawMessage, error)
}

// ContextualEffectRuntime optionally receives effect ownership from the engine.
// Existing AdapterRuntime implementations need only Apply. The context also
// accompanies recovery of an intent, when the latest journal record may belong
// to a different state or instance.
type ContextualEffectRuntime interface {
	ApplyAt(workspace string, at EffectContext, e ExternalEffect) (json.RawMessage, error)
}

// ContextualCheckRuntime optionally receives the context of a check, as
// ContextualEffectRuntime does of its effect.
type ContextualCheckRuntime interface {
	CheckAt(workspace string, at EffectContext, e ExternalEffect) (EffectStatus, error)
}

// Adapters is the registry mapping adapter.op to its runtime.
//
// Owns: choosing the runtime for a descriptor and classifying "no runtime for
// this op" and "tool missing" as ErrUnavailable, in one place, distinct from
// an op that ran and failed.
// Does not: import adapter packages (they are registered into it) or journal,
// retry or route. It is built once by the composition root and is read-only and
// shareable after that; the workspace is per call, so one registry serves any
// run.
// Extend by: a new tool is one Register call.
type Adapters struct{ ops map[string]AdapterRuntime }

// NewAdapters returns an empty registry.
func NewAdapters() *Adapters { return &Adapters{ops: map[string]AdapterRuntime{}} }

// Register adds every op rt serves. Registering an op twice, or a name that is
// neither adapter.op nor "*", is an error and registers nothing.
func (a *Adapters) Register(rt AdapterRuntime) error {
	seen := map[string]bool{}
	for _, op := range rt.Ops() {
		adapter, name, ok := strings.Cut(op, ".")
		if op != "*" && (!ok || adapter == "" || name == "") {
			return fmt.Errorf("adapter op %q is not adapter.op", op)
		}
		if _, dup := a.ops[op]; dup || seen[op] {
			return fmt.Errorf("adapter op %s registered twice", op)
		}
		seen[op] = true
	}
	for op := range seen {
		a.ops[op] = rt
	}
	return nil
}

func (a *Adapters) lookup(adapter, op string) (AdapterRuntime, error) {
	key := adapter + "." + op
	rt, ok := a.ops[key]
	if !ok {
		rt, ok = a.ops["*"]
	}
	if !ok {
		return nil, fmt.Errorf("%w: no runtime for %s", ErrUnavailable, key)
	}
	return rt, nil
}

// classifyAdapterErr is the one place a runtime's error becomes unavailable: a
// tool that exec could not find is unavailable even if the runtime did not
// call RequireCLI first.
func classifyAdapterErr(err error) error {
	if err != nil && !errors.Is(err, ErrUnavailable) && errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}

// Read performs the observation in workspace.
func (a *Adapters) Read(workspace string, d ObservationDesc) (json.RawMessage, error) {
	rt, err := a.lookup(d.Adapter, d.Op)
	if err != nil {
		return nil, err
	}
	v, err := rt.Read(workspace, d)
	return v, classifyAdapterErr(err)
}

// ReadAt performs an observation with its owning state instance and purpose.
// Runtimes without ContextualObservationRuntime retain ordinary Read behavior.
func (a *Adapters) ReadAt(workspace string, at EffectContext, d ObservationDesc) (json.RawMessage, error) {
	rt, err := a.lookup(d.Adapter, d.Op)
	if err != nil {
		return nil, err
	}
	var v json.RawMessage
	if contextual, ok := rt.(ContextualObservationRuntime); ok {
		v, err = contextual.ReadAt(workspace, at, d)
	} else {
		v, err = rt.Read(workspace, d)
	}
	return v, classifyAdapterErr(err)
}

// Apply performs the effect in workspace.
func (a *Adapters) Apply(workspace string, e ExternalEffect) (json.RawMessage, error) {
	rt, err := a.lookup(e.Adapter, e.Op)
	if err != nil {
		return nil, err
	}
	v, err := rt.Apply(workspace, e)
	return v, classifyAdapterErr(err)
}

// ApplyAt performs an effect with its owning state instance. Runtimes that
// do not implement ContextualEffectRuntime retain the ordinary Apply behavior.
func (a *Adapters) ApplyAt(workspace string, at EffectContext, e ExternalEffect) (json.RawMessage, error) {
	rt, err := a.lookup(e.Adapter, e.Op)
	if err != nil {
		return nil, err
	}
	var v json.RawMessage
	if contextual, ok := rt.(ContextualEffectRuntime); ok {
		v, err = contextual.ApplyAt(workspace, at, e)
	} else {
		v, err = rt.Apply(workspace, e)
	}
	return v, classifyAdapterErr(err)
}

// Check looks for the effect's prior completion in workspace. Any error, and a
// runtime that answers nothing, is EffectUnknown.
func (a *Adapters) Check(workspace string, e ExternalEffect) (EffectStatus, error) {
	rt, err := a.lookup(e.Adapter, e.Op)
	if err != nil {
		return EffectUnknown, err
	}
	st, err := rt.Check(workspace, e)
	if err != nil || st == 0 {
		return EffectUnknown, classifyAdapterErr(err)
	}
	return st, nil
}

// CheckAt is Check with the run's context, for a runtime that takes it.
func (a *Adapters) CheckAt(workspace string, at EffectContext, e ExternalEffect) (EffectStatus, error) {
	rt, err := a.lookup(e.Adapter, e.Op)
	if err != nil {
		return EffectUnknown, err
	}
	var st EffectStatus
	if contextual, ok := rt.(ContextualCheckRuntime); ok {
		st, err = contextual.CheckAt(workspace, at, e)
	} else {
		st, err = rt.Check(workspace, e)
	}
	if err != nil || st == 0 {
		return EffectUnknown, classifyAdapterErr(err)
	}
	return st, nil
}

// WorkspaceRelocator optionally repairs an adapter's filesystem links when
// the caller explicitly supplies a different workspace root for a saved run.
type WorkspaceRelocator interface {
	RelocateWorkspace(from, to string) error
}

func (a *Adapters) relocateWorkspace(from, to string) error {
	for op, rt := range a.ops {
		// A runtime owns all its registered ops; invoke its hook once.
		names := rt.Ops()
		if len(names) == 0 || op != names[0] {
			continue
		}
		if r, ok := rt.(WorkspaceRelocator); ok {
			if err := r.RelocateWorkspace(from, to); err != nil {
				return err
			}
		}
	}
	return nil
}

// CommandDirectoryChecker optionally validates a Command's working directory
// in the runtime's world before recovery launches it again. Real filesystem
// runtimes check the host; simulations may model or omit the check.
type CommandDirectoryChecker interface {
	CheckCommandDirectory(workspace, dir string) error
}

func (a *Adapters) checkCommandDirectory(workspace, dir string) error {
	rt, err := a.lookup("shell", "run")
	if err != nil {
		return nil
	} // A detached runner may operate without an inline runtime.
	if checker, ok := rt.(CommandDirectoryChecker); ok {
		return checker.CheckCommandDirectory(workspace, dir)
	}
	return nil
}
