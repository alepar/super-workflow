# Changelog

## v0.4.12

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.12`. GitHub installs use the plugin tag
`plugin-v0.4.12`. This release caps a CLI process's heap softly: 1 GiB, raised
to four times the journal it reads, unless GOMEMLIMIT is set. On a 217 MB
journal a child verb peaks at about 1.2 GB instead of 3.5-4.3 GB. No API,
journal format, hash or lock change.

### What changed

- **A CLI call caps its heap softly (sw-0kdy).** `TuneGCForCLI`, which
  `MainWith` calls, keeps the collector at GOGC 400 and now also sets a
  soft memory limit of 1 GiB. Each journal the call reads raises the limit
  to 4 times the file's bytes when that is more, so a long journal never
  keeps the collector running. On a 217 MB journal a child verb's peak RSS
  falls from 3.5-4.3 GB to about 1.2 GB for about 4% more CPU; a small
  journal's calls are unchanged.

### Upgrading

- No API, journal format, hash or lock change. A `GOMEMLIMIT` in the
  environment is kept, and then no journal read moves the limit; a `GOGC`
  in the environment is kept as before. A binary whose main calls `CLI`
  without `TuneGCForCLI` is unaffected. A process that already set a lower
  limit keeps it.

## v0.4.11

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.11`. GitHub installs use the plugin tag
`plugin-v0.4.11`. This release folds the journal before taking the run lock
and folds only the records appended since under it, so concurrent verbs on a
large journal no longer queue for minutes. No API, journal format, hash or
lock-semantics change.

### What changed

- **A verb folds the journal before it takes the run lock (sw-989).** Each
  lock acquisition used to read and fold the whole journal under the lock.
  On a 217 MB journal that held the lock for 1 to 2 s per acquisition, and
  a verb takes it 2 to 8 times, so concurrent children queued for minutes.
  A verb now folds the records committed so far with no lock. Under the
  lock it folds only the records appended since, onto that fold, after
  checking that the records it built on are still the journal's prefix. A
  rewritten or shortened journal, or a changed definition, is folded whole
  under the lock as before. The process's journal read cache is now shared
  by every journal value on the same file, so a verb that builds more than
  one engine (`next`, `heartbeat`) no longer parses the whole file again for
  each.

### Upgrading

- No API, journal format, definition hash or run hash change, and the lock
  semantics are unchanged: the state a verb acts on under the lock is still
  the fold of exactly the records it read under the lock. A verb now spends
  the fold's CPU before it waits for the lock, so a call that waits holds a
  fold it may extend. Journals that wrap a `wf.Journal` can pass a lock-free
  read through by implementing `ReadUnlocked(wf.RunID)
  ([]wf.JournalRecord, error)`; the engine only folds and compares the
  records it returns, which may share their payloads with the journal's
  cache. Without it, the wrapper is folded under the lock as before.
  `wftest.CrashJournal` passes it through.

## v0.4.10

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.10`. GitHub installs use the plugin tag
`plugin-v0.4.10`. This release makes Codex offers explain "agent thread
limit reached", keeps the bound a lock wait prints, and names a lock holder
that never appends. No definition or run hash changes. Read Upgrading.

### What changed

- **Codex offers say what "agent thread limit reached" means (sw-zwao).**
  Codex refuses `spawn_agent` with that text only while the session's
  running children fill its thread cap. It evicts finished children by
  itself and has no tool to close one. A child's Codex turn can run on after
  its `done` frees the unit's place in the engine, so `next` can offer
  before Codex has room. Each Codex offer of native spawns, including a
  re-offer, now prints one line under its header: do not end the turn on
  that refusal, run `next` again, and the unit comes back marked not started
  yet. The line is guidance; the engine does not read the refusal. A new
  optional adapter interface, `harness.SpawnNote`, carries it, and offer
  sizing counts it. Other harnesses print nothing new.
- **A lock wait keeps the bound it prints, and names a quiet holder
  (sw-ex2v).** Where the harness reads no tool limit (Codex) and no
  `--timeout` is given, `next` now waits for the run lock within its own
  default wait instead of up to 2 minutes per acquisition, so the bound its
  progress line prints is one the call keeps. Every acquisition that waits
  5 s prints its own line, which now says it is the most this call waits for
  the lock. For `next`, the line adds that the wait is not a stop: do not
  interrupt it, and past that bound run status, then next again. Each
  acquisition now writes its holder record when it takes the lock, not at
  its first append, so a call that holds the lock without appending is
  named to its waiters. A lease that appends nothing puts the lock file back
  as it found it on release.

### Upgrading

- No API break. A Codex coordinator's `next` output gains one line per
  offer, so an offer near the output limit may carry one unit fewer. Tools
  that match Codex offer text exactly see the new line after the header.
- No API break. On Codex, a `next` under a busy lock now answers "nothing
  yet" after about 100 s instead of waiting up to 2 minutes per lock.
  Tools that read the lock file during a hold now see the current holder's
  record, also for a call that has not appended. The record is gone again
  once that call lets go.

## v0.4.9

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.9`. GitHub installs use the plugin tag
`plugin-v0.4.9`. This release adds `wf dev heartbeat died`, an operator form
that releases a gone child's claim as an orphan without spending a retry, and
teaches `wf.Reject` as the default for a late judgement in the guide and the
authoring skill. No existing behavior changes.

### What changed

- **Docs: `wf.Reject` is the default for a late judgement (sw-ev2g).** The
  engine guide and the authoring skill now teach Reject for a compute or
  check that finds an upstream agent's output wrong. Faults the agent cannot
  fix, such as a failed read or a missing tool, keep a plain error or
  `wf.Unavailable`. A check on the submission is still preferred when the
  data exists then. No API or behavior change.
- **`wf dev heartbeat died` releases a gone child's claim from a plain shell
  (sw-d964).** A claimed unit whose child is gone stays running when no
  coordinator is left to answer its stall. A stop that drains, or a run
  waiting to resume, then keeps it until someone answers. Plain `heartbeat` is
  a coordinator verb and is refused outside the harness that drives the run.
  The new operator form `wf dev heartbeat died <name>` or
  `wf dev heartbeat died --session <id>` works from a shell with no agent
  harness markers, like the other `dev` forms. It loses the claim as an
  orphan: it spends no retry, never counts toward the lost cap, and is
  offered again to the next coordinator. The operator attests that the child
  is gone. `dev heartbeat active` is refused, and so is the form from an
  agent harness.

### Upgrading

- No change for existing callers. Operator tooling that used
  `dev failed --class transient` to clear the claims of a gone coordinator
  can use `dev heartbeat died --session <id>` instead, which spends no retry.

