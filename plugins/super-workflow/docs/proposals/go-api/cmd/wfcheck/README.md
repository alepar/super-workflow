# Example workflow checker

From `docs/proposals/go-api`, run the model-free tour and reference suites with:

```sh
go run ./cmd/wfcheck check tour
go run ./cmd/wfcheck check feature
```

This development-only binary links simulator fixtures from `examples/tour/checker` and
`examples/feature/checker`.
The shipped `cmd/workflow` runner does not link `wftest` or Go's `testing` package.
Both binaries use the same example definitions; only the checker registration
attaches the simulation callback. Findings exit nonzero.

Check examples may supply prior results with `wf.ExampleOutputOf(agent, value)`
and `wf.ExampleComputedOf(compute, value)`, alongside `wf.ExampleObserved`.
The selected check must actually read each supplied result through `OutputOf`
or `ComputedOf`; otherwise calibration reports an unread override. The state
must belong to the same definition and the value must match its result type.
Overrides appear in the calibration report and never change recorded run state.
Recorded `Corpus` values are shared by calibration and every simulation layer;
they do not automatically become designated `Progress` examples.

Accepted agent submissions record which context values each check consumed.
Prior outputs, computed results, child results and observations use references
to their producing journal events; harvest resolves them into a self-contained
corpus. Parameters, counters, child input and attempt numbers are recorded
inline when small. Artifact reads, budget evaluation and unsupported reads mark
the context incomplete.

Calibration evaluates each recording with its own context, without live world
or file reads. Missing reads and unused recorded values fail calibration.
Legacy recordings, incomplete contexts and changed check identities report
`context unknown`; they are not tested against another run's state. Explicit
`Valid` fixtures and declared examples still use the reached simulation context.

The definition suite calibrates checks, exercises mixed outcomes and gate answers,
and enforces state and edge coverage. Ordinary mixed runs still draw schema results.
Generated liveness prefixes instead submit curated `Valid` results, falling back to
`Progress` for states without `Valid`, so generated data cannot silently change the
context assumed by the progress fixtures. Other accepted results, such as negative
reviews, belong in `Valid`; `Progress` identifies results that advance successful work.

Liveness selects an actual live checkpoint with the active visit's recovery reserve
still available. Random selection excludes spent reserves; explicitly requesting
such a checkpoint reports `checkpoint-ineligible`, not a workflow liveness defect.
Once the all-good phase starts, a refused progress example produces an immediate
`progress-context` diagnostic identifying the result and refusal; it does not become
a generic liveness failure after repeated retries.

Targeted runs inject an effect failure, an observation failure, or a gate timeout,
then supply `Progress` results and successful effects. Observation faults use the
engine's owning state and instance, so shared adapter operations do not confuse
targets. A validation probe retains its selected observation fault until submission
consumes it. Crash targets interrupt a durable intent before its result, restart
the engine on the same journal, and reconcile the exact effect as unknown. Detached
launch adoption is not simulated; such a target reports a fixture gap.

Timeout targets first follow the progress path.
Unreached targets next try a seeded adverse prelude. If that still cannot reach a
gate, the checker tries each declared effect fault separately as `PreludeFault`,
keeping unrelated effects successful until the target timeout. This can reach a
failure-only gate that broad faults would prevent earlier in the run. A target
that no run reaches remains a fixture gap. Failure-only gate answers use isolated
preludes too. Separate coverage runs repeat agent failures and effect/observation
faults through retry exhaustion, and submit each corpus result with a progress
prefix. Repeated negative reviews can therefore cover bounded repair cycles.

`CheckConfig.Scenarios` adds named coverage runs for domain-specific paths. These
inherit `Progress`, artifacts and the step bound, with random hostile actions and
schema draws disabled by default. A scenario can combine `GateAnswers`, a repeated
`PreludeFault`, and `EffectResults`: finite JSON result sequences selected by the
owning state and adapter operation. Inline and detached applications consume one
sample per invocation, then return to the factory's ordinary behavior. Checks,
polls and all-good recovery consume no samples. Invalid targets/results and unused
samples are fixture gaps. The reference's `checker.Suite()` uses these scenarios
for structured build failures and coordinated repair caps; the core generator
does not know the workflow's build-output format.

Recovery must reach success or a human decision boundary within its step bound.
A run that actually consumed an injected fault may also reach a declared stopped
terminal, proven by its root `run_ended` record. A fault-free all-good run that stops
still fails liveness. Pauses, held units and unnamed stalls are not terminal proof.
A reserved gate's declared timeout directly to a stopped terminal is an intentional
boundary only after the journal proves the timeout transition occurred. For Sub or
Map children, a genuine stopped terminal settlement completes that gate's targeted
liveness obligation. Later parent reporting or other work does not reopen it, even
if the parent eventually stops. This is not a liveness claim about that later work;
independent findings, such as definition panics, are still reported. A timeout to
nonterminal repair work must still recover. Timeout edges need actual coverage and
reproducible replay.

Edge totals distinguish covered, uncovered, explicitly `Unreachable(reason)`, and
inherited defaults that are `n/a by class`. Structural exemptions never apply to
explicit state routes. If a seed traverses an exempt edge, coverage fails rather
than counting the exemption as success.

Maps can annotate definition-specific class exemptions with
`map.OnClass(class, map.To(target, reason).Unreachable(explanation))`. This API
currently requires an unreachable edge; it does not add retry or backoff policy.
Inherited Map Unavailable defaults without external effects, and inherited
Unavailable defaults on inline agents with only pure prompt bindings (params,
child input, prior outputs, or literal bound values), are structurally n/a when
they have no checks, observations, postconditions or external edge effects.
Artifacts, bound templates, file prompts and dynamic prompts remain obligations.

The optional `CheckConfig.ChildProtocol` assessment is enabled by this reference
registration. Its `child protocol` diagnostics test the framework's child protocol
through the real CLI using isolated harness records and positive controls based on
the recorded child fixtures in `harness/testdata/started`: a call holding its unit's
claim token is accepted whatever the harness's records show (no record, another
token, the token only quoted later, a stripped environment), and a token never
offered, a call with no token and the coordinator's own first fetch are refused.
Protocol findings describe a call the protocol should have taken or refused; protocol fixture gaps
mean a precondition could not be established. Neither is definition edge coverage,
and passing definition simulation does not clear a protocol finding.

The protocol matrix has 28 positive controls and 143 planned cases. It includes a
sessionless first Claude coordinator and a Codex claim token quoted in a later
user message, separately from a later tool result. Every refused call must leave
the journal byte-for-byte unchanged, except the coordinator's own first fetch,
which appends its one `child refused` record. A sessionless case refused at `next` is
reported separately as "blocked earlier (at next)" only when it exits 4, discloses
nothing, and leaves the journal byte-for-byte unchanged. It is covered without
counting as an executed child-verb case; other setup failures remain fixture gaps.
A passing probe test confirms complete
fixtures, not an absence of reported protocol problems.

The reference calibration contract also runs in `go test ./examples/feature`.
These checks invoke no model and do not replace a full model-driven acceptance run.
