package wf

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const fanoutCaveat = "Defect detector only: these isolated state probes do not estimate workflow acceptance. A full run remains the acceptance test."

var devFanout = Subcommand{
	Name: "fanout", subject: "run id", handle: (*cli).drive,
	Flags:   []string{"--state-dir dir", "--workspace dir", "--assets dir", "--out dir", "--states distinct|all|list", "--coordinator-cmd JSON-argv", "--parallel n", "--timeout duration", "--dry-run", "--model-map key=model[,key=model]"},
	Summary: "restore selected historical states into isolated forks, run one coordinator per unit, and report defects from journals and transcripts",
	define: func(fs *flag.FlagSet, f *flags) {
		fs.StringVar(&f.forkOut, "out", "", "new directory for isolated forks and report")
		fs.StringVar(&f.fanoutStates, "states", "distinct", "distinct (latest per state), all visits, or comma-separated states")
		fs.StringVar(&f.coordinatorCmd, "coordinator-cmd", "", "JSON argv array; placeholders: {workspace}, {state}, {run}, {unit}, {brief}, {prompt}")
		fs.IntVar(&f.parallel, "parallel", 0, "parallel coordinators; required for an arbitrary coordinator argv template")
		fs.DurationVar(&f.timeout, "timeout", 15*time.Minute, "wall-clock limit per coordinator; stops fork then kills process group")
		fs.BoolVar(&f.dryRun, "dry-run", false, "print planned forks and argv without creating forks or launching commands")
		fs.StringVar(&f.modelMap, "model-map", "", "tier, all or model = model[,...]: the models every fork spawns with")
	},
	act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
		if code := c.takeBoundaryModelMap("fanout", f); code != exitOK {
			return code
		}
		return c.withEngine(row, stateDir, func(x Executor) int { return c.fanout(x.(*Engine), run, f) })
	},
}

func init() { DevSubcommands = append(DevSubcommands, devFanout) }

// A JSON argv keeps spaces and substitutions as data; no shell parses it.
func fanoutArgv(raw string) ([]string, error) {
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil || len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("--coordinator-cmd must be a nonempty JSON argv array")
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("coordinator argv contains NUL")
		}
	}
	return args, nil
}

func expandFanoutArgv(args []string, fork SnapshotFork, unit, brief, prompt string) []string {
	replace := strings.NewReplacer("{workspace}", fork.Workspace, "{state}", fork.StateDir, "{run}", string(fork.Run), "{unit}", unit, "{brief}", brief, "{prompt}", prompt)
	out := make([]string, len(args))
	for i, arg := range args {
		out[i] = replace.Replace(arg)
	}
	return out
}

