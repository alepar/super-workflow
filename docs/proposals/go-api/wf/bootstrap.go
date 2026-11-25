package wf

import (
	"fmt"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
)

// wfLine is the command line of one `wf` verb of a single-registration binary:
// `<bin> wf <verb> --state-dir <dir> <args> --run-id <run>`. Every arg goes
// through lineArg. The run is always named: unit names repeat across the runs
// of one state root, so a line that left the run to the recorded default could
// act on another run (item 70). It comes last so the verb's own arguments keep
// their places.
func wfLine(bin, stateDir string, run RunID, verb string, args ...string) string {
	parts := []string{sink.Quote(bin), "wf", verb}
	// Keep prompt claims inside harness kickoff previews, before long paths.
	if verb == "prompt" && len(args) >= 2 && args[0] == "--claim" {
		parts = append(parts, "--claim", lineArg(args[1]))
		args = args[2:]
	}
	parts = append(parts, "--state-dir", sink.Quote(stateDir))
	for _, a := range args {
		parts = append(parts, lineArg(a))
	}
	if run != "" {
		parts = append(parts, "--run-id", sink.Quote(string(run)))
	}
	return strings.Join(parts, " ")
}

// Bootstrap renders the named-binary coordinator instructions (C1): the run
// header, one harness line, and the loop. Every other rule arrives with the
// output of the verb that makes it apply (C2): how to spawn with each unit
// next prints, the child's rules with its prompt, holds, stalls and the end
// with their events. A resumed run needs nothing more: a new coordinator's
// next clears what an earlier one left (B4).
func Bootstrap(rs RunStatus, bin, stateDir string) string {
	return bootstrap(rs, stateDir, wfLine(bin, stateDir, rs.Run, "next"))
}

// bootstrap uses the serving CLI's command line, including its run selection.
func bootstrap(rs RunStatus, stateDir, nextLine string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: this text replaces anything you remember or were told elsewhere about this tool.\n", ProtocolVersion)
	fmt.Fprintf(&b, "You are the coordinator of workflow run %s.\nrun: %s\nstate dir: %s\n", rs.Run, rs.Run, stateDir)
	b.WriteString(harnessLine(rs) + "\n")
	if line := modelsLine(rs.Models, segmentHarnessOf(rs)); line != "" {
		b.WriteString(line + "\n")
	}
	b.WriteString(toolWorkdirAdvice(rs.Workspace))
	switch rs.Harness {
	case "codex":
		b.WriteString(codexWaitAdvice + "\n")
	case "pi":
		b.WriteString(piSpawnAdvice + "\n")
	case "hermes":
		b.WriteString(hermesApprovalAdvice + "\n")
	}
	if rs.Detach == DetachNone {
		b.WriteString(detachNoneAdvice + "\n")
	}
	b.WriteString("\n" + whatThisRunDoes(rs.Workspace, rs.Work) + "\n")
	b.WriteString("\n" + agentRules(rs.usesTracker) + coordinatorRulesTail + "\n")
	b.WriteString("\nloop:\n")
	fmt.Fprintf(&b, "  %s\n", workspaceCommand(rs.Harness, rs.Workspace, nextLine))
	b.WriteString("    -> Start one fresh child agent per offer, as many as your harness allows. Use its call and brief. Repeat.\n")
	b.WriteString("Report when next prints the ended instruction or asks for a person, and quote it. For questions, use only the person's reply.\n")
	return b.String()
}

// The base rules apply to every agent of a run, a coordinator or a sub-agent: what moves the run, the defaults with their reasons, and the
// few hard rules (the engine protocol, the person's permissions, other runs).
// They say what to do; they do not police the agent (guidance, not
// containment). Every spawn brief carries their short form
// (RulesForSpawnBrief), since a child that never fetches its prompt sees only
// its brief. Exported so a client's prompt preamble quotes the same text.
const baseAgentRules = "Rules for this run. Move this run only with the workflow engine's verbs, the commands it prints; ordinary shell work (mkdir, reading files) is your own. " +
	"By default run a printed command as printed, one per line, with no wait loop around it: the engine's commands wait by themselves. " +
	"If a command refuses, say what blocked you and what you need: a child with its failed command; a coordinator goes on with next, and reports to the person who started the run when a command asks for a person. " +
	"Never edit or delete lock, journal or state files (see the lock verb), use flags that bypass a sandbox or approvals, reach a refused permission or approval another way, or address another run's state."

// RulesForAnyAgent includes the rule for definitions that declare a tracker.
const RulesForAnyAgent = baseAgentRules +
	" The workflow moves this run's tracker items: change one only if your task's prompt says so."

