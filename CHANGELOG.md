# Changelog

## Unreleased

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
