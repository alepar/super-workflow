package wf

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Registration is one definition a client binary serves.
type Registration struct {
	// Check runs calibration and simulation after the CLI has passed structural
	// and check-example lint. Register wftest.CheckCommand only in a separate
	// checker binary; shipped workflow binaries must not import wftest.
	// The callback prints its report and returns an error for findings.
	Check    func(io.Writer) error
	Def      *Def
	Required []string // run params that must be passed to run (e.g. skillsRoot)
	// Params documents the run params `run` accepts and computes the default of
	// each one --param leaves out, in order (a later default may read an earlier
	// one). `run --help <def>` lists them. A param missing here has no default.
	Params []ParamSpec
	// DefaultOptions is the options value a fresh run starts with. A given
	// --options file is decoded over it, so a field the file leaves out keeps
	// its default; nil leaves the definition's zero options as the base. A
	// successor's base is its predecessor's options, a resume's the journaled
	// ones.
	DefaultOptions any
	// Preflight are the client's host checks, run before a run exists.
	Preflight []PreflightCheck
	// PagedPrompts makes paged prompt delivery the default of the runs this
	// registration starts, as --paged-prompts does; --paged-prompts=false
	// opts one run out. A definition that routes on FetchCoverageOf needs
	// it. A continued run keeps its predecessor's choice.
	PagedPrompts bool
	// Budget computes the run budget of a run this registration creates from
	// the options it starts with (a value of the definition's options type,
	// its zero value when none are given). run --budget overrides the limits
	// it names; nil is no default budget. The budget is policy: the numbers
	// belong to the workflow, the stop to the engine.
	Budget func(options any) RunBudget
}

// ParamSpec is one run param of a registration: required, or with a default
// that is a fixed value or computed from the run.
type ParamSpec struct {
	Name string
	// Required makes run refuse (exit 2) without it; Value then names the shape
	// the refusal shows ("<cmd>").
	Required bool
	// Value is the fixed default; for a required or derived param, the shape of
	// the value --help shows.
	Value string
	// Source says where the value comes from, for --help.
	Source string
	// Derive computes the default; nil means Value is the default.
	Derive func(RunContext) (string, error)
	// File lets the value be @<path>: run replaces it with the text of that file
	// (relative to the workspace), so the run journals the text itself.
	File bool
	// AtCreation derives the default only when the call creates a run (a fresh
	// run or a successor), after the run key has picked the run: a resume
	// attaches to the journaled params, so a costly derivation (a network
	// probe) is not paid for nothing. Such a param cannot be part of the run key.
	AtCreation bool
}

// RunContext is what a param default reads.
type RunContext struct {
	Assets    string            // effective absolute asset root selected for this invocation
	Params    map[string]string // the params given and those derived before this one
	Workspace string            // absolute
	Now       time.Time         // UTC
}

// MainConfig is what a client binary hands to the CLI. wf cannot import the
// adapter packages or the evals, so the registry and the extra subcommands
// arrive from the client.
type MainConfig struct {
	Defs     []Registration
	Adapters *Adapters                                                    // nil means an empty registry: every op is unavailable
	Extra    map[string]func(args []string, stdout, stderr io.Writer) int // extra subcommands (cmd/workflow's eval); names starting __ are hidden from usage
	// EffectRunner runs .Detached() commands so they outlive the CLI call; nil
	// runs them inline.
	EffectRunner EffectRunner
	// Binary names a named binary (for example "super"), selected once in CLI:
	// `run [<def>]` starts the named definition, the first registration when
	// none is named, and records the run id in the state dir; `next` prints the
	// bootstrap, and the run-driving verbs sit under `wf` with the run id from
	// --run-id, else the state dir. Empty keeps the multi-definition CLI.
	Binary string
	// DefaultStateDir is the journal directory for this client entry when
	// --state-dir is absent. The client selects the entry and supplies its
	// directory; this applies to every definition in Defs and every driving
	// verb. Relative paths resolve against the caller's current directory.
	// Empty preserves the generic engine default, .workflow.
	DefaultStateDir string
	// StateDirFor, when set, is the default journal directory of `run <def>`
	// for the definition run starts (named binary): a client keeps one state
	// dir per entry without reading the command line itself. Every other
	// verb uses DefaultStateDir. An empty result falls back to it.
	StateDirFor func(def string) string
	// SuccessorNote, when set, is text the engine appends (after a space) to
	// its `previous run <id> ended: <cause>; continuing as <new id>` line on
	// `run`: the client's reading of the successor of definition def, whose
	// records directory (absolute, carried from the predecessor) is records.
	// An empty result appends nothing.
	SuccessorNote func(def, records string) string
	// EndedNote, when set, is text the engine prints after a text `wf next`
	// whose run has ended (not stopped): the client's hand-back for status
	// st, whose records directory (absolute) is records. It ends in a
	// newline; an empty result prints nothing. A JSON next never carries it.
	EndedNote func(st RunStatus, records string) string
	// InstalledBinary is the workspace-relative path a client installs its
	// binary at (for example .superpowers/bin/super). A snapshot fork whose
	// restored world lacks it gets the running binary there, since a world
	// snapshot (git) leaves an ignored install path out. Empty installs none.
	InstalledBinary string
	// SnapshotFiles are workspace-relative files a world snapshot carries
	// even when the world's git ignores them, so a fork restores them as they
	// were: the records an installer keeps of what it owns (super's
	// .superpowers/install-manifest.json), which only the checkpoint can
	// supply, unlike InstalledBinary, which the running build can.
	SnapshotFiles []string
}

// endedNote prints the client's EndedNote after a text next that handed out
// an ended or stopped instruction, when the run's status says it ended and
// was not stopped (a waiting human gate shares the exit code).
func (c *cli) endedNote(e *Engine, stateDir string, run RunID) {
	if c.cfg.EndedNote == nil {
		return
	}
	st, err := e.Status(run)
	if err != nil || st.Phase != RunEnded || st.Stopped || st.Run == "" {
		return
	}
	records, err := filepath.Abs(RecordsDir(stateDir, st.Run))
	if err != nil {
		return
	}
	fmt.Fprint(c.stdout, c.cfg.EndedNote(st, records))
}

// RecordsDir is the records directory of run under stateDir: where {records}
// binds for a CLI-driven run and where a successor's carried records land.
// It is a path only; nothing is created.
func RecordsDir(stateDir string, run RunID) string {
	return filepath.Join(stateDir, "runs", string(run), "records")
}

// named reports whether the CLI runs as a named binary.
func (cfg MainConfig) named() bool { return cfg.Binary != "" }

func (cfg MainConfig) stateDir() string {
	if cfg.DefaultStateDir != "" {
		return cfg.DefaultStateDir
	}
	return defaultStateDir
}

// MainWith runs the CLI over os.Args and exits with its code.
func MainWith(cfg MainConfig) {
	TuneGCForCLI()
	os.Exit(CLI(cfg, os.Args[1:], os.Stdout, os.Stderr))
}

// cliGCPercent is the collector's target for a CLI process; see TuneGCForCLI.
const cliGCPercent = 400

// TuneGCForCLI sets the garbage collector for a process that runs one CLI
// call, as MainWith does: a workflow binary whose main calls CLI itself calls
// it first. A call is short and allocation-heavy (it reads the journal and
// walks the definition), so collecting when the heap reaches five times its
// live size instead of twice saves about a quarter of its CPU, for a peak RSS
// up to about twice as high.
// A GOGC set in the environment is kept.
func TuneGCForCLI() {
	if _, set := os.LookupEnv("GOGC"); !set {
		debug.SetGCPercent(cliGCPercent)
	}
}

// ExitCode is one row of the CLI's exit code table.
type ExitCode struct {
	Code    int
	Name    string
	Meaning string
}

// Exit codes.
const (
	exitOK          = 0
	exitUsage       = 2
	exitUnknown     = 3
	exitRejected    = 4
	exitRefused     = 5
	exitInfra       = 6
	exitEnded       = 7
	exitCheckFailed = 8
	exitBusy        = 9
)

// ExitCodes is the CLI's exit code table, documented on Main.
var ExitCodes = []ExitCode{
	{exitOK, "ok", "done"},
	{exitUsage, "usage", "bad flags, unknown subcommand, unknown definition, missing required param, malformed --model-map, lint failure, or a host preflight refusal (stderr `refused: <check>: <cause>` then `fix: <remedy>`)"},
	{exitUnknown, "unknown-run", "no journal for the run id"},
	{exitRejected, "rejected", "stale or superseded token, an answer outside the Enum, or no such open gate or effect"},
	{exitRefused, "refused", "output malformed or refused: done spends the attempt (a fresh one is issued while retry remains, else the state takes its failure route); validate records nothing, and the child fixes its output and validates again. No file at the allocated output: the first such done keeps the attempt open, and the child writes the file and runs done again; a second on the same attempt spends it"},
	{exitInfra, "infrastructure", "journal or definition-hash error, or a call that became busy after it had already recorded something (it names what, and the command to run next)"},
	{exitEnded, "ended", "wf next found the run ended (its single ended instruction names the terminal, outcome and cause) or stopped (its single stopped instruction): the loop ends"},
	{exitCheckFailed, "check-failed", "check found structural lint, calibration, simulation or coverage problems"},
	{exitBusy, "busy", "another call held the run past this call's lock wait and this call changed nothing: run the same command again"},
}

// Subcommand is one row of the CLI's verb table: usage and dispatch both read
// it, so a new verb is one row.
type Subcommand struct {
	Name    string
	Flags   []string
	Summary string
	// Args are the positional names after the run id (or subject), in order; a
	// trailing "?" marks an optional one.
	Args []string

	// handle runs the verb with the arguments after its name.
	handle func(c *cli, row Subcommand, args []string) int
	// subject names what a run-driving verb takes first, for its missing-argument
	// message.
	subject string
	// define registers the verb's own flags; --state-dir is common to all.
	define func(fs *flag.FlagSet, f *flags)
	// act runs a run-driving verb on an open run, after the common parsing. A
	// run-driving row without act is `run`, which starts a run of the definition
	// its subject names.
	act func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int
}

// inspect runs an inspection verb over the registered definitions.
func inspect(c *cli, row Subcommand, args []string) int {
	defs := make([]*Def, len(c.cfg.Defs))
	for i, r := range c.cfg.Defs {
		defs[i] = r.Def
	}
	return run(append([]string{row.Name}, args...), defs, c.stdout, c.stderr)
}

