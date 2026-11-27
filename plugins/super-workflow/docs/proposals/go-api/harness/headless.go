package harness

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
)

// ParentVar is the environment entry the engine's spawn verb gives a headless
// child: "<coordinator harness>:<coordinator session>". A child's own CLI
// starts a fresh top-level session whose environment cannot tell it from a
// coordinator (a headless codex exec shell has CODEX_THREAD_ID equal to
// CODEX_SESSION_ID), so an adapter that reads this entry calls the caller
// that coordinator's child. pi's SUPER_PI_PARENT is the same idea, kept for
// its own spawn line.
const ParentVar = "SUPER_WF_PARENT"

// ClaimVar names the claim token a headless child was started for.
const ClaimVar = "SUPER_WF_CLAIM"

// parentSession is the coordinator session a ParentVar value names: the text
// after the first colon; "" when the value has none.
func parentSession(v string) string {
	_, s, ok := strings.Cut(v, ":")
	if !ok {
		return ""
	}
	return s
}

// headlessChild is the Caller of a headless child: own is its own session
// (its agent id), parent the coordinator's. ok is false when env carries no
// usable ParentVar or the parent is the caller itself.
func headlessChild(c Caller, own string, env Env) (Caller, bool) {
	parent := parentSession(env(ParentVar))
	if own == "" || parent == "" || parent == own {
		return c, false
	}
	c.Session, c.Agent, c.Is = parent, own, IsChild
	return c, true
}

// HeadlessSpawn is what a headless launch needs beyond the brief, which goes
// on the child's stdin.
type HeadlessSpawn struct {
	Model     string
	Effort    string
	Session   string // the session id the launch names; "" when the CLI picks its own
	Workspace string // the run's workspace: the child reads it
	StateDir  string // the run's state dir: the child writes its result under it
	WF        string // the workflow binary the brief's commands run
}

// Headless is an adapter whose CLI can run one unit as a separate headless
// process, started by the engine's spawn verb for a coordinator of another
// harness. The child is confined by its own CLI's native protections only:
// the argv asks for them, and the engine adds no containment of its own.
type Headless interface {
	// HeadlessArgv is the default launch of a read-mostly child: it reads
	// anything, writes only under the state dir, never prompts.
	HeadlessArgv(s HeadlessSpawn) []string
	// HeadlessSession is the session id a launch for claim names, or "" when
	// the CLI chooses its own (the child then reports it at its first fetch).
	HeadlessSession(claim string) string
	// HeadlessIdentity are the environment names (a trailing * matches a
	// prefix) that carry this harness's session identity: scrubbed from every
	// headless child, so no child inherits a coordinator's identity.
	HeadlessIdentity() []string
	// HeadlessHome is the variable naming the CLI's login home
	// (CODEX_HOME, CLAUDE_CONFIG_DIR).
	HeadlessHome() string
	// HeadlessLogin is the CLI's public login check: the argv to run and
	// whether its output and exit status say logged in.
	HeadlessLogin() (argv []string, ok func(stdout []byte, exit int) bool)
}

// HeadlessFor is the named harness's Headless, or nil when it has none.
func HeadlessFor(name string) Headless {
	h, _ := For(Canonical(name)).(Headless)
	return h
}

// HeadlessScrub is what the spawn verb removes from the environment it
// starts a child with: every registered harness's markers (MarkerNames),
// so no other harness's detection claims the child, and every Headless
// adapter's session identity, so no child inherits its coordinator's.
func HeadlessScrub() []string {
	registryRead.Store(true)
	out := append(MarkerNames(), ParentVar, ClaimVar)
	for _, a := range registry {
		if h, ok := a.(Headless); ok {
			out = append(out, h.HeadlessIdentity()...)
		}
	}
	return out
}

