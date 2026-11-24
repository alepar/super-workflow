package wf

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const turnBindingDir = "super-workflow/coordinator-stop"

func (c *cli) turnBindingRoot(create bool) (*os.Root, error) {
	var state string
	if c.env != nil {
		state = c.env("XDG_STATE_HOME")
	}
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		state = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(state) {
		return nil, fmt.Errorf("coordinator bindings require an absolute user state directory")
	}
	dir := filepath.Join(state, turnBindingDir)
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return os.OpenRoot(dir)
}

type turnBinding struct {
	StateRoot  string
	Workspace  string
	Run        RunID
	Assets     string
	Generation int64
}

func turnBindingName(co Coordinator) string {
	return fmt.Sprintf("%x.json", sha256.Sum256([]byte(normalizeHarness(co.Harness)+"\x00"+co.Session)))
}

func readTurnBinding(dir *os.Root, co Coordinator) (turnBinding, error) {
	var binding turnBinding
	f, err := dir.OpenFile(turnBindingName(co), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return binding, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return binding, err
	}
	const bindingLimit = 16 << 10
	if !info.Mode().IsRegular() || info.Size() > bindingLimit {
		return binding, fmt.Errorf("invalid coordinator stop binding")
	}
	raw, err := io.ReadAll(io.LimitReader(f, bindingLimit+1))
	if err != nil {
		return binding, err
	}
	if len(raw) > bindingLimit || json.Unmarshal(raw, &binding) != nil ||
		!filepath.IsAbs(binding.StateRoot) || !filepath.IsAbs(binding.Workspace) || validRunID(binding.Run) != nil || binding.Generation < 0 {
		return turnBinding{}, fmt.Errorf("invalid coordinator stop binding")
	}
	return binding, nil
}

type turnAttachOptions struct {
	initial       bool
	previousRun   RunID
	previousState string
}

func writeTurnBinding(dir *os.Root, co Coordinator, binding turnBinding) error {
	body, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	tmp := ".attach-" + rand.Text()
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer dir.Remove(tmp)
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := dir.Rename(tmp, turnBindingName(co)); err != nil {
		return err
	}
	d, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (c *cli) attachTurnBinding(e *Engine, stateDir string, run RunID, co Coordinator, opts turnAttachOptions) (int64, error) {
	if co.Session == "" {
		return 0, fmt.Errorf("%w: attach-coordinator requires a native session", ErrRejected)
	}
	probe, err := e.MayEndTurn(run, co)
	if err != nil {
		return 0, err
	}
	workspace := probe.WorkingDir
	if !filepath.IsAbs(workspace) {
		return 0, fmt.Errorf("%w: attach-coordinator requires the recorded workspace", ErrRejected)
	}
	dir, err := c.turnBindingRoot(true)
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	// A cross-run switch first invalidates the old generation under its
	// journal lock. Hold this per-session lock across both attachments.
	lock, err := dir.OpenFile(turnBindingName(co)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return 0, fmt.Errorf("another coordinator attach is in progress: %w", err)
	}
	old, err := readTurnBinding(dir, co)
	switch {
	case err == nil:
		if opts.initial || opts.previousRun != "" {
			return 0, fmt.Errorf("%w: the session already has a selection; run attach-coordinator without recovery flags", ErrRejected)
		}
	case !errors.Is(err, os.ErrNotExist):
		return 0, err
	case opts.previousRun != "":
		if !journalExists(opts.previousState, opts.previousRun) {
			return 0, fmt.Errorf("%w: cannot identify the previous run's journal", ErrRejected)
		}
		old = turnBinding{StateRoot: opts.previousState, Run: opts.previousRun}
	case !opts.initial:
		return 0, fmt.Errorf("%w: session selection is missing; use --initial for a new session or identify the previous run with --previous-state-dir and --previous-run", ErrRejected)
	}
	if old.Run != "" && (opts.previousRun != "" || old.Run != run || old.StateRoot != stateDir) {
		previous := *c
		previous.workspace, previous.assets = "", old.Assets
		x, err := previous.engine(old.StateRoot, EngineOptions{})
		if err != nil {
			return 0, err
		}
		if err := x.(*Engine).detachCoordinator(old.Run, co, old.Generation, opts.previousRun != ""); err != nil &&
			(opts.previousRun != "" || !errors.Is(err, ErrUnknownRun)) {
			return 0, err
		}
	}
	// Publish the target before appending its attachment. A pending selection
	// is inactive for the hook but identifies the run for retry or transfer,
	// even if the process dies immediately after the append.
	binding := turnBinding{StateRoot: stateDir, Workspace: workspace, Run: run, Assets: c.assets}
	if err := writeTurnBinding(dir, co, binding); err != nil {
		return 0, err
	}
	generation, err := e.AttachCoordinator(run, co)
	if err != nil {
		return 0, err
	}
	binding.Generation = generation
	if err := writeTurnBinding(dir, co, binding); err != nil {
		return 0, fmt.Errorf("coordinator selection is pending for %s; repeat attach-coordinator to finish publishing it: %w", run, err)
	}
	return generation, nil
}

func turnNativeRow() Subcommand {
	return Subcommand{Name: coordinatorStopVerb, Summary: "handle a native coordinator Stop event",
		Flags: []string{"--hook HARNESS", "--workspace DIR"}, handle: (*cli).nativeCoordinatorStop}
}

func turnInstallRow() Subcommand {
	return Subcommand{Name: "install-coordinator-stop", Summary: "install a local coordinator Stop hook for this client",
		Flags: []string{"--hook HARNESS", "--workspace DIR"}, handle: (*cli).installTurnStop}
}

func (c *cli) turnHookFlags(row Subcommand, args []string) (string, string, int) {
	fs := flag.NewFlagSet(row.Name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	var hook, workspace string
	fs.StringVar(&hook, "hook", "", "native harness name (claude or codex)")
	fs.StringVar(&workspace, "workspace", "", "absolute workspace for the binding")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", "", exitOK
		}
		return "", "", exitUsage
	}
	if fs.NArg() != 0 || (hook != "claude" && hook != "codex") || !filepath.IsAbs(workspace) {
		return "", "", c.failf(exitUsage, "%s: use --hook claude|codex and an absolute --workspace", row.Name)
	}
	return hook, filepath.Clean(workspace), exitOK
}