// version prints this binary's build stamp, the one a run it creates records.
func version(c *cli, row Subcommand, args []string) int {
	fs := flag.NewFlagSet(row.Name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	asJSON := fs.Bool("json", false, "print the BuildStamp as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return c.failf(exitUsage, "version: usage: version [--json]")
	}
	b := currentBuild()
	if *asJSON {
		return c.printJSON(b)
	}
	for _, line := range b.Lines() {
		fmt.Fprintln(c.stdout, line)
	}
	return exitOK
}

// Subcommands is the verb table. Only CLI and usage read it: the handlers it
// names must not, or the package fails to initialize.
var Subcommands = []Subcommand{
	turnAttachRow(), turnQueryRow(), turnHandoffRow(), turnNativeRow(), turnInstallRow(), setupCodexRulesRow(),
	{Name: "lint", Summary: "lint the definitions (optional definition name)", handle: inspect},
	{Name: "graph", Summary: "render the definitions (the default)", handle: inspect},
	{Name: "mermaid", Summary: "render the definitions as Mermaid", handle: inspect},
	{Name: "hash", Summary: "print the definition hashes", handle: inspect},
	{Name: "version", Flags: []string{"--json"}, Summary: "print this binary's build stamp: engine and workflow source, Go toolchain, journal format (--json: the BuildStamp)", handle: version},
	{
		Name:    "run",
		Flags:   []string{"--param k=v", "--options file", "--workspace dir", "--state-dir dir", "--assets dir", "--run-id id", "--snapshots", "--paged-prompts", "--halt-before STATE[,STATE]", "--model-map key=model[,key=model]", "--budget turns=N,tokens=N,wall=D", "--delegate gate=value", "--harness h", "--continue run", "--family coord=harness", "--family-home harness=dir", "--child-env-drop NAME[*]", "--headless-cmd harness=JSON-argv"},
		Summary: "start or resume a run of <def>; prints the RunID on stdout and the coordinator bootstrap on stderr",
		handle:  (*cli).drive,
		subject: "definition name",
		define: func(fs *flag.FlagSet, f *flags) {
			fs.Func("harness", "coordinator harness (claude, codex, or an explicit label)", func(v string) error {
				f.harness = normalizeHarness(v)
				if f.harness == "" {
					return fmt.Errorf("harness must not be empty")
				}
				return refuseScripted(f.harness)
			})
			fs.Func("halt-before", "pause the run before executing any listed state (comma-separated names or qualified paths)", func(v string) error {
				f.haltBefore = append(f.haltBefore, strings.Split(v, ",")...)
				return nil
			})
			fs.BoolVar(&f.pagedPrompts, "paged-prompts", false, "deliver prompts in bounded pages and gate completion on fetch coverage")
			fs.BoolVar(&f.snapshots, "snapshots", false, "record portable world checkpoints for rewind and fanout")
			fs.Var(&f.params, "param", "run parameter k=v (repeatable)")
			fs.StringVar(&f.options, "options", "", "JSON file holding the definition's options")
			fs.StringVar(&f.workspace, "workspace", "", "workspace directory (default: the current directory)")
			fs.StringVar(&f.runID, "run-id", "", "run id (resumes when its journal exists)")
			fs.StringVar(&f.modelMap, "model-map", "", "tier, all or model = model[,...]: the models the run spawns with")
			fs.StringVar(&f.budget, "budget", "", "turns=N,tokens=N,wall=D: the run budget, each named limit over the definition's default (0: no limit); journaled at creation")
			fs.Var(&f.delegates, "delegate", "delegate a gate's answer to the coordinator, gate=value (repeatable)")
			fs.StringVar(&f.cont, "continue", "", "the ended run this call continues as its successor (status's resume command names it): params derive from its recorded assets, and a run key outside its chain is refused")
			defineHeadlessFlags(fs, f)
		},
	},
	{
		Name:    "next",
		Flags:   []string{"--expected-admission seq", "--until-unit name", "--state-dir dir", "--assets dir", "--workspace dir", "--timeout d", "--children k", "--adopt", "--json"},
		Summary: "block until <run> has something for the coordinator, hand out up to n ready units (left out: up to your child cap, --children or the harness's own limit where its adapter reads one, less your children running or starting, else every ready unit) and print one line per item; --json: hand out at once, without blocking, as JSON, for a program",
		handle:  (*cli).drive,
		subject: "run id",
		Args:    []string{"n?"},
		define: func(fs *flag.FlagSet, f *flags) {
			// --wait is the earlier protocol's spelling of what next always
			// does now (H1): accepted and ignored, so a remembered loop runs.
			fs.Int64Var(&f.expectedAdmission, "expected-admission", 0, "require this coordinator attachment before driving")
			fs.StringVar(&f.untilUnit, "until-unit", "", "stop this isolated run after the named unit settles")
			fs.BoolVar(&f.wait, "wait", false, "(no effect: next always blocks)")
			fs.DurationVar(&f.timeout, "timeout", 0, "give up after this long and print the nothing-yet line (default: the harness's own tool limit less a margin, else 100s)")
			fs.BoolVar(&f.adopt, "adopt", false, "only adopt a changed definition whose live states are unchanged, as resume --adopt: hand out nothing and bind no harness (the coordinator's next drives on)")
			fs.IntVar(&f.children, "children", 0, "how many children you run at once (0: the harness's own limit where its adapter reads one, else no limit): next never offers more than k less your children running or starting, waits while k run, and counts no offer as unstarted while it waits")
			fs.BoolVar(&f.json, "json", false, "hand out at once, without blocking, and print the instructions as JSON (for a program, never the loop)")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			n, code := c.nextArgs(row.Name, f)
			if code != exitOK {
				return code
			}
			co := envCoordinator(c.env)
			co.Children = f.children
			co.ExpectedAdmission = f.expectedAdmission
			if f.expectedAdmission < 0 || f.expectedAdmission != 0 && (f.adopt || f.untilUnit != "") {
				return c.failf(exitUsage, "next: invalid flags for an attachment-fenced continuation")
			}
			c.adopt = f.adopt
			if f.adopt && f.untilUnit != "" {
				return c.failf(exitUsage, "%s: --adopt only adopts and hands out nothing; run next --adopt, then next --until-unit", row.Name)
			}
			// The run stayed busy for the whole of a blocking next's budget
			// in its preflight: the loop's nothing-yet answer, before the
			// harness cuts the call off.
			lockedOut := func() int {
				fmt.Fprint(c.stdout, RenderNext(NextResult{Timeout: true, Waited: time.Since(c.budgetFrom)}, "", nil))
				return exitOK
			}
			if c.lockedOut && !f.json {
				return lockedOut()
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				e := x.(*Engine)
				if f.adopt {
					// Adopt only: no coordinator is met, no harness
					// bound, no unit handed out.
					adopted, line, err := e.AdoptOnly(run)
					if err != nil {
						return c.fromError(row.Name, err)
					}
					if f.json {
						return c.printJSON([]Instruction{})
					}
					text := NextAdoptCurrent
					if adopted {
						text = NextAdopted
					}
					fmt.Fprintf(c.stdout, text+"\n", line)
					return exitOK
				}
				if f.untilUnit != "" {
					if err := e.bindUntilUnitNameChecked(run, f.untilUnit, false, func(recs []JournalRecord) error {
						return turnFence(recs, co)
					}); err != nil {
						if !f.json && errors.Is(err, ErrRunLocked) {
							return lockedOut()
						}
						return c.fromError(row.Name, err)
					}
				}
				if !f.json {
					timeout := f.timeout
					if timeout == 0 && co.Wait > 0 {
						timeout = capWait(co.Wait) // the harness's own tool limit less a margin (H2)
					}
					if !c.lockBy.IsZero() {
						// What is left of the call's budget after its
						// preflight and bind, either of which may have
						// waited for the lock.
						if left := max(time.Until(c.lockBy), time.Millisecond); timeout == 0 || left < timeout {
							timeout = left
						}
					}
					res, err := e.NextWait(run, n, co, timeout)
					if err != nil {
						return c.fromError(row.Name, err)
					}
					fmt.Fprint(c.stdout, RenderNext(res, res.Harness, e.resultLines(run, res)))
					for _, in := range res.Instructions {
						if in.Kind == InstructionEnded || in.Kind == InstructionStopped {
							c.endedNote(e, stateDir, run)
							return exitEnded
						}
					}
					return exitOK
				}
				co.OutBytes, co.OutLines = 0, 0 // the JSON hand-out is a program's: no tool output limit
				ins, err := e.HandOutAs(run, n, co)
				if err != nil {
					return c.fromError(row.Name, err)
				}
				if ins == nil {
					ins = []Instruction{}
				}
				if code := c.printJSON(ins); code != exitOK {
					return code
				}
				if len(ins) == 1 && (ins[0].Kind == InstructionEnded || ins[0].Kind == InstructionStopped) {
					return exitEnded
				}
				return exitOK
			})
		},
	},
	{
		Name:    "stop",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--reason r"},
		Summary: "stop <run>: next offers nothing from now on and running children finish; a person's resume continues it",
		handle:  (*cli).drive,
		subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) {
			fs.StringVar(&f.reason, "reason", "", "why the run is stopped")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			return c.withEngine(row, stateDir, func(x Executor) int {
				if err := x.(*Engine).Stop(run, f.reason); err != nil {
					return c.fromError(row.Name, err)
				}
				return exitOK
			})
		},
	},
	{
		Name:    "resume",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--operator name", "--reason r", "--adopt", "--model-map key=model[,key=model]", "--budget turns=N,tokens=N,wall=D"},
		Summary: "clear a stop of <run> as its operator (--operator, --reason: journaled; the run's own coordinator session is refused); --adopt carries it onto a changed definition whose live states are unchanged; --model-map sets the models from this resume on; --budget raises the run budget (a budget stop needs it)",
		handle:  (*cli).drive,
		subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) {
			fs.BoolVar(&f.adopt, "adopt", false, "adopt a changed definition with no declared Migration when every live state still exists with the same kind and result type (journaled as migrated)")
			fs.StringVar(&f.modelMap, "model-map", "", "tier, all or model = model[,...]: the models the run spawns from this resume on")
			fs.StringVar(&f.budget, "budget", "", "turns=N,tokens=N,wall=D: the run budget from this resume on, each named limit replaced (0: no limit); journaled")
			fs.StringVar(&f.operator, "operator", "", "who clears the stop: your name as the run's operator (journaled)")
			fs.StringVar(&f.reason, "reason", "", "why the stop is cleared (journaled)")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			c.adopt = f.adopt
			if code := c.takeBoundaryModelMap(row.Name, f); code != exitOK {
				return code
			}
			budget, err := ParseBudget(f.budget)
			if err != nil {
				return c.failf(exitUsage, "%s: %v", row.Name, err)
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				if f.budget != "" {
					x.(*Engine).opts.ResumeBudget = &budget
				}
				op := Operator{Name: f.operator, Reason: f.reason, Harness: envHarness(c.env), Session: envCoordinator(c.env).Session}
				if err := x.(*Engine).ResumeAs(run, op); err != nil {
					return c.fromError(row.Name, err)
				}
				return exitOK
			})
		},
	},

	{
		Name:    "prompt",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--claim token", "--page N", "--code code", "--json"},
		Summary: "claim a handed unit by --claim token and print its prompt (--json: full prompt and inputs)",
		handle:  (*cli).drive,
		subject: "run id",
		Args:    []string{"name?"},
		define:  promptFlagDefinitions,
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			name := ""
			if len(f.args) > 0 {
				name = f.args[0]
			}
			if name == "" && f.claim == "" {
				if c.operatorChild {
					return c.failf(exitUsage, "dev prompt: pass a unit name")
				}
				return c.failf(exitUsage, "%s: %s", row.Name, childClaimFix)
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				x.(*Engine).opts.operatorChild = c.operatorChild
				if err := x.(*Engine).prepareChild(run, "prompt", f); err != nil {
					return c.fromError(row.Name, err)
				}
				name = f.args[0]
				// Preflight is dry; caller mismatch on a mutating verb records
				// degradation without spending the attempt.
				if err := x.(*Engine).ChildVerbCheck(run, "prompt", name, f.claim, harness.For(envHarness(c.env)).Caller(c.env)); err != nil {
					return c.fromError(row.Name, x.(*Engine).childClaimError(run, "prompt", err))
				}
				on, err := x.(*Engine).pagedPrompts(run)
				if err != nil {
					return c.fromError(row.Name, err)
				}
				if on {
					return c.promptPage(x.(*Engine), row.Name, run, name, f)
				}
				if f.page != 1 || f.pageCode != "" {
					return c.failf(exitUsage, "page options require a run started with --paged-prompts")
				}
				fetch, err := promptFromEnv(x.(*Engine), run, name, f.claim, c.env, f.json)
				if err != nil {
					return c.fromError(row.Name, err)
				}
				if f.json {
					return c.printJSON(fetch)
				}
				fmt.Fprintln(c.stdout, fetch.Delivered)
				return exitOK
			})
		},
	},
	{Name: "read", Flags: []string{"--state-dir dir", "--assets dir", "--workspace dir", "--claim token", "--ref id", "--page N", "--code code"}, Summary: "fetch one bounded prompt reference page", handle: (*cli).drive, subject: "run id", Args: []string{"name?"},
		define: func(fs *flag.FlagSet, f *flags) {
			fs.StringVar(&f.claim, "claim", "", "the claim token of the owning unit")
			fs.IntVar(&f.page, "page", 1, "one-based reference page")
			fs.StringVar(&f.pageCode, "code", "", "continuation code from the preceding page")
			fs.StringVar(&f.reference, "ref", "", "reference ID listed in the prompt packet")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			name := ""
			if len(f.args) > 0 {
				name = f.args[0]
			}
			if f.reference == "" {
				f.reference = name
				name = ""
				if f.reference == "" {
					return c.failf(exitUsage, "read: pass the reference ID from the prompt packet")
				}
			}
			return c.withEngine(row, stateDir, func(x Executor) int { return c.promptPage(x.(*Engine), row.Name, run, name, f) })
		}},

	{
		Name:    "done",
		Flags:   []string{"--state-dir dir", "--claim token", "--assets dir", "--workspace dir", "--output allocated-file", "--usage tokens=N", "--escalated 'command: reason: outcome'"},
		Summary: "read a claimed unit's allocated result",
		handle:  (*cli).drive,
		subject: "run id",
		Args:    []string{"name?"},
		define: func(fs *flag.FlagSet, f *flags) {
			fs.StringVar(&f.claim, "claim", "", "the claim token in your task text")
			fs.StringVar(&f.output, "output", "", "the allocated output JSON file (optional)")
			fs.StringVar(&f.usage, "usage", "", "tokens=N spent by the attempt")
			fs.Var(&f.escalated, "escalated", escalatedHelp)
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			ds, code := c.escalations(row.Name, f)
			if code != exitOK {
				return code
			}
			if d, ok := c.unparsedUsage(f); ok {
				ds = append(ds, d)
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				x.(*Engine).opts.operatorChild = c.operatorChild
				if err := x.(*Engine).prepareChild(run, "done", f); err != nil {
					return c.fromError(row.Name, err)
				}
				// Preflight is dry; caller mismatch on a mutating verb records
				// degradation without spending the attempt.
				if err := x.(*Engine).ChildVerbCheck(run, "done", f.args[0], f.claim, harness.Caller{}); err != nil {
					return c.fromError(row.Name, x.(*Engine).childClaimError(run, "done", err))
				}
				output, err := x.(*Engine).allocatedOutput(run, f.args[0])
				if err != nil {
					return c.fromError(row.Name, err)
				}
				if output == "" {
					return c.failf(exitUsage, "done: no allocated output for this run")
				}
				if _, err := os.Stat(output); errors.Is(err, os.ErrNotExist) {
					// Nothing at the allocated path, whatever --output named: the
					// output was never written there. The first such done of an
					// attempt is refused with the attempt open: the child writes
					// it and runs done again. A second spends the attempt. Its
					// escalations share its lock.
					res, err := x.(*Engine).refuseMissingOutput(run, f.args[0], output, ds)
					return c.result(row.Name, res, err)
				}
				if f.output != "" {
					abs, err := filepath.Abs(f.output)
					if err != nil || f.output == "-" || abs != output {
						return c.failf(exitUsage, "done: --output must name allocated output %s", output)
					}
				}
				f.output = output
				s, code := c.submission(row.Name, f)
				if code != exitOK {
					return code
				}
				if err := x.(*Engine).withDryRun(run, func(c *cycle) error { return c.unitResultPaths(f.args[0], s.Output) }); err != nil {
					return c.fromError(row.Name, err)
				}
				if h := betweenLocksHook; h != nil {
					h(row.Name, run)
				}
				// The escalations and the result share one lock: a busy run
				// records neither.
				res, err := x.(*Engine).DoneEscalated(run, f.args[0], s, ds)
				return c.result(row.Name, res, err)
			})
		},
	},
	{
		Name:    "validate",
		Flags:   []string{"--state-dir dir", "--claim token", "--assets dir", "--workspace dir", "--output allocated-file"},
		Summary: "check a claimed unit's allocated result as done would, recording nothing",
		handle:  (*cli).drive,
		subject: "run id",
		Args:    []string{"name?"},
		define: func(fs *flag.FlagSet, f *flags) {
			fs.StringVar(&f.claim, "claim", "", "the claim token in your task text")
			fs.StringVar(&f.output, "output", "", "the allocated output JSON file (optional)")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			return c.withEngine(row, stateDir, func(x Executor) int {
				x.(*Engine).opts.operatorChild = c.operatorChild
				if err := x.(*Engine).prepareChild(run, "validate", f); err != nil {
					return c.fromError(row.Name, err)
				}
				if err := x.(*Engine).ChildVerbCheck(run, "validate", f.args[0], f.claim, harness.Caller{}); err != nil {
					return c.fromError(row.Name, err)
				}
				// Every engine call on this path is dry: nothing, not even a
				// migration, is written.
				output, err := x.(*Engine).allocatedOutputFor(row.Name, run, f.args[0])
				if err != nil {
					return c.fromError(row.Name, err)
				}
				if output == "" {
					return c.failf(exitUsage, "validate: no allocated output for this run")
				}
				var res SubmitResult
				if _, err := os.Stat(output); errors.Is(err, os.ErrNotExist) {
					res = SubmitResult{Class: Refused, Refusal: []string{"no output file at " + output}}
				} else {
					if f.output != "" {
						abs, err := filepath.Abs(f.output)
						if err != nil || f.output == "-" || abs != output {
							return c.failf(exitUsage, "validate: --output must name allocated output %s", output)
						}
					}
					f.output = output
					s, code := c.submission(row.Name, f)
					if code != exitOK {
						return code
					}
					if res, err = x.Validate(run, f.args[0], s); err != nil {
						return c.fromError(row.Name, err)
					}
				}
				return c.validated(res)
			})
		},
	},
	{
		Name:    "failed",
		Flags:   []string{"--state-dir dir", "--claim token", "--assets dir", "--workspace dir", "--reason r", "--class transient|blocked|approval", "--escalated 'command: reason: outcome'"},
		Summary: "abandon a claimed unit's attempt",
		handle:  (*cli).drive,
		subject: "run id",
		Args:    []string{"name?"},
		define: func(fs *flag.FlagSet, f *flags) {
			fs.StringVar(&f.claim, "claim", "", "the claim token in your task text")
			fs.StringVar(&f.reason, "reason", "", "why the attempt is abandoned")
			fs.StringVar(&f.class, "class", "", "transient (default: spend a retry), blocked (take the Blocked route), or approval (hold without spending a retry; give the exact blocked action in --reason)")
			fs.Var(&f.escalated, "escalated", escalatedHelp)
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			class, err := ParseFailureClass(f.class)
			if err != nil {
				return c.failf(exitUsage, "%s: %v", row.Name, err)
			}
			ds, code := c.escalations(row.Name, f)
			if code != exitOK {
				return code
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				x.(*Engine).opts.operatorChild = c.operatorChild
				if err := x.(*Engine).prepareChild(run, "failed", f); err != nil {
					return c.fromError(row.Name, err)
				}
				// Preflight is dry; caller mismatch on a mutating verb records
				// degradation without spending the attempt.
				if err := x.(*Engine).ChildVerbCheck(run, "failed", f.args[0], f.claim, harness.Caller{}); err != nil {
					return c.fromError(row.Name, x.(*Engine).childClaimError(run, "failed", err))
				}
				res, err := x.(*Engine).FailedEscalated(run, f.args[0], f.reason, class, ds)
				return c.result(row.Name, res, err)
			})
		},
	},
	{
		Name:    "answer",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--gate state", "--value choice", "--number INT", "--child id", "--visit n"},
		Summary: "answer a human gate (optional --number overrides its integer proposal)",
		Args:    []string{"gate?", "answer?"},
		handle:  (*cli).drive,
		subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) {
			fs.StringVar(&f.gate, "gate", "", "the gate's qualified state")
			fs.Func("value", "the enum answer", func(v string) error {
				if v == "" {
					return fmt.Errorf("--value must not be empty")
				}
				f.value = v
				return nil
			})
			fs.Func("number", "integer value (omitted: use the declared proposal)", func(v string) error {
				n, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("--number %q is not an integer: %w", v, err)
				}
				f.number = &n
				return nil
			})
			fs.StringVar(&f.child, "child", "", "bind the answer to this child instance (a Map child's gate)")
			fs.IntVar(&f.visit, "visit", 0, "bind the answer to this arrival at the gate (the instruction's visit)")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			if len(f.args) > 0 {
				if f.gate != "" && f.gate != f.args[0] {
					return c.failf(exitUsage, "answer: positional gate conflicts with --gate")
				}
				f.gate = f.args[0]
			}
			if len(f.args) > 1 {
				if f.value != "" && f.value != f.args[1] {
					return c.failf(exitUsage, "answer: positional answer conflicts with --value")
				}
				f.value = f.args[1]
			}
			if f.gate == "" {
				return c.failf(exitUsage, "answer: --gate or positional gate is required")
			}
			if f.value == "" {
				return c.failf(exitUsage, "answer: --value or positional answer is required")
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				var err error
				if f.child != "" || f.visit != 0 {
					err = x.AnswerVisit(run, GateAnswer{Gate: f.gate, Child: f.child, Visit: f.visit, Value: f.value, IntValue: f.number})
				} else if f.number != nil {
					err = x.(*Engine).AnswerInt(run, f.gate, f.value, f.number)
				} else {
					err = x.Answer(run, f.gate, f.value)
				}
				if err != nil {
					return c.fromError(row.Name, err)
				}
				return exitOK
			})
		},
	},
	{
		Name:    "settle",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--effect id", "--applied [--exit N [--stdout-file f]] | --not-applied", "--evidence text", "--operator name", "--reason r"},
		Summary: "settle an Uncertain effect",
		handle:  (*cli).drive,
		subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) {
			fs.Int64Var(&f.effect, "effect", 0, "the effect id")
			fs.BoolVar(&f.applied, "applied", false, "the effect was applied")
			fs.BoolVar(&f.notApplied, "not-applied", false, "the effect was not applied")
			fs.IntVar(&f.exit, "exit", -1, "a command settled as applied: the exit code observed")
			fs.StringVar(&f.stdoutFile, "stdout-file", "", "a command settled as applied: file holding the stdout observed (with --exit)")
			fs.StringVar(&f.evidence, "evidence", "", "what you checked in the world and what it showed, kept with the settlement")
			fs.StringVar(&f.operator, "operator", "", "an effect outside the run: who settles it, as the run's operator (journaled)")
			fs.StringVar(&f.reason, "reason", "", "an effect outside the run: why the operator settles it (journaled)")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			if f.applied == f.notApplied {
				return c.failf(exitUsage, "settle: pass exactly one of --applied and --not-applied")
			}
			if strings.TrimSpace(f.evidence) == "" {
				return c.failf(exitUsage, "settle: --evidence <what you checked and what it showed> is required")
			}
			if f.exit < -1 {
				return c.failf(exitUsage, "settle: --exit must be 0 or greater")
			}
			if (f.exit >= 0 || f.stdoutFile != "") && !f.applied {
				return c.failf(exitUsage, "settle: --exit and --stdout-file go with --applied")
			}
			if f.stdoutFile != "" && f.exit < 0 {
				return c.failf(exitUsage, "settle: --stdout-file needs --exit")
			}
			var rr *RunResult
			if f.exit >= 0 {
				rr = &RunResult{Exit: f.exit}
				if f.stdoutFile != "" {
					b, err := os.ReadFile(f.stdoutFile)
					if err != nil {
						return c.failf(exitUsage, "settle: %v", err)
					}
					rr.Stdout = string(b)
				}
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				by := Operator{Name: f.operator, Reason: f.reason, Harness: envHarness(c.env), Session: envCoordinator(c.env).Session}
				if err := x.(*Engine).SettleAs(run, f.effect, f.applied, rr, f.evidence, by); err != nil {
					return c.fromError(row.Name, err)
				}
				return exitOK
			})
		},
	},
	{
		Name:    "retry",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--child id", "--stop", "--approval-hold seq"},
		Summary: "return a halted instance of <run>, or release its held unit, to ready at the next attempt (--stop: answer a retry-exhausted halt by taking its fallback edge)",
		handle:  (*cli).drive,
		subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) {
			fs.StringVar(&f.child, "child", "", "the halted child instance, or the one holding the unit (default: the run itself)")
			fs.BoolVar(&f.stop, "stop", false, "answer a retry-exhausted halt with stop: take the state's exhausted (or stalled) edge instead of one more attempt")
			fs.Func("approval-hold", "release only the approval hold named by this sequence number", func(v string) error {
				seq, err := strconv.ParseInt(v, 10, 64)
				if err != nil || seq <= 0 {
					return fmt.Errorf("expected a positive approval hold sequence")
				}
				f.approvalHold = seq
				return nil
			})
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			if f.stop && f.approvalHold != 0 {
				return c.failf(exitRejected, "retry: --stop does not apply to an approval hold")
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				var err error
				if f.approvalHold != 0 {
					err = x.ReleaseApproval(run, f.child, f.approvalHold)
				} else {
					err = x.Retry(run, f.child, f.stop)
				}
				if err != nil {
					return c.fromError(row.Name, err)
				}
				return exitOK
			})
		},
	},
	{
		Name:    "recover",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir"},
		Summary: "answer a unit's recovery offer: resume (the same child continues, where the harness can), restart or give-up",
		handle:  (*cli).drive,
		subject: "run id",
		Args:    []string{"name", "choice"},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			return c.withEngine(row, stateDir, func(x Executor) int {
				if err := x.Recover(run, f.args[0], f.args[1]); err != nil {
					return c.fromError(row.Name, err)
				}
				return exitOK
			})
		},
	},
	{
		Name:    "status",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--json", "--full"},
		Summary: "print one line per subagent unit and a totals line (--json: the RunStatus as JSON); from an agent's shell on a running run, one line that says to run next (--full: the whole view)",
		handle:  (*cli).drive,
		subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) {
			fs.BoolVar(&f.json, "json", false, "print the RunStatus as JSON")
			fs.BoolVar(&f.full, "full", false, "print the whole view, even from an agent's shell (for people)")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			return c.withEngine(row, stateDir, func(x Executor) int {
				st, err := x.Status(run)
				if err != nil {
					return c.fromError(row.Name, err)
				}
				if f.json {
					return c.printJSON(st)
				}
				// H3: a coordinator that polls status while its run is running
				// gets one line that sends it back to next; people get the whole
				// view with --full or from their own terminal.
				if !f.full && len(harness.Markers(c.env)) > 0 && st.Phase == RunRunning && !st.Stopped && (st.CoordinatorTurn == nil || st.CoordinatorTurn.StallSeq == 0) {
					fmt.Fprintln(c.stdout, StatusForCoordinator(st))
					return exitOK
				}
				fmt.Fprint(c.stdout, RenderStatus(st))
				return exitOK
			})
		},
	},
	{
		Name:    "heartbeat",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--session s", "--note text"},
		Summary: "answer a stall: active <name> [--note <work you saw>], died <name>, or died --session <id> for a gone coordinator's orphans",
		handle:  (*cli).drive,
		subject: "run id",
		Args:    []string{"status", "name?"},
		define: func(fs *flag.FlagSet, f *flags) {
			fs.StringVar(&f.session, "session", "", "with died: every unit claimed by this session is lost")
			fs.StringVar(&f.note, "note", "", "with active: what you saw the child do since the last question (new output, a running command); journaled")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			status, name := f.args[0], ""
			if len(f.args) > 1 {
				name = f.args[1]
			}
			var t HeartbeatTarget
			switch {
			case status == "active" && name != "" && f.session == "":
				t = HeartbeatTarget{Name: name, Note: f.note}
			case status == "died" && name != "" && f.session == "":
				t = HeartbeatTarget{Name: name}
			case status == "died" && name == "" && f.session != "":
				t = HeartbeatTarget{Session: f.session}
			default:
				return c.failf(exitUsage, "%s: want active <name>, died <name> or died --session <id>", row.Name)
			}
			if f.note != "" && status != "active" {
				return c.failf(exitUsage, "%s: --note goes with active only", row.Name)
			}
			if c.operatorChild && status != "died" {
				return c.failf(exitUsage, "dev %s: an operator reports died only", row.Name)
			}
			co := envCoordinator(c.env)
			return c.withEngine(row, stateDir, func(x Executor) int {
				x.(*Engine).opts.operatorChild = c.operatorChild
				if err := x.(*Engine).HeartbeatAs(run, t, status == "active", co); err != nil {
					return c.fromError(row.Name, err)
				}
				return exitOK
			})
		},
	},
	{
		Name:    "spawn",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--claim token"},
		Summary: "start the headless child of a unit of another harness than the coordinator's, handed out with --claim (the command next prints for it); returns at once",
		handle:  (*cli).drive,
		subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) {
			fs.StringVar(&f.claim, "claim", "", "the claim token next printed in the spawn command")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			if f.claim == "" {
				return c.failf(exitUsage, "%s: --claim is required: run the spawn command next printed, as printed", row.Name)
			}
			co := envCoordinator(c.env)
			environ := os.Environ()
			if c.env != nil {
				if p := c.env(harness.ParentVar); p != "" {
					environ = append(environ, harness.ParentVar+"="+p)
				}
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				res, err := x.(*Engine).Spawn(run, f.claim, co, environ)
				if err != nil {
					return c.fromError(row.Name, err)
				}
				session := ""
				if res.Session != "" {
					session = " session " + res.Session
				}
				fmt.Fprintf(c.stdout, "started %s on %s: pid %d%s; its stderr goes to %s. Do not wait for it: keep running next.\n", res.Unit, res.Harness, res.Pid, session, res.ErrLog)
				return exitOK
			})
		},
	},
	{
		Name:    "explain",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir"},
		Summary: "explain a unit with its transition, route, checks and effects",
		handle:  (*cli).drive,
		subject: "run id",
		Args:    []string{"unit"},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			return c.withEngine(row, stateDir, func(x Executor) int { return c.explain(x, run, f.args[0]) })
		},
	},
	{
		Name:    "journal",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir"},
		Summary: "print the journal as JSON Lines",
		handle:  (*cli).drive,
		subject: "run id",
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			return c.journal(stateDir, run)
		},
	},
	{
		Name:    "lock",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--json"},
		Summary: "say whether a process holds the run's lock now, and who last held it (read without taking it)",
		handle:  (*cli).drive,
		subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) {
			fs.BoolVar(&f.json, "json", false, "print the LockState as JSON")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			return c.withEngine(row, stateDir, func(x Executor) int {
				e, ok := x.(*Engine)
				if !ok {
					return c.failf(exitUsage, "%s: this executor has no run lock", row.Name)
				}
				st, err := e.LockState(run)
				if err != nil {
					return c.fromError(row.Name, err)
				}
				if f.json {
					return c.printJSON(st)
				}
				fmt.Fprint(c.stdout, RenderLock(st))
				return exitOK
			})
		},
	},
}

