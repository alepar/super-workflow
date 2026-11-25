package wf

import (
	"fmt"
	"io"
	"sort"
)

// Main is the whole client binary.
//
// Registration convention: each definition package exports
// `func Definition() *wf.Def`, and the client binary is
// `func main() { wf.Main(a.Definition(), b.Definition()) }`. A binary that
// drives runs with adapters, required parameters or extra subcommands calls
// MainWith (see MainConfig); Main is MainWith over bare registrations and no
// adapters.
//
// Inspection subcommands: lint, graph (default), mermaid, hash; each takes an
// optional definition name and lints the selected definitions first, so an
// invalid definition never prints. Run-driving subcommands, each taking the run
// id first (`run` takes a definition name) and --state-dir (default .workflow,
// made absolute): run, next, submit, fail, answer, settle, status, journal. The
// table is Subcommands. `next <run>` (no count), `submit` and `fail` are
// aliases over the paged state machine (`next <run> <n>`, `done`, `failed`):
// `next` hands out every unit with an implicit claim under session "wave" and
// keeps the inline prompt, `submit` is done and `fail` is failed. A wave unit
// has no transcript, so it never stalls, and a paged coordinator sees it as an
// orphan of session "wave". Every run-driving subcommand builds its own FileJournal
// and Engine, so processes share nothing but the state directory.
//
// Single-registration mode. A binary built over one definition (MainConfig.Binary
// set, one Registration, for example a client binary) drops the definition name:
// `run [flags]` starts or resumes the run and records its id in
// <state-dir>/run-id. In both CLI modes, run prints the id on stdout and the
// coordinator bootstrap on stderr. Named `next` prints that bootstrap again
// (the loop, run id, state dir and harness), and the run-driving verbs
// sit under `wf <verb>`, taking the run id from --run-id, else <state-dir>/run-id,
// else the only run under <state-dir>/runs (none or several is exit 2).
// lint, graph, mermaid and hash act on the one definition. The command lines
// the engine emits then read `<bin> wf <verb> --state-dir <dir> <args>`.
//
// COORDINATOR PROTOCOL. The coordinator is an agent session that owns no
// workflow logic. It runs `run <def> --param k=v` (which prints the RunID), then
// repeats: call `next <run>`; spawn one subagent per instruction, in parallel,
// each with the instruction's dispatch hints (model, effort, agentType) and its
// prompt; each subagent does the work, writes its result JSON to a file and
// runs the instruction's submit command line; when every subagent has returned,
// call `next` again. Stop at a terminal status (`status <run>` shows phase
// ended) or at a human gate (a human-gate instruction, answered with `answer`
// or the instruction's own answer command). A manual-verification instruction
// carries an effect id: a human checks the world and runs `settle`. A wait
// instruction carries retryAfterSeconds: call `next` again after that long.
//
// NEXT OUTPUT. `next` prints a JSON array of instructions, indented, in the
// Engine's order, and `[]` when nothing is ready. Each element has: run (the run
// id), state (the qualified state), kind (agent, human-gate, wait,
// manual-verification or ended), attempt (1-based), token (echo it when submitting),
// prompt (the resolved prompt with its Inputs and Submission blocks), inputs
// (optional name to value map), submit (the exact submit command line, with
// <file> to replace by the output path), child (the child instance id, absent
// for the run itself), dispatch (model, effort and agentType hints, each absent
// when unset; the --model-map is already applied), workspace, effect (the effect
// id to settle), retryAfterSeconds, question and answers (human gates).
//
// SUBMIT. `submit <run> --token t --output file|- [--usage tokens=N]` reads the
// output JSON from the file (or stdin for -) and prints the SubmitResult as
// JSON. `fail <run> --token t --reason r` abandons an attempt (spent as
// Refused). `answer <run> --gate state --value v` answers a human gate.
// `settle <run> --effect id --applied|--not-applied` settles an Uncertain effect.
// `status <run>` prints the RunStatus as JSON; `journal <run>` prints the
// journal as JSON Lines. `run` accepts --param k=v (repeatable), --options
// file (JSON for the definition's options struct), --workspace dir (default the
// current directory), --run-id id, and --model-map key=model[,key=model]
// (a key is a tier, all, <tier>.effort, all.effort or a model; resolved per
// harness segment and journaled; resume, dev rewind and fanout take it too).
// Running again with the same run key or --run-id resumes the run.
//
// Exit codes (the table is ExitCodes): 0 ok; 2 usage (bad flags, unknown
// subcommand or definition, missing required parameter, malformed --model-map,
// lint failure, or a host preflight refusal: stderr `refused: <check>:
// <cause>` then `fix: <remedy>`, which the operator must act on, not a retry);
// 3 unknown-run (no journal for the run id); 4 rejected (stale
// or superseded token, an answer outside the Enum, no such open gate or
// effect); 5 refused (output malformed or refused: done spends the
// attempt, and a fresh one is issued while retry remains, else the state
// takes its failure route; validate records nothing; the first done of an
// attempt with no output file keeps the attempt open); 6 infrastructure
// (journal or definition-hash error, or a call that became busy after it had
// recorded something: it names what and the command to run next);
// 7 ended (`next` found the run ended and printed its
// single ended instruction: terminal, outcome, cause); 8 check-failed; 9 busy
// (the run's lock stayed held past the call's wait and it changed nothing:
// run the same command again). A submit classed Accepted, Stale, Blocked or
// Unavailable exits 0: the run moved and `next` shows where.
func Main(defs ...*Def) {
	regs := make([]Registration, len(defs))
	for i, d := range defs {
		regs[i] = Registration{Def: d}
	}
	MainWith(MainConfig{Defs: regs})
}

func run(args []string, defs []*Def, stdout, stderr io.Writer) int {
	defs = append([]*Def{}, defs...)
	sort.SliceStable(defs, func(i, j int) bool { return defs[i].name < defs[j].name })
	cmd := "graph"
	if len(args) > 0 {
		cmd = args[0]
	}
	if len(args) > 1 {
		var sel []*Def
		for _, d := range defs {
			if d.name == args[1] {
				sel = append(sel, d)
			}
		}
		if len(sel) == 0 {
			fmt.Fprintf(stderr, "unknown definition %s\n", args[1])
			return 2
		}
		defs = sel
	}
	bad := false
	for _, d := range defs {
		if ps := d.LintServed(); len(ps) > 0 {
			bad = true
			fmt.Fprintf(stderr, "lint %s: %d problem(s)\n", d.name, len(ps))
			for _, p := range ps {
				fmt.Fprintln(stderr, "  "+p.String())
			}
		}
	}
	if bad {
		return 2
	}
	for i, d := range defs {
		switch cmd {
		case "lint":
			fmt.Fprintf(stdout, "lint %s: ok\n", d.name)
			// The staged rules print as notes and fail nothing until every
			// client is migrated to them.
			for _, p := range d.LintStaged() {
				fmt.Fprintln(stdout, "  "+p.String())
			}
			for _, p := range d.Notes() {
				fmt.Fprintf(stdout, "  %s\n", p)
			}
		case "graph", "mermaid":
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			if cmd == "graph" {
				fmt.Fprint(stdout, d.Render())
			} else {
				fmt.Fprint(stdout, d.Mermaid())
			}
		case "hash":
			fmt.Fprintf(stdout, "%s %s\n", d.name, d.Hash())
		}
	}
	return 0
}
