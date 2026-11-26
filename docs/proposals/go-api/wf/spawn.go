package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// PerCommandSandboxInit are the pid 1 programs of a sandbox that gives each
// command its own PID namespace: Codex's sandbox shows pid 1 "codex". A
// process started inside one dies with the command, and a headless child
// started inside one has no network (a sandboxed claude -p hangs). The
// detach probe and the spawn verb share the list.
var PerCommandSandboxInit = []string{"bwrap", "codex-linux-sandbox", "codex"}

// SpawnResult is what the spawn verb started.
type SpawnResult struct {
	Unit    string
	Harness string
	Pid     int
	Session string // the session id the launch named, "" when the CLI picks its own
	ErrLog  string
}

// HeadlessProcess starts and checks the processes of headless children.
//
// Owns: how a child process is started (its own session and process group,
// detached from the caller, watched by a supervisor outside any sandbox)
// and how its liveness and end are read and its stop is asked for.
// Does not: choose what runs (the spawn verb builds argv and env) or judge
// the child's work.
//
// The verbs that read a child's state or stop it may run in another PID
// namespace than the child (a Codex coordinator's sandbox), so State and
// Stop work through files under the run's state directory, never through
// the caller's view of pids.
type HeadlessProcess interface {
	// Start runs argv in dir with env, stdin from the file stdin and stdout
	// and stderr appended to their files, in a new session under a
	// supervisor that keeps the status file status; it returns the child's
	// pid and the kernel's start time of it (0 where unknown).
	Start(argv, env []string, dir, stdin, stdout, stderr, status string) (pid int, start uint64, err error)
	// State is the recorded child's state now.
	State(h HeadlessStartedData) HeadlessState
	// Stop asks for the child's process group to be stopped and returns:
	// TERM, then KILL after killGrace if any of it remains.
	Stop(h HeadlessStartedData) error
	// Sandboxed is the per-command sandbox this process runs inside, "" for
	// none: a child started from it would be confined by it.
	Sandboxed() string
}

// HeadlessState is what a headless child's supervisor last said of it.
type HeadlessState int

const (
	// HeadlessRunning: the child runs, by a fresh supervisor heartbeat.
	HeadlessRunning HeadlessState = iota
	// HeadlessExited: the child exited (or its status file is gone).
	HeadlessExited
	// HeadlessUnknown: the heartbeat is stale and no exit was recorded (a
	// suspended host, a killed supervisor). It counts as still running: the
	// unit's lease, not a guess, decides what happens to it.
	HeadlessUnknown
)

// osProcess is the HeadlessProcess of the host: the spawn verb starts this
// binary as the supervisor (HeadlessSupervisorVerb), which owns the child.
type osProcess struct{}

func (osProcess) Start(argv, env []string, dir, stdin, stdout, stderr, status string) (int, uint64, error) {
	return startSupervised(argv, env, dir, stdin, stdout, stderr, status)
}

// State reads the supervisor's files. A record from before the supervisor
// (no Status) is judged by its pid, as it was then.
func (osProcess) State(h HeadlessStartedData) HeadlessState {
	if h.Status == "" {
		if procLive(h.Pid, h.Start) {
			return HeadlessRunning
		}
		return HeadlessExited
	}
	return supervisedState(h.Status)
}

// Stop writes the stop request the supervisor watches. A record from before
// the supervisor is signalled by its pid, as it was then.
func (osProcess) Stop(h HeadlessStartedData) error {
	if h.Status == "" {
		return killGroup(h.Pid, h.Start)
	}
	return os.WriteFile(headlessStopPath(h.Status), []byte("stop\n"), 0o600)
}