const defaultStateDir = ".workflow"

// coordinatorStopReport is the coordinator's stop instruction. It ends only
// an exit that is a person's to clear: a call from another harness. Any
// other error ends with coordinatorGoOn, and arms nothing: the next call
// simply runs.
const coordinatorStopReport = "stop and report this to the person who started the run"

// coordinatorGoOn ends a coordinator's error that no person has to clear.
const coordinatorGoOn = "you may run next again"

// childFailedInstruction is what a child does about a refusal or an error it
// cannot fix: its failed verb, with what blocked it and what it needs, so the
// run routes the unit instead of holding it claimed.
const childFailedInstruction = "run your failed command with --reason saying what blocked you and what you need, so the run can route it."

// childHeldInstruction is childFailedInstruction for a child whose prompt
// or read failed after an earlier fetch bound its claim: it holds the unit,
// and may not have been shown its failed command yet.
const childHeldInstruction = "run failed with the same --claim and --reason saying what blocked you and what you need, so the run can route it."

// childExitInstruction is for a child with no failed to run: its failed was
// itself refused, its attempt is spent (the run has recorded why), or its
// fetch bound nothing, so it never took the unit.
const childExitInstruction = "tell your coordinator the exact error and end only your child task. The coordinator continues with next."

// childBlocked is what a child that cannot resolve verb's error does next.
// held: the claim the call carried is bound to a unit that is still claimed.
func childBlocked(verb string, spent, held bool) string {
	switch {
	case spent || verb == "failed" || !childUnitVerbs[verb]:
		return childExitInstruction
	case verb == "prompt" || verb == "read":
		if held {
			return childHeldInstruction
		}
		return childExitInstruction
	}
	return childFailedInstruction
}

// FixableRefusalTail ends a refusal the child can fix in the same attempt
// : its result was refused and the attempt is still open, so it
// fixes and runs the command again, and fails the unit with what it needs
// only if the refusal repeats.
const FixableRefusalTail = "This refusal is yours to fix: fix what it names and run the same command again. If the same refusal comes back twice, " + childFailedInstruction

// attemptStillOpen begins the line of a refusal that keeps the attempt open.
const attemptStillOpen = "your attempt is still open:"

// refusalDisposition is how the refusal this command printed ends,
// set by the code that decided it, never read off the printed text: fixable
// (the attempt is open, the child fixes and runs it again) or spent (done
// ended the attempt). spent wins. One is shared by every cli of a command.
type refusalDisposition struct {
	fixable, spent, busy, partial bool
	// held: a prompt or read failed on a claim an earlier fetch had bound,
	// so the child holds the unit and its failed verb is open to it.
	held bool
}

// busy reports a run whose lock stayed held to the end of this call's wait
// . A call that has recorded nothing changed nothing: it exits
// exitBusy and is run again. One that had already recorded something stopped
// partway: it says what it recorded and the command to run next, never "run
// it again", which could record it twice.
func (c *cli) busy(cmd string, err error) int {
	held := strings.TrimPrefix(err.Error(), ErrRunLocked.Error()+": ")
	if len(c.committed) == 0 {
		if c.disposition != nil {
			c.disposition.busy = true
		}
		return c.failf(exitBusy, "%s: the run is busy, %s; nothing was changed", cmd, held)
	}
	if c.disposition != nil {
		c.disposition.partial = true
	}
	// Each run the call wrote to, in order, with what it wrote there: a
	// rewind or fork writes the new run, not the one it was called on.
	var runs []RunID
	kinds := map[RunID][]string{}
	for _, r := range c.committed {
		if _, ok := kinds[r.Run]; !ok {
			runs = append(runs, r.Run)
		}
		if k := r.Kind.String(); !slices.Contains(kinds[r.Run], k) {
			kinds[r.Run] = append(kinds[r.Run], k)
		}
	}
	parts := make([]string, len(runs))
	for i, r := range runs {
		parts[i] = strings.Join(kinds[r], ", ") + " for run " + string(r)
	}
	fmt.Fprintf(c.stderr, "%s: the run became busy before this call finished, %s; this call had already recorded %s, so it did not finish\n", cmd, held, strings.Join(parts, "; and "))
	last := runs[len(runs)-1]
	if c.verbLine != nil {
		var harnessName string
		if c.env != nil {
			harnessName = envHarness(c.env)
		}
		line := workspaceCommand(harnessName, c.workspace, c.verbLine(last, "status"))
		fmt.Fprintf(c.stderr, "%s: see where run %s stands before anything else; run:\n  %s\n", cmd, last, line)
	} else {
		fmt.Fprintf(c.stderr, "%s: see where run %s stands with its status before anything else\n", cmd, last)
	}
	return exitInfra
}

// BusyRunTail ends a refusal for a run whose lock stayed held to the end of
// the call's wait, when the call changed nothing (exit 9): another call is
// still working on the run, so the same command is run again; never a stop.
const BusyRunTail = "The run was busy with another call and nothing was changed: run the same command again. Only if it is still busy three times in a row, tell your coordinator the exact text."

// PartialCallTail ends a busy report from a call that had already recorded
// something: it is not run again as it was; the command it names comes next.
const PartialCallTail = "This call stopped partway after recording what it names: run the command it gives next, not the same command. Only if that fails too, tell your coordinator the exact text and stop."

func (c *cli) refusalFixable() {
	if c.disposition != nil {
		c.disposition.fixable = true
	}
}

func (c *cli) refusalSpent() {
	if c.disposition != nil {
		c.disposition.spent = true
	}
}

// CLI is the testable entry point: it runs one command line and returns the
// exit code.
func CLI(cfg MainConfig, args []string, stdout, stderr io.Writer) int {
	return CLIWithEnv(cfg, args, stdout, stderr, callerEnv())
}

