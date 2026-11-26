# Changelog

## Unreleased

- **World checkpoints cost what changed, not the world's size.** The git
  adapter hands the repository's object files to the run's snapshot store
  instead of writing a bundle of its whole history each time; the store keeps
  a hash cache of files it has read and trusts packs it has read back whole
  while they are unchanged. On a 310 MB repository a checkpoint fell from
  about 10 s to under 0.5 s, and the store no longer grows by the repository
  at every checkpoint. Forward-incompatible: a checkpoint taken by this
  engine (git metadata version 3) cannot be forked by an older engine, which
  refuses it; checkpoints taken by older engines fork as before.

- **`maxlen` bounds only what an agent submits.** It is checked on validate
  and done (and the calibration and recorded-output probes), no longer on
  compute outputs, Sub inputs, Map items or yield results. Those are built by
  the definition's code or read from the world, and a cap there turned
  ordinary data into a refusal or a halt with no agent to fix it. `pattern`
  is still checked everywhere. A workflow that relied on `maxlen` to refuse a
  computed, read or yielded value must check that length itself.

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
