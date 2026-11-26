package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// agy is the adapter for the Antigravity CLI, agy 1.5.12. A child is a
// subagent conversation the coordinator starts with its invoke_subagent tool;
// it runs inside the coordinator's own agy process. In every command's shell
// ANTIGRAVITY_CONVERSATION_ID is the caller's own conversation id, a child's
// its own. The shell has no parent marker, so the
// environment never tells a child from its coordinator: only agy's own
// records can. Identity is cooperative: the same OS user can edit those
// records, so it catches a wrong or accidental child, not a forger. The
// identity methods (Caller, Resolve, SameChild) live in agy_identity.go. Every
// agy-specific engine answer is a method on agy and nowhere else; callers
// never branch on the name agy. The readers it does not override are
// Generic's: they read nothing.
type agy struct{ Generic }

func (agy) Name() string { return "agy" }

var _ ModelChecker = agy{}

// CheckModel: invoke_subagent's Model takes only agy's tier names
// (agyTiers); a model id is refused before any spawn line names it.
func (agy) CheckModel(model string) error {
	if slices.Contains(agyTiers, model) {
		return nil
	}
	return fmt.Errorf("agy takes only %s", strings.Join(agyTiers, ", "))
}

// Markers: agy sets ANTIGRAVITY_CONVERSATION_ID in every command it runs.
func (agy) Markers() []string { return []string{"ANTIGRAVITY_CONVERSATION_ID"} }

// UnchainedCommands: agy 1.5.12 Default-mode permissions approve a command
// only by a command(<leading words>) allow rule, which refuses chained
// commands, so agy callers get one unchained engine command per line.
func (agy) UnchainedCommands() bool { return true }

// agyInherit is the tier value that runs a child on its coordinator's model.
const agyInherit = "inherit"

// agyTiers is agy's tier vocabulary, the values invoke_subagent's Model takes:
// SpawnLine and the model check read the same list.
var agyTiers = []string{agyInherit, "flash_lite", "flash", "pro"}

// agyAgentType is the subagent type every unit is started as: a copy of the
// coordinator's own agent, the only type the probe ran.
const agyAgentType = "self"

// AgentType: every unit is a "self" subagent.
func (agy) AgentType() string { return agyAgentType }

// Tiers: every engine tier runs on the coordinator's model; a --model-map
// value picks another agy tier.
func (agy) Tiers() map[string]Tier {
	return map[string]Tier{
		"light":    {Model: agyInherit},
		"standard": {Model: agyInherit},
		"strong":   {Model: agyInherit},
		"top":      {Model: agyInherit},
	}
}

// agySpawnArgs are invoke_subagent's flat arguments in 1.5.12's form, one
// call per child, in the order the line shows them.
type agySpawnArgs struct {
	TypeName string `json:"TypeName"`
	Role     string `json:"Role"`
	Prompt   string `json:"Prompt"`
	Model    string `json:"Model,omitempty"`
}

// SpawnLine is the literal invoke_subagent call that starts one unit: the
// brief verbatim as Prompt, its leading command the engine's one unchained
// command; Model only for a tier other than inherit. Effort and an agent type
// have no agy argument and are not passed.
func (agy) SpawnLine(s Spawn) string {
	args := agySpawnArgs{TypeName: agyAgentType, Role: "wf-unit", Prompt: s.Brief}
	if s.Model != agyInherit {
		args.Model = s.Model
	}
	return "invoke_subagent " + agyJSON(args)
}

// agyJSON is v as one line of JSON with <, > and & left literal, so the line
// a coordinator copies shows the brief as written; it decodes byte-equal to
// v's strings when they are valid UTF-8 (encoding/json replaces invalid
// bytes).
func agyJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // a struct of strings always encodes
	return strings.TrimSuffix(b.String(), "\n")
}

// ResumeLine continues the child conversation agent with message: agy
// delivers a send_message to an idle child, which wakes and answers (probe 2,
// matching the child). "" without an agent to address.
func (agy) ResumeLine(agent, message string) string {
	if agent == "" {
		return ""
	}
	return "send_message " + agyJSON(struct {
		Recipient string `json:"Recipient"`
		Message   string `json:"Message"`
	}{agent, message})
}

// ChildrenDieWithCoordinator: a child is a conversation inside the
// coordinator's agy process (probe 2's ps showed one agy process per run; no
// kill test was run).
func (agy) ChildrenDieWithCoordinator() bool { return true }

// Limit: agy exposes no subagent limit the adapter can read.
func (agy) Limit(Env, string) (int, string) { return 0, "" }

// OutputLimit: a tool result keeps 9,838 bytes, head and tail, the middle
// cut; no line
// limit. The task-<n> completion message's own cut was not measured and is
// assumed equal; when the engine's output looks cut, the coordinator runs the
// engine's read-only status command, never the blocking command again.
func (agy) OutputLimit(Env) (int, int) { return 9 << 10, 0 }

// ToolLimit: agy cuts no command off. One still running at its call's
// WaitMsBeforeAsync is backgrounded and its output delivered later by a
// task-<n> message.
func (agy) ToolLimit(Env) time.Duration { return 0 }