// CLIWithEnv runs the same command boundary with an explicit caller environment.
// This supports isolated protocol simulations without changing process globals.
// It does not substitute authorization decisions or harness records.
func CLIWithEnv(cfg MainConfig, args []string, stdout, stderr io.Writer, env harness.Env) (code int) {
	cmd := "graph"
	if len(args) > 0 {
		cmd = args[0]
	}
	if cmd == HeadlessSupervisorVerb { // the spawn verb re-executes this binary
		return RunHeadlessSupervisor(args[1:], stderr)
	}
	disposition := &refusalDisposition{}
	defer func() {
		if env == nil || len(harness.Markers(env)) == 0 || code == exitOK || code == exitEnded {
			return
		}
		if code == exitRefused && disposition.fixable && !disposition.spent {
			fmt.Fprintln(stderr, FixableRefusalTail)
			return
		}
		if code == exitBusy && disposition.busy {
			fmt.Fprintln(stderr, BusyRunTail)
			return
		}
		if code == exitInfra && disposition.partial {
			fmt.Fprintln(stderr, PartialCallTail)
			return
		}
		verb := cmd
		if cfg.named() && cmd == "wf" && len(args) > 1 {
			verb = args[1]
		}
		// Some harnesses cannot distinguish a child from its coordinator.
		// Use the child verb as a fallback only when the caller's role is unknown.
		if childCaller(env) || (childUnitVerbs[verb] && harness.For(envHarness(env)).Caller(env).Is != harness.IsSelf) {
			blocked := childBlocked(verb, disposition.spent, disposition.held)
			guidance := strings.ToUpper(blocked[:1]) + blocked[1:]
			switch {
			case code == exitUsage:
				guidance = "Read this command's --help and correct its arguments. If that fails, " + blocked
			case (code == exitRejected || code == exitRefused) && !disposition.spent:
				guidance = "Follow the refusal and the current workflow instruction. If you cannot resolve it, " + blocked
			}
			fmt.Fprintln(stderr, guidance)
			return
		}
		switch {
		case code == exitUsage:
			fmt.Fprintln(stderr, "Read this command's --help and correct its arguments, then run it again.")
		case code == exitRejected || code == exitRefused:
			fmt.Fprintln(stderr, "Follow the refusal and the current workflow instruction. Unless it asks for a person, "+coordinatorGoOn+".")
		default:
			fmt.Fprintln(stderr, "This command failed, and the error above says why: "+coordinatorGoOn+".")
		}
	}()
	if cmd == "--help" || cmd == "-h" || cmd == "help" {
		return (&cli{cfg: cfg, stdout: stdout, stderr: stderr}).topHelp()
	}
	if f, ok := cfg.Extra[cmd]; ok {
		return f(args[1:], stdout, stderr)
	}
	if cfg.named() {
		return namedCLI(cfg, cmd, args[min(1, len(args)):], stdout, stderr, env, disposition)
	}
	if cmd == "started" {
		return (&cli{cfg: cfg, stdout: stdout, stderr: stderr, env: env, disposition: disposition}).refuseStarted()
	}
	if cmd == "dev" && len(args) > 1 {
		if row, ok := findDevRow(args[1]); ok {
			return row.handle(&cli{cfg: cfg, stdout: stdout, stderr: stderr, env: env, disposition: disposition}, row, args[2:])
		}
	}
	for _, row := range Subcommands {
		if row.Name == cmd {
			return row.handle(&cli{cfg: cfg, stdout: stdout, stderr: stderr, env: env, disposition: disposition}, row, args[min(1, len(args)):])
		}
	}
	if now, ok := removedVerbs[cmd]; ok {
		fmt.Fprintf(stderr, "%s: removed: %s\n", cmd, strings.ReplaceAll(now, "wf ", "workflow "))
		return exitUsage
	}
	fmt.Fprintf(stderr, "%s: unknown subcommand\n", cmd)
	usage(cfg, stderr)
	return exitUsage
}

func usage(cfg MainConfig, w io.Writer) {
	if cfg.named() {
		fmt.Fprintf(w, "usage: %s <subcommand> [args]\n", cfg.Binary)
		names := make([]string, len(cfg.Defs))
		for i, r := range cfg.Defs {
			names[i] = r.Def.name
		}
		fmt.Fprintf(w, "  run      [%s] start or resume a run (default %s); records its id in <state-dir>/run-id; run --help <def> lists its params\n",
			strings.Join(names, "|"), names[0])
		fmt.Fprintln(w, "  next     print the coordinator bootstrap of the run")
		fmt.Fprintln(w, "  wf       <verb> [args] drive the run (run id: --run-id, else <state-dir>/run-id, else the only run)")
		for _, s := range Subcommands {
			if s.act == nil && s.Name != "run" {
				fmt.Fprintf(w, "  %-8s %s\n", s.Name, s.Summary)
			}
		}
		fmt.Fprintln(w, "wf verbs:")
	} else {
		fmt.Fprintln(w, "usage: workflow <subcommand> [args]")
	}
	for _, s := range Subcommands {
		if cfg.named() && s.act == nil {
			continue
		}
		fmt.Fprintf(w, "  %-8s %s %s\n", s.Name, strings.Join(s.Flags, " "), s.Summary)
	}
	var extra []string
	for name := range cfg.Extra {
		if strings.HasPrefix(name, "__") {
			continue
		}
		extra = append(extra, name)
	}
	sort.Strings(extra)
	for _, n := range extra {
		fmt.Fprintf(w, "  %-8s (client subcommand)\n", n)
	}
}

type cli struct {
	operatorChild bool
	workspace     string
	// lockBy is when this invocation's run-lock waits, and next's own wait,
	// give up (budget); zero when no limit applies.
	lockBy time.Time
	// budgetFrom is when budget fixed lockBy; zero until then.
	budgetFrom time.Time
	// lockedOut: a blocking next's preflight found the run locked to the
	// end of its budget.
	lockedOut bool

	assets         string
	cfg            MainConfig
	stdout, stderr io.Writer
	// ownerResolved: wf <verb> already picked the run from the unit's owner,
	// so drive need not scan again.
	ownerResolved bool
	// env is the caller's environment before the state-dir sidecar (callerEnv).
	env harness.Env
	// adopt: this call carries the run onto a changed definition (--adopt).
	adopt bool
	// disposition is how a refusal this command prints ends.
	disposition *refusalDisposition
	// boundaryModelMap: the --model-map a resume, rewind or fork journals on
	// the segment boundary it opens.
	boundaryModelMap map[string]string
	// manufactured marks the runs this call starts (wf dev manufacture).
	manufactured *ManufacturedData
	// owner is what this call's run-lock record says it runs for.
	owner lockOwner
	// committed is the run and kind of every record this call has appended,
	// over all its journals: a busy lock after any of them is not "nothing
	// was changed".
	committed []committedRecord
	// run is the run this call acts on, once known; verbLine builds its
	// commands (the engine's VerbLine), once an engine is made.
	run      RunID
	verbLine func(run RunID, verb string, args ...string) string
}

func (c *cli) failf(code int, format string, a ...any) int {
	fmt.Fprintf(c.stderr, format+"\n", a...)
	return code
}

// escalatedHelp is the --escalated usage text of done, failed and started.
const escalatedHelp = "a sandbox escalation the attempt requested, as 'command: reason: outcome' (repeatable; the outcome is a short phrase without \": \", such as ran or refused by reviewer); journaled as a degraded record of kind escalated"

// escalations parses the --escalated values into escalated degraded records:
// the cause is "command: reason" and the action the outcome after the last
// ": ", so an outcome holds no ": ". The code is exitOK or the usage failure.
func (c *cli) escalations(cmd string, f *flags) ([]DegradedData, int) {
	var ds []DegradedData
	for _, v := range f.escalated {
		i := strings.LastIndex(v, ": ")
		if i < 0 {
			return nil, c.failf(exitUsage, "%s: --escalated %q is not 'command: reason: outcome'", cmd, v)
		}
		cause, outcome := strings.TrimSpace(v[:i]), strings.TrimSpace(v[i+2:])
		command, reason, ok := strings.Cut(cause, ": ")
		if !ok || strings.TrimSpace(command) == "" || strings.TrimSpace(reason) == "" || outcome == "" {
			return nil, c.failf(exitUsage, "%s: --escalated %q is not 'command: reason: outcome'", cmd, v)
		}
		ds = append(ds, DegradedData{Kind: DegradedEscalated, Cause: cause, Action: outcome})
	}
	return ds, exitOK
}

// unparsedUsage is done's --usage when it is not tokens=N: a usage number
// never blocks a hand-back, so the flag is dropped with a warning and the
// attempt counts zero, recorded as a usage unparsed note (rule 5 step 2).
func (c *cli) unparsedUsage(f *flags) (DegradedData, bool) {
	if f.usage == "" {
		return DegradedData{}, false
	}
	if _, ok := parseUsage(f.usage); ok {
		return DegradedData{}, false
	}
	fmt.Fprintf(c.stderr, "done: --usage %q is not tokens=N; ignored, the attempt counts 0 tokens\n", f.usage)
	d := DegradedData{Kind: DegradedUsageUnparsed, Cause: fmt.Sprintf("--usage %q is not tokens=N", f.usage), Action: "ignored; counted 0 tokens"}
	f.usage = ""
	return d, true
}

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// flags is what one run-driving subcommand accepts.
type flags struct {
	pagedPrompts                                                      bool
	pagedPromptsSet                                                   bool // --paged-prompts was given, true or false
	haltBefore                                                        []string
	page                                                              int
	pageCode, reference                                               string
	number                                                            *int
	fanoutStates, coordinatorCmd, untilUnit                           string
	parallel                                                          int
	dryRun                                                            bool
	snapshots                                                         bool
	forkOut                                                           string
	changed                                                           string
	restoreSeq                                                        int64
	approvalHold                                                      int64
	expectedAdmission, receipt                                        int64
	turnAttach                                                        turnAttachOptions
	assets                                                            string
	args                                                              []string // the row's positional arguments, in Subcommand.Args order
	stateDir, child, token, output, usage, reason, gate, value, class string
	harness, session, agent, transcript, claim, note, cont            string
	context                                                           ContextMode
	workspace, options, runID, modelMap, budget                       string
	headless                                                          HeadlessConfig  // run's --family, --family-home, --child-env-drop, --headless-cmd
	optionsJSON                                                       json.RawMessage // the options themselves, as a manufacture script gives them
	effect                                                            int64
	children                                                          int
	since                                                             int64
	timeout                                                           time.Duration
	exit                                                              int
	stdoutFile, to, observe, operator, evidence                       string
	sameKey                                                           bool
	applied, notApplied, json, stop, wait, adopt, full                bool
	params, delegates, escalated                                      multiFlag
	visit                                                             int
}

// flagSet is the common flags, then the row's own.
func (c *cli) flagSet(row Subcommand, f *flags) *flag.FlagSet {
	fs := flag.NewFlagSet(row.Name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.StringVar(&f.stateDir, "state-dir", c.cfg.stateDir(), "journal directory")
	if row.Name != "run" {
		fs.StringVar(&f.workspace, "workspace", c.workspace, "current run workspace root")
	}
	fs.StringVar(&f.assets, "assets", c.assets, "explicit definition asset root for this invocation")
	if row.define != nil {
		row.define(fs, f)
	}
	return fs
}

// drive is the handler of every run-driving row: the common parsing, then the
// row's act. It takes its row as a parameter and never looks one up, because
// Subcommands names it.
func (c *cli) drive(row Subcommand, args []string) int {
	cmd := row.Name
	if row.act != nil && wantsHelp(args) && (len(args) == 0 || strings.HasPrefix(args[0], "-")) {
		return c.verbHelp(row, row.Name+" <"+row.subject+">") // help needs no run
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return c.failf(exitUsage, "%s: missing %s", cmd, row.subject)
	}
	subject, rest := args[0], args[1:]
	var f flags
	for _, a := range row.Args {
		optional := strings.HasSuffix(a, "?")
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			f.args, rest = append(f.args, rest[0]), rest[1:]
		} else if optional {
			break
		} else {
			return c.failf(exitUsage, "%s: missing <%s>", cmd, a)
		}
	}
	if row.act == nil && wantsHelp(rest) && c.registration(subject) != nil {
		return c.runHelp(subject)
	}
	fs := c.flagSet(row, &f)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		return c.failf(exitUsage, "%s: unexpected argument %q", cmd, fs.Arg(0))
	}
	fs.Visit(func(fl *flag.Flag) { f.pagedPromptsSet = f.pagedPromptsSet || fl.Name == "paged-prompts" })
	if !childUnitVerbs[cmd] && !childReadVerbs[cmd] && childCaller(c.env) { // run too: a child never starts or drives a run
		return c.failf(exitRejected, "%s: %s", cmd, childRefusal())
	}
	c.owner = lockOwner{Verb: cmd}
	if len(row.Args) > 0 && len(f.args) > 0 && unitArgs[strings.TrimSuffix(row.Args[0], "?")] {
		c.owner.Unit = f.args[0]
	} else if c.env != nil {
		c.owner.Session = harness.For(envHarness(c.env)).Caller(c.env).Session
	}
	stateDir, err := filepath.Abs(f.stateDir)
	if err != nil {
		return c.failf(exitInfra, "%s: %v", cmd, err)
	}
	// Control verbs use the native caller's environment. A run's env file
	// configures its workflow tools and must not select a hook binding.
	if cmd != "attach-coordinator" && cmd != "may-end-turn" && cmd != "acknowledge-handoff" {
		if err := applyStateEnv(stateDir); err != nil {
			return c.failf(exitInfra, "%s: %v", cmd, err)
		}
	}
	c.assets = f.assets
	c.workspace = f.workspace
	if f.workspace != "" {
		// Named before any verb runs: a command run there would report only
		// its interpreter's failed exec.
		if info, err := os.Stat(f.workspace); err != nil || !info.IsDir() {
			return c.failf(exitUsage, "%s: --workspace %s: no such directory", cmd, f.workspace)
		}
	}
	if row.act == nil {
		return c.start(subject, stateDir, &f)
	}
	run := RunID(subject)
	c.run = run
	if validRunID(run) != nil || !journalExists(stateDir, run) {
		return c.failf(exitUnknown, "%s: unknown run %q in %s", cmd, subject, stateDir)
	}
	if cmd == "next" {
		// Its arguments first: a refusal never waits for the lock.
		if _, code := c.nextArgs(cmd, &f); code != exitOK {
			return code
		}
		// next's own wait and every lock wait end together. A harness with
		// no tool limit (Codex) bounds them by next's default wait, so the
		// lock wait's printed bound is one the call keeps.
		budget := f.timeout
		if budget == 0 && harness.For(envHarness(c.env)).ToolLimit(c.env) == 0 {
			budget = capWait(DefaultNextWait)
		}
		c.budget(budget)
	} else {
		c.budget(0)
	}
	// next --adopt only adopts (AdoptOnly): it drives nothing, so it is the
	// operator's from any harness.
	if drivingVerbs[cmd] && !c.operatorChild && !(cmd == "next" && f.adopt) {
		why, err := c.driverMismatch(stateDir, run)
		if cmd == "next" && !f.json && errors.Is(err, ErrRunLocked) {
			// Answered by next itself once its arguments are checked.
			c.lockedOut, err = true, nil
		}
		if err != nil {
			return c.fromError(cmd, err)
		}
		if why != "" {
			return c.failf(exitRejected, "%s: %s", cmd, why)
		}
	}
	if h := afterPreflight; h != nil {
		h(cmd)
	}
	if unitVerbs[cmd] && len(f.args) > 0 && !c.ownerResolved {
		if owners := c.unitOwners(stateDir, f.args[0]); len(owners) == 1 && owners[0] != run {
			return c.failf(exitUsage, "%s: unit %s is not in run %s: token belongs to run %s; use the command in your instruction", cmd, f.args[0], run, owners[0])
		}
	}
	code := row.act(c, row, stateDir, run, &f)
	if code != exitOK && (cmd == "prompt" || cmd == "read") && c.disposition != nil {
		c.disposition.held = c.claimHeld(stateDir, run, f.claim)
	}
	return code
}

// claimHeld reports whether claim is bound to a unit of run that is still
// claimed: a fetch of it took the unit. A read; false when it cannot tell.
func (c *cli) claimHeld(stateDir string, run RunID, claim string) bool {
	if claim == "" {
		return false
	}
	x, err := c.engine(stateDir, EngineOptions{})
	if err != nil {
		return false
	}
	e, ok := x.(*Engine)
	if !ok {
		return false
	}
	held := false
	_ = e.withDryRun(run, func(c *cycle) error {
		if in, u, ok := c.st.unitByName(c.unitByClaim(claim)); ok && !in.Settled {
			held = u.Bound == claim && u.State.claimed()
		}
		return nil
	})
	return held
}