## v0.4.8

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.8`. GitHub installs use the plugin tag
`plugin-v0.4.8`. This release makes `run --options` overlay the run's options
instead of replacing them: fields a file leaves out keep the registration's
defaults (or the predecessor's or journaled values). Read Upgrading.

### What changed

- **`--options` overlays the run's options instead of replacing them
  (sw-a5s6).** `run --options file` (and a manufacture script's `options`)
  is decoded over the options the run would otherwise use: the
  registration's `DefaultOptions` for a fresh run, the predecessor's options
  for a successor, and the journaled options for a resume. A field the file
  leaves out keeps that value; a field it gives wins, `false` and `""`
  included. Undeclared fields are still refused. `run` prints the effective
  options on stderr as `run <id> options: <json>`.

### Upgrading

- A partial `--options` file that relied on the zero value for the fields it
  left out now gets the registration's defaults (or the predecessor's or
  journaled values) for them. Name those fields explicitly to keep the old
  values. A resume with a partial file that agrees with the journaled
  options now attaches instead of being refused as different options. A
  definition with no `DefaultOptions` behaves as before on a fresh run.

## v0.4.7

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.7`. GitHub installs use the plugin tag
`plugin-v0.4.7`. This release makes send-back automatic: a compute or check
that returns `wf.Reject(...)` sends the work back to the agent whose output it
read, once by default, with the reasons and the prior output attached by the
engine, then fails over as before. Nothing changes for a definition that does
not raise `wf.Reject`. Lint `send-back-unread` is removed. Read Upgrading.

### What changed

- **Automatic returns: a compute or check that rejects an upstream output
  sends the work back to its producer (sw-5blp).** A compute body or a check
  returns `wf.Reject(reasons...)` in place of a plain error. With no edge
  declared, the engine returns the work to the one agent whose output that
  call read through its Ctx (`OutputOf`, an artifact read). It does so at
  most once per rejecting state and producer in an instance; `d.Returns(budget)`
  changes the bound and `wf.Const(0)` switches it off. Then the state fails
  exactly as the plain error would have. A call that read no agent output,
  or several, returns nothing: it fails as before, with a `reject
  unattributed` note in `status`, and `wf.RejectOf(producer, reasons...)`
  names the producer. A check's Reject accepts the checked submission and
  returns the work upstream. A compute's Reject spends none of the compute's
  attempts. An explicit `SendBack` edge to the same producer takes
  precedence, with its bound and fallback. The producer's `MaxVisits` still
  wins on arrival.
- **A returned unit no longer needs a prompt binding.** Every returned visit,
  automatic or explicit, opens with "Your earlier output was returned by
  <state> (round r of n): <reasons>. Your prior output and files follow for
  revision." A prompt that does not bind `wf.Returned()` gets the returned
  document as a `returned` input, delivered like any input and paged when
  long. Lint rule `send-back-unread` is removed: the reasons always reach the
  producer.
- Journal plans mark an automatic return's step `Auto` (no declared edge,
  `Index` -1); `wf.Return`, status and `wftest.Trace.Returns` carry `Auto`.
  `dead-ends` notes the bound. The README has a recipe for making an
  existing workflow self-repairing.

### Upgrading

- Nothing changes for a definition that raises no `wf.Reject`: hashes, runs
  and journals are as before. `d.Returns` enters the hash only when set.
- To make a workflow self-repairing, replace `fmt.Errorf` with `wf.Reject` at
  each content check in a compute or check. Keep plain errors for
  operational failures.
- A definition that lists `send-back-unread` in its lint expectations drops
  it; a producer that does not bind `wf.Returned()` now passes lint.
- A returned producer's instruction gains the one-line return note above,
  including under an explicit `SendBack`.

## v0.4.6

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.6`. GitHub installs use the plugin tag
`plugin-v0.4.6`. This release adds three features: a step can send work back
to its producer with reasons, a bounded number of times, and then takes a
required fallback (`SendBack`); `workflow dead-ends` lists every route that
can end a run; and `workflow dev rewind --to unchanged` forks a run at the
longest prefix the new definition leaves unchanged. All three are additive.

### What changed

- **`dead-ends <definition>` lists every route that ends a run (sw-nqak).**
  A definition can end a run on paths its author did not mean as stops: one
  failing grader of five takes a panel's partial edge, or a verdict that
  reads like an approval shares its edge with stop. The new verb, and its
  library form `Def.DeadEnds`, list each route into a `Stopped` or `Stalled`
  terminal: the deciding state and edge, any states that only pass the run
  on (a cleanup command), the trigger that selects the edge (a case value, a
  gate answer, a retry or visit cap spent, a failure class, a timeout, a
  route-around, a Map join or a Sub exit), and whether one answer alone can
  take it (`yes`, `no` or `unknown` when a closure or the item count
  decides). A Sub exit or a Map partial edge lists the child's routes that
  produce it. `--variant name=JSON` (repeatable) names run options values:
  each route then lists the variants its `When` guards hold under, and each
  policy-answerable gate answer the variants whose policy gives it. It is a
  static listing over `Inspect`; `--json` prints the routes.

- **Send work back to the agent that produced it (sw-3op4).** A judging
  agent, compute or command declares `SendBack(producer, label, reasons,
  budget, fallback)`, an edge it routes like any other. Taking it journals
  the reasons the workflow's `reasons` function reads from the judge's
  result in the routing decision's plan, and enters the producer for a fresh
  unit. That unit's prompt binds `wf.Returned()`: the sender, the round of
  the budget, the reasons, the producer's prior output and, for each
  allocated artifact, the prior file and this visit's path, where the engine
  has copied it so the producer edits its own work in place. The block is
  empty on any other visit and travels through the same prompt delivery as
  any input, paged when the run pages. The returns over the edge are counted
  per instance from the journal: once the budget is spent, choosing the
  edge takes the fallback instead, and the producer's own `MaxVisits` still
  applies on arrival. `wf.ReturnedOf` reads the return; `status` shows it on
  the instance; `wftest.Trace.Returns` lists the returns a run took; the
  `dead-ends` report lists the fallback. New lint rules: `send-back-target`
  (the producer is an upstream agent of the definition, not the sender, and
  the edge is taken from a route or case), `send-back-fallback` (the
  fallback is not itself a send-back, and a fallback into a stop carries
  `Ends(reason)`) and `send-back-unread` (the producer's prompt binds
  `wf.Returned()`). A send-back edge bounds its own cycle for
  `cycle-no-measure`. New example: `examples/sendback`.

- **`dev rewind --to unchanged` forks a run at the longest prefix the
  current definition leaves unchanged (sw-0x90).** Run creation and every
  migration now record each state's identity (`StateIDs`): short hashes of
  its shape, prompt (with the content of the assets it reads), checks, edges
  and whole declared view, including the definition-level values it reads.
  The verb finds the first journal record that runs a state whose identity
  changed or that no longer exists, and cuts before it. A snapshot run is
  restored from the latest checkpoint before that record; any other run is
  forked by its journal. The kept prefix is adopted onto the current
  definition in memory first. A refused cut steps back to an earlier arrival
  or checkpoint, up to three times, and the report lists each refusal. The
  report names the first change and the identity parts that changed (shape,
  prompt, checks, edges), the records kept, and the effects applied after the
  cut. Closure bodies (routes, checks, computes) are not part of a state's
  identity, so `--changed <def>/<state>[,...]` names the states a
  code-only fix changed, and the cut lands before the first run of any of
  them. With nothing changed and nothing named, the verb refuses. If the
  binary's source differs from the run's build stamp, it says the change is
  in code it cannot see and asks for `--changed` or `--to <seq>`. Otherwise
  it reports nothing to rewind. `Engine.RewindUnchanged` is the library form.

### Upgrading

- Nothing to change. `dead-ends` is a new verb and `Def.DeadEnds` a new
  read; `SendBack`, `wf.Returned` and `wf.ReturnedOf` are new API, and the
  journal's `EdgeStep` gains the optional `SendBack` and `SendBackSpent`
  fields. Existing definitions, their hashes and journals behave as before.
- A back-edge with `MaxVisits` that returns a draft for revision can become
  a `SendBack`: the reasons then reach the producer as data and the bound
  and fallback are checked by lint. It is a definition change (a new hash),
  so a run in flight keeps its definition.
- `run_started` and `migrated` records gain the optional `StateIDs` field.
  Older engines ignore it. Journals written before it cannot use
  `--to unchanged`; pick a checkpoint with `--to <seq>` as before. A fix
  landing for a running workflow should name the states whose code it
  changed, so an operator can pass them to `--changed`.

## v0.4.5

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.5`. GitHub installs use the plugin tag
`plugin-v0.4.5`. This patch release fixes two behaviours: roots reached
through a symbolic link (macOS `/tmp` and `/var`) now checkpoint and fork,
and a `MaxVisits` budget is read after the arriving transition's counter
updates. Printed paths become real paths, and some visit caps allow one more
visit. Read Upgrading before you move to it.

