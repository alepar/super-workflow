# go-api proposal module

Start with the [five-minute tour](examples/tour/def.go). Its numbered sections
introduce typed agent results, review revisions, parallel child workflows,
engine-run tests, bounded repairs, and a reserved human decision. Read the
[simulation tests](examples/tour/sim_test.go) alongside it: they exercise the
same definition without running commands in your checkout.

From this module:

```sh
go test ./examples/tour -count=1
go run ./cmd/workflow lint tour
go run ./cmd/workflow graph tour
go run ./cmd/wfcheck check tour
```

`wf check` is the gate before running a workflow with a model. Compilation
checks types; lint checks the declared graph; a scripted simulation checks only
its chosen path. The full check adds calibration, hostile submissions, failure
and recovery paths, gate delivery, journal relocation and edge coverage. A
passing happy-path test alone does not establish that a definition is ready.

To start it with an agent coordinator, supply the goal and suite command:

```sh
go run ./cmd/workflow run tour --param goal='Add arithmetic operations' --param suite='go test ./...'
```

Give the coordinator this launch instruction, for every harness:

> Drive this workflow run until it ends or asks for a person. Start sub-agents whenever it asks you to. Never answer a question meant for a person yourself.

Follow the printed bootstrap and child instructions. Set the shell tool's
`workdir`/`cwd` argument to the recorded run workspace for every workflow command.
Printed commands also start with `cd <workspace> &&`; the explicit tool directory
is necessary for harnesses that change directory before running the command.
Task worktrees can be removed by cleanup. The engine defers `git.worktree_remove`
while an open agent instruction or unit has an issued workspace inside its target.
Deferred removals are journaled as run cleanup, so the child settles immediately
and quorum joins can cancel unused siblings. Cleanup survives restart and runs
as soon as the workspace is unused. `status --full` lists pending cleanups; a
claimed child can keep one pending beyond run end until it reports done or failed.
This guard uses issued workspaces, not a harness process's current directory.

The tour teaches the state
machine; the [feature walkthrough](examples/feature/README.md) adds isolated Git
worktrees, independent acceptance tests, and controlled merging.

## Your own runner

The [tour development runner](examples/tour/cmd/workflow/main.go) registers the
definition, its checker fixtures, and the shell adapter it uses:

```go
adapters := wf.NewAdapters()
if err := adapters.Register(shell.Runtime(shell.Config{})); err != nil {
    panic(err)
}
wf.MainWith(wf.MainConfig{
    Defs:     []wf.Registration{checker.Registration()},
    Adapters: adapters,
})
```

Run it here with `go run ./examples/tour/cmd/workflow check tour`.
For a standalone module, copy
that file to `cmd/workflow/main.go`, then add the engine dependency. While
working from a local engine checkout:

```sh
go mod init example.com/my-workflow
go mod edit -replace github.com/alepar/super-workflow/docs/proposals/go-api=/absolute/path/to/super-workflow/docs/proposals/go-api
go mod tidy
go run ./cmd/workflow check tour
```

Replace the checker import and `checker.Registration()` with your own registration
when ready. The tour's [checker package](examples/tour/checker/checker.go) shows
how `Registration.Check` uses `wftest.CheckCommand` to supply calibration and
simulation fixtures. `wf.Main` accepts definitions, not registrations;
`wf.MainWith` preserves your registration's checker and parameter configuration.
Register each adapter your definition uses; `wf.Main(def)` alone does not
register shell or Git runtimes. The tour uses inline commands. For detached
commands, see the effect runner registration in [cmd/workflow](cmd/workflow/main.go).

## Routing and child results

Use `d.OnFailures(stopped, wf.Blocked, wf.Unavailable, wf.Uncertain)` for a
shared operational failure target. State-local routes take precedence; later
defaults replace earlier ones for the same class. `OnFailure(class, target)`
still declares a single default.

For a command, `test.RouteExit(green, red)` selects `green` for exit 0 and
`red` for every nonzero exit. Execution errors use failure routes and retry
policies. Use `Route` or `RouteCases` when exit codes need more distinctions.
Agents, computes and commands all support `Then(edge)` for an unconditional
next step; a command's `Then` also follows that edge after a nonzero exit.

`NewMap[State, Item, Key, Result]` takes the key type third (it must be
comparable), and the child's yielded result type fourth. Read joined results
in a Compute with `wf.Joined(c, build)`, then feed that compute's output to a
parent agent with `wf.Out`. `wf.Out(build)` itself is not a valid prompt input.
For the tour's `TaskResult` values, the pattern is:

```go
type Collect struct{}
collect := wf.NewCompute[Collect, []string](d, func(c wf.Ctx) ([]string, error) {
    var summaries []string
    for _, item := range wf.Joined(c, build).Items {
        // item.Key identifies the task; Result is its typed wf.Yield value.
        if item.Terminal == "TaskDone" {
            summaries = append(summaries, item.Result.Summary)
        }
    }
    return summaries, nil
})
// Route build's join to collect, then collect to the consuming agent.
// In that agent's Spec, bind:
// wf.PromptText("Review {{results}}", wf.As("results", wf.Out(collect)))
```

See the `Report` compute in [the feature definition](examples/feature/def.go)
for a complete consumer that retains both successful results and degradation
reasons. The tour's review requires one evidence row for each named criterion
(`goal` and `independent`); checks refuse missing, duplicate or empty rows, and
the engine derives the verdict from their `Met` values. Evidence must cite the
plan, but nonempty text validation cannot establish whether a citation is true.
The feature workflow's `ReviewRow` and `checkRows` extend this to requirement IDs
and covering task IDs.

Map considers items in source order and deduplicates equal keys; for a fresh
entry, the first occurrence supplies that key's child input. Locks, dependencies,
and capacity affect when children start, and parallel completion order is not
guaranteed. Joined results follow child spawn order. Use explicit unique IDs
and look up results by `item.Key`; sort a report explicitly when order matters.
The tour names its fourth type argument `TaskResult` to distinguish it from
the third, `string` key. A compiler complaint about `comparable` often means
the key and result types were swapped.

Compute callbacks return `(T, error)` even when they cannot fail:

```go
compute := wf.NewCompute[Collect, []string](d, func(c wf.Ctx) ([]string, error) {
    return []string{"summary"}, nil
})
compute.Then(compute.To(next, "collected"))
compute.Retry(wf.Retry(wf.Const(2), compute.To(stopped, "attempts exhausted")).
    OnClass(wf.Unavailable, compute.To(recovery, "read unavailable")))
```

Compute has `Retry`, not a direct `OnClass` method. Declare observations with
`Observes` and return read errors; do not perform external I/O in the callback.
Definition defaults also apply, so add a local class route only when that state
needs different handling.

## Artifacts and human review

The tour's Plan writes an allocated artifact; Review consumes it and Ship
presents it to the person deciding whether to proceed:

```go
planDoc := wf.NewArtifact[string](plan, "plan.md")
// In Plan's prompt: "Write the plan and acceptance criteria to the allocated
// plan.md artifact; do not return its path."
wf.Consumes(review, planDoc)
// In Review's prompt inputs: wf.As("document", planDoc)
ship.Presents(planDoc) // chain Answers(...) as in the tour
```

The producer's instructions contain the allocated path. Its result remains
typed JSON; it does not carry a path invented by the agent. `Presents` includes
the artifact reference in the gate instruction so the person has evidence to
inspect, in addition to the question. The tour presents its plan; a production
approval can present a report and supporting evidence from their producers.

Every gate answer that goes directly to a terminal declares its expected outcome:

```go
ship.Answers(
    wf.On(Go, ship.To(shipped, "approved").Ends(wf.Success, "shipping approved")),
    wf.On(Hold, ship.To(stopped, "held").Ends(wf.Stopped, "shipping withheld")),
)
```

`wf check` rejects a different terminal outcome or a missing declaration, including
on policy-answerable gates. Existing `Ends(reason)` still documents intentional
stop paths, but does not assert an outcome. The outcome form requires a direct
terminal; it does not predict what an arbitrary continuation will do.

If two answers intentionally share the same destination without effects, declare
`gate.EquivalentAnswers(reason)`. For example, parking and skipping a draft may
both continue to a menu while preserving the distinct answer in the journal.
Dynamic routes are not assumed equivalent. `terminal.Intent(description)` can
explain exceptional terminal semantics: a review may successfully produce a
negative report without claiming convergence. These declarations require nonblank
text and are included in the definition hash; lint does not infer intent from names.

Simulated agents must supply artifact content too. Scripted submissions use
`WithArtifacts`, with declared file names rather than paths:

```go
wftest.SubmitAt("Plan", tour.PlanOut{Tasks: tasks}).WithArtifacts(
    map[string]string{"plan.md": "Plan and acceptance criteria"})
```

For calibration and fuzzing, set `CalibrationConfig.Artifacts` and
`FuzzConfig.Artifacts` to a state-to-files map, for example
`map[string]map[string]string{"Plan": {"plan.md": "Plan and acceptance criteria"}}`.
Use the qualified state name for a child (such as `tour-task/Implement`).
Artifact contents are not generated automatically. The tour's
[checker fixtures](examples/tour/checker/checker.go) and
[scripted simulations](examples/tour/sim_test.go) show both forms.

If `check` reports an uncovered edge, first add a scenario that exercises it.
For a provably impossible route, annotate the declared edge with
`.Unreachable("specific reason")`. An inherited default needs a state-local
`OnClass` route with that annotation, on the state's retry policy for an agent,
compute or command. An absent observation declaration alone does not prove a
Compute cannot fail: its closure may return an error or attempt an undeclared
read. Keep reachable failure edges covered.


Go module `github.com/alepar/super-workflow/docs/proposals/go-api` (standard library only). It is the
generic workflow engine: packages `wf`, `wftest`, `bd`, `fsx`, `gh`, `git`, `shell`, `examples` and the root
package `goapi`. From the engine module, build the engine and exercise the
bundled tour and feature checkers:

```sh
go build ./...
go test ./examples/tour -count=1
go run ./examples/tour/cmd/workflow check tour
go run ./cmd/wfcheck check feature
```

The plugin includes the tour tests and checker inputs. The full engine test
suite and the generated output table live in the development checkout.

### Development checker fixtures

Development checkers that set `wftest.CheckConfig.ChildProtocol` use synthetic
harness fixtures from `harness/testdata` in the engine source. A checker built with
`-trimpath` or run on a host without that source can use
`WF_HARNESS_FIXTURES_DIR` to point to a copy of that testdata tree.

## Coordinator protocol

The coordinator starts a run with `workflow run <def> --param k=v` (or
`--run-id <id>` for an unkeyed definition), then follows the generated bootstrap on stderr and the loop in
[COORDINATOR.md](COORDINATOR.md):

1. Call `workflow next <run>`: it blocks until there is something, and offers what the harness can start.
2. Follow each printed instruction. Spawn a child with the printed brief;
   the brief's `prompt` command claims the unit and fetches its full prompt.
3. The child writes result JSON to the allocated output file and runs the
   prompt's `done` command. A failed child follows its printed `failed` line.
4. Recompute capacity and call `next` again. Relay human gates and follow
   recovery instructions as printed. Stop when `next` reports stopped or ended;
   quote the ended instruction in the final report.

Nothing but a person's `workflow stop`, an open person gate or the run's end
ends the loop. A command that fails prints why, and `next` runs again. A unit
whose child never fetched its prompt is offered again, with what its spawn
printed where the harness records that. A unit that failed with no route for
the failure comes back as a recovery instruction carrying the cause: restart
it or give up.

For machine readers, `workflow next <run> <n> --json` hands out at once and prints JSON;
`n` can be `all`. A named binary such as `super` puts driving verbs under `wf`
and supplies the run id. Use the command lines the engine prints.

An agent's result type is shown to the child as JSON schema and checked on
validate and done. Two string tags add form checks: `pattern:"re"` (the whole
value must match) and `maxlen:"N"` (at most N characters, counted as runes;
shown as `maxLength`). Both apply to a string, a `*string` or each element of a
`[]string`. A `maxlen` refusal names the field, its length and the limit, not
the value. Lint refuses a `maxlen` that is not a positive whole number or that
sits on a field of another type. A recursive type shows its caps only at its
first level, though every level is checked.