// newUUID is a random version 4 UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// codex: `codex exec` in its own workspace-write sandbox rooted at the state
// dir (the workspace stays readable, not writable), approvals never, so no
// command can escalate; its commands have no network. The rollout id is the
// child's own thread, which Codex chooses.
func (codex) HeadlessArgv(s HeadlessSpawn) []string {
	argv := []string{"codex", "exec", "--skip-git-repo-check", "-s", "workspace-write", "-c", `approval_policy="never"`, "-C", s.StateDir}
	if s.Model != "" {
		argv = append(argv, "-m", s.Model)
	}
	if s.Effort != "" {
		argv = append(argv, "-c", "model_reasoning_effort="+s.Effort)
	}
	return append(argv, "--json", "-")
}

func (codex) HeadlessSession(string) string { return "" }

func (codex) HeadlessIdentity() []string {
	return []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_TOOL_CALL_ID", "CODEX_CI", "CODEX_SANDBOX*"}
}

func (codex) HeadlessHome() string { return "CODEX_HOME" }

func (codex) HeadlessLogin() ([]string, func([]byte, int) bool) {
	return []string{"codex", "login", "status"}, func(_ []byte, exit int) bool { return exit == 0 }
}

// claude: `claude -p` under its own permission engine: dontAsk denies, without
// prompting, every tool call the allow list does not name. The allow list is
// the workflow binary, reads, and writes under the state dir. The user's own
// settings still apply (their allow rules add to it). The brief goes on
// stdin: --allowedTools is variadic and would swallow a trailing prompt.
func (claude) HeadlessArgv(s HeadlessSpawn) []string {
	argv := []string{"claude", "-p"}
	if s.Model != "" {
		argv = append(argv, "--model", s.Model)
	}
	if s.Effort != "" {
		argv = append(argv, "--effort", s.Effort)
	}
	if s.Session != "" {
		argv = append(argv, "--session-id", s.Session)
	}
	argv = append(argv, "--permission-mode", "dontAsk", "--add-dir", s.StateDir, "--output-format", "json", "--allowedTools")
	if s.WF != "" {
		argv = append(argv, "Bash("+s.WF+" *)")
	}
	if s.Workspace != "" {
		argv = append(argv, "Bash(cd "+s.Workspace+" *)")
	}
	// Claude's permission rules read /path as relative to the project and
	// //path as absolute.
	abs := "/" + s.StateDir
	return append(argv, "Read", "Grep", "Glob", "Write("+abs+"/**)", "Edit("+abs+"/**)")
}

// HeadlessSession: Claude takes a session id of the caller's choosing.
func (claude) HeadlessSession(string) string { return newUUID() }

// HeadlessIdentity: the session's own variables only. Login and provider
// settings (CLAUDE_CODE_OAUTH_TOKEN, CLAUDE_CODE_USE_BEDROCK, ...) and
// CLAUDE_CONFIG_DIR stay: the child logs in as its coordinator's user does.
func (claude) HeadlessIdentity() []string {
	return []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_*", "CLAUDE_CODE_MESSAGING_*", "CLAUDE_CODE_CHILD_*",
		"CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_PLUGIN_DIRS", "CLAUDE_PID", "CLAUDE_EFFORT"}
}

func (claude) HeadlessHome() string { return "CLAUDE_CONFIG_DIR" }

func (claude) HeadlessLogin() ([]string, func([]byte, int) bool) {
	return []string{"claude", "auth", "status"}, func(out []byte, exit int) bool {
		var s struct {
			LoggedIn bool `json:"loggedIn"`
		}
		return exit == 0 && json.Unmarshal(bytes.TrimSpace(out), &s) == nil && s.LoggedIn
	}
}

var _ = []Headless{claude{}, codex{}}

// ScrubbedEnv is environ without the entries whose name matches one of
// names (a trailing * matches a prefix).
func ScrubbedEnv(environ []string, names []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, n := range names {
			if p, ok := strings.CutSuffix(n, "*"); ok && strings.HasPrefix(k, p) || k == n {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
