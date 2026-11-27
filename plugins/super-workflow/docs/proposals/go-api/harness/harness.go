// Package harness holds what the engine knows about each coordinator harness:
// who a caller is, where a child's transcript is, how a unit is spawned, and
// the model, effort and agent-type tables.
//
// Owns: one Adapter per known harness (claude, codex, hermes, pi, agy,
// opencode, and any a linking binary registers) and the generic one for every other; the environment variables each harness sets; the transcript
// layout each one writes; the literal spawn parameters of each one's tool.
// Hides: every path, variable and tool name. The engine asks an Adapter and
// never reads a harness environment variable itself.
// Does not: journal, decide liveness or read a run. The engine records what an
// Adapter returns and decides what it means.
// Extend by: a new harness is one Adapter value in registry, or one a binary
// passes to Register from an init function (new names and markers only); an unknown name resolves to Generic, which identifies
// nothing and resolves nothing.
package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Env reads one environment variable; os.Getenv in production.
type Env func(string) string

// Verdict is whether the environment can tell that a caller is the
// coordinator itself rather than one of its children.
type Verdict int

// Verdicts.
const (
	IsUnknown Verdict = iota // the environment cannot tell
	IsChild                  // the caller is a child of Session
	IsSelf                   // the caller is the coordinator of Session
)

// Caller is who ran a wf command, as its environment says.
type Caller struct {
	// Executable is the CLI binary whose prompt command is being checked.
	Executable string
	// PromptClaim parses a recorded invocation using the owning CLI flag set.
	PromptClaim func(args []string, named bool) (string, bool)

	Harness string // the adapter's name
	Session string // the coordinator's session (a child shares its coordinator's)
	Agent   string // the caller's own agent id, when the environment gives it
	Root    string // where the harness writes transcripts
	Is      Verdict
	// FromEnv marks a caller read from a process environment, as every
	// adapter's Caller does; the zero Caller is a library call that read none.
	FromEnv bool
}

// Want is one child the engine looks for: by its claim token, or by an agent
// id the environment already gave.
type Want struct {
	Token  string
	Agent  string
	After  time.Time // the child's transcript is written after this (its hand-out): older files are not read
	ErrLog string    // the claim's recorded stderr path; a launch whose 2> word differs is not this child. Empty: none recorded, no filter
}

// Child is what a search found for one Want.
type Child struct {
	Agent      string
	Transcript string
}

// Tier is one tier's model and effort; an empty Effort means the harness's own
// default.
type Tier struct {
	Model  string
	Effort string
}

// Spawn is everything a coordinator needs to start one unit.
type Spawn struct {
	Brief     string // the one line the child is given
	Claim     string // the claim token the brief carries, for a harness whose spawn line names it
	Model     string
	Effort    string
	AgentType string
	ErrLog    string // the absolute path the child's stderr is written to, chosen by the engine; only an adapter with StartLog uses it
}

// Telemetry is an adapter's optional reading of its own transcripts. No run
// transition or budget depends on it (rule 5): the budget counts only usage
// children report through the verbs, and a recovery offer reads without a
// Tail. The dev fanout tool reads it for its own verdict: a count it cannot
// read is unknown, and child verbs found in the coordinator's transcript fail
// the tool's run (exit 8). An adapter without it, Generic among them, reads
// nothing.
type Telemetry interface {
	// Usage is the tokens a child spent, read from its transcript: input
	// (uncached and cache writes) plus output, cache reads not counted. ok
	// is false when the transcript is unreadable or records no usage.
	Usage(transcript string) (tokens int, ok bool)
	// Tail is a short reading of where a child's transcript stopped: its
	// last tool call and its last message, each cut to one line; "" for
	// what it cannot find (F1).
	Tail(transcript string) (lastTool, lastMessage string)
	// CoordinatorTranscript is the coordinator session's own transcript
	// under root, the most recently written when several match; "" when
	// none is found or the harness has no known layout.
	CoordinatorTranscript(root, session string) string
}

// The adapters whose transcript layout is known read telemetry.
var _ = []Telemetry{claude{}, codex{}, pi{}, agy{}}

// Adapter is one harness.
type Adapter interface {
	// Name is the harness name the journal records.
	Name() string
	// Caller identifies the process whose environment env is.
	Caller(env Env) Caller
	// OutputLimit is the most one tool call's output may hold before the
	// harness cuts it, in bytes and in lines (0: no such limit, or unknown).
	// A blocking next offers no more units than fit under it.
	OutputLimit(env Env) (bytes, lines int)
	// Resolve finds the children of session under root: each Want whose
	// child is found exactly once maps to it. A child not found yet (its
	// transcript not flushed) is absent; the engine asks again later.
	Resolve(root, session string, wants []Want) map[Want]Child
	// Tiers is the tier -> model table; nil when the harness has none.
	Tiers() map[string]Tier
	// AgentType is the subagent type an instruction names when its state
	// sets none; "" when the harness takes none.
	AgentType() string
	// SpawnLine is the literal tool call that starts one unit.
	SpawnLine(s Spawn) string
	// ChildrenDieWithCoordinator reports whether a child runs inside its
	// coordinator's process (a subagent, a thread), so a coordinator session
	// that is replaced leaves no live child, whatever its last write.
	ChildrenDieWithCoordinator() bool
	// Ended reports whether the child's transcript ends on the harness's
	// explicit end marker: its turn is over and it will write no more. A
	// transcript that stops mid-turn has not ended (E2).
	Ended(transcript string) bool
	// Limit is the harness's own limit on concurrent subagents as its
	// configuration for a coordinator working in dir sets it, and where it
	// was read; 0 when the adapter cannot read one (E3). It is the
	// harness's number, never a framework cap.
	Limit(env Env, dir string) (limit int, source string)
	// ResumeLine is the literal call that continues the finished child agent
	// with message appended to its session; "" when this harness cannot
	// resume a child, so a recovery offers restart and give up only (F1).
	ResumeLine(agent, message string) string
	// ReadAny reports whether the child's transcript holds a tool call that
	// read one of paths (F2). known is false when the harness's transcript
	// format is unknown or the transcript cannot be read.
	ReadAny(transcript string, paths []string) (read, known bool)
	// ToolLimit is how long one tool call of this harness may run before the
	// harness cuts it off, as env configures it; 0 when the adapter cannot
	// tell (H2). A blocking call stays under it.
	ToolLimit(env Env) time.Duration
}

// Detector is an adapter whose harness sets environment variables of its
// own: a process with any of them set is that harness's agent, or runs in
// its shell.
type Detector interface {
	// Markers are the variables, in the order Detect reports them.
	Markers() []string
}

// WeakDetector is a Detector some of whose markers every process started
// from its shell inherits, whatever harness that process is (opencode's
// OPENCODE). Detect names it by one of those only when no registered
// adapter's other markers are set.
type WeakDetector interface {
	Detector
	// WeakMarkers are the markers, a subset of Markers, that count last.
	WeakMarkers() []string
}