Agent instructions allocate an absolute `output` JSON file under
`<state-dir>/runs/<run>/out/`. The brief and prompt name that same file; write
it, then run `done <run> <unit>` without `--output`. An optional `--output`
must normalize to the allocated path; stdin and unrelated files are rejected.
A result file that is not JSON is refused, with one tolerance: a file that is
exactly one fenced code block (a ```` ``` ```` or ```` ```json ```` first line, a
```` ``` ```` last line, only whitespace outside) around valid JSON is parsed from
inside the fence, and the `submitted` record carries a `Normalized` note.
Prose outside the fence, another info string or a second block is refused.
Only parent directories are created, and replacement units get fresh paths.
Programmatic `Submit` and `Done(Submission)` retain their raw submission
behavior. Exit codes, instruction fields and flags are documented in
[COORDINATOR.md](COORDINATOR.md) and the CLI's `--help`.

### Opt-in paged prompt delivery

Start a new run with `--paged-prompts` to trial engine-delivered prompts. The
choice is recorded in the run and inherited by successors; existing runs keep
legacy delivery. The child follows the first `prompt` command, then each printed
continuation separately. Pages may be fetched in any order and at any pace; the
continuation code a page prints is accepted and not checked. Re-fetching a page
is idempotent.

The adapter bounds each complete response by bytes, physical lines and columns:
6 KiB where the route is known to allow it, a 3 KiB fallback, at most 150 lines
and 100 columns. Smaller configured output limits lower the envelope. Unicode
characters conservatively count as two columns. Impossible framing is refused.
The identity, goal, required-reference manifest and finishing commands appear
on the first and last pages. `prompt --json` is unavailable in this mode.

Ordinary declared input values are rendered as readable text, with JSON indented
without changing numeric values. Their exact originals are kept in the packet
journal and under the run's `prompt-inputs/` directory. Values larger than four
reference pages become required indexed references fetched with `wf read <ref>
--page N`; definitions can explicitly use `wf.Reference(input, true)` or
`wf.Reference(input, false)` for required or optional material. A path-valued
input remains a path value; paging does not infer files from arbitrary strings.

`validate` and `done` do not count fetched pages: the child is told to read
every required page, and its `done` is trusted. Coverage belongs to one run, unit, attempt, child and
immutable packet digest. Status, explain and reports say **pages fetched**, never
read: CLI output completion cannot establish what survived harness truncation,
entered the model context, or was understood. The legacy transcript file-read
check is not used for paged runs. Page-budget checking and the cross-model trial
are separate follow-ups; this switch does not change the default.

## A complete feature workflow

Start with [examples/feature/def.go](examples/feature/def.go). It is a complete workflow:
a typed design, an independent design review, a Map of implementation tasks,
a suite/quality repair loop, independent executable acceptance, a computed report and a reserved human merge gate.
The [walkthrough](examples/feature/README.md) explains the contracts, failure
routes, worktree ownership and how to start a run. No tracker or client package
is required.

From this module:

```sh
go run ./cmd/wfcheck check feature
go run ./cmd/workflow lint feature
go run ./cmd/workflow graph feature
```

Each agent has a typed JSON result. Review results contain complete evidence
rows; the engine computes their verdict. Named enum types reject unknown values;
`RouteCases` declares each alternative. Ordinary revision edges are separate
from retry budgets: retries handle unusable attempts, while `MaxVisits` bounds
valid design and code revisions. `OnFailure` supplies operational failure
routes across the definition, including failed edge effects.

Artifacts use `NewArtifact` without a path. The engine allocates the producer's
path, and consumers declare `Consumes`. Across a Map boundary, pass a typed
`wf.Ref` in the child input. `PromptText` binds typed inputs explicitly through
`wf.As`; a run parameter with the same name is not a substitute for `Out`.

A command or effect belongs in the declared graph. Compute and check closures
read external facts only through declared observations; they do not run shell
commands themselves. Task merges are incoming-edge effects of a lock-holding
build state, so both the merge and its verification are serialized.

The simulations execute the actual definition. `SubmitAt` names a state;
`WithArtifacts` writes its allocated files. A fake world supplies adapter reads
and failures, while a real-Git test verifies workspace, artifact and merge
bindings. The full journal is printed when a script diverges.

Scripts test the routes you thought of; `wftest.Fuzz` tests the rest.
The feature checker drives the same definition with seeded agent results:

```go
result := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: seed, MaxSteps: 96, Valid: valid})
coverage.Add(result) // coverage := wftest.NewFuzzCoverage(factory, unreachable)
```

The factory returns a fresh Sim and World on every call. At each agent turn
the driver submits, refuses with malformed output, fails transient or
blocked, or ends the child without done; at a human gate it gives one of the
declared answers, or stops there one time in four; a submit uses a known-good output
from `Valid` (taken from your scenario fixtures, never written to pass a
check) half the time and an output drawn from the state's JSON schema
otherwise. A mixed seed that reaches its step bound with pending agent work
in a new state/counter context after accepted progress in the same instance
reports `End: "budget"` and `BudgetReason`; `Check` records this as
`not explored (budget)` in `NotExplored` and `CoverageProblems`. This is
incomplete exploration, not a successful run. Repeated completed contexts
remain loop findings, and all-good liveness bounds remain strict. Completed
seeds must end at success, a human gate or a named failure, with no unit left
running, one producer per allocated
artifact, a journal that folds to the same state, and a fix line on every
refusal. A read the World cannot answer is a `fixture-gap` finding, never a
pass; `wftest.UnknownFails(w, adapter, op, msg)` declares that such a read
fails as the real adapter would (a ref git cannot resolve). An output that
reaches an unresolved placeholder or an undeclared read is a
`definition-error` finding. A stop decided by a computed state takes its
cause from upstream: a named agent failure, budget stop or child halt
routed into it explains it, an accepted agent output does not. `FuzzCoverage.Problems` fails the suite when no seed reaches success
or a human gate, or an agent state is never reached and not marked
unreachable with a reason. A finding prints as
`fuzz: <rule> <def>/<state> [edge "<label>" -> <target>] · <why> · seed <n>`
with the concrete actions and the named journal; `result.Minimize(factory)`
deletes actions while the finding still reproduces, and the resulting script
replays with `result.Replay(factory)` or as `FuzzConfig.Prefix` in a
regression test.

For smaller focused patterns, see [examples/README.md](examples/README.md).
[critique](examples/critique/def.go) remains a compact review loop if you want
to learn agents and gates before adding commands and child workflows.

## Probe states from a recorded run

Start the golden run with `run --snapshots`. `dev fanout` restores an independent
world at each selected agent offer; older runs cannot supply historical worlds.
The default `--states distinct` selects the latest visit per qualified state;
`all` selects every visit (latest attempt), and a comma-separated state list
selects particular states. Missing or unavailable checkpoints fail explicitly.

Run-owned worktrees may live beneath the workspace or an external state directory.
Definitions store portable run-owned locations as `wf.RunPath("worktrees/task")`;
use `wf.RunPathOf(ctx, absolutePath)` to opt an existing run-owned artifact path
into that contract. Agent prompts receive resolved paths. Effect intents and
whole command arguments retain typed paths until execution, so restore and crash
recovery use the fork's run directory. Resolution rejects existing symlinks that
escape the run directory. Ordinary strings are never inferred to be paths, with one
exception: a fork rewrites an untyped run parameter that names a source root as a
path (the root or a path under it, anywhere in the string, such as a shell
command's operand) to the same path under its own root, and its `rewound` record
names each parameter it rewrote (`Relocated`). Any other untyped absolute
operational path beneath its source roots, or a spelling of one that rewrite
cannot map, refuses the fork.
What a command printed is history, not a path declaration. The acceptance
that routes a command's own result (its `RunResult`) is not checked, just as an
observation's value is not. The exception is `Output`, the engine's own
full-output file, which moves with the fork. What an agent wrote is history
too: its submission, the acceptance that decides it, and the values a
definition derives from them (a Map's child inputs and results, a Compute's
value), even when the prose cites the source workspace.

So the two checks split the work. At fork time, the relocation check reads
the typed paths (`wf.RunPath`, path parameters, owned path markers) in every
record, and the untyped strings only in engine-owned records: run parameters
(as the fork will rewrite them), the args of an effect still open at the
checkpoint (one with a result or a settlement is history), cleanup args, edge
plans, and an acceptance that is no agent's and no command's. At bind time, in the fork, the source-root refusal below guards
every operational use of untyped history. A state that keeps a path in its
output for later steps must type it (`wf.RunPathOf`) for a fork past it to use
it.

A fork records its source roots, and what history carries is checked again
when it is used. If a value bound into a command's argv, dir or stdin, an
effect's args, an observation's or `DoneWhen`'s descriptor, or an agent's
workdir contains a source root anywhere, that step is refused, naming the field
and the root. Nothing is rewritten, so a fork never acts on its source's world.
The fork's own roots pass, even where they lie below a source root. A typed
path is checked at its resolved target, not by its marker: a `wf.PathTemplate`
bound after the fork to a source path resolves as external and is refused. The
same check runs before the engine opens a file for a step (a prompt file, an
artifact a `wf.Read` loads), on the path as bound and on its final target with
links followed, so a link inside the fork cannot reach the source.

Prompts follow one contract. History interpolated into a fetched prompt is
citation text, what the agent reads, and it is not checked. A prompt input that
gives the agent a path to act on must be typed: an artifact input, a path
parameter, a path marker in a document (`wf.RunPath`, `wf.PathTemplate`), or a
`wf.PathTemplate` inside a `wf.Bound` input, whole or embedded in its prose (the
marked path runs to the next whitespace). Typed inputs go through the same
check, and a source-root target refuses the prompt.
A command's full output (`Output`) is carried into the fork at its relocated path.

Declare path-valued parameters with `d.Param("driver").Path(wf.WorkspaceRelative)`
or `wf.RunRelative`. Relative inputs use that base; absolute inputs inside the run
or workspace are recorded relative to their owning root. Paths elsewhere in the
state directory are refused, while external absolute paths remain pinned.
`Ctx.Param`, prompts and command execution resolve recorded ownership against the
current roots. Whole argument placeholders such as `"{driver}"` retain a typed
reference in durable command and effect intents; embedding one inside a larger
string produces an ordinary string. Replay, successors and snapshot forks preserve
recorded ownership even if declarations change. Old snapshot headers can normalize
explicitly declared parameters, but other untyped historical paths still refuse
relocation. Empty supplied path parameters are invalid; ordinary parameters keep
their existing string semantics.

Use `wf.PathTemplate("{records}/review-{round}.json")` for a composed path in
command or effect arguments. The string-returning helper fits existing argv
builders; appended `#` fragments and `strings.ReplaceAll` substitutions remain
supported. Binding records the resolved suffix and owning root, so a later fork
does not re-evaluate the original template. Empty optional templates stay empty.
Prompt references and `wf.Bound` prose render the paths without authoring tokens.

