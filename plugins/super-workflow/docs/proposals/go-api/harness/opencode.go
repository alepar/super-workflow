package harness

import (
	"path/filepath"
	"strconv"
	"time"
)

// opencode is the adapter for the OpenCode CLI, 1.18.x. A child is a
// session the coordinator starts with OpenCode's task tool; it runs inside
// the coordinator's own opencode process. The adapter uses only OpenCode's
// public surface: the task tool's parameters, the variables a plugin's
// shell.env hook exports, and OpenCode's documented environment variables
// and defaults. It reads no OpenCode storage and runs no program, so every
// record question (Resolve, Ended, ReadAny, telemetry) has Generic's
// no-records answer: what the engine knows of a child comes from the child
// through the engine's verbs, and liveness from heartbeats and the lease.
//
// OpenCode 1.18 puts no session id in a command's environment. The
// engine's OpenCode plugin (opencode/super-workflow.js in this module) exports
// OPENCODE_SESSION_ID, the calling session, and OPENCODE_PARENT_SESSION_ID,
// its parent: empty for a root session, "unknown" when the plugin's lookup
// failed. Without the plugin a shell has OPENCODE alone and no identity.
type opencode struct{ Generic }

func (opencode) Name() string { return "opencode" }

// The plugin's variables.
const (
	opencodeSession = "OPENCODE_SESSION_ID"
	opencodeParent  = "OPENCODE_PARENT_SESSION_ID"
	opencodeMarker  = "OPENCODE"
	// opencodeUnknownParent is the plugin's parent value when its lookup failed.
	opencodeUnknownParent = "unknown"
)

// Markers: the plugin's session variable, then OpenCode's own OPENCODE=1.
func (opencode) Markers() []string { return []string{opencodeSession, opencodeMarker} }

// WeakMarkers: anything started from an opencode shell inherits OPENCODE,
// so it names opencode only when no adapter's other markers are set. The
// plugin sets OPENCODE_SESSION_ID afresh for every opencode command.
func (opencode) WeakMarkers() []string { return []string{opencodeMarker} }

// Caller: the coordinator's session is the parent for a child and the
// session itself for a root. A parent the plugin could not look up leaves
// the caller with no session, its own as its agent id: it may be a child,
// so it never drives a run a coordinator drives (J1 refuses it), and the
// plugin looks the parent up again on its next command. Its own child verbs
// are bound by their claim token.
//
// Root names opencode's data directory, $XDG_DATA_HOME/opencode, else
// ~/.local/share/opencode: it tells one opencode profile from another and is
// never read.
func (opencode) Caller(env Env) Caller {
	c := Caller{Harness: "opencode", Session: env(opencodeSession), FromEnv: true}
	if r := home(env, "XDG_DATA_HOME", filepath.Join(".local", "share")); r != "" {
		c.Root = filepath.Join(r, "opencode")
	}
	switch parent := env(opencodeParent); {
	case c.Session == "":
	case parent == "":
		c.Is = IsSelf
	case parent == opencodeUnknownParent:
		c.Session, c.Agent = "", c.Session
	default:
		c.Session, c.Agent, c.Is = parent, c.Session, IsChild
	}
	return c
}

// WeakHint: OPENCODE without the plugin's session variable is opencode
// without the plugin, or another harness started from an opencode shell.
func (opencode) WeakHint(env Env) string {
	if env(opencodeMarker) == "" || env(opencodeSession) != "" {
		return ""
	}
	return "OPENCODE is set with no OPENCODE_SESSION_ID: if this is opencode, install the engine's opencode plugin (opencode/super-workflow.js), or its sessions are not told apart"
}

// SessionHint: a caller has no session when the plugin could not look up
// its parent.
func (opencode) SessionHint(env Env) string {
	if env(opencodeSession) == "" || env(opencodeParent) != opencodeUnknownParent {
		return ""
	}
	return "the opencode plugin could not look up this session's parent; run the command again"
}

// SameChild: a child's agent is its own session; a fetch made while the
// plugin's parent lookup failed recorded no coordinator session, so the
// agent alone matches it.
func (opencode) SameChild(owner, caller Caller) bool {
	return owner.Agent == caller.Agent && (owner.Session == "" || caller.Session == "" || owner.Session == caller.Session)
}

// opencodeAgentType is the subagent every unit runs as: OpenCode's built-in
// general agent.
const opencodeAgentType = "general"

// AgentType: every unit is a general subagent.
func (opencode) AgentType() string { return opencodeAgentType }

// opencodeDescription is the task tool's required short description.
const opencodeDescription = "run unit"

// SpawnLine is the task call that starts one unit, the brief verbatim as
// its prompt. The task tool takes no model or effort: a general child runs
// on the calling message's model and variant.
func (opencode) SpawnLine(s Spawn) string {
	return "task(" + spawnArgs("description", opencodeDescription, "subagent_type", s.AgentType, "prompt", s.Brief) + ")"
}

// ResumeLine continues the child session agent through the same tool with
// task_id: OpenCode appends message to that session. "" without an agent.
func (opencode) ResumeLine(agent, message string) string {
	if agent == "" {
		return ""
	}
	return "task(" + spawnArgs("description", opencodeDescription, "subagent_type", opencodeAgentType, "task_id", agent, "prompt", message) + ")"
}

// ResumeNote: OpenCode does not refuse a task_id it does not know.
func (opencode) ResumeNote() string {
	return "a task_id OpenCode does not know starts a fresh child instead, which has no claim and is not this unit; restart the unit then"
}

// Tiers: none. The task tool takes no model.
func (opencode) Tiers() map[string]Tier { return nil }

var _ NoModelSpawn = opencode{}

// NoModel: no model reaches a child, so no tier and no --model-map value
// applies.
func (opencode) NoModel() string {
	return "opencode's task tool takes no model: children run on the coordinator's model"
}

// ChildrenDieWithCoordinator: a child is a session inside the coordinator's
// opencode process.
func (opencode) ChildrenDieWithCoordinator() bool { return true }

// OutputLimit: OpenCode cuts a tool's output at 2000 lines or 51200 bytes
// (its tool_output defaults). A config that changes them is not read.
func (opencode) OutputLimit(Env) (int, int) { return 51200, 2000 }

// opencodeBashTimeout is the bash tool's default timeout.
const opencodeBashTimeout = 120 * time.Second

// ToolLimit: the bash tool's default timeout,
// OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS when set, else 120 s.
func (opencode) ToolLimit(env Env) time.Duration {
	if ms, err := strconv.Atoi(env("OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS")); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return opencodeBashTimeout
}