func (c *cli) fanout(source *Engine, run RunID, f *flags) int {
	argv, err := fanoutArgv(f.coordinatorCmd)
	if err != nil {
		return c.failf(exitUsage, "fanout: %v", err)
	}
	if f.forkOut == "" || f.timeout <= 0 || f.parallel < 0 {
		return c.failf(exitUsage, "fanout: --out and a positive --timeout are required; --parallel cannot be negative")
	}
	// an earlier revision: a bad --model-map is refused before anything, dry runs too.
	if err := source.CheckBoundaryModelMap(run); err != nil {
		return c.fromError("fanout", err)
	}
	plan, err := source.PlanFanout(run, f.fanoutStates)
	// Every point the plan leaves out is named, never capped silently.
	for _, sk := range plan.Skipped {
		fmt.Fprintf(c.stderr, "fanout: skipped %s offer %d: %s\n", sk.State, sk.IssueSeq, sk.Reason)
	}
	if err != nil {
		return c.fromError("fanout", err)
	}
	targets := plan.Targets
	out, err := filepath.Abs(f.forkOut)
	if err != nil {
		return c.fromError("fanout", err)
	}
	if _, err = os.Lstat(out); !os.IsNotExist(err) {
		return c.failf(exitUsage, "fanout: --out must not exist")
	}
	// Validate even dry runs; never put a fork inside the source world.
	var recs []JournalRecord
	if err = source.withRecords(run, func(r []JournalRecord) { recs = r }); err != nil {
		return c.fromError("fanout", err)
	}
	var started RunStartedData
	if err = json.Unmarshal(recs[0].Data, &started); err != nil {
		return c.fromError("fanout", err)
	}
	// The batch root passes the same destination checks as each fork
	// (overlap, symlink ancestors) before anything is written there.
	if _, err := checkForkDestination(out, started.Workspace, source.opts.StateRoot); err != nil {
		return c.failf(exitUsage, "fanout: --out: %v", err)
	}
	parallel := f.parallel
	// An arbitrary wrapper/argv template does not identify its effective
	// harness or profile. The caller supplies that harness's cap per batch.
	if parallel <= 0 {
		return c.failf(exitUsage, "fanout: coordinator harness limit is unknown for an argv template; pass --parallel for this invocation")
	}
	jobs := make([]fanoutJob, len(targets))
	for i, target := range targets {
		dir := filepath.Join(out, fmt.Sprintf("%04d", i+1))
		fork := SnapshotFork{Run: run, Seq: target.SnapshotSeq, Workspace: filepath.Join(dir, "workspace"), StateDir: filepath.Join(dir, "state")}
		cc := *c
		cc.workspace = fork.Workspace
		cc.stderr = io.Discard
		x, err := cc.engine(fork.StateDir, EngineOptions{Adopt: true})
		if err != nil {
			return c.fromError("fanout", err)
		}
		engine := x.(*Engine)
		lead := "Drive this workflow run until it ends or asks for a person. Start sub-agents whenever it asks you to. Never answer a question meant for a person yourself.\n"
		if target.Gate {
			// The fork measures the answer at this person gate: its
			// coordinator gives it, as the run's person would.
			lead = fmt.Sprintf("Drive this workflow run until its person gate %s is answered. Start sub-agents whenever it asks you to. When it presents that gate, answer it yourself, choosing from its choices as the person who started the run would, with the answer command it prints. Answer no other question meant for a person.\n", target.State)
		}
		prompt := lead + toolWorkdirAdvice(fork.Workspace) + engine.workspaceCommand(fork.Workspace, engine.opts.VerbLine(run, "next", "--until-unit", target.Unit)) + "\n"
		brief := filepath.Join(dir, "brief.txt")
		jobs[i] = fanoutJob{Target: target, Fork: fork, Engine: engine, Directory: dir, Brief: prompt, Command: expandFanoutArgv(argv, fork, target.Unit, brief, prompt)}
	}
	fmt.Fprintln(c.stdout, fanoutCaveat)
	if m := started.Manufactured; m != nil {
		fmt.Fprintf(c.stdout, "Manufactured source run (wf dev manufacture, script edge %s): its world is scripted; no outcome here is a real run's.\n", m.Edge)
	}
	if f.dryRun && len(targets) == 0 {
		return exitCheckFailed // every point was left out (named above)
	}
	if f.dryRun {
		for _, job := range jobs {
			data, _ := json.Marshal(struct {
				Target  FanoutTarget
				Fork    SnapshotFork
				Command []string
			}{job.Target, job.Fork, job.Command})
			fmt.Fprintln(c.stdout, string(data))
		}
		return exitOK
	}
	// The batch's directory exists from here on, so its report is written
	// even when every fork is refused.
	if err = os.MkdirAll(filepath.Dir(out), 0o700); err == nil {
		err = os.Mkdir(out, 0o700)
	}
	if err != nil {
		return c.fromError("fanout", err)
	}
	// Prepare every fork before launching any coordinator. A fork whose
	// journal prefix cannot be relocated is refused on its own and reported;
	// any other failure leaves inspectable partial output, but can never
	// launch a partly prepared batch.
	refused := map[int]fanoutResult{}
	for i := range jobs {
		job := &jobs[i]
		if job.Target.Refused != "" {
			refused[i] = fanoutResult{Target: job.Target, Command: job.Command, Outcome: FanoutOutcome{Status: "refused", Refused: []string{job.Target.Refused}}}
			continue
		}
		fork, err := source.ForkSnapshot(run, job.Target.SnapshotSeq, job.Directory)
		if errors.Is(err, errForkUnrelocatable) {
			refused[i] = fanoutResult{Target: job.Target, Command: job.Command, Outcome: FanoutOutcome{Status: "refused", Refused: []string{err.Error()}}}
			continue
		}
		if err != nil {
			return c.fromError("fanout", err)
		}
		job.Fork = fork
		if err = job.Engine.BindFanoutTarget(run, job.Target); errors.Is(err, ErrRejected) {
			// This fork alone cannot bind its target (the plan names such an
			// offer; this is the fork's own word): it is refused and its
			// coordinator never starts.
			refused[i] = fanoutResult{Target: job.Target, Fork: fork, Command: job.Command, Outcome: FanoutOutcome{Status: "refused", Refused: []string{err.Error()}}}
			continue
		}
		if err != nil {
			return c.fromError("fanout", err)
		}
		if err = os.WriteFile(filepath.Join(job.Directory, "brief.txt"), []byte(job.Brief), 0600); err != nil {
			return c.fromError("fanout", err)
		}
	}
	var launch []fanoutJob
	var at []int
	for i, job := range jobs {
		if _, no := refused[i]; !no {
			launch, at = append(launch, job), append(at, i)
		}
	}
	reports := make([]fanoutResult, len(jobs))
	for i, r := range refused {
		reports[i] = r
	}
	for k, r := range runFanoutJobs(launch, parallel, f.timeout, startFanoutProcess) {
		reports[at[k]] = r
	}
	report := fanoutReport{Caveat: fanoutCaveat, SourceRun: run, Parallel: parallel, Timeout: f.timeout.String(), Forks: reports, Skipped: plan.Skipped, Manufactured: started.Manufactured}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return c.fromError("fanout", err)
	}
	if err = os.WriteFile(filepath.Join(out, "report.json"), append(data, '\n'), 0600); err != nil {
		return c.fromError("fanout", err)
	}
	fmt.Fprintln(c.stdout, "STATE\tUNIT\tOUTCOME\tTIME\tCHILD TOKENS\tCOORDINATOR TOKENS\tCOORDINATOR CALLS\tCHILD VERBS ATTEMPTED")
	failed := len(targets) == 0 // nothing could be forked: the report names why
	for _, r := range reports {
		fmt.Fprintf(c.stdout, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Target.State, r.Target.Unit, r.Outcome.Status, r.Elapsed, fanoutNumber(r.ChildTokens), fanoutNumber(r.CoordinatorTokens), fanoutNumber(r.Outcome.CoordinatorCalls), fanoutNumber(r.Outcome.ChildVerbsAttempted))
		if r.Target.Substituted != "" {
			fmt.Fprintf(c.stdout, "  substituted: %s\n", r.Target.Substituted)
		}
		for _, refusal := range r.Outcome.Refused {
			fmt.Fprintf(c.stdout, "  refused: %s\n", refusal)
		}
		if r.Error != "" {
			fmt.Fprintf(c.stdout, "  error: %s\n", r.Error)
		}
		if r.ExitError != "" {
			fmt.Fprintf(c.stdout, "  coordinator exit: %s\n", r.ExitError)
		}
		verbs := make([]string, 0, len(r.Coordinator.ChildVerbs))
		for verb, n := range r.Coordinator.ChildVerbs {
			if n > 0 {
				verbs = append(verbs, verb)
			}
		}
		sort.Strings(verbs)
		for _, verb := range verbs {
			fmt.Fprintf(c.stdout, "  coordinator attempted child verb %s: %d\n", verb, r.Coordinator.ChildVerbs[verb])
			failed = true
		}
		if r.Outcome.Status != "done" || r.Error != "" || r.ExitError != "" || len(r.Outcome.Refused) > 0 || (r.Outcome.ChildVerbsAttempted != nil && *r.Outcome.ChildVerbsAttempted > 0) {
			failed = true
		}
	}
	if failed {
		return exitCheckFailed
	}
	return exitOK
}

func fanoutNumber(n *int) string {
	if n == nil {
		return "unknown"
	}
	return fmt.Sprint(*n)
}