// killGroup sends the process group of pid SIGTERM, then SIGKILL after
// killGrace if any of it remains. The group is signalled while its leader
// runs, or, once the leader is gone, while members of the group remain: a
// pid in use as a group id is not reused, so those members are the child's
// own tool processes, never an unrelated group. Only a process in the
// child's PID namespace can do this: the supervisor, or a caller of a
// record from before it.
func killGroup(pid int, start uint64) error {
	if pid <= 0 {
		return nil
	}
	alive := func() bool { return procLive(pid, start) || groupMembers(pid) }
	if !alive() {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	for deadline := time.Now().Add(killGrace); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !alive() {
			return nil
		}
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// killGrace is how long a child's group has to exit on SIGTERM.
const killGrace = 3 * time.Second

func (osProcess) Sandboxed() string {
	pid1, err := os.ReadFile("/proc/1/comm")
	if err != nil {
		return ""
	}
	if name := strings.TrimSpace(string(pid1)); slices.Contains(PerCommandSandboxInit, name) {
		return name
	}
	return ""
}

// headlessProcess is the engine's HeadlessProcess: the option's, else the
// host's.
func (e *Engine) headlessProcess() HeadlessProcess {
	if e.opts.HeadlessProcess != nil {
		return e.opts.HeadlessProcess
	}
	return osProcess{}
}

// spawnSandboxFix is what a coordinator inside a per-command sandbox does
// for the spawn verb to start a child outside it. ruled: this segment's
// preflight found a rule that lets the printed command run outside the
// sandbox, so the command was most likely not run as printed.
func spawnSandboxFix(coord string, ruled bool) string {
	if normalizeHarness(coord) == "codex" && ruled {
		return "a Codex rule lets the spawn command run outside the sandbox only as next printed it (the run's preflight checked that form), so this one was most likely run chained or altered (a cd or another command before it, a pipe, changed arguments): run the printed spawn command again exactly as printed, alone"
	}
	if normalizeHarness(coord) == "codex" {
		return "install the Codex rule that lets the spawn verb run outside the sandbox (run the setup-codex-rules verb and follow it), then run the printed spawn command again as printed"
	}
	return "allow the spawn verb to run outside your harness's sandbox in its own settings, then run the printed spawn command again as printed"
}

// Spawn starts the headless child of the unit handed out with claim, a unit
// of another harness than the coordinator's (HandOutSpawn.Harness): the
// child harness's CLI, with the unit's brief on stdin, its own native
// protections (harness.Headless) or the run's configured launch, and an
// environment with every harness's session identity and the run's EnvDrop
// names scrubbed. The child's process is its own session; it outlives this
// call, which returns at once. A caller that is itself a headless child is
// refused (children do not fan out), and so is a call from inside a
// per-command sandbox, where the child would be confined and hang.
func (e *Engine) Spawn(run RunID, claim string, co Coordinator, environ []string) (SpawnResult, error) {
	var res SpawnResult
	err := e.withRun(run, func(c *cycle) error {
		if claim == "" {
			return fmt.Errorf("%w: spawn: --claim is required", ErrRejected)
		}
		if parent := envValue(environ, harness.ParentVar); parent != "" {
			return fmt.Errorf("%w: spawn: this is a headless child of %s; a child does not start children", ErrRejected, parent)
		}
		name := c.unitByClaim(claim)
		in, u, ok := c.st.unitByName(name)
		switch {
		case !ok || in.Settled:
			return fmt.Errorf("%w: spawn: no live unit of run %s was handed out with claim %s", ErrRejected, c.run, claim)
		case u.ChildHarness == "":
			return fmt.Errorf("%w: spawn: unit %s runs on the coordinator's own harness: start it with the call next printed", ErrRejected, u.Name)
		case u.State != UnitHanded:
			return fmt.Errorf("%w: spawn: unit %s is %s: its child already claimed it", ErrRejected, u.Name, u.State)
		case len(u.Offered) == 0 || u.Offered[len(u.Offered)-1] != claim:
			return fmt.Errorf("%w: spawn: claim %s is not unit %s's latest offer: run next and use the command it prints", ErrRejected, claim, u.Name)
		}
		p := e.headlessProcess()
		if hs := u.Headless; hs != nil && hs.Claim == claim && p.State(*hs) != HeadlessExited {
			return fmt.Errorf("%w: spawn: unit %s's child is already running (pid %d): keep running next", ErrRejected, u.Name, hs.Pid)
		}
		coord := c.segmentHarness(len(c.st.segments) - 1)
		if sb := p.Sandboxed(); sb != "" {
			return fmt.Errorf("%w: spawn: this command runs inside a per-command sandbox (pid 1 %s), where a %s child would be confined and hang; %s", ErrRejected, sb, u.ChildHarness, spawnSandboxFix(coord, c.st.harnessChecks[u.ChildHarness].Available))
		}
		target := u.ChildHarness
		hl := harness.HeadlessFor(target)
		if hl == nil {
			return fmt.Errorf("%w: spawn: harness %s has no headless launch", ErrRejected, target)
		}
		d := c.callerDispatch(in)
		brief := c.brief(claim)
		errLog := u.errLogOf(claim)
		if errLog == "" {
			errLog = c.childErrLog(claim)
		}
		if errLog == "" {
			return fmt.Errorf("%w: spawn: the run has no state dir for the child's logs", ErrRejected)
		}
		base := strings.TrimSuffix(errLog, ".stderr")
		briefFile, outFile, statusFile := base+".brief", base+".stdout", base+".status"
		if err := makeChildLogDir(c.e.opts.StateRoot, filepath.Dir(errLog)); err != nil {
			return fmt.Errorf("spawn: create the child log directory: %w", err)
		}
		if err := os.WriteFile(briefFile, []byte(brief), 0o600); err != nil {
			return fmt.Errorf("spawn: write the brief: %w", err)
		}
		session := hl.HeadlessSession(claim)
		hs := harness.HeadlessSpawn{Model: d.Model, Effort: d.Effort, Session: session, Workspace: c.workspace(), StateDir: c.e.opts.StateRoot, WF: briefBinary(brief)}
		argv, err := headlessArgv(c.headless(), target, hl, hs, c.run, u.Name)
		if err != nil {
			return err
		}
		parent := u.HandSession
		if parent == "" {
			parent = "wf-" + claim // a sessionless coordinator: still never the child's own
		}
		env := append(childEnv(environ, c.headless(), target, hl), harness.ParentVar+"="+normalizeHarness(co.Harness)+":"+parent, harness.ClaimVar+"="+claim)
		dir := c.workspace()
		if dir == "" {
			dir = c.e.opts.StateRoot
		}
		pid, start, err := p.Start(argv, env, dir, briefFile, outFile, errLog, statusFile)
		if err != nil {
			return fmt.Errorf("%w: spawn: start %s: %v", ErrRejected, argv[0], err)
		}
		started := HeadlessStartedData{Name: u.Name, Claim: claim, Harness: target, Pid: pid, Start: start, Session: session, ErrLog: errLog, Out: outFile, Status: statusFile}
		if err := c.emit(in, JournalHeadlessStarted, 0, started); err != nil {
			_ = p.Stop(started) // unrecorded, it would run unwatched
			return err
		}
		res = SpawnResult{Unit: u.Name, Harness: target, Pid: pid, Session: session, ErrLog: errLog}
		return nil
	})
	return res, err
}

// childEnv is the environment a child of harness target starts with, and
// the one its preflight checks: environ with every harness's identity and
// the run's EnvDrop names scrubbed, and its configured login home set.
func childEnv(environ []string, cfg *HeadlessConfig, target string, hl harness.Headless) []string {
	drop := harness.HeadlessScrub()
	if cfg != nil {
		drop = append(drop, cfg.EnvDrop...)
	}
	env := harness.ScrubbedEnv(environ, drop)
	if cfg != nil && cfg.Homes[target] != "" {
		env = append(harness.ScrubbedEnv(env, []string{hl.HeadlessHome()}), hl.HeadlessHome()+"="+cfg.Homes[target])
	}
	return env
}

// headlessArgv is the launch of a child of harness target: the run's
// configured template for it, its placeholders replaced, else the adapter's
// default.
func headlessArgv(cfg *HeadlessConfig, target string, hl harness.Headless, s harness.HeadlessSpawn, run RunID, unit string) ([]string, error) {
	tmpl := []string(nil)
	if cfg != nil {
		tmpl = cfg.Cmd[target]
	}
	if len(tmpl) == 0 {
		return hl.HeadlessArgv(s), nil
	}
	r := strings.NewReplacer("{workspace}", s.Workspace, "{state}", s.StateDir, "{run}", string(run), "{unit}", unit,
		"{model}", s.Model, "{effort}", s.Effort, "{session}", s.Session, "{wf}", s.WF)
	out := make([]string, len(tmpl))
	for i, a := range tmpl {
		out[i] = r.Replace(a)
	}
	return out, nil
}

// ParseHeadlessCmd parses a --headless-cmd value, harness=JSON-argv.
func ParseHeadlessCmd(v string) (string, []string, error) {
	h, raw, ok := strings.Cut(v, "=")
	h = normalizeHarness(h)
	if !ok || h == "" {
		return "", nil, fmt.Errorf("want harness=JSON-argv, got %q", v)
	}
	if harness.HeadlessFor(h) == nil {
		return "", nil, fmt.Errorf("harness %s has no headless launch", h)
	}
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil || len(argv) == 0 || argv[0] == "" {
		return "", nil, fmt.Errorf("%s: want a nonempty JSON argv array; placeholders: %s", h, strings.Join(headlessPlaceholders, ", "))
	}
	return h, argv, nil
}

// briefBinary is the workflow binary the brief's first command runs: the
// first word after "&& " of its first line, unquoted; "" when it has none.
func briefBinary(brief string) string {
	line, _, _ := strings.Cut(brief, "\n")
	if _, rest, ok := strings.Cut(line, "&& "); ok {
		line = rest
	}
	return firstShellWord(line)
}

// firstShellWord is the first word of a POSIX shell line in the forms
// sink.Quote writes, "" when it has none.
func firstShellWord(line string) string {
	if w := shellWords(line); len(w) > 0 {
		return w[0]
	}
	return ""
}

// shellWords splits a POSIX shell line in the forms sink.Quote writes: bare
// characters, single-quoted runs and backslash escapes, separated by
// unquoted blanks. An operator (&&, |, ;) stays a word of its own when
// blanks surround it, as the engine prints them. An unterminated quote ends
// the words before it.
func shellWords(line string) []string {
	var words []string
	var b strings.Builder
	in := false
	for i := 0; i < len(line); i++ {
		switch ch := line[i]; ch {
		case ' ', '\t':
			if in {
				words, in = append(words, b.String()), false
				b.Reset()
			}
		case '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				return words
			}
			b.WriteString(line[i+1 : i+1+j])
			i += j + 1
			in = true
		case '\\':
			if i+1 < len(line) {
				i++
				b.WriteByte(line[i])
			}
			in = true
		default:
			b.WriteByte(ch)
			in = true
		}
	}
	if in {
		words = append(words, b.String())
	}
	return words
}

// envValue is the value of name in environ, "" when unset.
func envValue(environ []string, name string) string {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}