// ChildMatcher is an adapter that decides itself whether caller is the child
// that owns a claim (owner, the caller recorded at the claim's first fetch).
// An adapter without one matches by session, and by agent when the owner
// has one.
type ChildMatcher interface {
	SameChild(owner, caller Caller) bool
}

// ModelChecker is an adapter whose harness takes only some model values: a
// --model-map value it refuses is a bad map, never written into a spawn line.
// An adapter without one takes any value. It knows nothing of --model-map
// keys, tiers or precedence (package wf owns those).
type ModelChecker interface {
	// CheckModel is nil for a value the harness takes, else an error naming
	// the values it takes.
	CheckModel(model string) error
}

// WeakHinter is a WeakDetector that can say what its weak marker alone
// most likely means (opencode without its plugin), whichever harness
// Detect names: the engine journals the sentence once at a run's start.
type WeakHinter interface {
	// WeakHint is that sentence for env; "" when it does not apply.
	WeakHint(env Env) string
}

// WeakHints are every registered adapter's WeakHint for env, in detection
// order, empty ones left out.
func WeakHints(env Env) []string {
	registryRead.Store(true)
	var out []string
	for _, a := range registry {
		if w, ok := a.(WeakHinter); ok {
			if s := w.WeakHint(env); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// SessionHinter is an adapter that can say why its shell names no session
// when it can tell (opencode: the plugin's parent lookup failed). The
// engine appends the sentence to a no-session refusal.
type SessionHinter interface {
	// SessionHint is that sentence for env; "" when it does not apply.
	SessionHint(env Env) string
}

// NoModelSpawn is an adapter whose spawn tool takes no model: a child runs on
// its coordinator's model whatever the tier or --model-map, so the engine
// resolves no tier for it and applies no map value, and says so once per run.
// It owns the sentence that says where its children's model comes from.
type NoModelSpawn interface {
	NoModel() string
}

// EndVerdict is an adapter's reading of whether a child's transcript has
// ended, for an adapter whose harness can still write after its last turn.
type EndVerdict int

// End verdicts.
const (
	EndNo        EndVerdict = iota // not ended, or the transcript cannot tell
	EndYes                         // ended: nothing more will be written
	EndIfSettled                   // ended once nothing more is written and no process of the child is live
)

// SettlingEnd is an adapter whose harness may still write a child's
// transcript, or keep its process running, after a turn that looks final (pi
// retries a failed turn and compacts after a stop). It owns the three-way
// reading of the transcript and the check that a process of the child is
// still live. It does not keep a clock or decide when to look: the engine
// owns the settle time (loseEndedChildren). ChildLive counts the child's own
// launched process only, not a process it started; its error means unknown
// (the scan could not see every process that could be the child), which the
// engine never reads as ended. Adapters that write a final end record keep
// Ended alone.
type SettlingEnd interface {
	EndVerdict(transcript string) EndVerdict
	ChildLive(child string) (bool, error)
}

// StartLog is an adapter whose launch line writes the child's stderr to the
// engine-chosen file Spawn.ErrLog. It owns the not-started sentence that
// points the coordinator at that file, and it is the one signal that the
// engine should choose ErrLog and create its directory. It does not choose or
// create the path. Other adapters ignore ErrLog.
type StartLog interface {
	StartLog(errLog string) string
}

// BriefChecker is an adapter that can refuse a spawn brief its launch line
// cannot carry verbatim. It owns what makes a brief quotable for its harness
// and the refusal text; the engine calls it once, before a hand-out is
// journaled, and never escapes or rewrites a brief to fit. It does not know
// where a bad character in the brief came from: the engine names that.
type BriefChecker interface {
	CheckBrief(brief string) error
}

// ResumeNote supplies a harness-specific condition or fallback for a resume.
// The engine prints it beside the recovery offer's harness call.
type ResumeNote interface {
	ResumeNote() string
}

// registry is every known adapter in detection order: a process started
// from another harness's shell inherits that harness's variables, so the
// harness most likely to be the inner one comes first: Hermes, then pi
// (whose bash commands, run from a Claude or Codex tab, carry that tab's
// variables too), then agy (it precedes Codex and Claude because an agy
// started from their tabs inherits their variables), then opencode (for the
// same reason; its bare OPENCODE marker is weak, see WeakDetector), then
// Codex and Claude.
var registry = []Adapter{hermes{Generic{name: "hermes"}}, pi{Generic{name: "pi"}}, agy{Generic{name: "agy"}}, opencode{Generic{name: "opencode"}}, codex{}, claude{}}

// registryRead is set by the first lookup (For, Known, Names, Detect,
// Markers): Register is for package initialization only, and the registry
// is read without a lock once the engine runs.
var registryRead atomic.Bool

// reservedNames are harness names no adapter may take: the generic
// fallback and the scripted manufacture driver.
var reservedNames = map[string]bool{"generic": true, "scripted": true}

// Canonical is a harness label's canonical name: trimmed, lower case, with
// the aliases a person may type (claude-code, codex-cli) mapped to the
// adapter's name. The engine compares harness names only in this form.
func Canonical(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "claude-code", "claude_code", "claudecode":
		return "claude"
	case "codex-cli", "codex_cli":
		return "codex"
	}
	return name
}

// Register adds a new adapter, detected before the built-in ones, so a
// binary that links the engine can supply one the engine module does not
// carry (its dependencies). It refuses, leaving the registry as it was, an
// adapter whose name is not canonical (Canonical: an alias or a variant of
// another adapter's name), or is taken or reserved (a built-in is never replaced: a
// wrapper would lose its detection and its same-child rule), one whose
// markers overlap a registered adapter's (detection order would pick it in
// that harness's environment), and any call after the registry was first
// read. Call it from an init function.
func Register(a Adapter) error {
	if registryRead.Load() {
		return fmt.Errorf("harness: Register(%q) after the registry was read; register from an init function", a.Name())
	}
	name := a.Name()
	if name == "" || Canonical(name) != name {
		return fmt.Errorf("harness: the name %q is not canonical (want %q)", name, Canonical(name))
	}
	if reservedNames[name] {
		return fmt.Errorf("harness: the name %q is reserved", name)
	}
	var own []string
	if d, ok := a.(Detector); ok {
		own = d.Markers()
	}
	for _, b := range registry {
		if b.Name() == name {
			return fmt.Errorf("harness: an adapter named %q is already registered", name)
		}
		if d, ok := b.(Detector); ok {
			for _, m := range d.Markers() {
				if slices.Contains(own, m) {
					return fmt.Errorf("harness: %q's marker %s is %q's", name, m, b.Name())
				}
			}
		}
	}
	registry = append([]Adapter{a}, registry...)
	return nil
}

// For is the adapter of the named harness; Generic for any other name.
func For(name string) Adapter {
	registryRead.Store(true)
	for _, a := range registry {
		if a.Name() == name {
			return a
		}
	}
	return Generic{name: name}
}

// SpawnSession returns the child session named by the launch protocol, where
// the adapter assigns one in advance. It uses no harness records.
func SpawnSession(name, claim string) string {
	if claim != "" {
		if a, ok := For(name).(interface{ SpawnSession(string) string }); ok {
			return a.SpawnSession(claim)
		}
	}
	return ""
}

// Known reports whether name is a registered adapter's, not Generic.
func Known(name string) bool {
	registryRead.Store(true)
	for _, a := range registry {
		if a.Name() == name {
			return true
		}
	}
	return false
}

// Names are the registered adapters' names, in detection order.
func Names() []string {
	registryRead.Store(true)
	out := make([]string, 0, len(registry))
	for _, a := range registry {
		out = append(out, a.Name())
	}
	return out
}

// Detect names the harness whose environment env is: the first registered
// adapter, in detection order, with a marker set that is not weak; else the
// first with a weak marker set (WeakDetector); otherwise generic.
func Detect(env Env) string {
	registryRead.Store(true)
	for _, weak := range []bool{false, true} {
		for _, a := range registry {
			d, ok := a.(Detector)
			if !ok {
				continue
			}
			var weakOnes []string
			if w, ok := a.(WeakDetector); ok {
				weakOnes = w.WeakMarkers()
			}
			for _, v := range d.Markers() {
				if slices.Contains(weakOnes, v) == weak && env(v) != "" {
					return a.Name()
				}
			}
		}
	}
	return "generic"
}

// Markers are the variables Detect found set in env, in its order: what
// makes env an agent's shell.
func Markers(env Env) []string { return markersExcept(env, "") }

// MarkerNames are every registered harness's marker variables, set or not:
// the names a client scrubs from an environment so a process it starts does
// not inherit the identity of the session that launched it.
func MarkerNames() []string {
	registryRead.Store(true)
	var out []string
	for _, a := range registry {
		if d, ok := a.(Detector); ok {
			out = append(out, d.Markers()...)
		}
	}
	return out
}

// markersExcept is Markers without the named harness's own variables.
func markersExcept(env Env, skip string) []string {
	registryRead.Store(true)
	var out []string
	for _, a := range registry {
		d, ok := a.(Detector)
		if !ok || a.Name() == skip {
			continue
		}
		for _, v := range d.Markers() {
			if env(v) != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// SameChild reports whether caller is the child recorded as owner: never
// across harnesses or transcript roots; then by the owner's harness adapter,
// its ChildMatcher, else the same session and, when the owner has an agent
// id, the same agent.
func SameChild(owner, caller Caller) bool {
	if owner.Harness != caller.Harness || owner.Root != caller.Root {
		return false
	}
	if m, ok := For(owner.Harness).(ChildMatcher); ok {
		return m.SameChild(owner, caller)
	}
	return owner.Session == caller.Session && (owner.Agent == "" || owner.Agent == caller.Agent)
}

// Contradicts is why a --harness value is refused: the environment shows
// another harness. generic always fits, and so does any label in an
// environment that shows none (a test, a CI job). "" when it fits.
func Contradicts(named string, env Env) string {
	got := Detect(env)
	if named == got || named == "generic" || got == "generic" {
		return ""
	}
	return fmt.Sprintf("--harness %s does not match this environment, which is %s; run without --harness, or with --harness generic", named, got)
}

func home(env Env, v, dir string) string {
	if p := env(v); p != "" {
		return p
	}
	h := env("HOME")
	if h == "" {
		h, _ = os.UserHomeDir()
	}
	if h == "" {
		return ""
	}
	return filepath.Join(h, dir)
}

// spawnArgs renders key=value pairs of a tool call, skipping empty values.
// Each value is a JSON string, written by the encoder: Go's %q escapes (\x00,
// \a) are not JSON and cannot be copied into a tool call.
func spawnArgs(kv ...string) string {
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			parts = append(parts, kv[i]+"="+jsonText(kv[i+1]))
		}
	}
	return strings.Join(parts, ", ")
}

// jsonText is s as a JSON string, < > & kept as they are (they are not
// HTML here). Invalid UTF-8 becomes U+FFFD: JSON cannot carry it.
func jsonText(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// claude is Claude Code. A subagent's shell carries its parent's
// CLAUDE_CODE_SESSION_ID and no id of its own (probe 2026-11-14), so a caller
// is never known to be a child from its environment. Its transcript is the one
// file under <config>/projects/*/<session>/subagents/ that holds its claim
// token: the coordinator's own transcript, which also holds the token, is not
// under subagents/.
type claude struct{}

func (claude) Name() string { return "claude" }

// Markers: Claude Code's own variables.
func (claude) Markers() []string {
	return []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID"}
}

// Caller: a Claude Code shell's session. A headless claude -p child started
// by the spawn verb also carries ParentVar: it is that coordinator's child,
// its own session its agent id.
func (claude) Caller(env Env) Caller {
	own := env("CLAUDE_CODE_SESSION_ID")
	c := Caller{Harness: "claude", Session: own, Root: home(env, "CLAUDE_CONFIG_DIR", ".claude"), FromEnv: true}
	if kid, ok := headlessChild(c, own, env); ok {
		return kid
	}
	return c
}

// OutputLimit: Claude Code shows a Bash command's output inline up to its
// bashOutputMaxChars setting, 30000 characters by default, clamped to
// 4000..128000; longer output goes to a file and the model sees a preview
// (Claude Code 2.1.325; BASH_MAX_OUTPUT_LENGTH now only sizes the read-back
// window). The setting is read from the user's settings.json.
func (claude) OutputLimit(env Env) (int, int) {
	n := 30000
	if b, err := os.ReadFile(filepath.Join(home(env, "CLAUDE_CONFIG_DIR", ".claude"), "settings.json")); err == nil {
		var s struct{ BashOutputMaxChars *int }
		if json.Unmarshal(b, &s) == nil && s.BashOutputMaxChars != nil {
			n = min(max(*s.BashOutputMaxChars, 4000), 128000)
		}
	}
	return n, 0
}

func (claude) Resolve(root, session string, wants []Want) map[Want]Child {
	if root == "" || len(wants) == 0 {
		return nil
	}
	// A headless claude -p child (the spawn verb's) is its own top-level
	// session, named by the agent id its environment gave: its transcript is
	// <root>/projects/<cwd slug>/<session id>.jsonl, not a subagent file.
	var out map[Want]Child
	var rest []Want
	for _, w := range wants {
		if w.Agent == "" {
			rest = append(rest, w)
			continue
		}
		files, _ := filepath.Glob(filepath.Join(root, "projects", "*", w.Agent+".jsonl"))
		if len(files) == 1 {
			if out == nil {
				out = map[Want]Child{}
			}
			out[w] = Child{Agent: w.Agent, Transcript: files[0]}
		}
	}
	if session == "" || len(rest) == 0 {
		return out
	}
	wants = rest
	all, _ := filepath.Glob(filepath.Join(root, "projects", "*", session, "subagents", "agent-*.jsonl"))
	sort.Strings(all)
	// A child's transcript is written after its hand-out: a file last
	// written before the earliest wanted hand-out is not read.
	var since time.Time
	for i, w := range wants {
		if i == 0 || w.After.Before(since) {
			since = w.After
		}
	}
	var files []string
	for _, f := range all {
		if fi, err := os.Stat(f); err == nil && !fi.ModTime().Before(since) {
			files = append(files, f)
		}
	}
	found := byToken(files, wants, func(f string) string {
		return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "agent-"), ".jsonl")
	})
	for w, c := range out {
		if found == nil {
			found = map[Want]Child{}
		}
		found[w] = c
	}
	return found
}

func (claude) Tiers() map[string]Tier {
	return map[string]Tier{
		"light":    {"haiku", "low"},
		"standard": {"sonnet", "medium"},
		"strong":   {"opus", "high"},
		"top":      {"fable", "high"},
	}
}

func (claude) AgentType() string { return "general-purpose" }

func (claude) SpawnLine(s Spawn) string {
	return "Agent(" + spawnArgs("subagent_type", s.AgentType, "model", s.Model, "effort", s.Effort, "prompt", s.Brief) + ")"
}

// codex is the Codex CLI. A child's shell has its own CODEX_THREAD_ID (its
// rollout id) and its coordinator's CODEX_SESSION_ID; the coordinator's own
// shell has CODEX_THREAD_ID equal to its session. The transcript is the
// rollout file named by the thread id: no search by token is needed.
type codex struct{}

func (codex) Name() string { return "codex" }

// Markers: a Codex shell's thread id.
func (codex) Markers() []string { return []string{"CODEX_THREAD_ID"} }

// SameChild: a Codex child is its own thread under the same session.
func (codex) SameChild(owner, caller Caller) bool {
	return owner.Agent == caller.Agent && owner.Session == caller.Session
}

func (codex) Caller(env Env) Caller {
	thread := env("CODEX_THREAD_ID")
	session := env("CODEX_SESSION_ID")
	c := Caller{Harness: "codex", Session: session, Root: home(env, "CODEX_HOME", ".codex"), FromEnv: true}
	// A headless codex exec child has its own thread as its session; only
	// the spawn verb's ParentVar tells it from a coordinator.
	if kid, ok := headlessChild(c, thread, env); ok {
		return kid
	}
	switch {
	case thread == "":
	case session == "":
		// Without CODEX_SESSION_ID the environment cannot tell a child from
		// its coordinator: the thread is the session (a coordinator's own
		// next), and the verdict stays unknown, so no child is refused on a
		// guess.
		c.Session = thread
	case thread == session:
		c.Is = IsSelf
	default:
		c.Agent, c.Is = thread, IsChild
	}
	return c
}

func (codex) Resolve(root, _ string, wants []Want) map[Want]Child {
	if root == "" {
		return nil
	}
	out := map[Want]Child{}
	for _, w := range wants {
		if w.Agent == "" {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(root, "sessions", "*", "*", "*", "rollout-*-"+w.Agent+".jsonl"))
		if len(files) == 1 {
			out[w] = Child{Agent: w.Agent, Transcript: files[0]}
		}
	}
	return out
}

// OutputLimit: Codex's exec output cap, about 10 KiB or 256 lines (head and
// tail). Newer Codex versions count a tool_output_token_limit or a model's
// truncation policy in tokens instead; 10 KiB stays under those, so it is the
// conservative number: fewer offers per call, none lost.
func (codex) OutputLimit(Env) (int, int) { return 10 << 10, 256 }

func (codex) Tiers() map[string]Tier {
	return map[string]Tier{
		"light":    {"gpt-6-luna", "low"},
		"standard": {"gpt-6.2-sol", "medium"},
		"strong":   {"gpt-6.2-astra", "high"},
		"top":      {"gpt-6.2-astra", "xhigh"},
	}
}

func (codex) AgentType() string { return "" }

func (codex) SpawnLine(s Spawn) string {
	return "spawn_agent(" + spawnArgs("message", s.Brief, "model", s.Model, "reasoning_effort", s.Effort) + `, fork_turns="none")`
}

// Generic is any harness without an adapter: it identifies nothing and
// resolves nothing, and its children run on the session's model.
type Generic struct{ name string }

// Name is the label the run was given, or generic.
func (g Generic) Name() string {
	if g.name == "" {
		return "generic"
	}
	return g.name
}

// Caller is empty: nothing in the environment is known, not even a harness
// to put on a claim, so a manual started may still name one (A4).
func (g Generic) Caller(Env) Caller { return Caller{FromEnv: true} }

// Resolve finds nothing.
func (Generic) Resolve(string, string, []Want) map[Want]Child { return nil }

// OutputLimit: unknown for a generic harness.
func (Generic) OutputLimit(Env) (int, int) { return 0, 0 }

// Tiers is none.
func (Generic) Tiers() map[string]Tier { return nil }

// AgentType is none.
func (Generic) AgentType() string { return "" }

// SpawnLine describes the spawn in words: the harness's own tool is unknown.
func (Generic) SpawnLine(s Spawn) string {
	extra := ""
	if s.Model != "" {
		extra = " on model " + s.Model
		if s.Effort != "" {
			extra += " at effort " + s.Effort
		}
	}
	return fmt.Sprintf("spawn a subagent with fresh context%s, its whole prompt being: %s", extra, s.Brief)
}

// byToken reads each file once and maps every Want whose token is in exactly
// one file to that file, the agent id named by agent.
func byToken(files []string, wants []Want, agent func(string) string) map[Want]Child {
	hits := map[Want][]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, w := range wants {
			if w.Token != "" && bytes.Contains(b, []byte(w.Token)) {
				hits[w] = append(hits[w], f)
			}
		}
	}
	out := map[Want]Child{}
	for w, fs := range hits {
		if len(fs) == 1 {
			p := fs[0]
			if r, err := filepath.EvalSymlinks(p); err == nil {
				p = r
			}
			out[w] = Child{Agent: agent(fs[0]), Transcript: p}
		}
	}
	return out
}

// eachLine calls f with each line of the file at path; false when it cannot
// be opened.
func eachLine(path string, f func([]byte)) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	r := bufio.NewReader(file)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			f(line)
		}
		if err != nil {
			return true
		}
	}
}

