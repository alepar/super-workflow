# Examples

Start with the [five-minute tour](tour/def.go): its numbered sections introduce
planning, review, parallel tasks, test-driven repairs and a human decision.
The [simulation tests](tour/sim_test.go) run those paths without host commands.
From the module directory, run `go test ./examples/tour` and
`go run ./cmd/wfcheck check tour`; `go run ./cmd/workflow graph tour` renders it.

Launch the tour with an agent coordinator using this instruction, for every harness:

> Drive this workflow run until it ends or asks for a person. Start sub-agents whenever it asks you to. Never answer a question meant for a person yourself.

Then read [feature](feature/README.md) for isolated Git worktrees, independent
acceptance tests, review and controlled merging in a complete workflow.

Each example exports `Definition() *wf.Def`. From the module directory,
`go run ./cmd/workflow graph <name>` renders a registered example.

| pattern / example | package | key API elements |
|---|---|---|
| P1 / E1 | `examples/gatedchain` | `wf.NewAgent`, `wf.Check`, `fsx.SchemaCheck`, `wf.NewTerminal` |
| P2 / E17a | `examples/router` | `wf.NewEnum`, `wf.Agent.RouteCases`, `wf.Cases`, `wf.On` |
| P3 | `examples/fanout` | `wf.NewMap`, `wf.FromObservation`, `bd.Ready`, `wf.AllSettled` |
| P4 / E8 | `examples/orchestrator` | `wf.FromOutput`, `wf.NewMap`, `wf.Map.Parallel`, `wf.Map.Requery` |
| P5 / E9 | `examples/critique` | `wf.MaxVisits`, `wf.Retry`, `wf.RetryPolicy.NoProgressAfter`, `wf.NewGate` |
| P6 / E10 | `examples/agentless` | `wf.NewMap`, `wf.NewCommand`, `wf.NewCompute`, `wf.ComputedOf` |
| P7 / E11 | `examples/tournament` | `wf.NewMap`, `wf.Quorum`, `wf.Def.Param`, `wf.Param` |
| P8 / E12 | `examples/approval` | `wf.NewGate`, `wf.Reserved`, `wf.Gate.Presents`, `wf.Gate.Timeout` |
| P9 / E17b | `examples/escalation` | `wf.RetryPolicy.OnClass`, `wf.Ctx.Attempt`, `wf.Cases`, `wf.Out` |
| P10 | `examples/broaden` | `wf.Agent.RouteCases`, `wf.Cases`, `wf.Out`, `wf.PromptText` |
| P11 / E3 | `examples/resumable` | `wf.DoneWhen`, `fsx.Exists`, `wf.Def.RunKey`, `wf.Uncertain` |
| P12 / E13 | `examples/typedexit` | `wf.NewEnum`, `wf.Cases`, `wf.MaxVisits`, `wf.Budget` |
| P13 | `examples/checkrepair` | `shell.Command`, `shell.Run`, `wf.RunResult`, `wf.Out` |
| P14 | `examples/park` | `wf.PolicyAnswerable`, `wf.Policy`, `wf.PolicyAnswer`, `wf.PolicyRule.Park` |
| E2 | `examples/noprogress` | `wf.Retry`, `wf.RetryPolicy.NoProgressAfter`, `wf.RetryPolicy.OnStalled`, `wf.RetryPolicy.Fingerprint` |
| E14 | `examples/dispatch` | `wf.Agent.Dispatch`, `wf.Model`, `wf.Effort`, `wf.AgentType`, `wf.Map.Parallel` |
| E15 | `examples/unavailable` | `wf.Unavailable`, `wf.RetryPolicy.OnClass`, `wf.Retry`, `bd.Create` |
| E16 | `examples/stale` | `wf.NewWait`, `git.Head`, `wf.Check.Observes`, `wf.Migration` |
| E17 | `examples/router` and `examples/escalation` | `wf.Cases`, `wf.NewEnum`, `wf.RetryPolicy.OnClass` |
