# Feature: the reference workflow

`feature` takes a goal and a shell suite command in a clean Git checkout on a
branch. Its definition is in [def.go](def.go). It uses only the engine and its
Git and shell adapters; it has no task tracker or Superpowers dependency.
The target is a Go module: readiness includes formatting, vet and an external-package test.

## Read the graph

1. `Setup` records the base commit/branch and creates an integration worktree
   under the run's worktrees directory, beside records. `workdir` in its computed output binds
   subsequent agents to that checkout. The original checkout stays untouched.
2. `Design` writes the engine-allocated `spec.md` and returns a short spec plus
   tasks (`id`, `title`, `files`, `acceptance` as a nonempty list). Paths must be local and disjoint (a directory owns its descendants; comparison
   is case-sensitive, matching the Linux target);
   the reviewer must also check that tasks can be implemented independently.
3. `DesignReview` returns one evidence row per goal requirement. The engine
   checks completeness and computes `ok` or `revise`. After the initial design there are
   at most two revisions. At the cap, the last design proceeds and its open
   review reasons remain in the final report's `Degraded` list.
4. `PrepareTasks` passes each task a typed spec reference and its own worktree
   path. `Tasks` maps those inputs into the child definition. Each child starts
   from the recorded base commit, implements and commits, runs the suite, and
   gets a fresh-context review with one evidence row per acceptance criterion. The first implementation plus two repairs is
   the limit; an unapproved task never merges.
5. A successful review merges the submitted commit SHA into integration on an
   edge entering `Merge`. The state holds the merge lock through both that
   effect and its build check. A red build keeps the reviewed merge and records
   an integration-check qualifier; it does not blame or stop the task. The Map
   waits for all task merges, then hands integration verification to `Suite`.
   A conflicting merge is aborted, then `ResolveTaskMerge` merges integration
   into the task branch and resolves the conflict. The engine reruns the task
   suite and independent review before retrying the integration merge. After
   two resolution attempts, a reserved `Recovery` gate asks a person to inspect
   the cause in the journal and retain the branches.
6. `Verify` records the clean integration commit. `Suite` runs on that result,
   then `Quality` requires clean
   `gofmt -l .` output and successful `go vet ./...`, even if the supplied suite
   is just `true`.
7. `GoalCheck` gets only the original goal and integrated checkout in fresh context.
   It writes independent black-box Go tests to the allocated `goal_test.go`
   artifact and returns `{}`. `RunGoalCheck` builds its argv from the human-owned
   `goal-check-command` run parameter (default `go test -count=1 -json`) and
   appends the allocated file path. A build failure (the Go test JSON `FailedBuild` field) returns to `GoalCheck`
   with compiler diagnostics; failed tests or zero executed tests enter `Fix`.
   A second build failure on the same product version enters `Fix`, since the
   public API may be missing or unexported. A repaired product gets a fresh
   authoring retry; there are at most six GoalCheck visits in the run, shared
   by product repairs and base-conflict resolutions. Tests must use an external `package <name>_test` and import the
   module's public API. Go test JSON must record run and pass events. No validator
   reads test source.
8. Red suite, quality or acceptance checks enter `Fix`, with at most two repair
   commits across all these checks. Every repair reruns the suite, quality and
   independent acceptance. Repair commits have no code review, so the report
   records "unreviewed integration repair commits; human review required".
   Fix submissions are refused if `git diff` from the pinned pre-repair commit
   adds or changes `goal_test.go`, including under a subdirectory. This covers
   all commits made by the repair; the allocated test must stay outside the
   product checkout.
9. `Report` computes built tasks, readiness, open design reasons and the exact
   integration commit. Acceptance submissions must leave that commit and checkout
   unchanged. After Suite/Quality, `Snapshot` fingerprints ignored-file names and contents with SHA-256.
   Acceptance may not add, remove or rewrite ignored files; Report checks the set again
   after running the artifact. Existing ignored build output is allowed, but ignored `.go` files are
   refused because they affect compilation without entering the merged commit.
   Fingerprinting uses Bash and `sha256sum` on the Linux target. Missing
   work, mutations during verification or exhausted checks cannot report ready.
   Dirty checkout or detached HEAD preconditions stop immediately with their
   concrete cause, without spending retries.