// Usage sums each assistant message of a Claude transcript once. A streamed
// message is written as several lines with one id; the last one holds its
// final output count.
func (claude) Usage(transcript string) (int, bool) {
	type usage struct {
		Input      int `json:"input_tokens"`
		CacheWrite int `json:"cache_creation_input_tokens"`
		Output     int `json:"output_tokens"`
	}
	byID := map[string]usage{}
	if !eachLine(transcript, func(line []byte) {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				ID    string `json:"id"`
				Usage *usage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Message.ID == "" || e.Message.Usage == nil {
			return
		}
		byID[e.Message.ID] = *e.Message.Usage
	}) || len(byID) == 0 {
		return 0, false
	}
	n := 0
	for _, u := range byID {
		n += u.Input + u.CacheWrite + u.Output
	}
	return n, true
}

// Usage is the last cumulative token_count of a Codex rollout. Its
// input_tokens include the cached ones. The total is the thread's: a rollout
// forked with its parent's turns counts those turns too (a unit spawns with
// fork_turns "none", so it has none).
func (codex) Usage(transcript string) (int, bool) {
	n, found := 0, false
	if !eachLine(transcript, func(line []byte) {
		var e struct {
			Payload struct {
				Type string `json:"type"`
				Info *struct {
					Total struct {
						Input  int `json:"input_tokens"`
						Cached int `json:"cached_input_tokens"`
						Output int `json:"output_tokens"`
					} `json:"total_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) != nil || e.Payload.Type != "token_count" || e.Payload.Info == nil {
			return
		}
		t := e.Payload.Info.Total
		n, found = t.Input-t.Cached+t.Output, true
	}) {
		return 0, false
	}
	return n, found
}

// Usage reads nothing: the generic harness knows no transcript format.

// ChildrenDieWithCoordinator: a Claude Code subagent runs inside its
// coordinator's process.
func (claude) ChildrenDieWithCoordinator() bool { return true }

// ChildrenDieWithCoordinator: a Codex child is a thread of its coordinator's
// process.
func (codex) ChildrenDieWithCoordinator() bool { return true }

// ChildrenDieWithCoordinator: nothing is known of a generic child's process.
func (Generic) ChildrenDieWithCoordinator() bool { return false }

// tailWindow is how much of a transcript's end Ended reads: its last records
// are all it looks at.
const tailWindow = 256 << 10

// tailLines calls f with each whole line of the last tailWindow bytes of the
// file at path, in order; false when it cannot be read.
func tailLines(path string, f func([]byte)) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false
	}
	off := info.Size() - tailWindow
	if off < 0 {
		off = 0
	}
	buf := make([]byte, info.Size()-off)
	if _, err := file.ReadAt(buf, off); err != nil && len(buf) > 0 {
		return false
	}
	lines := bytes.Split(buf, []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // the window cut the first line
	}
	for _, l := range lines {
		if len(bytes.TrimSpace(l)) > 0 {
			f(l)
		}
	}
	return true
}

// Ended: the transcript's last record is the tool result marked
// toolEndsTurn, the subagent's hand-back. Nothing else is an end: an
// end_turn message may be a child waiting on a background command that
// re-invokes it, or one a hook sends on; any record after the hand-back (a
// hook's attachment, a new request, a resume) starts a new turn. A half
// written last line is skipped.
func (claude) Ended(transcript string) bool {
	ended := false
	return tailLines(transcript, func(line []byte) {
		var e struct {
			Type         string `json:"type"`
			ToolEndsTurn bool   `json:"toolEndsTurn"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type == "" {
			return
		}
		ended = e.Type == "user" && e.ToolEndsTurn
	}) && ended
}

// Ended: the last task event is task_complete; a later task_started is a new
// turn.
func (codex) Ended(transcript string) bool {
	ended := false
	return tailLines(transcript, func(line []byte) {
		var e struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "event_msg" {
			return
		}
		switch e.Payload.Type {
		case "task_complete":
			ended = true
		case "task_started":
			ended = false
		}
	}) && ended
}

// Ended: no end marker is known for a generic child.
func (Generic) Ended(string) bool { return false }

// tailCut is how much of a last tool call or message Tail keeps.
const tailCut = 240

// oneLineCut is s on one line, cut to tailCut runes.
func oneLineCut(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > tailCut {
		s = string(r[:tailCut]) + "…"
	}
	return s
}

// Tail: the last assistant message's tool_use (name and input) and text.
func (claude) Tail(transcript string) (string, string) {
	var tool, msg string
	tailLines(transcript, func(line []byte) {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type  string          `json:"type"`
					Text  string          `json:"text"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" {
			return
		}
		for _, c := range e.Message.Content {
			switch c.Type {
			case "tool_use":
				tool = oneLineCut(c.Name + " " + string(c.Input))
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					msg = oneLineCut(c.Text)
				}
			}
		}
	})
	return tool, msg
}

// Tail: the last function_call (name and arguments) and assistant message.
func (codex) Tail(transcript string) (string, string) {
	var tool, msg string
	tailLines(transcript, func(line []byte) {
		var e struct {
			Type    string `json:"type"`
			Payload struct {
				Type      string `json:"type"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				Role      string `json:"role"`
				Content   []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "response_item" {
			return
		}
		switch {
		case e.Payload.Type == "function_call":
			tool = oneLineCut(e.Payload.Name + " " + e.Payload.Arguments)
		case e.Payload.Type == "message" && e.Payload.Role == "assistant":
			var t []string
			for _, c := range e.Payload.Content {
				t = append(t, c.Text)
			}
			if s := strings.TrimSpace(strings.Join(t, " ")); s != "" {
				msg = oneLineCut(s)
			}
		}
	})
	return tool, msg
}

// Tail: a generic transcript has no known layout.

// ResumeLine: a Claude Code coordinator continues a finished subagent by
// sending it a message (SendMessage to its agent id).
func (claude) ResumeLine(agent, message string) string {
	if agent == "" {
		return ""
	}
	return "SendMessage(" + spawnArgs("to", agent, "message", message) + ")"
}

// ResumeLine uses Codex multi-agent v2's follow-up tool. The claim already
// holds the child's thread ID from its prompt fetch.
func (codex) ResumeLine(agent, message string) string {
	if agent == "" {
		return ""
	}
	args, _ := json.Marshal(struct {
		Target  string `json:"target"`
		Message string `json:"message"`
	}{Target: agent, Message: message})
	return "collaboration.followup_task(" + string(args) + ")"
}

// ResumeNote distinguishes dispatch from a child's continued activity.
func (codex) ResumeNote() string {
	return "If the call is refused, use the restart line below while this offer is open. " +
		"After successful dispatch, record the resume and keep running next. " +
		"If the child stays silent, follow next's stall question. " +
		"If you confirm in Codex that the child exited without done or failed, use the stall question's heartbeat died command to get a fresh attempt."
}

// ResumeLine: a generic harness resumes nothing.
func (Generic) ResumeLine(string, string) string { return "" }

// Limit: Claude Code exposes no subagent limit the adapter can read.
func (claude) Limit(Env, string) (int, string) { return 0, "" }

// Limit: Codex's [agents] max_threads, from the nearest .codex/config.toml at
// or above dir (a project's own), else $CODEX_HOME/config.toml (~/.codex when
// unset). A config that sets none leaves the limit unknown.
func (codex) Limit(env Env, dir string) (int, string) {
	homeInfo, _ := os.Stat(env("HOME"))
	for d := dir; d != ""; {
		// HOME/.codex is the global fallback, never a project override.
		// Searching past it would shadow an explicit run-specific CODEX_HOME.
		if info, err := os.Stat(d); err == nil && homeInfo != nil && os.SameFile(info, homeInfo) {
			break
		}
		p := filepath.Join(d, ".codex", "config.toml")
		if n, ok := tomlMaxThreads(p); ok {
			return n, "max_threads in " + p
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	home := env("CODEX_HOME")
	if home == "" {
		if h := env("HOME"); h != "" {
			home = filepath.Join(h, ".codex")
		}
	}
	if home != "" {
		p := filepath.Join(home, "config.toml")
		if n, ok := tomlMaxThreads(p); ok {
			return n, "max_threads in " + p
		}
	}
	return 0, ""
}

// tomlMaxThreads reads max_threads under [agents] (or agents.max_threads at
// the top level) from the TOML file at path: the one key, by line.
func tomlMaxThreads(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	table := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			table = strings.TrimSpace(strings.Trim(line, "[]"))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		if table != "" {
			key = table + "." + key
		}
		if key != "agents.max_threads" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// Limit: a generic harness's limit is unknown.
func (Generic) Limit(Env, string) (int, string) { return 0, "" }

// newest is the most recently modified of paths; "" for none.
func newest(paths []string) string {
	best, at := "", time.Time{}
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && (best == "" || fi.ModTime().After(at)) {
			best, at = p, fi.ModTime()
		}
	}
	return best
}

// CoordinatorTranscript: a Claude Code session's own transcript is
// <root>/projects/<project>/<session>.jsonl, beside its subagents' directory.
func (claude) CoordinatorTranscript(root, session string) string {
	if root == "" || session == "" {
		return ""
	}
	all, _ := filepath.Glob(filepath.Join(root, "projects", "*", session+".jsonl"))
	return newest(all)
}

// CoordinatorTranscript: a Codex coordinator's own rollout is named by its
// thread id, which its children see as CODEX_SESSION_ID.
func (codex) CoordinatorTranscript(root, session string) string {
	if root == "" || session == "" {
		return ""
	}
	all, _ := filepath.Glob(filepath.Join(root, "sessions", "*", "*", "*", "rollout-*-"+session+".jsonl"))
	return newest(all)
}

// namesIn reports whether s, a command run in directory dir, names one of
// paths: namesAny, or a path relative to dir that resolves to one (cat
// prompt.md run from the body's directory, sed -n 1,200p prompt/<h>/prompt.md
// from the run's).
func namesIn(s, dir string, paths []string) bool {
	if namesAny(s, paths) {
		return true
	}
	// The directories the command reads from: the one it ran in, and each it
	// changes into (cd or pushd, absolute or relative, quoted or not).
	dirs := []string{}
	if filepath.IsAbs(dir) {
		dirs = append(dirs, dir)
	}
	for _, m := range cdTarget.FindAllStringSubmatch(s, -1) {
		t := m[2]
		switch {
		case filepath.IsAbs(t):
			dirs = append(dirs, filepath.Clean(t))
		case filepath.IsAbs(dir):
			dirs = append(dirs, filepath.Join(dir, t))
		}
	}
	for _, d := range dirs {
		for _, p := range paths {
			if rel, err := filepath.Rel(d, p); err == nil && !strings.HasPrefix(rel, "..") && containsPath(s, rel) {
				return true
			}
		}
	}
	return false
}

// cdTarget is a cd or pushd and its directory, quoted or not.
var cdTarget = regexp.MustCompile(`(?:^|[\s;&|(])(?:cd|pushd)\s+(\\?["']?)([^"'\\\s;&|)]+)`)

// containsPath reports whether s holds rel as a whole path word: not part of
// a longer name (prompt.md inside other-prompt.md).
func containsPath(s, rel string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], rel)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(rel)
		lead := s[:start]
		before := start == 0 || strings.ContainsRune(" \t\n'\"=<(:;|&\\", rune(s[start-1])) ||
			strings.HasSuffix(lead, "./") && !strings.HasSuffix(lead, "../")
		after := end == len(s) || strings.ContainsRune(" \t\n'\");|&>\\", rune(s[end]))
		if before && after {
			return true
		}
		i = start + 1
	}
}

// codexWorkdir is the workdir an exec call names: workdir:"<dir>" in a
// custom tool's input, "workdir":"<dir>" in a function call's arguments.
var codexWorkdir = regexp.MustCompile(`workdir\\?"?\s*:\s*\\?"([^"\\]+)`)

// namesAny reports whether s names one of paths whole, or after a cd into
// the body's own directory (cd <dir> && cat prompt.md).
func namesAny(s string, paths []string) bool {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if strings.Contains(s, p) {
			return true
		}
		if dir := filepath.Dir(p); containsPath(s, dir) && strings.Contains(s, "cd ") && containsPath(s, filepath.Base(p)) {
			return true
		}
	}
	return false
}

// ReadAny: an assistant tool_use of Read whose file_path is one of paths, or
// of any other tool (Bash cat, sed, head) whose input names one.
func (claude) ReadAny(transcript string, paths []string) (bool, bool) {
	read, calls := false, 0
	ok := eachLine(transcript, func(line []byte) {
		if read || !bytes.Contains(line, []byte(`"tool_use"`)) {
			return
		}
		var e struct {
			Type    string `json:"type"`
			Cwd     string `json:"cwd"`
			Message struct {
				Content []struct {
					Type  string          `json:"type"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" {
			return
		}
		for _, c := range e.Message.Content {
			if c.Type != "tool_use" {
				continue
			}
			var in map[string]any
			if json.Unmarshal(c.Input, &in) != nil {
				continue
			}
			calls++
			if c.Name == "Read" {
				if fp, _ := in["file_path"].(string); slices.Contains(paths, fp) || namesIn(fp, e.Cwd, paths) {
					read = true
				}
				continue
			}
			for _, v := range in {
				if s, ok := v.(string); ok && namesIn(s, e.Cwd, paths) {
					read = true
				}
			}
		}
	})
	// A transcript in which no tool call is recognised has a format this
	// adapter does not know (the child ran wf prompt through some tool).
	return read, ok && (read || calls > 0)
}

// ReadAny: a tool call (exec's input, a function call's arguments) that names
// one of paths: Codex reads files through its shell.
func (codex) ReadAny(transcript string, paths []string) (bool, bool) {
	read, calls, cwd := false, 0, ""
	ok := eachLine(transcript, func(line []byte) {
		if read {
			return
		}
		var e struct {
			Type    string `json:"type"`
			Payload struct {
				Type      string `json:"type"`
				Input     string `json:"input"`
				Arguments string `json:"arguments"`
				Cwd       string `json:"cwd"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) != nil {
			return
		}
		if e.Type == "turn_context" && e.Payload.Cwd != "" {
			cwd = e.Payload.Cwd // the directory an exec with no workdir runs in
			return
		}
		if e.Type != "response_item" {
			return
		}
		switch e.Payload.Type {
		case "custom_tool_call", "function_call":
			calls++
			s := e.Payload.Input + e.Payload.Arguments
			dir := cwd
			if m := codexWorkdir.FindStringSubmatch(s); m != nil {
				dir = m[1]
				if !filepath.IsAbs(dir) && filepath.IsAbs(cwd) {
					dir = filepath.Join(cwd, dir) // a relative workdir runs under the turn's cwd
				}
			}
			read = namesIn(s, dir, paths)
		}
	})
	return read, ok && (read || calls > 0)
}

// ReadAny: a generic transcript's format is unknown.
func (Generic) ReadAny(string, []string) (bool, bool) { return false, false }

// hermes is Hermes Agent: its children are delegate_task subagents, each
// with a live transcript log under <root>/cache/delegation/live/<delegation>/
// task-<n>.log whose goal line carries the brief (and so the claim token).
// The adapter reads only those logs: no usage (Hermes keeps it in its own
// database, which the engine does not read), and no claim that children die
// with their coordinator until that is shown. Tiers, agent type and the
// spawn line are the generic adapter's.
type hermes struct{ Generic }

// Caller: a Hermes process's session and its home (HERMES_HOME, else
// ~/.hermes). Hermes stamps HERMES_DELEGATED_CHILD_CONTEXT into every
// subprocess of a delegate_task child (agent/delegation_context.py,
// tools/environments/local.py), and a child has its own HERMES_SESSION_ID:
// with the marker the caller is a child, without it the session's own agent,
// the coordinator (a child that ran next was taken for a newer
// coordinator; a coordinator's own prompt fetch went unrefused).
func (hermes) Caller(env Env) Caller {
	c := Caller{Harness: "hermes", Session: env("HERMES_SESSION_ID"), Root: home(env, "HERMES_HOME", ".hermes"), FromEnv: true}
	switch {
	case c.Session == "":
	case env("HERMES_KANBAN_TASK") != "":
		// A kanban worker's terminal carries the marker too
		// (delegated_child_subprocess_env), and a top-level worker may
		// coordinate: the environment cannot tell. Its delegate_task
		// children have the kanban keys scrubbed, so they are children.
	case env("HERMES_DELEGATED_CHILD_CONTEXT") != "":
		c.Is = IsChild // positive evidence, whatever else the environment holds
	case otherAgent(env):
		// Another harness runs inside a Hermes terminal: HERMES_SESSION_ID
		// is the outer session's, shared by that harness and its children.
	default:
		c.Is = IsSelf
	}
	return c
}

// Markers: a Hermes process's session.
func (hermes) Markers() []string { return []string{"HERMES_SESSION_ID"} }

// SameChild: a Hermes child has its own session, the one it fetched with.
func (hermes) SameChild(owner, caller Caller) bool { return owner.Session == caller.Session }

// OutputLimit: Hermes's terminal keeps tool_output.max_bytes characters of a
// command's output, 50000 by default (tools/tool_output_limits.py), head and
// tail. A config.yaml that lowers it is not read: such a user sees a long
// next cut.
func (hermes) OutputLimit(Env) (int, int) { return 50000, 0 }

// hermesLogs are the delegation logs under root.
func hermesLogs(root string) []string {
	logs, _ := filepath.Glob(filepath.Join(root, "cache", "delegation", "live", "*", "task-*.log"))
	return logs
}

// hermesHead is the start of a delegation log, where its goal line is.
const hermesHead = 16 << 10

// Only the initial goal and kickoff context are evidence. Their text can
// span lines; subsequent tool output and user turns cannot establish a claim.
var hermesEvent = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}\s+\w+\s*\|`)
var hermesKickoff = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}\s+user\s+\|\s+kickoff: `)

// hermesClaims finds every claim a brief names, in the syntax the CLI parses:
// --claim, then exactly one '=' or a run of spaces and tabs, then the value
// up to the next whitespace. The value is compared exactly: Go's flag
// parsing keeps everything after the first '=', so --claim==T is "=T".
var hermesClaims = regexp.MustCompile(`--claim(?:=|[ \t]+)(\S+)`)

// hermesNamesClaim reports whether the log head's goal or kickoff text names
// token as its claim, and names no other: a brief that names two claims
// vouches for neither. Only the brief is evidence. Hermes logs a goal and
// kickoff cut at about 500 characters and records neither the child's
// session nor its parent's, so nothing past the cut, and nothing the child
// did later, can bind the log to a caller. A coordinator
// passes the brief verbatim, its command first.
func hermesNamesClaim(head []byte, token string) bool {
	var brief []byte
	goal, kickoff, events := false, false, false
	for _, line := range bytes.Split(head, []byte("\n")) {
		if hermesEvent.Match(line) {
			if events || !hermesKickoff.Match(line) {
				break // only the goal and the first event, the kickoff, are the brief
			}
			events, goal, kickoff = true, false, true
		} else if !events {
			if bytes.HasPrefix(line, []byte("goal: ")) {
				goal = true
			}
			if bytes.HasPrefix(line, []byte("started: ")) {
				goal = false
			}
		}
		if goal || kickoff {
			brief = append(append(brief, line...), '\n')
		}
	}
	return onlyClaim(brief, token)
}

// onlyClaim reports whether brief names token as a claim and names no other
// claim: a brief that names two vouches for neither.
func onlyClaim(brief []byte, token string) bool {
	found := false
	for _, m := range hermesClaims.FindAllSubmatch(brief, -1) {
		if string(m[1]) != token {
			return false // a claim that is not this one
		}
		found = true
	}
	return found
}

// Resolve: the one delegation log, written no earlier than the hand-out,
// whose goal or kickoff line names the claim token; its agent id is
// <delegation>/task-<n>.
func (hermes) Resolve(root, _ string, wants []Want) map[Want]Child {
	logs := hermesLogs(root)
	out := map[Want]Child{}
	for _, w := range wants {
		if w.Token == "" {
			continue
		}
		var found []string
		for _, p := range logs {
			fi, err := os.Stat(p)
			if err != nil || (!w.After.IsZero() && fi.ModTime().Before(w.After)) {
				continue
			}
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			buf := make([]byte, hermesHead)
			n, _ := f.Read(buf)
			f.Close()
			if hermesNamesClaim(buf[:n], w.Token) {
				found = append(found, p)
			}
		}
		if len(found) == 1 {
			p := found[0]
			agent := filepath.Base(filepath.Dir(p)) + "/" + strings.TrimSuffix(filepath.Base(p), ".log")
			if w.Agent == "" || w.Agent == agent {
				out[w] = Child{Agent: agent, Transcript: p}
			}
		}
	}
	return out
}

// Ended: the log's last line is its final end line ("final | end status=").
// A child turn that failed ends the same way, "final | end status=failed
// exit_reason=... error: ..." (finalize in tools/delegation_live_log.py);
// Hermes's failed-turn notice ("This turn did not complete. ...") goes to the
// session database only.
func (hermes) Ended(transcript string) bool {
	last := ""
	return tailLines(transcript, func(line []byte) { last = string(line) }) && hermesEnd.MatchString(last)
}

var hermesEnd = regexp.MustCompile(`^\S+\s+final\s+\|\s+end status=`)

// hermesTool is one tool call line of a delegation log: "-> name(args".
var hermesTool = regexp.MustCompile(`^\S+\s+tool\s+\|\s+->\s+([a-z_]+)\((.*)$`)

// hermesLines is the line range Hermes appends to a read_file preview
// ("prompt.md L1-200").
var hermesLines = regexp.MustCompile(`\s+L\d+(?:-\d+)?$`)

// ReadAny: a tool call naming the body's full path is a read. Hermes shows a
// read_file by its file's base name alone (agent/display.py
// _preview_read_file), so a read_file of prompt.md cannot tell this unit's
// body from any other. Its result line can (G5): Hermes logs the tool's JSON
// result, whitespace collapsed and cut at 400 characters
// (tools/delegation_live_log.py), so a read_file result whose content
// preview is the start of this body, at least hermesPreviewMin characters of
// it, is a read. A base-name read with no such result is unknown (context
// unverified), never passed.
func (hermes) ReadAny(transcript string, paths []string) (bool, bool) {
	read, baseOnly, calls := false, false, 0
	var bodies []string // each body as Hermes numbers it, whitespace collapsed
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			bodies = append(bodies, collapseSpace(numberLines(string(b))))
		}
	}
	ok := eachLine(transcript, func(line []byte) {
		line = bytes.TrimRight(line, "\n")
		if read {
			return
		}
		if r := hermesReadResult.FindSubmatch(line); r != nil {
			if p := collapseSpace(hermesPreview(string(r[1]))); len(p) >= hermesPreviewMin {
				for _, b := range bodies {
					if strings.HasPrefix(b, p) {
						read = true
					}
				}
			}
			return
		}
		m := hermesTool.FindSubmatch(line)
		if m == nil {
			return
		}
		calls++
		name, args := string(m[1]), strings.TrimSuffix(string(m[2]), ")")
		if name == "read_file" {
			args = hermesLines.ReplaceAllString(args, "")
			for _, p := range paths {
				switch args {
				case p:
					read = true
				case filepath.Base(p):
					baseOnly = true
				}
			}
			return
		}
		read = namesAny(args, paths)
	})
	switch {
	case !ok:
		return false, false
	case read:
		return true, true
	case baseOnly:
		return false, false
	}
	return false, calls > 0
}

