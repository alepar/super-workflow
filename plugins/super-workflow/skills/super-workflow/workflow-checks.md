# Workflow recovery and verification

Engine paths are relative to the module used to build the workflow. In a framework
checkout that is `docs/proposals/go-api`.

## Bind each reachable path

At a join or re-entry, trace the happy path, each early exit, a failed producer, and
each retry or repair route. Use named typed inputs and explicit artifacts. Add a
scripted assertion for each incoming path that can reach the consumer. On an
existing journal, test adoption at the changed boundary, then drive the fixture
through the consumer and verify the bound value or the intended stop before it.
The binding checks in `wf/adopt_bindings.go` distinguish a lost producer, a
missing field, and a failure edge that supplies no output. Drive opaque callbacks
with boundary inputs to establish the actual producer order.

Use an explicitly templated field for a binding and keep agent-produced values
literal. For example, the shell effect API in `shell/shell.go` accepts
`InTemplate(wf.Template("{workdir}"))` when the effect directory has a declared
`workdir` binding. Use `wf.PathTemplate("{records}/review-{round}.json")`
as the whole command or effect path argument to retain its recorded root.
Use `shell.Run(...).WithStdin("{body}")` for a command that reads free text or
JSON from stdin. Exercise the bound path and payload in a script or test.

Declare the root of shipped prompts and scripts with `d.Assets("assets")` and address them
through `{assets}`. Children inherit the root unless they declare their own. Use
`--assets` to supply an explicit root at run time and `wftest.WithAssets` for
simulations. Keep inputs and generated files in their run or workspace paths.

## Compose phases and rounds

- For a `Sub`, declare `wf.Typed[I, R]`, read `I` with `wf.InputOf`, and `wf.Yield`
  an `R` at each child terminal. Supply the input at `wf.NewSub` and map child
  terminals through `sub.Exits(wf.OnExit[...](edge), ...)`. Read the typed result
  with `wf.ChildResult`. Test the exits that continue and the exits that stop.
- For a `Map`, choose a stable key for each item within an entry. A loop back to
  the Map starts a new entry. The same key in the next round starts a fresh child,
  and `wf.Joined` exposes the results after that entry joins. Persist a summary or
  declared ledger when a later round needs previous results. Include the round
  in caller-managed paths that must keep both rounds' files.
- `AllSettled` waits for the children to settle, then takes the partial edge if
  any child failed. `AllOK` requires every item to succeed for the met edge.
  A source read failure takes the partial edge and sets `SourceError`.
  An empty input meets the join. To use results from a mixed round, route the
  partial edge to a collector that distinguishes source errors from child
  outcomes. Choose `ContinueSiblings` or `CancelSiblings` with
  `OnSiblingFailure` and test the resulting policy.
- Use `wf.MaxVisits(wf.Const(3), state.To(exhausted, "round cap"))` for a loop's entry cap.
  It counts visits within the definition instance. Each Map item or Sub launch
  gets a new instance. Put a round cap on the parent loop and per-item retries
  in the child. Route exhaustion to a state that can use the evidence collected
  so far or record why the run must stop. Compose a counter-based cap with
  `wf.Const(3).Plus(wf.Count(counter))`, using a counter owned by that definition.
  A reusable child builder can accept a `*wf.Budget` in its Go configuration.
  If supplied, dereference it when declaring `MaxVisits` and the over-edge.
  Inside the running child, use `wf.InputOf` for the child's typed data.
- `PartialWhen` runs before `Yield`. For a `Sub`, read the child's readiness
  with `wf.ChildOutcome`. For a `Map`, include readiness in the yielded result
  if the collector needs it. Test the continuation for a delivered partial result.

## Choose where data lives

- Use typed results for verdicts, counts, small summaries, and references used
  to route or join. Use an allocated `wf.NewArtifact` for bulky products and
  evidence owned by one producer. The engine allocates the path per producer
  instance. Carry a `wf.Ref[T]` obtained with `wf.RefOf` in the child's yielded
  result. At fan-in, read the current entry with `wf.Joined`, collect the usable
  references, and pass the joining command a manifest or the bound paths.
  Test the consumer with mixed outcomes and real fixture contents.
- A ledger updated by several states can live in the run's working directory.
  Give it an explicit initializer, bind the path for the readers and writers,
  and declare observations for reads in compute steps. Run mutations through
  commands or declared effects, with an interruption and replay policy. Use
  `wf.NewRepoArtifact` for a declared file that belongs in the product repository.
- Declare a Map's static write set with `Writes` and per-item paths with
  `ItemWrites`. Both sets apply to each item, so overlapping static globs can
  serialize items even when per-item paths are disjoint. When steps need
  exclusive access to a shared resource, create a lock with `d.Lock` and
  attach it to the participating states with `wf.Hold`. Hold an edge effect's
  lock on its target state, where the engine acquires it before running the
  incoming effect. Choose a scope that matches the resource. See the merge
  lock in `examples/feature/def.go`.

## Test failure and recovery

Use `examples/resumable/def.go` for observation-backed `DoneWhen` and an
`Uncertain` route. Use `examples/unavailable/def.go` and
`examples/checkrepair/def.go` for outcomes and bounded repair, and
`examples/sendback/def.go` for returning work to its producer. That example
declares an explicit `SendBack` edge; a compute or check reaches the same
return by returning `wf.Reject`. Script a rejected output and assert that the
producer's next visit gets the reasons. Also script an unfixable fault and
assert that it stops without a return. Test exhaustion,
resume after an interrupted effect, and required operator decisions. Choose
whether a bookkeeping failure stops the phase or permits a reported degraded
result.

When shortening an existing edge, compare the graph and definition hash before
adopting the change. The tour simulation tests in `examples/tour/sim_test.go` show
helper forms preserving both render and hash. Changing the shape of a case
table can change its recorded identity even when the destination stays the same.

| Check | What to inspect |
| --- | --- |
| Lint and graph render | Run `d.Lint()`, `d.LintExec()` for executable arguments, and `d.LintSound()` for stops, refusal loops, and cycle bounds. The tour's `examples/tour/def_test.go` also runs `LintChecks` and `LintReachability`. Try `go run ./cmd/workflow graph tour` from the engine module. |
| Journal relocation | `wftest.Check` checks the simulated journal with `wf.CheckJournalRelocation`. Supply realistic roots and bound paths. Fix the reported journal field, then exercise the same consumer on the rerun. |
| Structure and hash | Compare baseline and candidate renders and hashes. Re-run any edge inventory checks owned by the workflow. Investigate differences before adopting a live journal. |
| Scripted paths and product checks | Feed representative results and adapter faults into the real graph. Assert route, terminal outcome, required evidence, and whether a requested side effect happened. Exercise callback behavior in path tests. |
| Live eval | Check whether a real harness and model produce usable results, recover within bounds, and leave the expected evidence. Attribute a failure to the task, workflow, engine, or setup using the journal and receipts. |

Use `wftest.Check(factory, cfg)` with a fresh simulator and world per call.
Start with the tour's `examples/tour/checker/checker.go` for valid and progress
outputs, then add named `CheckConfig.Scenarios` for domain-specific paths.
The feature checker at `examples/feature/checker/checker.go` supplies ordered
`FuzzEffectSequence` results and explicit gate answers for repair and exhaustion.
Use the scenario count and findings to check coverage. A scripted test such as
`TestTourRevisesPlanAndRepairsFailedSuite` can assert the exact route and terminal.

For a continuing run, keep the baseline pin and inspect the first changed edge,
binding, or effect before choosing whether to adopt, rewind, or keep the run on
its starting definition.