10. `Approve` is a reserved human gate. Inspect the report and diff; `merge`
    merges the recorded commit into the original branch, and `keep` preserves
    the worktrees without changing base. No answer for 24 hours also keeps them.
    A failed base merge enters `ResolveBaseMerge`, which merges the current base
    into integration without editing the original checkout. The repaired commit
    gets full verification and fresh human approval; earlier acceptance cannot
    authorize it. After two resolution attempts, `Recovery` needs a person.

Worktree creation failures also enter `Recovery`: inspect the journal for the
failed path and cause, fix stale paths, worktree locks or disk space, then start
a new run. Choosing `keep`, or leaving recovery unanswered for 24 hours, stops
the run and retains the worktrees. No operational failure claims success.

All agent steps require fresh context, including both reviews. The engine's
coordinator protocol determines how to launch them; the workflow sets no
harness concurrency cap. The suite command must include the project's build
and acceptance tests and be safe to repeat after a crash.

The `Report` accepted record is in `workflow journal`; `status` also exposes
engine-level degradation such as escalations and orphan recovery. The report's
`Degraded` field describes workflow review/task outcomes, not that separate
operational history. A `degraded` verdict is qualified, never a clean `ready`.

## Evidence contracts and independent tasks

Write **one requirement per nonempty goal line** for the strongest completeness
check. Trailing whitespace (including CR from CRLF files) is removed; indentation
is preserved. Lines are numbered
`g1`, `g2`, etc.; blank lines are ignored. A one-line paragraph remains one
compound requirement: the engine does not infer semantic clauses. Task
`acceptance` is an explicit string list, numbered `c1`, `c2`, etc.

Each review prompt starts with the engine's table of IDs and requirement text,
above the output schema. Reviews return `{"rows":[...]}`; each row contains
`id`, `met` and `evidence`. Design rows also name covering task IDs in `tasks`.
A met design row must name at least one covering task. Empty inventories are
refused. The engine also refuses missing, duplicate or unknown row IDs and unknown task IDs;
its refusal lists the expected IDs with their text and the missing or unknown
IDs. It does not match copied prose. There is no agent-supplied verdict: any
failed row selects revision or repair.

Checks refuse only on typed fields and commands the engine runs. They do not
inspect agent-written code or prose for semantic correctness. Reviewers should
cite code, tests or spec sections and observed results; the engine cannot prove
that this evidence is truthful or that tests cover every requirement.
`GoalCheck` supplies only the test artifact. The engine runs the configured
command and checks its exit status and JSON execution events. No agent output
field is ever executed. Methods, zero-value types and composite literals need
no special validator support. Fix receives the allocated artifact path and engine command output, including
stderr. The optional `goal-check-command` parameter is whitespace-separated argv:
quotes, shell operators and glob expansion are not supported. Its command must
accept the appended Go test file path and emit Go test JSON events. CLI registration
supplies the default; callers using `Definition()` directly must supply this run
parameter. Setup rejects a missing command or a whitespace-only goal before
creating worktrees. Run parameters take precedence over agent output fields, so
those fields cannot replace the configured command.

Execution events are not proof that the check is honest. For example, a
`TestMain` can print fabricated run/pass JSON without calling `m.Run`. This
workflow does not inspect the test source to prevent that; human review must
assess the artifact as well as its reported result.

Tasks deliberately start independently from the same base and own disjoint
paths. Shared helpers may therefore be duplicated within those paths. This
example does not schedule task dependencies; keep a mutually dependent change
in one task. The design reviewer must catch invalid partitions.

## Run it

Build the binary in this module, then use an absolute path to it from the
fixture repository. Keep the state directory **outside** the product checkout,
so its records and worktrees do not dirty that checkout.