// hermesReadResult is a successful read_file result line of a delegation
// log, its JSON result in group 1.
var hermesReadResult = regexp.MustCompile(`^\S+\s+result\s+\|\s+read_file ok[^:]*: (\{.*)$`)

// hermesPreviewMin is the shortest content preview that counts as a read of
// a body: shorter, it could be any file's start.
const hermesPreviewMin = 200

// hermesPreview is the content a read_file result line shows: the JSON
// "content" string as far as the 400-character cut kept it, decoded.
func hermesPreview(result string) string {
	const key = `{"content": "`
	if !strings.HasPrefix(result, key) {
		return ""
	}
	s := result[len(key):]
	if i := strings.LastIndex(s, " …(+"); i >= 0 && strings.HasSuffix(s, " chars)") {
		s = s[:i]
	}
	// The string ends at its first unescaped quote, or where the cut fell.
	for i, esc := 0, false; i < len(s); i++ {
		switch {
		case esc:
			esc = false
		case s[i] == '\\':
			esc = true
		case s[i] == '"':
			s = s[:i]
			i = len(s)
		}
	}
	// A cut inside an escape leaves it unfinished: drop up to its length.
	for cut := 0; cut <= 6 && cut <= len(s); cut++ {
		var out string
		if json.Unmarshal([]byte(`"`+s[:len(s)-cut]+`"`), &out) == nil {
			return out
		}
	}
	return ""
}