### What changed

- **Roots reached through a symbolic link now checkpoint and fork
  (sw-vi0h).** The engine refuses to write through a symbolic link inside a
  run's roots, and before this change it also refused a link above them. On
  macOS `/tmp` and `/var` lead into `/private`, so a workspace, state
  directory or fork destination under either one could not be checkpointed
  (`symlink workspace`, `symlink path ancestor`, `openat ...: not a
  directory`), and a `--snapshots` run could not fork or rewind. The engine
  now resolves the roots it is handed (workspace, state root, run directory,
  and the `fork`, `fanout` and `manufacture` destinations) to their real
  paths once, when it receives them. A link inside a root is still refused.
  A fork destination reached through a link into the source world is now
  refused as `fork destination overlaps source world` rather than as a
  symlink ancestor.
- **The engine's own tests pass under macOS's temp directory (sw-vi0h).**
  Test isolation resolves `TMPDIR` to its real path, and two prompt-size
  tests no longer depend on how long the temp directory's path is.
- **A `MaxVisits` budget is read after the arriving transition's counter
  updates (sw-h8ge).** The engine checked a target state's visit budget
  against the counters as they stood before the edge into it was taken, so
  the edge's own `Inc` did not count. A budget such as `2 + fixes` therefore
  refused the very visit that the `fixes.Inc()` on the arriving edge was
  meant to allow. The budget is now read over the counters the target will
  see: the edge's `Inc`s apply, after the scope resets they cause (`ScopedTo`),
  in the same order the transition applies them. When a spent budget
  redirects through an over edge, the next target's budget also counts the
  over edge's `Inc`s. A budget of constants only, or one whose counters the
  arriving edge does not touch, behaves as before.

### Upgrading

- Paths the engine prints and records (the workspace in `cd` lines, a fork's
  workspace and state directory) are now real paths. On macOS a workspace
  under `/tmp` prints as `/private/tmp/...`. A run recorded under an alias
  of its workspace is treated as the same run in the same place: `run`
  attaches to it and `next` does not relocate it.
- Run behaviour changes for a definition whose `MaxVisits` budget reads a
  counter that an edge into the capped state increments, or resets through
  `ScopedTo`. That arrival now counts the update: one more visit per `Inc`,
  or a smaller budget after a reset. Check each such cap against the visits
  it should allow; a cap written as "one per record written" now means what
  it says. The definition hash does not change, and plans already journaled
  keep the target they chose, so a resumed run follows the new order from
  its next decision.

## v0.4.4

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.4`. GitHub installs use the plugin tag
`plugin-v0.4.4`. This patch release adds one read, `wf.HarnessOf`, which
names the harness that produced an agent state's accepted output. Nothing
existing changes.

### What changed

- **`wf.HarnessOf(c, agent)` reads which harness produced an accepted output
  (sw-o2f9).** A definition that tags or routes on where an agent state's
  output came from could only infer it from the coordinator's harness, which
  changes meaning across a resume on another harness. `wf.HarnessOf` now
  returns the canonical harness name (`claude`, `codex`, ...) of the
  hand-out whose output the state last accepted in this instance: the child
  harness for a unit dispatched with `OnFamily`, otherwise the harness of
  the coordinator the unit was handed out to. It is `""` before the state
  accepts an output. The value is folded from the hand-out record of the
  accepted attempt, so a read after a resume on another coordinator harness
  still names the harness that produced the output. The state's own checks
  and route read the attempt they decide. It needs no declaration and the
  engine attaches no meaning to the name.

### Upgrading

- Nothing to change. `wf.HarnessOf` is a new read; existing definitions and
  journals behave as before.

## v0.4.3

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.3`. GitHub installs use the plugin tag
`plugin-v0.4.3`. This release adds a run budget (`run --budget`, a
registration default, raising it on resume), fetch coverage readable by the
definition, paged delivery as a registration default, and a Codex rule
preflight that reads the project rules Codex loads. A run that hits its
budget now pauses instead of ending, and the rule preflight ignores an
untrusted project's rules. Read Upgrading before you move to it.

### What changed

- **Liveness honours a stop declared with `Ends` (sw-l6xo).** An all-good
  fuzz run that ended at a `Stopped` terminal was a liveness finding unless
  a fault had been injected, even when the edge into that terminal carried
  `Edge.Ends(reason)`. An options-guarded hand-off to a person, opened by a
  `Check` variant, was reported although its author had declared it
  intentional. Now the root's last transition into the `Stopped` terminal
  passes liveness when every edge from its source to that terminal is a
  plain edge marked `Ends`. A stop reached through a timeout, an exhausted
  retry or visit cap, a failure-class route, a forwarded child stop or an
  unmarked edge is still a finding, and so is a source with both a marked
  and an unmarked edge to the terminal, since the journal does not record
  which one the run took.
- **The Codex rule preflight reads the project rules Codex loads (sw-gk89).**
  It read the Codex home's rules and `<workspace>/.codex/rules` only, so a
  rule in the repository root was missed when the run's workspace was a
  worktree, and an untrusted project's rules counted although Codex skips
  them. It now reads `.codex/rules` in every directory from the working
  directory's project root (the nearest `.git`) down to the working
  directory, as Codex layers project config, and only while Codex's
  `config.toml` trusts that root; a linked worktree counts as trusted when its
  main checkout is. When Codex would skip the project's rules, the refusal
  says the project is untrusted and names the files.
- **`MainConfig.CodexProjectRule(repo)`** returns the rules file
  (`repo/.codex/rules/super-workflow.rules`) and the rule for the binary a
  client installs at `InstalledBinary`, so an install can write a per-checkout
  rule instead of a profile-wide one. `setup-codex-rules --write` is
  unchanged.