func agentRules(tracker bool) string {
	if tracker {
		return RulesForAnyAgent
	}
	return baseAgentRules
}

// whatThisRunDoes is the coordinator bootstrap's statement of the run's work
// : an unattended run, the commands its coordinator runs, and the
// work its units do (Def.Describe), as plain fact, so a permission checker
// reading only this context sees the expected work.
func whatThisRunDoes(workspace, work string) string {
	s := "What this run does: an unattended workflow run in " + workspace
	if work != "" {
		s += "; its units " + work
	}
	return s + ". You run next, which blocks for minutes while units work (normal), status and any recovery command it prints."
}

// whatThisUnitRuns is a spawn brief's second line, after the prompt command
// : the commands the unit runs and the work it does.
func whatThisUnitRuns(work string) string {
	s := "This unit runs the commands its prompt prints (prompt, validate, done, failed), by default as printed, one per line, with no loops, $(...), variable expansion or bash -c"
	if work != "" {
		s += "; its work: " + work
	}
	return s + "."
}

// rulesHead opens every agent prompt the engine renders (agentOps.render).
func (c *cycle) rulesHead() string {
	return agentRules(c.st.def.usesTracker()) + "\n\n"
}

// The base spawn rules are the rules for any agent as every spawn brief ends:
// the same rules, short, since a coordinator reads every brief it is offered
// and its tool output caps how many fit (a child that never
// fetched saw only its brief). It holds no single quote: a pi brief is
// single-quoted.
const baseSpawnRules = "Rules for this run: do your assigned task with ordinary tools, and change workflow state only through the workflow commands you are given for this run. Never edit the lock, journal or state files of the workflow, bypass a sandbox or approvals, or start another agent CLI except through the child dispatch of the engine."

const spawnBlockedRule = " If you are blocked, run your failed command with --reason saying what blocked you and what you need."

// RulesForSpawnBrief includes the rule for definitions that declare a tracker.
const RulesForSpawnBrief = baseSpawnRules +
	" The workflow moves the tracker items of this run: change one only if the prompt of your task says so." + spawnBlockedRule

func spawnRules(tracker bool) string {
	if tracker {
		return RulesForSpawnBrief
	}
	return baseSpawnRules + spawnBlockedRule
}

// RulesForCoordinators are the coordinator's own rules: how it fans out
// units. The agent CLI rule is the user's own; it excepts the engine's child
// dispatch, since the pi, opencode and agy adapters launch agent CLIs as
// children.
const RulesForCoordinators = "When you hand out this run's units, start each sub-agent with your own harness's sub-agent tool. Do not start another agent CLI (codex, claude, hermes) to do a unit, except through the engine's child dispatch (the call next prints), unless the workflow or the person who started the run explicitly asks you to."

// RulesForThisRun are the rules the coordinator bootstrap states up front.
const RulesForThisRun = RulesForAnyAgent + coordinatorRulesTail

const coordinatorRulesTail = " " + RulesForCoordinators +
	" The workflow, not your judgment, decides when a phase or the run is over: keep following next. End your turn only if your child is running, the run ends, or next says to wait or stop."

// ProtocolVersion names the coordinator protocol the bootstrap teaches: its
// first line, so a coordinator that remembers an older loop sees that this
// one replaces it (G2).
const ProtocolVersion = "coordinator protocol 4 (next blocks)"

// codexWaitAdvice is the bootstrap's line for a Codex coordinator: Codex's
// exec_command hands back after its yield time with the command still
// running, so the
// loop's one call is kept by waiting on that same session (H2).
const codexWaitAdvice = "codex: next blocks up to 100 s. If exec_command returns while next runs, wait on that same session with write_stdin (empty input, your longest yield_time_ms); never start a second next."

// piSpawnAdvice is the bootstrap's line for a pi coordinator: pi has no
// sub-agent tool, so the rules' "your harness's sub-agent tool" is the
// background pi -p line each offer prints, not "another agent CLI".
const piSpawnAdvice = "pi: no sub-agent tool; run the background pi -p command each offer prints with bash, the brief pasted unchanged in place of <the text between the lines> (a here-document)."

const hermesApprovalAdvice = "hermes: smart approvals may deny a delegated child without prompting. Have it report the exact action with failed --class approval --reason. Use the normal prompt or leave it parked."

// detachNoneAdvice is the bootstrap's line for a run at detach none; an
// instruction of a run that stepped down to none mid-run carries it too.
const detachNoneAdvice = "detach: none on this host (a command's child processes die with it): a long command step runs inside next itself, past its usual wait, printing progress there. Run next with your longest tool timeout or your harness's background-task tool, never a wait loop. If a timeout kills next, your following next recovers the step."