// nextArgs is next's count from its arguments, or the usage refusal of
// arguments it does not take.
func (c *cli) nextArgs(name string, f *flags) (int, int) {
	n := -1
	switch {
	case len(f.args) == 0 && !f.json:
		n = NextAuto
	case len(f.args) == 0:
		return 0, c.failf(exitUsage, "%s: missing <n> (next --json needs a count); the loop is next, with no count", name)
	case f.args[0] != "all":
		var err error
		n, err = strconv.Atoi(f.args[0])
		if err != nil || n < 0 {
			return 0, c.failf(exitUsage, "%s: n %q is not a count of 0 or more, or all", name, f.args[0])
		}
	}
	if f.children < 0 {
		return 0, c.failf(exitUsage, "%s: --children must be 0 (not declared) or more", name)
	}
	if f.timeout < 0 {
		return 0, c.failf(exitUsage, "%s: --timeout must be 0 (the harness's limit) or more", name)
	}
	if f.json && (f.wait || f.timeout != 0) {
		return 0, c.failf(exitUsage, "%s: --json hands out at once and never blocks; drop --wait and --timeout, or drop --json for the loop", name)
	}
	return n, exitOK
}

// afterPreflight, when set by a test, runs after a run verb's preflight.
var afterPreflight func(cmd string)

// betweenLocksHook, when set, runs where a verb has released one run-lock
// acquisition that recorded something and has not yet taken the next: after
// run's journal is durable, and after done's --escalated records (tests).
var betweenLocksHook func(cmd string, run RunID)

// unitVerbs are the verbs that name a unit handed to a child: the brief's
// prompt, started, done and failed. The unit, not the default run, decides
// which run they act on.
var unitVerbs = map[string]bool{"prompt": true, "started": true, "validate": true, "done": true, "failed": true}

// unitOwners lists the live runs under stateDir that hold the live unit name.
// It is a read-only peek: no lock, no migration, committed lines only, so it
// never waits on or writes to another coordinator's run. A run that has ended,
// or that this binary cannot read (another definition, a corrupt journal),
// holds nothing. The verb itself still validates the unit under its run's lock.
func (c *cli) unitOwners(stateDir, name string) []RunID {
	defs := map[string]*Def{}
	for _, r := range c.cfg.Defs {
		defs[r.Def.name] = r.Def // runs are started from registrations only
	}
	entries, _ := os.ReadDir(filepath.Join(stateDir, "runs"))
	var owners []RunID
	for _, e := range entries {
		run := RunID(e.Name())
		if !e.IsDir() || validRunID(run) != nil {
			continue
		}
		recs := peekRecords(filepath.Join(stateDir, "runs", string(run), journalFile))
		if len(recs) == 0 || recs[0].Kind != JournalRunStarted {
			continue
		}
		var started RunStartedData
		if json.Unmarshal(recs[0].Data, &started) != nil || defs[started.Def] == nil {
			continue
		}
		st, err := foldRun(recs, defs[started.Def])
		if err != nil || st.ended {
			continue
		}
		if _, _, ok := st.unitByName(name); ok {
			owners = append(owners, run)
		}
	}
	return owners
}

// peekRecords reads the committed records of a journal file without its lock:
// a final line with no newline is still being written and is skipped, and
// nothing is truncated. Any parse failure reads as no records.
func peekRecords(path string) []JournalRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	recs, _, err := parseCommitted(data, 0, nil)
	if err != nil {
		return nil
	}
	return recs
}

// verbHelp prints one verb's usage, summary and flags to stdout.
func (c *cli) verbHelp(row Subcommand, usage string) int {
	for _, a := range row.Args {
		usage += " <" + strings.TrimSuffix(a, "?") + ">"
	}
	fmt.Fprintf(c.stdout, "usage: %s %s [flags]\n%s\n\nflags:\n", c.binaryName(), usage, row.Summary)
	fs := c.flagSet(row, &flags{})
	fs.SetOutput(c.stdout)
	fs.PrintDefaults()
	return exitOK
}

// withEngine builds the engine over the state directory and hands it to do.
func (c *cli) withEngine(row Subcommand, stateDir string, do func(Executor) int) int {
	x, err := c.engine(stateDir, EngineOptions{Adopt: c.adopt, BoundaryModelMap: c.boundaryModelMap})
	if err != nil {
		return c.failf(exitInfra, "%s: %v", row.Name, err)
	}
	return do(x)
}

func (c *cli) submit(x Executor, run RunID, f *flags) int {
	s, code := c.submission("submit", f)
	if code != exitOK {
		return code
	}
	s.Token = f.token
	res, err := x.Submit(run, s)
	return c.result("submit", res, err)
}

// submission reads --output and --usage into a Submission without a token; cmd
// names the verb in messages. The code is exitOK or the usage failure.
func (c *cli) submission(cmd string, f *flags) (Submission, int) {
	if f.output == "" {
		return Submission{}, c.failf(exitUsage, "%s: --output is required (a file, or - for stdin)", cmd)
	}
	var raw []byte
	var err error
	if f.output == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(f.output)
	}
	if err != nil {
		return Submission{}, c.failf(exitUsage, "%s: reading the output: %v", cmd, err)
	}
	tokens := 0
	if f.usage != "" {
		n, ok := parseUsage(f.usage)
		if !ok {
			return Submission{}, c.failf(exitUsage, "%s: --usage %q is not tokens=N", cmd, f.usage)
		}
		tokens = n
	}
	raw = bytes.TrimSpace(raw)
	wrapped := !json.Valid(raw)
	if wrapped {
		// The engine journals the submission, so malformed output is handed over
		// as a JSON string: unless the state's output type takes text, or the
		// file is one fenced block the engine unwraps (unfencedFor), it fails
		// the output type and spends the attempt as Refused, like any other bad
		// output.
		raw, _ = json.Marshal(string(raw))
	}
	return Submission{Output: raw, Tokens: tokens, textWrapped: wrapped}, exitOK
}

// unfenced returns the contents of a result file that is exactly one fenced
// code block around valid JSON: a first line of ``` or ```json, a last line
// of ```, and no other fence line between them. Whitespace around the
// fence lines is ignored. Nothing more lenient is tolerated: prose outside the fence,
// another info string or a second block leaves the file as written.
func unfenced(raw []byte) ([]byte, bool) {
	lines := bytes.Split(raw, []byte("\n"))
	if len(lines) < 3 {
		return nil, false
	}
	line := func(i int) string { return string(bytes.TrimSpace(lines[i])) }
	if open := line(0); open != "```" && open != "```json" {
		return nil, false
	}
	if line(len(lines)-1) != "```" {
		return nil, false
	}
	body := lines[1 : len(lines)-1]
	for _, l := range body {
		if bytes.HasPrefix(bytes.TrimLeft(l, " \t"), []byte("```")) {
			return nil, false
		}
	}
	inner := bytes.TrimSpace(bytes.Join(body, []byte("\n")))
	if !json.Valid(inner) {
		return nil, false
	}
	return inner, true
}

// result prints a SubmitResult and maps its class to an exit code.
func (c *cli) result(cmd string, res SubmitResult, err error) int {
	if err != nil {
		return c.fromError(cmd, err)
	}
	if code := c.printJSON(res); code != exitOK {
		return code
	}
	if f := res.Failure; f != nil {
		// An accepted failure: the journaled outcome says what happens next.
		fmt.Fprintf(c.stderr, "%s: %s\n", cmd, f.Message)
		return exitOK
	}
	switch res.Class {
	case Rejected:
		word := "rejected"
		if cmd == "failed" {
			word = "refused" // the verb itself was rejected
		}
		fmt.Fprintf(c.stderr, "%s: %s: %s\n", cmd, word, strings.Join(res.Refusal, "; "))
		return exitRejected
	case Refused:
		fmt.Fprintf(c.stderr, "%s: refused: %s\n", cmd, strings.Join(res.Refusal, "; "))
		fmt.Fprintf(c.stderr, "%s: this attempt is spent\n", cmd)
		c.refusalSpent()
		return exitRefused
	}
	return exitOK
}

// validated prints validate's verdict: done would accept the output (exit 0),
// or the refusals done would print (exit refused). Nothing was recorded, so
// the child fixes its output and validates again.
func (c *cli) validated(res SubmitResult) int {
	if code := c.printJSON(res); code != exitOK {
		return code
	}
	switch res.Class {
	case Accepted:
		fmt.Fprintln(c.stderr, "validate: ok: done would accept this output")
		return exitOK
	case Refused:
		for _, r := range res.Refusal {
			fmt.Fprintf(c.stderr, "validate: refused: %s\n", r)
		}
		fmt.Fprintln(c.stderr, "validate: nothing was recorded: fix the output and validate again, then run done")
		c.refusalFixable()
		return exitRefused
	}
	// Stale, Unavailable, Blocked or a rejected token: not the output's fault,
	// and done would end the attempt the same way.
	fmt.Fprintf(c.stderr, "validate: done would end this attempt as %s: %s\n", res.Class, strings.Join(res.Refusal, "; "))
	return exitOK
}

func (c *cli) fromError(cmd string, err error) int {
	var accepted *alreadyAccepted
	var failed *alreadyFailed
	switch {
	case errors.As(err, &accepted):
		fmt.Fprintln(c.stdout, accepted.Error())
		return exitOK
	case errors.As(err, &failed):
		fmt.Fprintln(c.stdout, failed.Error())
		return exitOK
	case errors.Is(err, errResultRunPaths), errors.Is(err, errResultWorkspacePath), errors.Is(err, errResultAmbiguous):
		fmt.Fprintf(c.stderr, "%s: refused: %s\n", cmd, strings.TrimPrefix(err.Error(), ErrRejected.Error()+": "))
		fmt.Fprintf(c.stderr, "%s: %s fix the result, then run %s again\n", cmd, attemptStillOpen, cmd)
		c.refusalFixable()
		return exitRefused
	case errors.Is(err, ErrUnknownRun):
		return c.failf(exitUnknown, "%s: %v", cmd, err)
	case errors.Is(err, errBadModelMap):
		return c.failf(exitUsage, "%s: %s", cmd, strings.TrimPrefix(err.Error(), cmd+": "))
	case errors.Is(err, ErrRejected):
		if cmd == "failed" {
			return c.failf(exitRejected, "%s: refused: %s", cmd, strings.TrimPrefix(err.Error(), ErrRejected.Error()+": "))
		}
		return c.failf(exitRejected, "%s: %v", cmd, err)
	case errors.Is(err, ErrRunLocked):
		return c.busy(cmd, err)
	case errors.Is(err, errOutputMissing):
		fmt.Fprintf(c.stderr, "%s: refused: %v\n", cmd, err)
		fmt.Fprintf(c.stderr, "%s: %s write your result JSON to that file, then run %s again\n", cmd, attemptStillOpen, cmd)
		c.refusalFixable()
		return exitRefused
	}
	return c.failf(exitInfra, "%s: %v", cmd, err)
}

func (c *cli) printJSON(v any) int {
	enc := json.NewEncoder(c.stdout)
	enc.SetEscapeHTML(false) // keep <file> readable in the submit line
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return c.failf(exitInfra, "encoding output: %v", err)
	}
	return exitOK
}

// stateEnvFile is the sidecar in a state dir that sets the environment of every
// invocation on that dir, so the scripts a run executes see it whoever called
// the binary. Each line is K=V (set) or -K (unset); blank lines are ignored.
const stateEnvFile = "env"