- **Fetch coverage is readable by the definition (sw-lb6u).** Paged delivery
  recorded every fetched page per unit and attempt, but only status printed
  it. Now an agent state that declares `ReadsFetchCoverage()` can be read with
  `wf.FetchCoverageOf(c, state)`. The read returns the page count and the
  pages fetched for the prompt and for each reference input, with whether
  each reference is required. It reports the accepted attempt alone, counts a
  re-fetched page once, and returns `ok == false` when the run is not paged.
  The record means fetched, never read. Lint notes each declaring state
  (`fetch-coverage`). `Engine.UnitCoverage` reads a unit's live count. In
  `wftest`, `PagedPrompts()` with `Cap(n)` runs a simulation paged, and
  `Step.Fetching(wftest.Pages(...))` scripts the pages fetched.
- **A registration can make paged delivery its default (sw-lb6u).**
  `Registration.PagedPrompts` starts the registration's runs paged, as
  `--paged-prompts` does. `--paged-prompts=false` opts one run out.

- **Run budget from the command line and the registration (sw-zpeo).** A run
  budget could be set only through `EngineOptions.RunBudget`, so a binary
  built on `wf.MainWith` could not cap a run. `run --budget
  turns=N,tokens=N,wall=D` now sets it, over a default that
  `Registration.Budget` computes from the run's options; the flag replaces
  the limits it names and `0` removes one. It is journaled on `run_started`
  and shown in `RunStatus.Budget`. `wf.ParseBudget`, `wf.BudgetChange` and
  `wf.DefaultBudget` are the parser, the change and the default the CLI uses.
- **A budget stop of the run is a stop, and a resume raises the budget
  (sw-zpeo).** A stop that reached the run journaled `run_ended`, so the
  only way on was a new run. Now the run is stopped (`RunStatus.Stopped`,
  with `BudgetStop` naming the spent limit and `BudgetResume` the operator's
  exact resume command, which `next` and `status` also print), and only the
  run's operator raises it: `resume --budget` raises the
  limits it names, journaled on `resumed` (`ResumedData.Budget`); the stopped
  instances carry on from where they stopped. A resume that leaves the spent
  limit spent is refused. `resume` now also refuses an ended run that a stop
  had preceded, which it used to journal `resumed` on.
- **A budget stop below an instance shows on it (sw-zpeo).** An inner Map
  item the budget stopped was visible only in that inner Map's result: the
  outer `Joined` saw its parent reach a success terminal, and the run could
  end at success with no trace of the stop. Now every ancestor of a stopped
  instance has `Instance.BudgetStoppedBelow` set in its own parent's result.
- **`wftest.Check` simulates a registration's default budget (sw-zpeo).**
  With `CheckConfig.Budget` set and a budget for the variant's options,
  `Check` runs an all-good run and the same run stopped by the budget at its
  first, middle and last turns (`CheckReport.RunBudgetSeeds`). A fuzz run
  takes `FuzzConfig.RunBudget`; a run the budget stops ends with `End: "run
  budget"`, which is not a `stall-unnamed` finding.

### Upgrading

- **A run whose budget stops it no longer ends.** It stays stopped, with no
  `run_ended`, until its operator resumes it with `--budget`; a `run` with
  its run key attaches to it instead of starting a successor. A journal that
  already has `run_ended` after a budget stop stays ended. Code that waited
  for `RunEnded` with outcome `Stopped` after a budget stop checks
  `RunStatus.Stopped` and `BudgetStop` instead.
- **`run --budget` on a live run** is refused when it would change the
  budget: change it with `stop`, then `resume --budget`.
- **`RunBudget` has a `String` method**, so `%v` prints it as
  `turns=N,tokens=N,wall=D` (`none` for no limit).
- **A checker that recorded an all-good liveness finding at an `Ends`-marked
  stop as known** no longer gets that finding. Remove the known entry.
- **A Codex-coordinated run that relied on a rule in `<workspace>/.codex/rules`**
  or in an untrusted project now fails the rule preflight. Put the rule in the
  project root's `.codex/rules` (an install can write it with
  `MainConfig.CodexProjectRule`) and trust that project in Codex, or keep it in
  the Codex home's rules.
- **A definition that routes on fetch coverage** declares
  `ReadsFetchCoverage()` on the agent state it reads, and its registration
  sets `PagedPrompts`, or its runs start with `--paged-prompts`. Without
  paged delivery the read is unknown. Nothing changes for a definition that
  does not read coverage: its hash, lint and runs are as before.

## v0.4.2

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.2`. GitHub installs use the plugin tag
`plugin-v0.4.2`. This patch release changes one `wftest` behaviour: each
`Check` variant now spends its own `MaxRuns`, so a bounded check with
variants runs longer and `CheckReport.Runs` is the sum over variants. Read
Upgrading before you move a test to it.

### What changed

- **Each `Check` variant spends its own `MaxRuns` (sw-ze6r).** Variants ran
  in name order against one shared run budget, so in a bounded check the
  first variant spent it and the later ones barely ran. The edges guarded
  for them stayed uncovered, and the result depended on the variant names.
  Now every variant calibrates and simulates under its own `MaxRuns`.
  `CheckReport.Runs` is the sum over variants, and each `NotExplored` refusal
  reads `variant <name>: <target>`. A config without `Variants` is
  unchanged.

### Upgrading

- **A checker with `Variants` and a bounded `MaxRuns`** now spends up to
  `MaxRuns` per variant, so it runs longer and covers more. One that
  multiplied `MaxRuns` by its variant count to make room should drop the
  factor. A gate on `CheckReport.Runs` sees the sum over variants.

## v0.4.1

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.1`. GitHub installs use the plugin tag
`plugin-v0.4.1`. This patch release changes one interface: `HeadlessProcess`
gains a supervisor (`Start(..., status)`, `State`, `Stop`, `Sandboxed`), and a
binary whose main is not `MainWith` or `CLI` must dispatch
`wf.HeadlessSupervisorVerb`. Shell commands also drop five more inherited
`GIT_*` repository variables, and in `wftest` a keyed shell effect scripted to
exit non-zero runs again on a loop back, as in production. Codex-to-Claude
children are no longer experimental: an uncoached live Codex-to-Claude smoke
on sw-gzdt succeeded with no spurious recovery offer. Read Upgrading before
you move a binary, a test or a live run to it.

### What changed

- **Headless children are supervised, so a sandboxed coordinator sees them
  (sw-gzdt).** A Codex coordinator runs each command in its own PID
  namespace, so `next` could not see the pid that `spawn` recorded. It took
  every live claude child for dead, with a spurious recovery offer right
  after the claim, and its stop at run end did nothing. Now `spawn`
  re-executes the workflow binary as a hidden supervisor,
  `__headless-supervisor`, outside the sandbox. The supervisor starts the
  child and owns it. It writes `<log>.status` with the pid, a heartbeat
  file and then the exit code, and it watches a `.stop` request, on which
  it sends the child's process group SIGTERM, then SIGKILL. `next`,
  `recover`, `status` and the run end only read and write those files, in
  any PID namespace and on every harness. A stale heartbeat with no exit is
  unknown, left to the lease, not dead. This ends the experimental status
  of Codex-to-Claude children.
- **Shell commands drop the rest of git's repository-local variables
  (sw-xb23).** Besides `GIT_DIR` and the six sw-itkk dropped, `shell.Run`
  commands, inline and detached, now also run without
  `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_GRAFT_FILE`, `GIT_SHALLOW_FILE`,
  `GIT_REPLACE_REF_BASE` and `GIT_IMPLICIT_WORK_TREE`, as the git adapter
  already does. `GIT_CONFIG_*`, identity, credential and transport
  variables still pass through.