A bound value that a prompt places in a command the agent runs should be
`wf.ShellBound("WORKTREE", "{workspace}/{dir}")`. Its value is the bound text as
exactly one quoted shell word (`sink.Quote`), so `cd [WORKTREE]` stays one
argument whatever the path holds, in prose, a code span or a fenced block, and
paged delivery presents it as bound. Text no prompt line can carry as one word
blocks the state: a control character (a line break, a tab, NUL), a backtick or
invalid UTF-8. A shell value is never paged, so keep it small.
Put the token in the command as a bare word, not inside `"..."`, and write `--`
before it where a leading `-` must not read as an option. `wf.ShellWords` quotes
each element of a list separated by whitespace (pathspecs an agent wrote, for
example: `git add -- [FILES]`), and text with no elements binds empty. Both bind
like `wf.Bound`, path templates included. In a fork, the whole bound text is
checked like a command argument. `sink.Command` renders a command line in text
the program writes whole, such as a report: an inline code span for one line,
and a fenced block when the command holds a line break.

`{scratch}` is a step's scratch directory: `<run dir>/scratch/<qualified
state>-<digest of the full qualified state and the instance id>/`. The engine
creates it when it binds the placeholder, so a step never runs `mkdir` itself.
It is the same for every attempt and revisit of a state in one instance, and
distinct per instance. It lies outside the run data a checkpoint captures, so a
fork never copies it and binds its own. It lives until the run directory is removed: nothing cleans it
automatically. It needs a run directory. `scratch` is reserved: it cannot be a
run parameter, and no output field or prompt input named scratch stands in for
it.

`{predecessor}` is the run id of the ended run this run succeeds, and empty for
a run that succeeds none. It always binds. A param or output field of the same
name takes precedence, as it does for every builtin.

Compute outputs, Sub/Map inputs and terminal results encode paths in actual
`wf.Ref[T]` values automatically, preserving the public `Path string` and `Hash`
fields. Ordinary JSON marshaling outside the engine keeps its existing format.
For a plain string field, wrap the producer value with `wf.PathTemplate(path)`
explicitly. Context reads return resolved strings; wrap a plain path again when
returning it from another producer. External reference paths remain pinned, and
ordinary string fields receive no path inference.
Use `wf.ResolvePath(ctx, wf.PathTemplate("{integrationWorktree}"))` when a
compute or check needs the current absolute location, for example to compare an
absolute reference with repository-relative Git status paths.

For tests that inspect operational values, use `trace.DecodeValue(raw, &value)`
instead of unmarshaling a path-bearing payload directly. `trace.Journal()` still
returns the original markers for wire-format assertions. Recorded checks retain
their producer sequence and the path roots of the view they observed; harvested
fixtures can reconstruct that view after the original directories are removed.

At `validate` and `done`, result strings containing the state root are refused;
artifacts already have engine-owned paths. A whole absolute path beneath the
workspace is also refused: return it relative to the repository root. Prose
mentioning a workspace file is allowed. These refusals leave the attempt open
so the child can correct its result in place. Offline `ReplayWith` still feeds
historical recorded outputs unchanged; this exemption is private to its isolated
replay engine and does not apply to live submissions.

```sh
./workflow dev fanout GOLDEN --state-dir /absolute/golden/state \
  --out /absolute/new-probes --parallel 2 --timeout 15m \
  --coordinator-cmd '["/absolute/launch-coordinator","{brief}"]' --dry-run