func applyStateEnv(stateDir string) error {
	raw, err := os.ReadFile(filepath.Join(stateDir, stateEnvFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		var err error
		if k, ok := strings.CutPrefix(line, "-"); ok && k != "" && !strings.Contains(k, "=") {
			err = os.Unsetenv(k)
		} else if k, v, ok := strings.Cut(line, "="); ok && k != "" {
			err = os.Setenv(k, v)
		} else {
			return fmt.Errorf("%s:%d: %q is neither K=V nor -K", filepath.Join(stateDir, stateEnvFile), i+1, line)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *cli) journal(stateDir string, run RunID) int {
	// A read: the committed records, with no lock to wait on.
	recs, err := rootedFileJournal(stateDir, c.workspace).(committedReader).ReadCommitted(run)
	if err != nil {
		return c.fromError("journal", err)
	}
	enc := json.NewEncoder(c.stdout)
	for _, r := range recs {
		if err := enc.Encode(journalRecordView(r)); err != nil {
			return c.failf(exitInfra, "journal: %v", err)
		}
	}
	return exitOK
}

// journalExists keeps a mistyped run id from creating a run directory. A
// journal with no committed record does not count as a saved run, like
// Engine.Run (ErrUnknownRun): it agrees with fileJournal.load, which drops a
// final line with no newline, or a malformed final line, as a torn write.
func journalExists(stateDir string, run RunID) bool {
	f, err := os.Open(filepath.Join(stateDir, "runs", string(run), journalFile))
	if err != nil {
		return false
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return false // empty, or only a torn first line
	}
	var rec JournalRecord
	if json.Unmarshal(line[:len(line)-1], &rec) == nil {
		return true
	}
	_, err = r.Peek(1)
	return err == nil // a malformed line before more data is corruption, not a torn write
}

// engine builds a fresh Engine over the state directory. The submit, settle
// and answer command lines name the running binary by absolute path.
func (c *cli) engine(stateDir string, opts EngineOptions) (Executor, error) {
	opts.StateRoot = stateDir
	if opts.manufactured == nil {
		opts.manufactured = c.manufactured
	}
	if opts.ChildLive == nil {
		opts.ChildLive = adapterChildLive
	}
	if opts.Workspace == "" {
		opts.Workspace = c.workspace
	}
	if opts.Workspace != "" {
		root, err := filepath.Abs(opts.Workspace)
		if err != nil {
			return nil, err
		}
		opts.Workspace = root
		c.workspace = root
	}
	if c.assets != "" {
		c.assets = effectiveAssets(nil, c.assets)
	}
	opts.Assets = c.assets
	opts.OnAdopt = func(run RunID, data MigratedData) {
		fmt.Fprintf(c.stderr, "adopted %s: %s -> %s\n", run, data.FromHash, data.ToHash)
		ids := make([]string, 0, len(data.Carried))
		for id := range data.Carried {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			label := id
			if label == "" {
				label = "root"
			}
			fmt.Fprintf(c.stderr, "carried %s: %s\n", label, data.Carried[id])
		}
	}
	opts.caller = harness.For(envHarness(c.env)).Caller(c.env)
	opts.oneVerb = true
	bin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating the running binary: %w", err)
	}
	opts.caller.Executable = bin
	if opts.HeadlessCheck == nil {
		opts.HeadlessCheck = c.headlessCheck
	}
	opts.InstalledBinary = c.cfg.InstalledBinary
	opts.SnapshotFiles = c.cfg.SnapshotFiles
	opts.caller.PromptClaim = c.recordedPromptClaim
	for _, r := range c.cfg.Defs {
		opts.Defs = append(opts.Defs, r.Def)
	}
	base := func(sub string, run RunID) []string {
		args := []string{sink.Quote(bin), sub, sink.Quote(string(run)), "--state-dir", sink.Quote(stateDir)}
		if c.assets != "" {
			args = append(args, "--assets", sink.Quote(c.assets))
		}
		if c.workspace != "" {
			args = append(args, "--workspace", sink.Quote(c.workspace))
		}
		return args
	}
	// The resume line names the running binary, the state dir and the
	// workspace absolutely, and only the params the caller gave: a successor
	// derives the rest afresh (a new run tag, the current skills).
	opts.ResumeLine = func(started RunStartedData) string {
		if !c.canSucceed(started) {
			return ""
		}
		parts := []string{sink.Quote(bin), "run", sink.Quote(started.Def), "--state-dir", sink.Quote(stateDir)}
		if started.self != "" {
			// Bound to its chain: the successor of this run, whatever this
			// call's default tree would key.
			parts = append(parts, "--continue", sink.Quote(string(started.self)))
		}
		if started.Workspace != "" {
			parts = append(parts, "--workspace", sink.Quote(started.Workspace))
		}
		names := started.Given
		if names == nil {
			// A journal written before run_started named the given params:
			// repeat them all, so the line still runs.
			names = slices.Sorted(maps.Keys(started.Params))
		}
		for _, k := range names {
			parts = append(parts, "--param", sink.Quote(k+"="+started.Params[k]))
		}
		return strings.Join(parts, " ")
	}
	if c.cfg.named() {
		line := func(run RunID, verb string, args ...string) string {
			if c.assets != "" {
				args = append(args, "--assets", c.assets)
			}
			if c.workspace != "" {
				args = append(args, "--workspace", c.workspace)
			}
			return wfLine(bin, stateDir, run, verb, args...)
		}
		opts.SubmitLine = func(run RunID, token string) string {
			return line(run, "submit", "--token", token, "--output", "<file>")
		}
		opts.SettleLine = func(run RunID, effect int64) string {
			return line(run, "settle", "--effect", strconv.FormatInt(effect, 10), "<settlement>")
		}
		opts.AnswerLine = func(run RunID, gate string) string {
			return line(run, "answer", "--gate", gate, "--value", "<value>")
		}
		opts.VerbLine = func(run RunID, verb string, args ...string) string {
			return line(run, verb, args...)
		}
		c.verbLine = opts.VerbLine
		opts.EffectRunner = c.cfg.EffectRunner
		opts.Progress = c.stderr
		opts.RunDir = func(run RunID) string { return filepath.Join(stateDir, "runs", string(run)) }
		return NewEngine(c.fileJournal(stateDir), c.cfg.Adapters, opts), nil
	}
	opts.SubmitLine = func(run RunID, token string) string {
		return strings.Join(append(base("submit", run), "--token", sink.Quote(token), "--output", "<file>"), " ")
	}
	opts.SettleLine = func(run RunID, effect int64) string {
		return strings.Join(append(base("settle", run), "--effect", strconv.FormatInt(effect, 10), "<settlement>"), " ")
	}
	opts.AnswerLine = func(run RunID, gate string) string {
		return strings.Join(append(base("answer", run), "--gate", sink.Quote(gate), "--value", "<value>"), " ")
	}
	opts.VerbLine = func(run RunID, verb string, args ...string) string {
		return multiVerbLine(bin, stateDir, run, verb, args...)
	}
	c.verbLine = opts.VerbLine
	opts.EffectRunner = c.cfg.EffectRunner
	opts.Progress = c.stderr
	opts.RunDir = func(run RunID) string { return filepath.Join(stateDir, "runs", string(run)) }
	return NewEngine(c.fileJournal(stateDir), c.cfg.Adapters, opts), nil
}

// adapterChildLive is the CLI engine's EngineOptions.ChildLive: the
// claim's harness adapter answers whether a process of the child is live
// (harness.SettlingEnd); a harness without SettlingEnd has no live check
// and answers not live.
func adapterChildLive(h, child string) (bool, error) {
	se, ok := harness.For(h).(harness.SettlingEnd)
	if !ok {
		return false, nil
	}
	return se.ChildLive(child)
}

// budget fixes, once per invocation, when its lock waits and next's own wait
// end: an explicit next --timeout, else its harness's tool limit less
// nextWaitMargin (as next's own wait). A harness whose limit is unknown, with
// no --timeout, has none: each lock waits fileLockWait.
func (c *cli) budget(timeout time.Duration) {
	if !c.budgetFrom.IsZero() {
		return
	}
	c.budgetFrom = time.Now()
	switch limit := harness.For(envHarness(c.env)).ToolLimit(c.env); {
	case timeout > 0:
		c.lockBy = c.budgetFrom.Add(timeout)
	case limit > 0:
		c.lockBy = c.budgetFrom.Add(max(limit-nextWaitMargin, limit/2))
	}
}

// budgeted is j with its lock waits ending at this invocation's budget.
func (c *cli) budgeted(j Journal) Journal {
	c.budget(0)
	if !c.lockBy.IsZero() {
		setLockDeadline(j, c.lockBy)
	}
	setLockOwner(j, c.owner, c.stderr, &c.committed)
	return j
}

// unitArgs are the first positional arguments that name the unit a verb
// runs for.
var unitArgs = map[string]bool{"name": true, "unit": true}

// fileJournal is the run journal under stateDir, budgeted.
func (c *cli) fileJournal(stateDir string) Journal { return c.budgeted(FileJournal(stateDir)) }

// multiVerbLine is the command line of one verb of a multi-registration
// binary: `<bin> <verb> <run> <args> --state-dir <dir>`. The positional
// arguments come right after the run id, so the flags (including --state-dir)
// follow them.
func multiVerbLine(bin, stateDir string, run RunID, verb string, args ...string) string {
	parts := []string{sink.Quote(bin), verb, sink.Quote(string(run))}
	for _, a := range args {
		parts = append(parts, lineArg(a))
	}
	return strings.Join(append(parts, "--state-dir", sink.Quote(stateDir)), " ")
}

// parseUsage reads a --usage value: tokens=N, N a count of 0 or more.
func parseUsage(s string) (int, bool) {
	v, ok := strings.CutPrefix(s, "tokens=")
	n, err := strconv.Atoi(v)
	return n, ok && err == nil && n >= 0
}

// lineArg quotes one argument of an agent-facing command line. A template slot
// (`<` + lowercase letters + `>`, what the engine leaves for the reader to
// fill) stays bare; every other value is shell-quoted. A real value equal to a
// slot would print bare; ids and names are never of that form (a unit name
// starts with its state).
func lineArg(s string) string {
	if isSlot(s) {
		return s
	}
	// key=<slot> (tokens=<n>) is a placeholder too, when key is a plain word.
	if k, v, ok := strings.Cut(s, "="); ok && k != "" && isSlot(v) && sink.Quote(k) == k {
		return s
	}
	return sink.Quote(s)
}

// isSlot is a <lowercase> placeholder the reader fills in.
func isSlot(s string) bool {
	return len(s) > 2 && s[0] == '<' && s[len(s)-1] == '>' && strings.Trim(s[1:len(s)-1], "abcdefghijklmnopqrstuvwxyz") == ""
}

// takeBoundaryModelMap parses f's --model-map for the boundary this verb
// opens; a malformed one is a usage error.
func (c *cli) takeBoundaryModelMap(verb string, f *flags) int {
	m, err := ParseModelMap(f.modelMap)
	if err != nil {
		return c.failf(exitUsage, "%s: %v", verb, err)
	}
	c.boundaryModelMap = m
	return exitOK
}

// ParseModelMap parses "key=model[,key=model]". An empty string is no map.
func ParseModelMap(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		from, to, ok := strings.Cut(pair, "=")
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("--model-map entry %q is not key=model", pair)
		}
		if _, dup := m[from]; dup {
			return nil, fmt.Errorf("--model-map maps %q twice", from)
		}
		m[from] = to
	}
	return m, nil
}

// registration is the registration of the named definition, nil if none.
func (c *cli) registration(name string) *Registration {
	for i := range c.cfg.Defs {
		if c.cfg.Defs[i].Def.name == name {
			return &c.cfg.Defs[i]
		}
	}
	return nil
}

func (c *cli) start(name, stateDir string, f *flags) int {
	reg := c.registration(name)
	if reg == nil {
		return c.failf(exitUsage, "unknown definition %s", name)
	}
	d := reg.Def
	params := map[string]string{}
	for _, kv := range f.params {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return c.failf(exitUsage, "run: --param %q is not k=v", kv)
		}
		params[k] = v
	}
	workspace := f.workspace
	if workspace == "" {
		workspace = "."
	}
	workspace, err := realPath(workspace)
	if err != nil {
		return c.failf(exitInfra, "run: %v", err)
	}
	// --continue binds the call to an ended run's chain (the resume command
	// status prints names it): its recorded asset selection is resolved
	// before anything is derived, so nothing depends on this call's default
	// tree, and a run key outside its chain is refused below.
	var cont *RunStartedData
	if f.cont != "" {
		if f.runID != "" {
			return c.failf(exitUsage, "run: --continue and --run-id are exclusive: --continue finds the successor's id from the run it continues")
		}
		started, ended, err := c.journalEnded(stateDir, RunID(f.cont))
		if err != nil {
			return c.fromError("run", err)
		}
		if !ended {
			return c.failf(exitUsage, "run: --continue %s: no ended run %s in %s", f.cont, f.cont, stateDir)
		}
		if started.Def != d.name {
			return c.failf(exitUsage, "run: --continue %s: that run is of %s, not %s", f.cont, started.Def, d.name)
		}
		started.self = RunID(f.cont)
		cont = started
	}
	deriveAssets := effectiveAssets(d, c.assets)
	if cont != nil && c.assets == "" && cont.Assets != "" {
		deriveAssets = cont.Assets
	}
	given := maps.Clone(params)
	if err := reg.resolveParams(params, RunContext{Assets: deriveAssets, Params: params, Workspace: workspace, Now: time.Now().UTC()}); err != nil {
		return c.failf(exitUsage, "run: definition %s %v", d.name, err)
	}
	if f.runID == "" && len(d.runKey) == 0 {
		return c.failf(exitUsage, "run: definition %s has no run key: pass --run-id so the run can be resumed", d.name)
	}
	for _, p := range reg.Params {
		if p.AtCreation && slices.ContainsFunc(d.runKey, func(k Param) bool { return k.name == p.Name }) {
			return c.failf(exitInfra, "run: definition %s: param %s is AtCreation and in the run key: the run key is resolved before a run exists", d.name, p.Name)
		}
	}
	if f.runID == "" {
		for _, p := range d.runKey {
			if _, ok := params[p.name]; !ok {
				return c.failf(exitUsage, "run: definition %s needs --param %s=<value> (its run key) or --run-id", d.name, p.name)
			}
		}
	}
	modelMap, err := ParseModelMap(f.modelMap)
	if err != nil {
		return c.failf(exitUsage, "run: %v", err)
	}
	budgetFlag, err := ParseBudget(f.budget)
	if err != nil {
		return c.failf(exitUsage, "run: %v", err)
	}
	// The given options are checked here and decoded once the call's
	// landing is known, over the options it would otherwise use.
	rawOptions, err := loadOptions(d, f.options)
	if err == nil && f.optionsJSON != nil {
		rawOptions = f.optionsJSON
		_, err = decodeOptions(d, nil, rawOptions)
	}
	if err != nil {
		return c.failf(exitUsage, "run: %v", err)
	}
	var options any
	// Where this call lands: a live run (resumed), the successor of an
	// ended one, or a fresh run. A fresh run takes the registration's
	// default options, a resume the journaled ones and a successor its
	// predecessor's; given options are decoded over that base, so a field
	// they leave out keeps its base value.
	var liveOptions json.RawMessage
	if cont != nil {
		if key, err := deriveRunID(d, params, "", nil); err == nil && key != chainBase(cont.self) {
			return c.failf(exitUsage, "run: --continue %s: these params give run key %s, another chain than %s's (%s); nothing was started. Run the resume command status prints for %s unchanged, or drop --continue to start run %s", f.cont, key, f.cont, chainBase(cont.self), f.cont, key)
		}
	}
	live, pred, err := c.runChain(d, params, stateDir, f)
	if err != nil {
		return c.fromError("run", err)
	}
	// A successor keeps its predecessor's asset selection when the call
	// names no --assets (an explicit one wins), so the resume command status
	// prints runs as printed: its per-definition roots for lint,
	// and its selected root for the params derived from assets, which are
	// derived again (and the chain found again) when that root is not the
	// default this call resolved. Engine.Run records the same roots; it
	// also keeps the snapshots, paged prompts, options and delegations, and
	// the model map is kept below.
	var predRoots map[string]string
	if pred != nil && live == "" && c.assets == "" {
		if pred.Assets != "" && pred.Assets != deriveAssets {
			// The params were derived from another tree than the one the
			// chain recorded: derive them again from the recorded one. A key
			// that changes with it is refused: this call cannot tell which
			// chain it meant, and --continue says.
			again := maps.Clone(given)
			if again == nil {
				again = map[string]string{}
			}
			if err := reg.resolveParams(again, RunContext{Assets: pred.Assets, Params: again, Workspace: workspace, Now: time.Now().UTC()}); err != nil {
				return c.failf(exitUsage, "run: definition %s %v", d.name, err)
			}
			before, _ := deriveRunID(d, params, "", nil)
			if after, err := deriveRunID(d, again, "", nil); err == nil && after != before {
				return c.failf(exitUsage, "run: ended run %s recorded assets %s, which derive run key %s, not %s; nothing was started. Continue it with --continue %s, or pass --assets to start run %s", pred.self, pred.Assets, after, before, pred.self, before)
			}
			params = again
		}
		deriveAssets = pred.Assets
		predRoots = (&cycle{e: &Engine{}, st: &runState{started: pred}}).selectedAssetRoots(d)
	}
	roots := assetRoots(d, c.assets)
	if predRoots != nil {
		roots = predRoots
	}
	if live != "" {
		j := c.budgeted(rootedFileJournal(stateDir, ""))
		unlock, err := j.Lock(live)
		if err != nil {
			return c.fromError("run", err)
		}
		recs, err := j.Records(live)
		unlock()
		if err != nil {
			return c.fromError("run", err)
		}
		st, err := foldRun(recs, d)
		if err != nil {
			return c.fromError("run", err)
		}
		liveOptions = st.started.Options
		if st.started.Workspace != "" && !sameRealPath(st.started.Workspace, workspace) {
			return c.failf(exitUsage, "run: run %s uses workspace %s; use next or resume with --workspace to relocate it", live, st.started.Workspace)
		}
		if f.modelMap != "" {
			// an earlier revision: an attach never changes the map, and never ignores one.
			if err := checkModelMap(d, modelMap); err != nil {
				return c.failf(exitUsage, "run: %s", strings.TrimPrefix(err.Error(), errBadModelMap.Error()+": "))
			}
			// The map in force is the one the attaching caller would bind
			//: resolve for its harness (--harness, else its
			// environment's), never the last segment's. "" (no harness shown)
			// leaves it to the segment's own, which is what binds then.
			caller := normalizeHarness(f.harness)
			if caller == "" {
				if h := envHarness(c.env); h != "generic" {
					caller = h
				}
			}
			cur, err := (&cycle{st: st}).currentModels(caller)
			if err != nil {
				return c.failf(exitUsage, "run: run %s's current --model-map is refused by the harness that would bind it (%s): re-issue the boundary verb (wf resume, rewind or fork) with a corrected --model-map", live, strings.TrimPrefix(err.Error(), errBadModelMap.Error()+": "))
			}
			if !maps.Equal(cur.ModelMap, modelMap) {
				return c.failf(exitUsage, "run: run %s is already running with --model-map %q; its map changes only at a segment boundary: stop it (wf stop), then resume it with --model-map", live, mapText(cur.ModelMap))
			}
		}
		roots = (&cycle{e: &Engine{opts: EngineOptions{Assets: c.assets}}, st: st}).selectedAssetRoots(d)
		if f.budget != "" && budgetFlag.Over(st.budget) != st.budget {
			return c.failf(exitUsage, "run: run %s is already running with budget %s; its budget changes only when a resume clears a stop: stop it (wf stop), then resume it with --budget", live, st.budget)
		}
	}
	if ps := d.lintServedAssetRoots(roots); len(ps) > 0 {
		fmt.Fprintf(c.stderr, "lint %s: %d problem(s)\n", d.name, len(ps))
		for _, p := range ps {
			fmt.Fprintln(c.stderr, "  "+p.String())
		}
		return exitUsage
	}
	if live == "" {
		if err := reg.deriveAtCreation(params, RunContext{Assets: deriveAssets, Params: params, Workspace: workspace, Now: time.Now().UTC()}); err != nil {
			return c.failf(exitUsage, "run: definition %s %v", d.name, err)
		}
	}
	if pred != nil && live == "" {
		if pred.Workspace != "" && pred.Workspace != workspace {
			return c.failf(exitUsage, "run: run %s ended in workspace %s; its successor runs there: run from it or pass --workspace %s", pred.self, pred.Workspace, pred.Workspace)
		}
		if f.modelMap == "" {
			modelMap = pred.ModelMap
		}
	}
	switch {
	case rawOptions != nil:
		var base any = reg.DefaultOptions
		if live != "" {
			base = liveOptions
		} else if pred != nil {
			base = pred.Options
		}
		if options, err = decodeOptions(d, base, rawOptions); err != nil {
			return c.failf(exitUsage, "run: options: %v", err)
		}
	case live == "" && pred == nil:
		options = reg.DefaultOptions
	}
	var budget RunBudget
	if live == "" {
		if reg.Budget != nil {
			o, err := c.budgetOptions(d, options, pred)
			if err != nil {
				return c.failf(exitUsage, "run: %v", err)
			}
			budget = DefaultBudget(d, reg.Budget, o)
		}
		budget = budgetFlag.Over(budget)
	}
	if f.runID != "" && validRunID(RunID(f.runID)) != nil {
		return c.failf(exitUsage, "run: invalid --run-id %q", f.runID)
	}
	delegations, err := parseDelegations(f.delegates)
	if err != nil {
		return c.failf(exitUsage, "run: %v", err)
	}
	// Resume is decided first. On creation only, the harness is resolved here
	// (once; the engine receives the result) and the preflight checks run
	// before the engine, the state dir or any journal exists. On resume the
	// journaled harness governs: only an explicit --harness reaches the engine,
	// whose resumeHarnessMatches guard refuses a differing one.
	runHarness := f.harness
	if live == "" {
		if f.harness != "" {
			if why := harness.Contradicts(f.harness, c.env); why != "" {
				return c.failf(exitUsage, "run: %s", why)
			}
		}
		runHarness = normalizeHarness(f.harness)
		if runHarness == "" {
			runHarness = envHarness(c.env)
		}
		if code := runPreflight(reg, PreflightContext{Workspace: workspace, Harness: runHarness, ModelMap: modelMap, Params: maps.Clone(params)}, c.stderr); code != exitOK {
			return code
		}
	}
	var givenNames []string
	for _, kv := range f.params {
		k, _, _ := strings.Cut(kv, "=")
		if to, ok := d.aliases[k]; ok {
			k = to // an old name binds its param
		}
		givenNames = append(givenNames, k)
	}
	fromEnv := live == "" && f.harness == "" && runHarness != "generic"
	var hints []string
	if live == "" && c.env != nil {
		hints = harness.WeakHints(c.env)
		for _, h := range hints {
			fmt.Fprintln(c.stderr, "note: "+h)
		}
	}
	paged := f.pagedPrompts
	if !f.pagedPromptsSet {
		paged = reg.PagedPrompts
	}
	x, err := c.engine(stateDir, EngineOptions{PagedPrompts: paged, HaltBefore: f.haltBefore, Snapshots: f.snapshots, Workspace: workspace, ModelMap: modelMap, RunID: RunID(f.runID), RunBudget: budget, Delegations: delegations, Harness: runHarness, HarnessFromEnv: fromEnv, GivenParams: givenNames, HarnessHints: hints,
		Headless: f.headless.orNil()})
	if err != nil {
		return c.failf(exitInfra, "run: %v", err)
	}
	id, err := x.Run(d, params, options)
	if err != nil {
		return c.fromError("run", err)
	}
	c.run = id
	// Run is durable now. Keep its ID available even if status or recording
	// the named-binary default fails afterward.
	fmt.Fprintln(c.stdout, id)
	if h := betweenLocksHook; h != nil {
		h("run", id)
	}
	// A successor (run on an ended run) says so: the ended run, why it ended,
	// and the run that carries its work on.
	st, err := x.Status(id)
	if err != nil {
		return c.fromError("run", err)
	}
	if st.Predecessor != "" {
		cause := "ended"
		if prev, err := x.Status(st.Predecessor); err == nil && prev.Cause != "" {
			cause = prev.Cause
		}
		note := ""
		if c.cfg.SuccessorNote != nil {
			if records, err := filepath.Abs(RecordsDir(stateDir, id)); err == nil {
				if s := c.cfg.SuccessorNote(d.name, records); s != "" {
					note = " " + s
				}
			}
		}
		fmt.Fprintf(c.stderr, "previous run %s ended: %s; continuing as %s%s\n", st.Predecessor, cause, id, note)
	}
	// The effective options, after the base and any given overlay.
	if len(st.Options) > 0 {
		fmt.Fprintf(c.stderr, "run %s options: %s\n", id, st.Options)
	}
	if c.cfg.named() {
		if err := os.WriteFile(filepath.Join(stateDir, runIDFile), []byte(id+"\n"), 0o644); err != nil {
			return c.failf(exitInfra, "run: recording the run id: %v", err)
		}
	}
	if h := envHarness(c.env); h != "generic" {
		st.Harness, st.LegacyHarness = h, false
		if sm, err := x.(*Engine).ModelsFor(RunID(id), h); err == nil {
			st.Models = sm
		}
	}
	nextLine := x.(*Engine).opts.VerbLine(id, "next")
	fmt.Fprint(c.stderr, bootstrap(st, stateDir, nextLine))
	fmt.Fprintln(c.stderr, "Drive this workflow run until it ends or asks for a person. Start sub-agents whenever it asks you to. Never answer a question meant for a person yourself.")
	return exitOK
}