- **The first-fetch bound holds when a supervisor dies (sw-3rdo).** A
  handed unit has no lease before its claim. So a child whose supervisor
  was killed before the child fetched its prompt stayed unknown, and its
  unit was never offered again. Past the first-fetch bound with no fetch
  and no fresh heartbeat, that launch is now dead: the unit is offered
  again under a fresh claim, and its spawn starts a new child. If the
  orphaned process still runs and fetches first, it claims the unit under
  its old claim, as any earlier offer's child may. The new child's fetch is
  then refused, so the unit never has two children working on it.
- **wftest: a keyed command that exited non-zero is not done for the
  world's check (sw-yvoy).** Since v0.4.0 the engine checks a keyed edge
  effect whose last result failed before applying it again. The fake
  `World` answered done for a `shell.run` scripted to exit non-zero with
  `wftest.Exit` or `ExitOnce`, so the engine skipped the re-run that the
  real shell adapter, which answers not done for a keyed command, would
  have made. A `shell.run` whose result exits non-zero now counts as failed
  for the check, as `FailApply` does.

### Upgrading

- `HeadlessProcess` is now `Start(..., status)`, `State(record)` (running,
  exited, unknown), `Stop(record)` and `Sandboxed()`. `headless_started`
  gains `Status`. A record without it, from an earlier binary, is judged by
  pid as before. A binary whose main is not `MainWith` or `CLI` must
  dispatch `wf.HeadlessSupervisorVerb` to `wf.RunHeadlessSupervisor`, or
  its spawns fail with "the headless supervisor ended before starting".
- **A shell command that relied on an inherited
  `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_GRAFT_FILE`, `GIT_SHALLOW_FILE`,
  `GIT_REPLACE_REF_BASE` or `GIT_IMPLICIT_WORK_TREE`** no longer sees it.
  Set it in the command's own argv, for example
  `env GIT_SHALLOW_FILE=<file> git ...`.
- **wftest: a test that loops back to a keyed shell edge effect after a
  scripted non-zero `Exit`** now sees the command run again, as in
  production, and takes the route that follows its new result. A workaround
  that scripted the failure with `FailApplyIf` to force the re-run still
  works and can go back to `Exit`. A test that expected the skip asserted
  behavior production never has.