```

`--coordinator-cmd` is a JSON argv array, not shell source. Placeholders are
`{workspace}`, `{state}`, `{run}`, `{unit}`, `{brief}` (a generated prompt file),
and `{prompt}` (its contents). Substitution preserves argument boundaries and
does not expand shell expressions. Each command runs in its fork workspace.
Dry-run prints paths and commands without creating forks or starting processes.
Remove `--dry-run` only when ready to run the chosen harness. `--parallel` is the required per-invocation limit obtained from the launched
harness. Arbitrary wrappers do not expose their effective harness/profile to the
engine, so it never borrows the historical run's limit or a static slot table.

Every generated brief starts with the standard launch instruction above and a
`next --until-unit <name> --adopt` command. Fanout durably binds the boundary
before launch: subsequent calls cannot clear it by omitting the flag or running
`resume`. It allows only that attempt, then returns a stopped instruction before
successor effects. It does not claim the whole workflow ended successfully.
All forks are prepared before any coordinator starts; preparation failure leaves
inspectable partial output. A wall-clock timeout records an engine stop, kills
the coordinator process group, and reports `timed out`. If the command holds the
journal lock, stop is retried after killing it. Processes that deliberately
escape their process group are outside this cleanup contract.

Hermes example using a launcher that selects a profile for the run:

```sh
./workflow dev fanout GOLDEN --state-dir /absolute/golden/state \
  --out /absolute/hermes-probes --parallel 2 --timeout 15m \
  --coordinator-cmd '["/absolute/path/to/launch-hermes.sh","{brief}"]' --dry-run