// budgetOptions is the options value a new run starts with, for its
// registration's Budget: the given options, else its predecessor's.
func (c *cli) budgetOptions(d *Def, options any, pred *RunStartedData) (any, error) {
	if options == nil && d.options != nil && pred != nil && len(pred.Options) > 0 {
		return decodeOptions(d, nil, pred.Options)
	}
	return options, nil
}

// DefaultBudget is the budget fn (a Registration's Budget) gives a run of d
// started with options. Nil options read as the zero value of d's options
// type, as run passes them; a nil fn is no budget.
func DefaultBudget(d *Def, fn func(options any) RunBudget, options any) RunBudget {
	if fn == nil {
		return RunBudget{}
	}
	if options == nil && d.options != nil {
		options = reflect.Zero(d.options.typ).Interface()
	}
	return fn(options)
}

// runChain is where a run call lands: live is the run it resumes (a journal
// that has not ended), and pred, when every run of the chain <id>, <id>.2, ...
// has ended, the run_started of the last one, whose successor this call
// starts. A run with no explicit id and no run key is always new (both
// empty). An explicit id is the chain's only member and is never succeeded.
func (c *cli) runChain(d *Def, params map[string]string, stateDir string, f *flags) (live RunID, pred *RunStartedData, err error) {
	if f.runID == "" && len(d.runKey) == 0 {
		return "", nil, nil
	}
	base, err := deriveRunID(d, params, RunID(f.runID), nil)
	if err != nil {
		return "", nil, nil
	}
	id := base
	for n := 2; journalExists(stateDir, id); n++ {
		started, ended, err := c.journalEnded(stateDir, id)
		if err != nil {
			return "", nil, err
		}
		if !ended || f.runID != "" {
			return id, nil, nil
		}
		started.self = id
		pred, id = started, RunID(fmt.Sprintf("%s.%d", base, n))
	}
	return "", pred, nil
}

// chainBase is the run id a successor chain starts from: id without its
// ".<n>" successor suffix (n >= 2).
func chainBase(id RunID) RunID {
	s := string(id)
	if i := strings.LastIndex(s, "."); i > 0 {
		if n, err := strconv.Atoi(s[i+1:]); err == nil && n >= 2 && strconv.Itoa(n) == s[i+1:] {
			return RunID(s[:i])
		}
	}
	return id
}

// journalEnded reports whether run id's journal records that it ended, with
// its run_started; an unreadable journal is reported as not ended. A run
// whose lock stays held past the invocation's budget is an error.
func (c *cli) journalEnded(stateDir string, id RunID) (*RunStartedData, bool, error) {
	j := c.budgeted(rootedFileJournal(stateDir, ""))
	unlock, err := j.Lock(id)
	if errors.Is(err, ErrRunLocked) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, nil
	}
	defer unlock()
	recs, err := j.Records(id)
	if err != nil {
		return nil, false, nil
	}
	started, err := endedStart(recs)
	if err != nil || started == nil {
		return nil, false, nil
	}
	return started, true, nil
}

// parseDelegations parses the repeated --delegate gate=value flags.
func parseDelegations(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, kv := range kvs {
		gate, value, ok := strings.Cut(kv, "=")
		if !ok || gate == "" || value == "" {
			return nil, fmt.Errorf("--delegate %q is not gate=value", kv)
		}
		if _, dup := m[gate]; dup {
			return nil, fmt.Errorf("--delegate delegates %q twice", gate)
		}
		m[gate] = value
	}
	return m, nil
}

// runIDFile is the sidecar in a state dir that records the run a named
// binary drives.
const runIDFile = "run-id"

// namedCLI is CLI as a named binary: `run` names its definition optionally
// (the first registration by default), `next` prints the bootstrap and the
// run-driving verbs sit under `wf`.
func namedCLI(cfg MainConfig, cmd string, args []string, stdout, stderr io.Writer, env harness.Env, disposition *refusalDisposition) int {
	c := &cli{cfg: cfg, stdout: stdout, stderr: stderr, env: env, disposition: disposition}
	row, ok := findRow(cmd)
	switch {
	case cmd == "next":
		return c.bootstrap(args)
	case cmd == "wf":
		return c.wfVerb(args)
	case ok && row.act == nil && cmd == "run":
		if len(cfg.Defs) == 0 {
			return c.failf(exitUsage, "run: %s serves no definition", cfg.Binary)
		}
		name := cfg.Defs[0].Def.name
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			name, args = args[0], args[1:]
		} else if wantsHelp(args) {
			// `run --help <def>`: the definition may follow the flag.
			for _, a := range args {
				if c.registration(a) != nil {
					name = a
				}
			}
		}
		if cfg.StateDirFor != nil {
			if dir := cfg.StateDirFor(name); dir != "" {
				c.cfg.DefaultStateDir = dir
			}
		}
		return row.handle(c, row, append([]string{name}, args...))
	case ok && row.act == nil:
		return row.handle(c, row, args)
	}
	if now, ok := removedVerbs[cmd]; ok {
		fmt.Fprintf(stderr, "%s: removed: %s\n", cmd, strings.ReplaceAll(now, "wf ", cfg.Binary+" wf "))
		return exitUsage
	}
	fmt.Fprintf(stderr, "%s: unknown subcommand\n", cmd)
	usage(cfg, stderr)
	return exitUsage
}

// removedVerbs are verbs an earlier protocol had, each with what replaced it
// (G2): a coordinator that remembers one fails with its fix.
var removedVerbs = map[string]string{
	"wait":   "the loop is one call now: wf next; it blocks until the run has something for you",
	"submit": "a child finishes with wf done <name> (it writes its result to the allocated output file first)",
	"fail":   "a child gives up with wf failed <name> --reason <text> (--class blocked for a refusal)",
}

var devStarted = Subcommand{
	Name:    "started",
	Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--context fresh|inherited", "--harness h", "--session s", "--agent id", "--transcript path", "--usage tokens=N", "--escalated 'command: reason: outcome'"},
	Summary: "claim a handed unit; attach its metadata late, or its usage once its child ended",
	handle:  (*cli).drive,
	subject: "run id",
	Args:    []string{"name"},
	define: func(fs *flag.FlagSet, f *flags) {
		fs.Func("harness", "the coordinator's harness", func(v string) error {
			f.harness = v
			return refuseScripted(normalizeHarness(v))
		})
		fs.StringVar(&f.session, "session", "", "the coordinator's session id")
		fs.StringVar(&f.agent, "agent", "", "the subagent's agent id")
		fs.StringVar(&f.transcript, "transcript", "", "the subagent's transcript path")
		fs.StringVar(&f.usage, "usage", "", "tokens=N the ended child spent, as its spawn result reports")
		fs.Var(&f.escalated, "escalated", escalatedHelp+" (the coordinator's own, for this unit)")
		fs.Func("context", "the context the subagent was spawned with: fresh or inherited", func(v string) error {
			m, err := ParseContextMode(v)
			f.context = m
			return err
		})
	},
	act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
		tokens, usage := 0, f.usage != ""
		if usage {
			var ok bool
			if tokens, ok = parseUsage(f.usage); !ok {
				return c.failf(exitUsage, "%s: --usage %q is not tokens=N", row.Name, f.usage)
			}
		}
		ds, code := c.escalations(row.Name, f)
		if code != exitOK {
			return code
		}
		claim := f.agent != "" || f.session != "" || f.transcript != "" || f.harness != "" || f.context != ""
		if usage && claim {
			return c.failf(exitUsage, "%s: --usage attaches to an ended unit alone; pass the claim flags in their own started", row.Name)
		}
		escalatedOnly := len(ds) > 0 && !usage && !claim
		if !usage && !escalatedOnly && (f.agent == "" || f.session == "") {
			return c.failf(exitUsage, "%s: --agent and --session are required", row.Name)
		}
		if f.transcript != "" {
			abs, err := filepath.Abs(f.transcript) // against the caller's cwd: the engine may run elsewhere
			if err != nil {
				return c.failf(exitInfra, "%s: %v", row.Name, err)
			}
			f.transcript = abs
		}
		return c.withEngine(row, stateDir, func(x Executor) int {
			if err := x.(*Engine).DegradeChecked(run, f.args[0], ds); err != nil {
				return c.fromError(row.Name, err)
			}
			if h := betweenLocksHook; h != nil && len(ds) > 0 {
				h("started", run)
			}
			if !usage && !escalatedOnly {
				state, err := x.StartedNote(run, f.args[0], Claim{Harness: f.harness, Session: f.session, Agent: f.agent, Transcript: f.transcript, Context: f.context})
				if err != nil {
					return c.fromError(row.Name, err)
				}
				if state != "" {
					fmt.Fprintf(c.stdout, "note: unit already %s\n", state)
				}
			}
			if usage {
				if err := x.(*Engine).AttachUsage(run, f.args[0], tokens); err != nil {
					return c.fromError(row.Name, err)
				}
			}
			return exitOK
		})
	},
}

// DevSubcommands are the developer verbs under `wf dev`: rewind (converge
// E2). They take the run-driving rows' common parsing.
var DevSubcommands = []Subcommand{
	{
		Name:    "rewind",
		Flags:   []string{"--state-dir dir", "--assets dir", "--workspace dir", "--to died", "--out dir", "--changed def/state[,def/state]", "--model-map key=model[,key=model]"},
		Summary: "fork <run> at the state that died, a checkpoint, or (--to unchanged) before the first state this binary's definition changed, into a new run that carries on from there (the source is left as it was); resume the fork with next --adopt on a fixed binary",
		handle:  (*cli).drive,
		subject: "run id",
		define: func(fs *flag.FlagSet, f *flags) {
			fs.StringVar(&f.to, "to", "", "where to fork: died (the state whose decision ended the run, or a halted state), or a checkpoint's journal seq (with --out), to run again what followed it")
			fs.StringVar(&f.forkOut, "out", "", "restore a snapshot into a new directory containing workspace and state")
			fs.StringVar(&f.changed, "changed", "", "with --to unchanged: states whose code changed in ways their declaration does not show (<definition>/<state>, comma-separated); the cut lands before the first run of any of them")
			fs.StringVar(&f.modelMap, "model-map", "", "tier, all or model = model[,...]: the models the fork spawns with")
		},
		act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
			if f.to == "" {
				return c.failf(exitUsage, "dev rewind: --to is required (died)")
			}
			if code := c.takeBoundaryModelMap("dev rewind", f); code != exitOK {
				return code
			}
			if f.changed != "" && f.to != "unchanged" {
				return c.failf(exitUsage, "dev rewind: --changed needs --to unchanged")
			}
			return c.withEngine(row, stateDir, func(x Executor) int {
				e := x.(*Engine)
				if f.to == "unchanged" {
					return c.rewindUnchanged(e, stateDir, run, f)
				}
				if f.forkOut != "" {
					rep, err := e.RewindInto(run, f.to, f.forkOut)
					if err != nil {
						return c.fromError("dev rewind", err)
					}
					fmt.Fprintf(c.stdout, "%s\n", rep.Run)
					fmt.Fprintf(c.stderr, "restored checkpoint %d into %s; state: %s\n", rep.Seq, rep.Workspace, rep.StateDir)
					c.reportInstalled(rep)
					return exitOK
				}
				rep, err := e.Rewind(run, f.to)
				if err != nil {
					return c.fromError("dev rewind", err)
				}
				fmt.Fprintln(c.stdout, rep.Run)
				if h := betweenLocksHook; h != nil {
					h("dev rewind", rep.Run)
				}
				fmt.Fprintf(c.stderr, "rewound %s at seq %d: %s runs again in %s\n", run, rep.Data.Seq, rep.Data.State, rep.Run)
				for _, a := range rep.Data.Applied {
					fmt.Fprintf(c.stderr, "  applied after that point, not undone: %s\n", a)
				}
				if _, err := os.Stat(filepath.Join(stateDir, runIDFile)); err == nil {
					if err := os.WriteFile(filepath.Join(stateDir, runIDFile), []byte(string(rep.Run)+"\n"), 0o644); err != nil {
						return c.failf(exitInfra, "dev rewind: %v", err)
					}
				}
				lines, err := e.commandLines(rep.Run)
				if err != nil {
					return c.fromError("dev rewind", err)
				}
				fmt.Fprintf(c.stderr, "carry on: adopt the fork with:\n  %s\nthen the coordinator drives it with:\n  %s\n", lines("next", "--adopt"), lines("next"))
				return exitOK
			})
		},
	},
}