## v0.4.0

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.4.0`. GitHub installs use the plugin tag
`plugin-v0.4.0`. This release breaks compatibility: effects gain the outcomes
`uncertain` and `refused`, and a keyed effect is checked before it is applied
again; sandboxed runs refuse adapters that implement only `Apply`; Map keys
enter instance ids escaped; `run_started` carries a build stamp; the `spawn`
verb, offered as one bare command, and the cross-family hint add the journal
kinds `harness_checked` and `headless_started`; `LintExec` gains the
`exec-unbound` rule and takes param names (`LintExec(params...)`);
`{records}` ignores document fields; the git adapter ignores inherited
`GIT_*` repository selectors; and so do shell commands. Read Upgrading before
you move a definition, a binary or a live run to it.

### What changed

- **Every run records its build (sw-wxp8).** `run_started` gains a `Build`
  stamp: the engine and workflow modules (path, version, any replacement,
  revision, modified flag), the Go toolchain, GOOS/GOARCH and
  `JournalFormat` 1. It is read from `runtime/debug.ReadBuildInfo`. A gap is
  recorded as unknown, never guessed. A build with `-buildvcs=false`, an
  engine from a `go.work` workspace and an engine from a local-directory
  `replace` all have no revision. The stamp also says where each revision
  came from: `vcs`, `module` (a real module version) or `declared`. A build
  without VCS stamping can declare its revisions at link time:

  ```sh
  M=github.com/alepar/super-workflow/docs/proposals/go-api/wf
  go build -buildvcs=false -ldflags "-X $M.declaredWorkflowRevision=<rev> -X $M.declaredEngineRevision=<rev> -X $M.declaredModified=false" ./cmd/workflow
  ```

  A declared value only fills a gap. A VCS revision or a module version
  always wins. A fork keeps its source run's stamp. The stamp enters no
  definition hash, prompt digest or effect key.
- **`workflow version` prints this binary's stamp** (`--json` for the
  `BuildStamp`). **`status` prints the run's stamp** on a `build:` line, with
  `exact` or `not exact: <why>`. When the binary reading the status is not
  the one that created the run, status adds `this binary differs: <what>`. That
  line is a note only: nothing is refused and nothing is journaled. A run
  started before stamps shows `build: not recorded`.
- **Artifacts from a newer engine are refused by name.** Git and bd
  checkpoint metadata, the snapshot store's meta and manifest, world snapshot
  records and recorded check contexts now refuse a version this engine does
  not know. The refusal reads "<artifact> is format N; this engine reads up to M:
  written by a newer engine, so use that engine or a newer one" and wraps
  `wf.ErrNewerFormat`. Before, the refusal was a generic one, such as
  "unsupported snapshot metadata". A newer hash cache is still discarded and
  rebuilt, since it is only a cache, and its error now names the version.
- **`LintExec` reports a placeholder nothing can bind (sw-j95s).** The new rule
  `exec-unbound` flags a `{name}` in a command's or a read's argv that no param,
  builtin or stored field can bind. The engine binds every such `{name}` when
  the state runs, so that state was certain to halt on an unresolved
  placeholder. The usual cause is script source passed inline (`node -e`,
  `bash -c`), where JavaScript destructuring such as `{execFileSync}` or a
  `${var}` expansion reads as a placeholder. Put the script in a file. Edge
  effects are not checked for this, because a route may attach their run-time
  form with literal args. `LintExec` takes the names of the params a served
  entry supplies (its `Registration` `ParamSpecs`), since a definition may read
  params it never declares with `Param`.
- **A Map key no longer collides with another instance's id (sw-ivuk).** A
  Map child's id held its key as given, so the outer keys `c1` and
  `c1]/Panel[s1` gave the second outer child and the first child's inner
  `Panel` child the same id. `Engine.Next` then never returned and the
  journal grew without bound. The key now enters the id escaped: each `/`,
  `[`, `]` and `%` is written `%XX`, and every other byte is kept. The id
  also feeds unit names and artifact directories, which change the same way
  for those keys. Replay now refuses a second `child_spawned` for an id that
  is already spawned, with `ErrCorruptJournal`, so a collision is an error,
  not a hang.
- **A fork of a fork binds its own records root (sw-3b1q, sw-3nim).** No
  input or output field shadows `{records}` any more; a run param still
  binds it. A child input field named `records` that copied the root held
  the recording run's path, so a fork cut inside that child bound its
  source's records directory and the step was refused. The fork-time
  relocation check also reads a chosen edge's plan as history once a later
  transition of its instance consumed it, as it already did for a done
  effect intent's args: a literal path in that plan's effect args no longer
  refuses the fork.
- **Keyed effects are checked before any re-apply that may double them
  (sw-luwj).** Previously only an intent a crash left without a result was
  checked. A keyed effect whose last result in the run is `failed` (it ran
  and errored) or `uncertain` (see below) is now checked by its key before it
  is applied again, on a backoff retry, a retry or a loop back to the same
  edge. Done is journaled `done` and not applied. Not done is applied. An
  unknown answer is Uncertain. A NonIdempotent effect in that position is
  Uncertain (manual verification), never applied blind. `unavailable` never
  ran and is retried without a check.
- **An adapter call that timed out is `uncertain`, not `unavailable`.**
  `wf.RunTool` wraps a timeout in the new `wf.ErrTimedOut` as well as
  `wf.ErrUnavailable`, so reads behave as before. An effect whose call timed
  out is journaled with the new outcome `uncertain`. It is retried on the
  Unavailable schedule, and each retry checks first. Shell commands keep
  their own timeout (exit 124).
- **A check can return the effect's result.** The new optional
  `wf.CheckResultRuntime` (`CheckResultAt`) returns what a done check found.
  The engine journals it as the `done` result's `Result`, on crash recovery
  and before a re-apply alike. `wf.Adapters.CheckResultAt` exposes it.
- **Sandboxed runs fail closed.** A run made by a rewind or a snapshot
  restore could reach real GitHub: the in-tree `gh` runtime took no context,
  so the sandbox flag was dropped and a rewound run created its issue again.
  Now, in a sandboxed run, an effect whose runtime has no `ApplyAt` is
  refused with `wf.ErrSandboxRefused` and journaled with the new outcome
  `refused`. The edge stops Blocked with no retry, and the reason names the
  adapter, the op and why (`wf.SandboxRefusal`). A check through a runtime
  without `CheckAt` is refused too and answers Unknown. Observations are not
  refused. Shell commands that reach the network themselves are not covered.
- **In-tree adapters.** `gh`'s `IssueCreate` now searches for its key marker
  before creating, and returns the issue it finds instead of creating a
  second one. Its check returns that issue. `gh` refuses every effect and
  check in a sandboxed run (its reads still run). `git` gains `ApplyAt` and
  `CheckAt`: its ops are local, so forks run them as before. `shell` gains
  `CheckAt`.
- **`wftest.Real(world, runtime)`** runs a real adapter's own `Apply` and
  `Check` under the simulator, against a stub the test sets up. The fault
  targets still apply: `CrashAfterIntent` and the fuzz `effect-uncertain`
  fault apply the real effect first and then kill the engine, so the
  adapter's check must find it. The fuzz `effect-timeout` fault on an
  adapter op now models a real timeout (`wf.ErrTimedOut`).
- **A Map inside a Map child is a stated guarantee (sw-fcsg).** The suite
  now runs one nested fixture, a panel of seats for each candidate. It covers
  `Parallel` limits per outer item, stopped seats in both levels' results, a
  budget stop, distinct artifact paths, a crash before and after every
  journal write of the drive, and a `wftest.Check` of a nested definition.
  The guide's Map section states identity, limits, results, budget and
  recovery for nested Maps. No behavior changed.
- **`wftest.Check` simulates named option variants (sw-145v).**
  `CheckConfig.Variants` maps a name to a value of the definition's options
  struct. Check then calibrates and runs every simulation phase once per
  variant and judges coverage once on the union, so an `Edge.When` path that
  one options value never opens no longer needs a false `.Unreachable`. An
  edge or state marked unreachable that any variant reaches is a violation
  naming that variant. Findings carry `FuzzResult.Variant` and their problems
  read `variant <name>: ...`. Before simulating, every `Edge.When` guard must
  hold under at least one variant (`variants: guard holds under no variant:
  ...`). The check uses the new `Def.UnheldGuards(options...)`, which
  evaluates guards with the engine's own rule. A config without `Variants`
  behaves as before.
- **A unit can run on another harness than the coordinator's (sw-drx1).**
  `a.Dispatch(wf.OnFamily(wf.FamilyNamed("codex")), wf.ModelTier(wf.TierTop))`
  runs that state's units as headless codex children at codex's top model;
  `wf.FamilyOther` means the run's other family (codex for a claude
  coordinator, claude for a codex one; `run --family coord=harness` changes
  it). The offer is one command, `spawn <run> --claim <token>`, which starts
  the child's CLI (`codex exec`, `claude -p`, through the new
  `harness.Headless`) with the brief on stdin in its own process session,
  and returns at once.
  - **Confinement:** the child is confined by its own CLI only: Codex's
    workspace-write sandbox with approvals never, or Claude's `dontAsk` with
    an allow list.
  - **Environment:** every harness's session identity is scrubbed, and
    `SUPER_WF_PARENT` tells the child's CLI it is the coordinator's child.
  - **Run flags:** `--headless-cmd`, `--family-home` and `--child-env-drop`
    configure the launch.
  - **Tiers:** they resolve on the child's harness. `--model-map` keys
    qualified with a harness (`codex.top=...`) set its table, which is
    journaled per segment.
  - **Preflight:** each segment checks the child's CLI, its login and, for a
    Codex coordinator, the Codex rule (`setup-codex-rules` prints it, and
    `--write` writes it). An unavailable harness fails its units with class
    `unavailable` and never falls back to the coordinator's.
  - **Liveness:** a child that exits without `done` or `failed` gets a
    recovery offer at once, quoting its stderr, with no resume. A child that
    never fetches is stopped after 120 s, and its unit is offered again.
    Lost attempts and a run's end stop their children's process groups.
  - **Status:** it shows each unit's harness, its pid and why it did not
    start, and each child harness's preflight.
  - **Live testing:** a Claude coordinator with a Codex child has passed a
    live run. A Codex coordinator's launch of a Claude child passed an
    uncoached live smoke with the sw-7mam fix below. Codex-to-Claude children
    are experimental in this release: a Codex coordinator's sandboxed commands
    cannot see its headless children's processes, so it may get a spurious
    recovery offer right after a child claims its unit and cannot stop its
    children when the run ends (sw-gzdt, planned for v0.4.1).
- **A Codex coordinator's spawn offer matches its rule (sw-7mam).** `next`
  printed a cross-harness unit's offer as `cd <ws> && <wf> spawn ...`. Codex
  splits that chain into two commands, and no rule matches `cd`, so the
  spawn verb ran sandboxed even with the rule installed and refused to
  start the child. The offer is now one bare command,
  `<wf> spawn <run> --claim <token> --state-dir <dir>`, for every
  coordinator harness. Spawn needs no `cd`: it starts the child in the run's
  recorded workspace from any directory. The preflight's `codex execpolicy check`
  asks about that exact printed form (`EngineOptions.HeadlessCheck` takes
  it as a new `spawn` argument), so it cannot pass while the real command
  is refused. A spawn that finds itself sandboxed after the check passed
  now says the command was most likely run chained or altered, and no
  longer tells the person to install the rule again. Other Codex commands
  keep their `cd` prefix.
- **The git adapter ignores an inherited repository environment (sw-j721).**
  An engine started from a git hook, or with `GIT_DIR` and `GIT_WORK_TREE`
  exported for a worktree, sent the adapter's ordinary calls (commit, merge,
  worktree, status and the reads) to the repository those variables named.
  Those calls now drop the variables that select another repository
  (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_COMMON_DIR`,
  `GIT_OBJECT_DIRECTORY`, `GIT_CONFIG_PARAMETERS`, `GIT_CONFIG_COUNT` with its
  pairs and the rest of `git rev-parse --local-env-vars`), as snapshot calls
  and the writable probe already did. `dev manufacture`'s own `git config`
  calls run without any `GIT_*` variable. The engine's test packages that run
  git now clear every `GIT_*` variable and turn off system and global git
  config before any test starts, so a suite started under those variables
  never writes to the repository they name.