// numberLines is text as Hermes's read_file shows it: "1|first\n2|second".
func numberLines(text string) string {
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	for i, l := range lines {
		lines[i] = strconv.Itoa(i+1) + "|" + l
	}
	return strings.Join(lines, "\n")
}

// collapseSpace joins s's words with single spaces, as Hermes's log lines do.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// ToolLimit: Claude Code's Bash tool stops a command after 120 s unless the
// call asks for more (BASH_DEFAULT_TIMEOUT_MS sets that default).
func (claude) ToolLimit(env Env) time.Duration {
	if ms, err := strconv.Atoi(env("BASH_DEFAULT_TIMEOUT_MS")); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 120 * time.Second
}

// ToolLimit: Codex's limit on one exec call is not one the adapter can read.
func (codex) ToolLimit(Env) time.Duration { return 0 }

// ToolLimit: Hermes's terminal tool times a command out after the timeout
// the model passes on that call, TERMINAL_TIMEOUT seconds (180 when unset)
// only when it passes none (tools/terminal_tool.py). The adapter cannot see
// the call's own value, and models pass short ones: deepseek-v4-flash passed
// 30 s on 41 of 45 next calls in FANOUT-2 run B, and next was cut off at
// 30 s. The limit is therefore the shorter of the default and hermesCallLimit.
func (hermes) ToolLimit(env Env) time.Duration {
	if s, err := strconv.Atoi(env("TERMINAL_TIMEOUT")); err == nil && s > 0 {
		return min(time.Duration(s)*time.Second, hermesCallLimit)
	}
	return hermesCallLimit
}

// hermesCallLimit is the shortest per-call terminal timeout Hermes models
// were seen to pass to a workflow command.
const hermesCallLimit = 30 * time.Second

// ToolLimit: a generic harness's limit is unknown.
func (Generic) ToolLimit(Env) time.Duration { return 0 }

// otherAgent reports whether env shows an agent harness other than Hermes.
func otherAgent(env Env) bool {
	for _, m := range markersExcept(env, "hermes") {
		if !weakMarker(m) {
			return true
		}
	}
	return false
}

// weakMarker reports whether m is a registered adapter's weak marker
// (WeakDetector): one a process inherits whatever harness it is.
func weakMarker(m string) bool {
	for _, a := range registry {
		if w, ok := a.(WeakDetector); ok && slices.Contains(w.WeakMarkers(), m) {
			return true
		}
	}
	return false
}
