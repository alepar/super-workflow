---
name: super-workflow
description: Use when writing or changing a Go super-workflow definition, adding a phase, diagnosing a workflow route or binding, reviewing the tests for a definition, or driving or resuming a super-workflow run.
---

# Author a super-workflow

Start with one executable path. Name the input, one work step, how to verify its result,
and the success and stop terminals.

1. Resolve the engine module using **Build a one-off** below. Read its `README.md`,
   `examples/tour/def.go`, and `examples/tour/sim_test.go`. Use the same engine
   revision for the API reference, build, and verification.
2. Use an agent for judgment, a command for execution, a compute step for decisions from
   typed values and declared observations, and a reserved gate for a person's decision.
   Put engine-controlled writes in declared effects or commands. Agents can create their
   declared artifacts.
3. Run `Lint`, `LintExec`, and `LintSound` in definition tests. Run the checker
   with realistic fixtures to exercise the journal relocation check.
4. Script your successful and rejected results. Assert the route and terminal. Corroborate
   an agent's test claim with a receipt from execution at the relevant revision. Run this
   example from the engine module.

   ```sh
   go test ./examples/tour -run '^TestTourRevisesPlanAndRepairsFailedSuite$' -count=1
   ```

## Extend one phase at a time

When porting a prose skill, keep evidence, rubrics, and judgment instructions in the
agent prompt. Use states for work with its own input or outcome, a recovery boundary,
a separately checked effect, parallel workers, or a decision reserved for the person.
Keep closely related judgments in one state if they consume and produce the same evidence.

Group each phase's reads, types, work, failure policy, and destinations. Use a typed
`Sub` for a phase with its own lifecycle and map each child exit at the parent call
site. Use a `Map` for independent instances of one child definition. Choose join and
sibling-failure policies, then test empty input and mixed outcomes. The engine's
`examples/feature/def.go` shows a complete workflow with fan-out and recovery.

**Trace every input to a producer on every reachable path.** Include retries, re-entry,
and failed producers. If a fallback needs the original work set, freeze its IDs and
have the prompt, check, and fallback use the same set.

| Need | Current API |
| --- | --- |
| Plain unconditional edge | `step.ThenTo(next, "receipt verified")` |
| Unconditional edge with a write | `step.Then(step.To(next, "receipt recorded").With(effect))` |
| Exhaustive enum verdict | `wf.RouteValue` for the whole output. Use `RouteCases(wf.Cases(...))` with a selector for a field. |
| Bounded recovery | `wf.Attempts(n, exhaustedEdge)` includes the initial ordinary attempt. Use `wf.Retry` for a computed budget and `wf.MaxVisits` for re-entry. Give unavailable input a separate route from a completed false result. |
| Delivered result with work outstanding | Use `PartialWhen` on a success terminal when the user can use the result and readiness still matters. A child that ends Partial settles Accepted. See the reference for reading the readiness signal at the parent. |

Read [Workflow recovery and verification](workflow-checks.md) for repeated fan-out,
bulky results, shared files, write coordination, and verification when the graph changes.

## Build a one-off

In a framework checkout, the module is `docs/proposals/go-api`. For an installed
Claude Code plugin, use `${CLAUDE_PLUGIN_ROOT}/skills/super-workflow/verify-release.py`.
In Codex, use `verify-release.py` beside the installed `SKILL.md` from the skill
path reported by the harness. Run it with `python3`. It reports the verified
release version, payload SHA-256 digest, and absolute engine module path. Use its
`--snapshot <new-absolute-directory>` for a retained or asset-backed run. Put
the snapshot outside the installer cache and point both the build and any
runtime assets at that snapshot. The installer can remove old versions.

Choose a writable run directory outside the user's repository unless they ask
to use it. The authoring session, orchestrator, and workers must be able to
access the directory. Create the workspace under the selected directory before
launch. Put your source in a `src` subdirectory and keep
the binary, state, and Go cache alongside it. Read the engine's **Your own runner** section for a minimal `wf.MainWith`
registration. Use a fresh standalone Go module and a local replacement. From
the directory with `main.go`, set `engine_module` and `engine_version` to the
verified module path and release version. For a framework checkout, use an
absolute checkout module path and `v0.0.0`.

```sh
export GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly
export GOCACHE="$(cd .. && pwd)/go-cache"
mkdir -p "$GOCACHE"
go version
go mod init example.com/one-off
go mod edit -go=1.26
go mod edit -require="github.com/alepar/super-workflow/docs/proposals/go-api@$engine_version"
go mod edit -replace="github.com/alepar/super-workflow/docs/proposals/go-api=$engine_module"
GOPROXY=off go build -o ../workflow .
```

Use a Go toolchain that satisfies the bundled `go.mod` (Go 1.26 for v0.1).
If Go is missing or too old, report the required version and ask the user to
provide it. The bundled engine builds without a module download. If your
own graph imports extra modules, resolve those separately before this build.

## Drive the run

Hand the binary, release version and payload digest, and absolute workspace and
state paths to a separate orchestrating session. Confirm it and its workers can use the same paths under
their normal permissions. A graph with an agent step needs a harness with
native subagents and authorization to delegate. Start with
`workflow run <definition> --run-id <id> --workspace <abs> --state-dir <abs>`
for an unkeyed definition, adding any declared parameters. Keep stdout's run
ID and stderr's bootstrap, including the exact re-entry command.

- Call the printed `next` command unchanged. If it is still running, wait on the
  same tool invocation. On a spawn offer, start one native worker with the
  provided brief. The worker runs its printed `prompt`, `validate`, `done`, or
  `failed` commands as applicable. Continue `next` as directed.
- Settle coordinator-owned effects only when the workflow provides the
  authority and evidence. Relay reserved gates and person-owned effects to
  the person and use their decision.
- To replace a coordinator session, hand the successor the same run ID and
  printed `next` command and use the same workspace and state. An operator
  stop is durable and needs the engine's printed `resume --operator --reason`
  command and an operator decision before work continues.
- When the run ends, verify its status and delivered artifacts. Keep the full
  run by default. If the user chooses cleanup, copy and verify the delivered
  result outside the run directory first. Once the run and workers are
  quiescent, remove the run directory the user approved for cleanup.

For the driving protocol and recovery commands, read `COORDINATOR.md` beside
the engine module's `README.md`. For a rebuild, reuse the original verified
snapshot or restore the exact release identity and point the Go replacement
at its new absolute location before rebuilding.