- **Shell commands ignore an inherited repository selector (sw-itkk).**
  `shell.Run` commands, inline and detached, passed `GIT_DIR` and its kin
  through, so a workflow's `git diff` under an engine started from a git
  hook read the hook's repository. They now run without `GIT_DIR`,
  `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`,
  `GIT_COMMON_DIR`, `GIT_NAMESPACE` and `GIT_PREFIX`: the workspace defines
  the repository. Identity, credential, transport and config variables
  (`GIT_AUTHOR_*`, `GIT_COMMITTER_*`, `GIT_SSH*`, `GIT_ASKPASS`,
  `GIT_TERMINAL_PROMPT`, `GIT_CONFIG_*`) still pass through.

### Upgrading

- **A workflow that relied on an inherited `GIT_DIR`** to point the git
  adapter at a repository other than the workspace's now gets the
  workspace's. Name the worktree in the effect or observation instead.
  `GIT_CONFIG_GLOBAL`, author and committer variables still pass through.
- **A shell command that relied on an inherited `GIT_DIR`** (or
  `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`,
  `GIT_COMMON_DIR`, `GIT_NAMESPACE`, `GIT_PREFIX`) now acts on its
  workspace's repository. Name the other repository in its argv, for
  example `git -C <dir>` or `git --git-dir=<dir>`.
- **`{records}` ignores document fields.** A definition that bound
  `{records}` from an input or output field gets the run's records
  directory instead. Pass a different root under another name.
- **Adapters that only implement `Apply` are refused in forks (sw-luwj).**
  A workflow's own adapter runtime must implement `ApplyAt` (and `CheckAt`,
  or `CheckResultAt`) and decide what `EffectContext.Sandboxed` means for it.
  For local ops, pass through. For a remote it cannot sandbox, refuse with
  `wf.SandboxRefusal`. Until it does, a rewound or restored run stops Blocked
  at its effects. The in-tree `fsx`, `shell`, `git`, `gh` and `bd` need
  nothing.
- **New journal outcomes.** `effect_result` gains the outcomes `uncertain`
  and `refused`. Code that reads journals or `EdgeResult` and compares
  outcomes against `failed`/`unavailable` should treat both as failures. An
  older engine does not know them and would not stop the edge on them. Do
  not drive such a run with an older binary: nothing in this release
  refuses it, and a journal format range is planned (sw-u94w).
- **A rewind no longer reaches remotes through `gh`.** A rewind used to carry
  a fix into an in-flight run now stops at that run's first `gh` effect. A
  live takeover is planned (sw-yl5d).
- **Timeouts.** A `wf.RunTool` timeout still satisfies
  `errors.Is(err, wf.ErrUnavailable)`. Test for `wf.ErrTimedOut` to tell it
  apart.
- **A NonIdempotent edge effect that failed is no longer re-applied** by a
  later traversal of the same edge in the same instance. It is held for
  manual verification.
- **Older engines read stamped journals.** They decode `run_started`
  leniently and ignore `Build`: a v0.3.0 binary's `status` reads a journal
  this engine wrote.
- **Eval and gate builds that pass `-buildvcs=false`** record an unknown
  workflow revision unless they pass the `-ldflags` above.
- **`LintExec` callers** should pass each entry's param names
  (`d.LintExec(names...)`). Without them, a read of a param that only the
  entry supplies is reported as `exec-unbound`. The call without arguments
  still compiles.
- **Map keys holding `/`, `[`, `]` or `%`** give their children new ids,
  unit names and artifact directories. Any other key keeps the id it had.
  A live run keeps the ids its journal recorded: children are matched to
  their items by key, never by recomposing the id. A child the resumed run
  spawns after the upgrade takes the escaped id. Code that parsed a key out of
  an instance id or unit name should read `item.Key`, or unescape `%XX`.
- **Checkers with option-guarded edges** can list one variant per side of
  each guard in `CheckConfig.Variants` and drop the `.Unreachable` marks
  that stood in for the untested side: once a variant reaches such an edge,
  the mark is a violation.
- **New journal kinds `harness_checked` and `headless_started`.** An older
  engine does not know them. Do not drive a run that has cross-harness
  units with an older binary. Runs with no family hints write neither.
- **`--model-map` keys that begin with a harness name and a dot**
  (`codex.top`, `claude.all.effort`) now set that harness's child table and
  no longer the coordinator's. A model key spelled that way was never
  valid, so no working map changes.
- **The coordinator rules sentence now reads** "except through the engine's
  child dispatch (the call or the spawn command next prints)". A check that
  matches the bootstrap text verbatim needs updating.
- **`wf.Instruction` and `wf.DispatchHints` gain `spawn` and `family`
  (JSON).** `wf.RunStatus` gains `Children`, and `wf.UnitStatus` gains
  `Harness`, `Pid` and `StartErr`. All are omitted when unused, so existing
  readers see no change.

