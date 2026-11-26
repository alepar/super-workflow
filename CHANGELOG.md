# Changelog

## Unreleased

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

### Upgrading

- **Older engines read stamped journals.** They decode `run_started`
  leniently and ignore `Build`: a v0.3.0 binary's `status` reads a journal
  this engine wrote.
- **Eval and gate builds that pass `-buildvcs=false`** record an unknown
  workflow revision unless they pass the `-ldflags` above.

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