```

Codex noninteractive example. Set the profile and project configuration for
the intended run. Fanout uses that profile's permissions, model configuration,
and trust settings.

```sh
./workflow dev fanout GOLDEN --state-dir /absolute/golden/state \
  --out /absolute/codex-probes --parallel 2 --timeout 15m \
  --coordinator-cmd '["env","CODEX_HOME=/absolute/path/to/codex-profile","codex","exec","-C","{workspace}","-m","gpt-6.1-terra","-s","workspace-write","--add-dir","{state}","{prompt}"]' --dry-run
```

The table and `report.json` use fork journals and resolved transcripts, never the
coordinator's stdout. Outcomes are `done`, `refused` (with texts),
`ended-without-done`, `stalled`, and `timed out`; launch/read/cleanup errors are
reported separately. Elapsed time is measured for each coordinator. Token counts
and coordinator tool-call/child-verb counts are `unknown` when evidence is absent
or unsupported, not zero. Claude/Codex coordinator JSONL is supported for call
counts; Hermes coordinator call counts are currently unknown. `coordinator.log`
is retained for diagnosis. Exit 8 indicates a finding; exit 0 means all selected
probes returned done without reported errors/refusals or observed coordinator
attempts to call child-only verbs. Unknown evidence remains unknown even on exit 0.

This is a defect detector, not an estimate of workflow acceptance. A full run
remains the acceptance test. The automated checks use dry runs or simulated coordinators.

Reserved gates may declare an optional integer on an answer with
`gate.IntValue(answer, min, max, proposal)`, and a named dynamic lower bound with
`gate.IntMinimum(answer, name, minimum)`. Both callbacks read `wf.Ctx`. The gate
shows the proposal and effective range; `--number N` overrides the proposal while
`--value` remains the answer choice. The engine validates before journaling and
`gate.AnswerValue(c)` reads the latest accepted integer in that instance.

## License

Licensed under either of Apache-2.0 or MIT, at your option (SPDX: `MIT OR Apache-2.0`). See [LICENSE-APACHE](LICENSE-APACHE) and [LICENSE-MIT](LICENSE-MIT).