// devReplay is `wf dev replay <run>`: the run's journal replayed against this
// binary's definition (converge E3).
var devReplay = Subcommand{
	Name:  "replay",
	Flags: []string{"--state-dir dir", "--assets dir", "--workspace dir", "--observe fixture.json", "--same-key"},
	define: func(fs *flag.FlagSet, f *flags) {
		fs.StringVar(&f.observe, "observe", "", "answer reads the run never recorded from this fixture: a JSON list of {adapter, op, contains, value}")
		fs.BoolVar(&f.sameKey, "same-key", false, "answer an effect whose key the run applied, with other arguments, with the recorded result (listed as rebound)")
	},
	Summary: "replay <run>'s recorded agent results, reads and effects against this binary's definition, with no model, and print the first difference (the state, the route then and now)",
	handle:  (*cli).drive,
	subject: "run id",
	act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
		return c.withEngine(row, stateDir, func(x Executor) int {
			e := x.(*Engine)
			var recs []JournalRecord
			if err := e.withRecords(run, func(r []JournalRecord) { recs = r }); err != nil {
				return c.fromError("dev replay", err)
			}
			d, err := decodeData(recs[0])
			if err != nil {
				return c.failf(exitInfra, "dev replay: %v", err)
			}
			started, _ := d.(*RunStartedData)
			if started == nil {
				return c.failf(exitInfra, "dev replay: %s does not start with run_started", run)
			}
			var def *Def
			for _, cand := range e.opts.Defs {
				if cand.name == started.Def {
					def = cand
				}
			}
			if def == nil {
				return c.failf(exitUsage, "dev replay: this binary has no definition %s", started.Def)
			}
			source := ""
			if e.opts.RunDir != nil {
				source = e.opts.RunDir(run)
			}
			opts := ReplayOptions{SameKey: f.sameKey}
			if f.observe != "" {
				obs, err := ReplayFixture(f.observe)
				if err != nil {
					return c.failf(exitUsage, "dev replay: %v", err)
				}
				opts.Observe = obs
			}
			rep, err := ReplayWith(recs, def, source, opts)
			if err != nil {
				return c.failf(exitInfra, "dev replay: %v", err)
			}
			fmt.Fprint(c.stdout, rep.String())
			return exitOK
		})
	},
}

func init() { DevSubcommands = append(DevSubcommands, devReplay) }

// findDevRow is the `dev` verb named name.
func findDevRow(name string) (Subcommand, bool) {
	for _, row := range DevSubcommands {
		if row.Name == name {
			return row, true
		}
	}
	return Subcommand{}, false
}

func findRow(name string) (Subcommand, bool) {
	for _, row := range Subcommands {
		if row.Name == name {
			return row, true
		}
	}
	return Subcommand{}, false
}

// splitCommon takes --state-dir, --assets and --run-id out of args wherever they stand
// and returns the rest, so a verb's own parsing sees positionals first.
func (c *cli) splitCommon(args []string) (rest []string, stateDir, runID string, err error) {
	stateDir = c.cfg.stateDir()
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		if !strings.HasPrefix(args[i], "--") || (name != "state-dir" && name != "run-id" && name != "assets" && name != "workspace") {
			rest = append(rest, args[i])
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, "", "", fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = args[i]
		}
		if name == "state-dir" {
			stateDir = val
		} else if name == "workspace" {
			c.workspace = val
		} else if name == "assets" {
			c.assets = val
		} else {
			runID = val
		}
	}
	return rest, stateDir, runID, nil
}

// resolveRun is the run a named binary drives: --run-id, else
// <state-dir>/run-id, else the only run under <state-dir>/runs.
func (c *cli) resolveRun(verb, stateDir, runID string) (RunID, int) {
	if runID != "" {
		return RunID(runID), exitOK
	}
	if raw, err := os.ReadFile(filepath.Join(stateDir, runIDFile)); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return RunID(id), exitOK
		}
	}
	entries, _ := os.ReadDir(filepath.Join(stateDir, "runs"))
	var runs []string
	for _, e := range entries {
		if e.IsDir() {
			runs = append(runs, e.Name())
		}
	}
	switch len(runs) {
	case 1:
		return RunID(runs[0]), exitOK
	case 0:
		return "", c.failf(exitUsage, "%s: no run in %s; start one with `%s run` or pass --run-id", verb, stateDir, c.cfg.Binary)
	}
	return "", c.failf(exitUsage, "%s: several runs in %s (%s); pass --run-id", verb, stateDir, strings.Join(runs, ", "))
}

// wfVerb runs `wf <verb> [args]`: a run-driving row on the resolved run, or an
// inspection row on the one definition.
func (c *cli) wfVerb(args []string) int {
	if len(args) == 0 {
		return c.failf(exitUsage, "wf: missing verb")
	}
	if args[0] == "started" {
		return c.refuseStarted()
	}
	args0 := args[0] // the verb as typed, before a dev group is stepped into
	row, ok := findRow(args[0])
	if args[0] == "dev" && len(args) > 1 {
		row, ok = findDevRow(args[1])
		args = args[1:]
	}
	if now, removed := removedVerbs[args[0]]; !ok && removed && args[0] == args0 {
		fmt.Fprintf(c.stderr, "wf %s: removed: %s\n", args[0], strings.ReplaceAll(now, "wf ", c.cfg.Binary+" wf "))
		return exitUsage
	}
	if !ok || row.Name == "run" {
		fmt.Fprintf(c.stderr, "wf %s: unknown verb\n", args[0])
		usage(c.cfg, c.stderr)
		return exitUsage
	}
	if row.act == nil {
		if row.Name == "coordinator-stop" || row.Name == "install-coordinator-stop" {
			return row.handle(c, row, args[1:])
		}
		if args0 == "dev" {
			// A dev verb that drives no existing run (manufacture) parses
			// its own arguments.
			return row.handle(c, row, args[1:])
		}
		return row.handle(c, row, nil)
	}
	if len(args) > 1 && wantsHelp(args[1:2]) { // help needs no run; only right after the verb, never a flag value
		return c.verbHelp(row, "wf "+row.Name)
	}
	rest, stateDir, runID, err := c.splitCommon(args[1:])
	if err != nil {
		return c.failf(exitUsage, "wf %s: %v", row.Name, err)
	}
	if stateDir, err = filepath.Abs(stateDir); err != nil {
		return c.failf(exitInfra, "wf %s: %v", row.Name, err)
	}
	if runID == "" && unitVerbs[row.Name] && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		// The unit names its run: never the recorded default when the unit is
		// elsewhere.
		switch owners := c.unitOwners(stateDir, rest[0]); len(owners) {
		case 1:
			runID, c.ownerResolved = string(owners[0]), true
		case 0:
		default:
			names := make([]string, len(owners))
			for i, o := range owners {
				names[i] = string(o)
			}
			return c.failf(exitUsage, "wf %s: unit %s is live in runs %s; pass --run-id with the run your instruction names", row.Name, rest[0], strings.Join(names, ", "))
		}
	}
	if row.Name == "attach-coordinator" && runID == "" {
		return c.failf(exitUsage, "wf attach-coordinator: pass --run-id explicitly")
	}
	run, code := c.resolveRun(row.Name, stateDir, runID)
	if code != exitOK {
		return code
	}
	return row.handle(c, row, append(append([]string{string(run)}, rest...), "--state-dir", stateDir))
}

// bootstrap prints the coordinator bootstrap of the run (`./super next`).
func (c *cli) bootstrap(args []string) int {
	if wantsHelp(args) { // help needs no run
		fmt.Fprintf(c.stdout, "usage: %s next [--state-dir dir] [--run-id id] [--assets dir] [--workspace dir]\nprint the coordinator bootstrap of the run, or its ended instruction once it ended\n", c.cfg.Binary)
		return exitOK
	}
	rest, stateDir, runID, err := c.splitCommon(args)
	if err != nil || len(rest) > 0 {
		return c.failf(exitUsage, "next: usage: %s next [--state-dir dir] [--run-id id] [--assets dir] [--workspace dir]", c.cfg.Binary)
	}
	if childCaller(c.env) {
		return c.failf(exitRejected, "next: %s", childRefusal())
	}
	if stateDir, err = filepath.Abs(stateDir); err != nil {
		return c.failf(exitInfra, "next: %v", err)
	}
	if err := applyStateEnv(stateDir); err != nil {
		return c.failf(exitInfra, "next: %v", err)
	}
	run, code := c.resolveRun("next", stateDir, runID)
	if code != exitOK {
		return code
	}
	if validRunID(run) != nil || !journalExists(stateDir, run) {
		return c.failf(exitUnknown, "next: unknown run %q in %s", run, stateDir)
	}
	x, err := c.engine(stateDir, EngineOptions{})
	if err != nil {
		return c.failf(exitInfra, "next: %v", err)
	}
	armed, err := x.(*Engine).turnArmed(run)
	if err != nil {
		return c.fromError("next", err)
	}
	attachedNext := func() int {
		return c.wfVerb([]string{"next", "--state-dir", stateDir, "--run-id", string(run)})
	}
	if armed {
		return attachedNext()
	}
	// Inspect the existing gate without driving: a first coordinator must see
	// bootstrap before any pending workflow effects execute.
	probe, open, err := x.(*Engine).openReservedCoordinatorInstruction(run)
	if errors.Is(err, errCoordinatorAttached) {
		return attachedNext()
	}
	if err != nil {
		return c.fromError("next", err)
	}
	if open {
		fmt.Fprintln(c.stdout, probe.Prompt)
		return exitEnded
	}
	st, err := x.Status(run)
	if err != nil {
		return c.fromError("next", err)
	}
	if st.Breakpoint != nil {
		b, err := x.(*Engine).admitBreakpointCoordinator(run, envCoordinator(c.env))
		if err != nil {
			return c.fromError("next", err)
		}
		if b != nil {
			fmt.Fprintf(c.stdout, "The run is stopped at a %s. An operator can continue it with wf resume.\n", b.summary())
			return exitEnded
		}
		// An operator resumed the run between status and admission.
		st, err = x.Status(run)
		if err != nil {
			return c.fromError("next", err)
		}
	}
	if st.Phase == RunEnded {
		// An ended run has no loop to follow: the one ended instruction that
		// every other form of next returns.
		ins, err := x.HandOut(run, 0)
		if err != nil {
			return c.fromError("next", err)
		}
		if code := c.printJSON(ins); code != exitOK {
			return code
		}
		return exitEnded
	}
	// A session that already coordinates this run has its loop: a repeat of
	// the bootstrap verb is a poll, so it gets the loop line once and blocks as
	// wf next does (H1). A new session, or one the run never saw, gets the
	// bootstrap.
	if co := envCoordinator(c.env); co.Session != "" && x.(*Engine).knownCoordinator(run, co.Session) {
		// A reserved gate can open after the probe above. Choose the preamble
		// from the actual response so its human handoff remains the whole output.
		var reply strings.Builder
		nextCLI := *c
		nextCLI.stdout = &reply
		code := nextCLI.wfVerb([]string{"next", "--state-dir", stateDir, "--run-id", string(run)})
		if !strings.HasPrefix(reply.String(), "This run is waiting for a person at ") {
			e := x.(*Engine)
			fmt.Fprintf(c.stdout, "you already coordinate run %s; your loop is:\n  %s\n", run, e.workspaceCommand(st.Workspace, e.opts.VerbLine(run, "next")))
		}
		fmt.Fprint(c.stdout, reply.String())
		return code
	}
	// The harness line is the caller's (A3a): next reads it from the
	// environment of each call, so the bootstrap does too.
	if h := envHarness(c.env); h != "generic" {
		st.Harness, st.LegacyHarness = h, false
		if sm, err := x.(*Engine).ModelsFor(run, h); err == nil {
			st.Models = sm
		}
	}
	fmt.Fprint(c.stdout, bootstrap(st, stateDir, x.(*Engine).opts.VerbLine(run, "next")))
	return exitOK
}

// loadOptions reads the --options JSON file and checks it against the definition's
// options struct; nil when no file is given.
func loadOptions(d *Def, path string) (json.RawMessage, error) {
	if path == "" {
		return nil, nil
	}
	if d.options == nil {
		return nil, fmt.Errorf("definition %s declares no options", d.name)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading --options: %w", err)
	}
	if _, err := decodeOptions(d, nil, raw); err != nil {
		return nil, fmt.Errorf("--options %s: %w", path, err)
	}
	return raw, nil
}

// decodeOptions decodes raw JSON into a value of the definition's options
// struct, refusing fields it does not declare. The value starts as base (an
// options value or its JSON; nil is the zero value), so a field raw leaves
// out keeps base's value, as encoding/json decodes over a value.
func decodeOptions(d *Def, base any, raw []byte) (any, error) {
	if d.options == nil {
		return nil, fmt.Errorf("definition %s declares no options", d.name)
	}
	v := reflect.New(d.options.typ)
	if rm, ok := base.(json.RawMessage); ok && len(rm) == 0 {
		base = nil
	}
	if base != nil {
		b, err := json.Marshal(base)
		if err == nil {
			err = json.Unmarshal(b, v.Interface())
		}
		if err != nil {
			return nil, fmt.Errorf("base options: %w", err)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v.Interface()); err != nil {
		return nil, err
	}
	return v.Elem().Interface(), nil
}

// recordedPromptClaim shares the live CLI's flag definitions and common-flag
// extraction. It only parses; it does not resolve a run or perform any verb.
func (c *cli) recordedPromptClaim(args []string, named bool) (string, bool) {
	copyCLI := *c
	copyCLI.stderr = io.Discard
	if named {
		var err error
		args, _, _, err = copyCLI.splitCommon(args)
		if err != nil {
			return "", false
		}
	} else {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return "", false
		}
		args = args[1:] // run id
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = args[1:]
	} // optional unit
	row := Subcommand{Name: "prompt", define: promptFlagDefinitions}
	var f flags
	fs := copyCLI.flagSet(row, &f)
	if fs.Parse(args) != nil || fs.NArg() != 0 || f.claim == "" {
		return "", false
	}
	return f.claim, true
}

func promptFlagDefinitions(fs *flag.FlagSet, f *flags) {
	fs.IntVar(&f.page, "page", 1, "one-based prompt page")
	fs.StringVar(&f.pageCode, "code", "", "continuation code from the preceding page")
	fs.StringVar(&f.claim, "claim", "", "the claim token of the hand-out this child was spawned from")
	fs.BoolVar(&f.json, "json", false, "print {delivered, prompt, inputs} as JSON: the full rendered prompt and its input values")
}

var errCoordinatorAttached = errors.New("run has an explicit coordinator attachment")

// openReservedCoordinatorInstruction inspects journal state without advancing it.
func (e *Engine) openReservedCoordinatorInstruction(run RunID) (Instruction, bool, error) {
	var ins Instruction
	var open bool
	err := e.withDryRun(run, func(c *cycle) error {
		if c.st.turn != nil {
			return errCoordinatorAttached
		}
		if !c.st.ended && !c.st.paused {
			ins, open = c.reservedCoordinatorInstruction()
		}
		return nil
	})
	return ins, open, err
}

// refuseScripted refuses the engine-internal harness kind wherever a caller
// names a harness: only the manufacture driver presents it.
func refuseScripted(h string) error {
	if h == ScriptedHarness {
		return fmt.Errorf("harness %s is internal to wf dev manufacture; no caller can name it", ScriptedHarness)
	}
	return nil
}

// reportInstalled names the binary a snapshot fork installed: the invoking
// build, not the run's own, and whether it hashes the run's definition
// differently.
func (c *cli) reportInstalled(f SnapshotFork) {
	if f.Installed == "" {
		return
	}
	fmt.Fprintf(c.stderr, "installed the invoking build %s as %s: not the run's own binary, so the fork is not a reproduction of the original run; resume it on this build with next --adopt\n", f.InstalledFrom, f.Installed)
	if f.RunHash != "" && f.RunHash != f.BuildHash {
		fmt.Fprintf(c.stderr, "the run recorded definition hash %s; this build's is %s: next --adopt carries the run over only when every live state still exists with the same kind and result type, else it refuses naming the state\n", f.RunHash, f.BuildHash)
	}
}