func (c *cli) installTurnStop(row Subcommand, args []string) int {
	hook, workspace, code := c.turnHookFlags(row, args)
	if code != exitOK || hook == "" {
		return code
	}
	bin, err := os.Executable()
	if err == nil {
		err = installCoordinatorStop(hook, workspace, bin, c.cfg.named())
	}
	if err != nil {
		return c.fromError(row.Name, err)
	}
	return exitOK
}

func (c *cli) nativeCoordinatorStop(row Subcommand, args []string) int {
	hook, workspace, code := c.turnHookFlags(row, args)
	if code != exitOK || hook == "" {
		return code
	}
	c.budget(1500 * time.Millisecond)
	continuation, diagnostic := c.nativeTurnResult(hook, workspace, os.Stdin)
	raw, err := renderCoordinatorStop(continuation, diagnostic)
	if err == nil && len(raw) > 0 {
		_, err = c.stdout.Write(raw)
	}
	if err != nil {
		return c.fromError(row.Name, err)
	}
	return exitOK
}

func (c *cli) nativeTurnResult(hook, workspace string, input io.Reader) (continuation, diagnostic string) {
	session, _, active, err := parseCoordinatorStop(hook, input)
	if err != nil {
		return "", "Workflow stop check unavailable: " + err.Error()
	}
	if !active {
		return "", ""
	}
	dir, err := c.turnBindingRoot(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ""
		}
		return "", "Workflow stop binding unavailable: " + err.Error()
	}
	defer dir.Close()
	co := Coordinator{Harness: hook, Session: session}
	binding, err := readTurnBinding(dir, co)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ""
		}
		return "", "Workflow stop binding unavailable: " + err.Error()
	}
	co.ExpectedAdmission = binding.Generation
	if binding.Generation == 0 || binding.Workspace != workspace || !journalExists(binding.StateRoot, binding.Run) {
		return "", ""
	}
	c.assets = binding.Assets
	c.owner = lockOwner{Verb: coordinatorStopVerb, Session: session}
	x, err := c.engine(binding.StateRoot, EngineOptions{})
	if err != nil {
		return "", "Workflow stop check unavailable: " + err.Error()
	}
	e := x.(*Engine)
	decision, err := e.RecordCoordinatorStop(binding.Run, co, rand.Text(), workspace)
	if err != nil {
		return "", "Workflow stop check unavailable: " + err.Error()
	}
	switch decision.Decision {
	case "continue":
		decision.command(e.opts.caller.Executable, binding.StateRoot, binding.Run, binding.Assets, c.cfg.named())
		parts := make([]string, len(decision.Argv))
		for i, arg := range decision.Argv {
			parts[i] = shellQuote(arg)
		}
		return toolWorkdirAdvice(decision.WorkingDir) + "Continue the workflow with:\n" +
			workspaceCommand(hook, decision.WorkingDir, strings.Join(parts, " ")), ""
	case "allow":
		switch decision.Reason {
		case "continuation_stalled":
			return "", "Workflow run " + string(binding.Run) + " reached its continuation limit at progress sequence " +
				strconv.FormatInt(decision.Progress, 10) + ". An operator can stop and resume the run to open a fresh segment."
		case "operator_boundary":
			return "", "Workflow run " + string(binding.Run) + " is stopped at an operator boundary."
		}
	}
	return "", ""
}