## v0.3.0

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.3.0`. GitHub installs use the plugin tag
`plugin-v0.3.0`. This release breaks compatibility: the `harness` API loses
`Unsupported`, and most checkpoints it takes cannot be forked by older
engines. Read Upgrading before you move a definition, a binary or a live run
to it.

### What changed

- **Built-in opencode adapter.** The `harness` package now has an opencode
  adapter that uses only opencode's public surface: it reads none of
  opencode's storage, runs no program and adds no module requirements. A new
  opencode plugin, `opencode/super-workflow.js` in the engine module and the
  plugin payload, exports `OPENCODE_SESSION_ID` and
  `OPENCODE_PARENT_SESSION_ID` into every command opencode's bash tool runs;
  install it by copying or linking it into `~/.config/opencode/plugins/` or a
  project's `.opencode/plugins/`. The plugin is required: without it an
  opencode session cannot drive a run, and its driving verbs are refused,
  naming `OPENCODE_SESSION_ID`. Units are spawned and resumed with opencode's
  `task` tool, and liveness comes only from the engine's verbs, heartbeats and
  the lease. The `task` tool takes no model, so no tier resolves and a
  `--model-map` is not applied: children run on the coordinator's model. When
  the definition uses tiers or a `--model-map` is given, one degraded `tiers`
  record per run or segment says so. A bare `OPENCODE` variable counts only
  when no other harness's markers are set; when it is set without
  `OPENCODE_SESSION_ID` (a plugin-less opencode started from another harness's
  tab, say), `run` prints a note and journals one degraded `harness hint`
  record. [COORDINATOR.md](docs/proposals/go-api/COORDINATOR.md) documents the
  adapter (written against opencode 1.18.x), the plugin and its limits. The
  adapter is covered by synthetic tests and has passed a live run on
  opencode 1.18.1: the run ended in success with the child's identity
  recorded. (The notes as tagged said it was untested live; the live run
  passed after they were written.)

- **`wf.ShellBound` binds a prompt value as one shell word.** It binds like
  `wf.Bound`, path templates included, but its value is the bound text as
  exactly one POSIX shell word (`sink.Quote`), so `cd [WORKTREE]` in a prompt
  stays one argument whatever the path holds, in prose, a code span, a list
  item or a fenced block. `wf.ShellWords` quotes each element of a
  whitespace-separated list as its own word, and text with no elements binds
  empty. A shell value is never paged. Text no prompt line can carry as one
  word blocks the state: a control character (a line break, a tab, NUL), a
  backtick or invalid UTF-8. In a fork the whole bound text is checked like a
  command argument. `wf.Reference` has no effect on a shell input. The view
  and the definition hash show the mode; definitions that do not use it keep
  their hash. `sink.Command` renders a command line in text the program writes
  whole: an inline code span for one line, a fenced block otherwise.

- **World checkpoints cost what changed, not the world's size.** The git
  adapter hands the repository's object files to the run's snapshot store
  instead of writing a bundle of its whole history each time; the store keeps
  a hash cache of files it has read and trusts packs it has read back whole
  while they are unchanged. On a 310 MB repository a checkpointing submit fell
  from 11-17 s to about 0.5 s, and the store no longer grows by the repository
  at every checkpoint. Forward-incompatible: a checkpoint this engine
  takes of an ordinary clone (git metadata version 3) cannot be forked by an
  older engine, which refuses it. Shallow, partial and alternates clones still
  get version-2 bundles, and checkpoints taken by older engines fork as before.

- **`maxlen` bounds only what an agent submits.** It is checked on validate
  and done (and the calibration and recorded-output probes), no longer on
  compute outputs, Sub inputs, Map items or yield results. Those are built by
  the definition's code or read from the world, and a cap there turned
  ordinary data into a refusal or a halt with no agent to fix it. `pattern`
  is still checked everywhere. A workflow that relied on `maxlen` to refuse a
  computed, read or yielded value must check that length itself.

### Upgrading

- **`harness.Unsupported` is removed.** opencode was its only entry, and it
  now has a built-in adapter. Drop calls to it. A binary that registers its
  own `"opencode"` adapter now fails with `already registered`: remove that
  registration and use the built-in one. An adapter under another name that
  declares `OPENCODE` or `OPENCODE_SESSION_ID` as a marker now fails to
  register too, because those markers are opencode's.
- **Checkpoints are forward-incompatible.** A checkpoint v0.3.0 takes of an
  ordinary clone uses git metadata version 3, which older engines refuse with
  `git restore: invalid metadata`. Do not fork a run started or checkpointed
  under v0.3.0 with an older engine. Shallow, partial and alternates clones
  still get version-2 bundles, and checkpoints taken by older engines fork
  under v0.3.0 as before.
- **`maxlen` no longer refuses computed values.** It is checked only on what
  an agent submits. A workflow that relied on it to refuse a compute output, a
  Sub input, a Map item or a yield result must check that length itself.

## v0.2.1

v0.2.0's plugin payload was missing an engine source file and did not build;
use v0.2.1. The missing file was `wf/lint_maxlen.go`, so any workflow built
from the v0.2.0 plugin failed with `undefined: ruleMaxlenTag`. The engine
module at `docs/proposals/go-api/v0.2.0` is complete and unaffected.

- The plugin payload lists `wf/lint_maxlen.go`.
- An engine test fails when a non-test Go file is missing from the payload
  allowlist, and the package test builds the staged payload offline.

Everything in v0.2.0 below applies, including its Upgrading notes.

## v0.2.0

The engine module is `github.com/alepar/super-workflow/docs/proposals/go-api`
at tag `docs/proposals/go-api/v0.2.0`. GitHub installs use the plugin tag
`plugin-v0.2.0`. This release breaks the API; read Upgrading before you move a
definition or a live run to it.

### What changed

- **Sink types and escaping.** Values pass through typed sinks before they
  reach a shell line, Markdown, a bd label or a path. The new package `sink`
  has `Quote`, `Argv.Line` and `CheckArg` for shell lines; `Text`, `Code`,
  `Link`, `Row` and `Item` for Markdown; `NewLabel`, `MustLabel` and `Labels`
  for bd labels; and `Under` for path components that come from model output.
  `wftest/hostile` ships a shared hostile-input corpus that every renderer's
  round-trip test runs. The harness, `wf`, `bd`, `git`, `gh` and the feature
  example all use these sinks.
- **Fenced result files.** A result file that is exactly one fenced code block
  (```` ``` ```` or ```` ```json ````) holding valid JSON is unwrapped and
  parsed. The record carries a `Normalized` note. Prose around the block, two
  blocks, another info string or invalid JSON is refused as before. String,
  `any` and `RawMessage` outputs are never altered.
- **`maxlen` tag.** `maxlen:"N"` caps a string field, a list of strings or a
  pointer to either, in characters. The agent sees it as `maxLength`. It is
  checked wherever `pattern` is, and the `maxlen-tag` lint rule refuses an
  invalid tag or one on another type. Untagged definitions keep the same hash
  and schema.
- **Definition hash memo.** A definition handed to an engine is frozen and is
  walked and hashed once per process. Asset files are still read on every
  call. Hash values are unchanged.
- **Faster CLI calls.** The run lock is polled every 5ms instead of 50ms.
  `TuneGCForCLI` sets GOGC to 400 for a one-call process unless GOGC is set in
  the environment; `MainWith` calls it. Journal reads and state view walks are
  cheaper.
- **Duplicate child names.** Siblings that share a child name each run their
  own definition. The new lint rule `child-name-duplicate` refuses two child
  definitions that share a name but differ in structure or asset root, and
  `Engine.Run` refuses such a definition at start, re-attach and replay.
  Existing runs resume through `next` and `resume`.

### Upgrading

- **`git.Commit` no longer globs.** Its paths are matched literally. A
  definition that commits a pattern must call `git.CommitGlob`.
- **Git 2.36 or newer.** The engine reads `git worktree list -z`. `git status
  -z` lists a rename as two paths, the new path and then the source.
- **bd label encoding.** Labels are percent-encoded for `%`, `,`, `"`, CR, LF
  and edge whitespace.
  - Non-key labels and `RootLabel` values that hold a raw `%` or edge
    whitespace no longer match beads stamped by v0.1.0, because `ReadyIn` and
    `ListByLabel` query the encoded form.
  - An empty label is an error.
  - Key labels that hold `%` or edge whitespace changed form. `Check` also
    finds the earlier stamp where that is unambiguous.
- **gh effect-key marker.** The marker now encodes anything that would close
  the comment or break the search phrase, with no fallback to the old form.
  This matters only for a live key that holds `%`, `"`, `\`, `<`, `>` or a
  control byte.
- **Stale replay prompts.** Replaying a journal recorded by v0.1.0 shows Stale
  prompt entries where input rendering changed. Outputs still feed and the
  verdict is the same.
- **Refused control bytes.** Run ids, map item keys and integer gate answers
  that hold control bytes are refused, as is a Stop-hook command that is not
  valid UTF-8.

## v0.1.0

First public release.