Add `--snapshots` to retain world checkpoints for `dev rewind --out` and
`dev fanout`. The definition records integration, task and spec locations as
`wf.RunPath` values, so restored runs use their own state directory. Older feature
runs that recorded absolute locations cannot be relocated by guessing what their
strings mean; start a new snapshot-enabled run with this definition.

```sh
nice -n 19 ionice -c 2 -n 7 go build -o ./workflow ./cmd/workflow
./workflow lint feature
./workflow graph feature
/path/to/workflow run feature --workspace /path/to/tinycalc \
  --state-dir /path/to/feature-state \
  --param 'goal=@/path/to/goal.md' \
  --param 'suite=go test ./...'
```

Keep `goal.md` outside the fixture checkout, with one requirement per line.
`run` prints only the run ID on stdout and a complete coordinator loop on stderr,
with the actual executable, run and state directory already filled in. Follow
that generated loop and the
[coordinator protocol](../../COORDINATOR.md). Agents write result JSON and
artifacts only to the allocated paths in their prompts. Do not manufacture
artifact paths or substitute an unrelated result file. Design returns only task
contracts; the engine records `spec.md` separately and passes its reference to
reviewers and task workflows. Agent results must not duplicate artifact paths.
The `agent-output-path` lint rejects typed `wf.RunPath` result fields and result
fields matching an `wf.ArtifactPath` prompt binding.

The final gate always belongs to the human. An unattended evaluation should
stop with a ready report at this gate; it must not invent human approval.
After approval or `keep`, the same goal run key starts a successor with a new
run id and new branches. This example starts its design again against the
current base; it does not silently reuse an unmerged predecessor's tasks.
Inspect retained branches before starting a successor after `keep`.

Worktrees intentionally remain available for inspection at the human gate and
after a stop. Once the work has landed and been inspected, remove this run's
worktrees with `git worktree remove`, then remove its task/integration branches.
Never delete the run directory while it still contains registered worktrees.

## Validate without a model

```sh
go run ./cmd/wfcheck check feature
```

The simulations cover ID completeness and diagnostics, prompt inventories, computed verdicts, command failures,
revision caps, approval and immutable approval commits. Real-adapter tests run
temporary Go repositories through worktrees and base merge, including methods and
zero-value types. They also prove
that dirty formatting and a vet error force repair even when the supplied
suite passes. An unexported API first returns to the test author, then enters product repair
when the external test still fails to build. An internal-package test can be corrected without product
repair; an assertion failure still enters Fix. Ignored output created by the suite is
allowed, while ignored files added, removed or rewritten by acceptance are refused. The
64-seed fuzz test reaches every agent state and checks hostile submissions and
recovery paths. All examples run `LintSound`; intentional stops carry `Ends`
reasons, and repair/review cycles have bounded visits.

Evaluate model behavior separately by running the workflow with a real model.

`go run ./cmd/wfcheck check feature` (from `docs/proposals/go-api`) uses the
separate development checker binary and also calibrates the declared checks against the
addition scenario and runs the randomized property-test driver with liveness
and edge coverage. It runs no model or real checkout commands. A nonzero exit
reports findings; it does not mean the scenario has been accepted by a model.
The checker currently reports uncovered failure/gate edges and stopped runs
that need author review. Calibration reports accepted one-field mutations as
`not checked`, so the author can decide which fields require validation.

To retain accepted results from a real run for a scenario's corpus:

```sh
./workflow dev harvest RUN_ID --state-dir /path/to/feature-state --into ./corpus
```

`wf.LoadCorpus` reads those entries, including source run/sequence and an
optional prompt hash. Supply their outputs under the matching state keys in
`wftest.CalibrationConfig.Valid`. Designate the forward-progress subset in
`Progress`; accepted negative reviews belong only in `Valid`. Use the original requirements, prior outputs,
and observed world in the scenario factory. Results from different goals or
worktree revisions are not interchangeable fixtures. Legacy journals have no
prompt hash; an absent hash does not establish freshness.
