# Coordinator protocol

How an agent session (Claude Code, Codex, agy (the Antigravity CLI), opencode or any
other harness that can spawn subagents) drives a workflow run. The coordinator owns no logic of the
workflow: the engine decides what happens next, the coordinator spawns what it
is told to spawn and relays questions to the human. Every successful `run`
prints the coordinator bootstrap. `wf/coordinator_doc_test.go` checks that every
verb, flag, exit code and `next` field named here exists, and that
every verb and exit code of the CLI is named here.

Below, `workflow <verb>` is the client binary. A single-registration binary such as `./super` carries the same verbs
under its `wf` group, `./super wf <verb>`, and fills in the run id. Every
command line the engine prints (the bootstrap, a spawn line, a brief, a fix
line) is already in the form of the binary at hand, and every argument in it is
already shell-quoted, unit names and claim tokens included: run it as printed
and never re-quote, unquote or rebuild it. Every run-driving verb takes
`--state-dir`; pass the same absolute directory to all of them.

The engine does the scheduling, the stall detection, the retries and the
bookkeeping of who each child is. It sets no cap on how many units run at once.
That limit is the harness's, and only the coordinator knows it.

## 1. Start and bootstrap

Start a run with either a named binary or a multi-definition binary:

```
./super run <def> --param k=v --state-dir <abs> --workspace <abs>
workflow run <def> --param k=v --state-dir <abs> --workspace <abs>
```

Every successful `run` prints the run ID alone on stdout and the coordinator
bootstrap on stderr. Capture stderr too when launching a coordinator. It contains
the actual executable and an already shell-quoted loop command bound to that run
and absolute state directory: `wf next` for a named binary, or
`next <run>` for a multi-definition binary. Execute the printed line
unchanged; no prompt file or placeholder substitution is needed.

The bootstrap's first line names the protocol version (`coordinator protocol 4
(next blocks)`) and says the text replaces anything the coordinator remembers
or was told elsewhere about this tool. A removed verb (`wait`, `submit`,
`fail`) is refused with what replaced it, in the binary's own form, so a
remembered command fails with its fix. Then it prints the run header (run id
and state dir), one harness line, the rules for the run, the loop of
section 2, and one rule: report when `next` prints the ended
instruction or asks for a person, and quote it. Never choose a reserved answer for the person. On a run whose detach mode is none it adds one line:
a long command step runs inside `next`, so give `next` your longest tool
timeout. Every other rule arrives with the output that makes it apply.
A named binary also offers `./super next` to print the bootstrap again.

Ahead of the rules the bootstrap says what the run does, as plain fact, so a
permission checker reading only this context sees the expected work: an
unattended workflow run in the workspace, the commands the coordinator runs
(`next`, which blocks for minutes while units work, `status` and the recovery
commands it prints), the harness's own sub-agent tool, and the work the
definition's units do, in the definition's own words (`d.Describe`, left out
when a definition gives none). Every spawn brief's third line says the same
for the unit: the commands its prompt prints (prompt, validate, done, failed),
by default as printed, one per line, and that work.

The rules for the run, as the bootstrap prints them:

> Rules for this run. Move this run only with the workflow engine's verbs, the commands it prints; ordinary shell work (mkdir, reading files) is your own. By default run a printed command as printed, one per line, with no wait loop around it: the engine's commands wait by themselves. If a command refuses, say what blocked you and what you need: a child with its failed command; a coordinator goes on with next, and reports to the person who started the run when a command asks for a person. Never edit or delete lock, journal or state files (see the lock verb), use flags that bypass a sandbox or approvals, reach a refused permission or approval another way, or address another run's state. The workflow moves this run's tracker items: change one only if your task's prompt says so. When you hand out this run's units, start each sub-agent with your own harness's sub-agent tool. Do not start another agent CLI (codex, claude, hermes) to do a unit, except through the engine's child dispatch (the call next prints), unless the workflow or the person who started the run explicitly asks you to. The workflow, not your judgment, decides when a phase or the run is over: keep following next. End your turn only if your child is running, the run ends, or next says to wait or stop.

The tracker sentence appears when the definition declares a typed Beads
observation or effect, including in a nested definition. Workflows that use
Beads through raw shell commands or agent work alone put their tracker
policy in the workflow prompt.

Every agent prompt the engine issues opens with them, ahead of the
definition's own text, which stays as written. Every spawn brief ends with a
short form of them, so a child that never fetches its prompt has them too:

> Rules for this run: do your assigned task with ordinary tools, and change workflow state only through the workflow commands you are given for this run. Never edit the lock, journal or state files of the workflow, bypass a sandbox or approvals, or start another agent CLI except through the child dispatch of the engine. The workflow moves the tracker items of this run: change one only if the prompt of your task says so. If you are blocked, run your failed command with --reason saying what blocked you and what you need.

Running again with the same run key or `--run-id` resumes the run, so a crashed
coordinator restarts from the bootstrap. `--model-map key=model[,key=model]`
sets the models the run's children spawn with. A key is a tier (`light`,
`standard`, `strong`, `top`), `all` (every tier), `<tier>.effort` or
`all.effort` (an effort: low, medium, high, xhigh, or `harness default`), or a
model the harness's tier table or an untiered state names (rewritten once, not
chained). Per tier a tier key wins over `all`, `all` over a model key, and a
model key over the table; `<tier>.effort` wins over `all.effort`. Any other
key is a usage error listing the valid ones. The map can change only at a
harness segment's boundary (below): `resume --model-map` after `wf stop`,
`dev rewind --model-map`, `fanout --model-map`. A resume with no map keeps the
segment's map on the same harness; on another harness the map is dropped, that
harness's defaults apply, and a degraded `tiers` record says so. The bootstrap
and `status` print the segment's resolution after the harness line, e.g.
`models: strong=gpt-6.1-terra top=gpt-6.1-terra (--model-map all=gpt-6.1-terra)`
or `models: light=haiku standard=sonnet (claude defaults)`.
A harness that takes only some model values refuses any other: agy takes
`inherit`, `flash_lite`, `flash` and `pro` (every agy tier defaults to
`inherit`, the coordinator's own model), and a real model id is refused.
opencode takes none: its `task` tool has no model parameter, so every opencode
unit runs on the coordinator's model, no tier resolves, and a `--model-map`
(given or carried over) is not applied; one degraded `tiers` record per run or
segment says so. The
verb that receives a map checks its keys and efforts; a value is checked once,
against the harness that binds the segment: when that segment's first
coordinator `next` binds it, and for the first segment by `run`, against the
starting harness. A value the binding harness refuses fails that `next` with a
usage error (exit 2) naming the key, the value and the accepted values, and
binds and journals nothing. Then re-issue the boundary verb with a corrected
map, `workflow resume <run> --model-map ...` (accepted while the segment the
refused map opened is still unbound, though the run is not stopped), or
`dev rewind --model-map ...` or `fanout --model-map ...` again, and call
`next`. Never retry `next` with the same map: it fails the same way.
`--delegate gate=value` delegates a gate's answer to the coordinator (section
6); `--options file` carries the definition's options.

The harness is read from the environment. `run --harness <h>` names it
explicitly; a value the environment positively contradicts (it shows another
harness) is refused, and `generic` always fits. On a harness the engine has no
adapter for, start with `workflow run <def> --harness generic --model-map
all=<model>` (or one key per tier) and the run's children get the models the
map names.

Model tiers are hints. A harness with no tier table runs those children on the
session's model, and the run journals a degraded `tiers` record that the report
counts. Run creation never refuses for a missing table.

## 2. The loop

```
workflow next <run>   -> do what it prints, then run it again
```

`next <run>` is the only call in the loop, and it always blocks (H1): it
returns when the run has something for the coordinator. `--wait`, the
earlier protocol's spelling, is accepted and changes nothing. Leave `n` out: the engine
offers what your harness can start. Where the harness adapter reads the
harness's own limit on concurrent subagents (Codex: `[agents] max_threads` in
the nearest `.codex/config.toml`, else `$CODEX_HOME/config.toml`), it offers up
to that limit less your children running or still starting, and prints the
count and where the limit came from. That is the harness's number, never a cap
of the engine's. Where it reads none (Claude Code, agy, opencode, any other harness), it offers
every ready unit with the line `start all of these`. You may still pass a smaller `n`
(`next <run> <n>`, within your harness's own limit); `n` may be 0, and
the call still reports gates, stalls, a stop and the end.

If you run fewer children at once than that (your instructions cap them, or a
provider refuses more), declare it on every call: `next <run> --children <k>`.
The declared count replaces the harness's limit. `next` then never offers more
than `k` less your children running or still starting, with or without `n`.
While `k` of your children run it offers nothing and keeps blocking until one
finishes, a gate or the end, so you never sleep or poll between children. An
offer you hold while `k` children run waits for one of them: it is not
re-offered, not counted toward the three below, and never named stuck for that
wait. A child whose lease runs out (section 8) is reported as `stalled`, with
the claim its child fetched and two commands, whether or not your cap is full.
The engine only asks: check your child yourself, then answer `heartbeat
active`, or, only once you confirmed it exited, `heartbeat died`, which frees
its place; when you cannot tell, answer neither.

Of the offered units, spawn as many as your harness allows (count the
subagents and other native work already running in this session). You keep
no list. Nothing you were offered is lost, however slow your spawn (J4); it
counts as one of your children starting. A unit you were offered and could
not start (the harness refused the spawn) comes back when your blocking `next`
reaches the end of its wait with nothing else and the unit is still
unclaimed: the call then prints `not started yet: start these too`, the units
again under their names. These are the offers you already have, with the same
claim tokens: nothing new is handed out, so start each child only if its
earlier start is no longer running. Each unit's own line says the same; two
children of one offer would both do the work and neither would be told to stop. However many waits end this way, the run
records one hand-out and one `never started` note per unit, and another note
only when what the spawn printed changed. A unit you were offered also comes
back, marked `not started yet:`, in any `next` once a child of a unit a later
`next` offered you has fetched its prompt while no child of this one has (you
moved past it, so its spawn was refused or left out), or once 5 minutes passed
since the offer with no child of it fetching; these are fresh offers, under
new claim tokens. It comes back as often as that happens: no count of offers ends your session or takes the unit from you.
Start its child again each time. Where your harness records what a spawn
printed, the offer shows the last lines of the earlier one, so you can see why
the child did not start. If the same child fails to start repeatedly, say what
its spawn printed and keep driving the run: the other units are still yours to
start. Each of these offers waits for its evidence (a later offer's fetch, the 5
minutes, or a whole wait), so `next` never returns one at once, over and over.
Whichever child of the unit fetches
first owns it; the other is told another child owns the unit and stops.

The call returns as soon as the run has something for the coordinator.
Otherwise it blocks for as long as the harness allows one tool call, less a
10 s margin (H2): Claude Code's Bash tool 120 s (or `BASH_DEFAULT_TIMEOUT_MS`),
Hermes's terminal tool 30 s, the timeout its models pass on each call (or
`TERMINAL_TIMEOUT` when shorter); pi's bash tool 30 s, the shortest timeout
its models were seen to pass (pi itself has none); opencode's bash tool 120 s
(or `OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS`); 100 s where the adapter
reads no limit (Codex, agy, any other harness); `--timeout` overrides it. agy
cuts no command off (its adapter reads no limit), but it backgrounds a command
still running when its call's `WaitMsBeforeAsync` runs out and delivers the
output later in a `task-<n>` message: run the blocking `next` with
`WaitMsBeforeAsync` at least the engine's blocking wait. If it was backgrounded
anyway, end the turn and act on the `task-<n>` message, which carries the
output. When that output looks cut (an agy tool result keeps 9,838 bytes, head
and tail), run `workflow status <run>` (read-only), never the blocking command
again. A busy
run lock is waited for within the same budget, across all of the call's
locks (where the adapter reads no limit and no `--timeout` is given, each
lock is waited for up to 2 minutes, with one progress line naming the
holder after 5 s); a next still locked out at its end prints the
nothing-yet line, and any other verb that changed nothing exits 9 with "the
run is busy" and a tail that says to run the same command again: a busy run
is never a stop. `workflow lock <run>` (read-only, `--json` for the
LockState) says whether the kernel's lock table shows a call holding the
run lock now, read without taking it, and names the lock's last writer,
with its release when that writer stamped one. It never says the lock is
free: a table that lists no holder is "no visible holder" (a hold it does
not show is still a hold, and the engine's verbs wait for it), and a table
it cannot read is unknown. A record with no stamp says only that no release
was recorded. There is no clearing verb: the kernel lets a dead holder's
lock go by itself.
Then it
prints `nothing yet after <wait>: run next again; next returns as soon as
there is something, so sleeping or checking status in between gains nothing`. A child finishing is not
an event when nothing new became ready, unless ready units or unclaimed offers
wait: then the call returns them, or `a child finished and units are ready: run
next again`. Neither is a wait state or a running
command: the engine drives those itself. Never call `status` in the loop;
`status` is for a human or a resume.

What it prints, one entry per line, each with the rule that applies to it:

| line | do |
|---|---|
| `start a child agent for <name>` | the offer of one unit: start a fresh child agent with the harness call it names, its prompt the text between the two `-----` lines (the brief), one child per offer, all of them in parallel in one step (section 4). The brief is the child's: never run it yourself |
| `<n> more are ready` | ready units this call left out so its output fits your harness's tool-output limit: start the offers above, then run next again |
| `This run is waiting for a person at <gate>: ...` | a reserved question: ask the person, wait, then relay only their reply using the printed scoped command (section 6) |
| `human-gate <state>: this needs a person` | a non-reserved question with its `answer` line (section 6) |
| `manual-verification <state>: yours to settle` | an effect to check in the world yourself and settle with your evidence, with the `settle` line under it (section 7) |
| `manual-verification <state>: this needs a person` | an effect outside the run (a push, a merge to base) for a person to check and settle (section 7) |
| `recovery <unit>:` | a unit that failed, offered to you before its failure route (section 7a): read the evidence, choose, and run the `workflow recover` line of your choice |
| `blocked <state>: <reason>` | the run waits on a human at a halt: report the cause and end the session. The line names the `retry` and `retry --stop` answers (section 7). A unit that failed with no route is offered to you first, as `recovery <unit>:` (section 7a) |
| `waiting:` | every remaining unit is held for a human: report each `held:` line and its `fix:` line, and end the session (section 7) |
| `stalled <name>: its child ... has shown the engine no sign of life since <time>. Is it still running?` | look at that child with your own harness, without stopping it, then run the `heartbeat active` line, or `heartbeat died` only for a confirmed exit; if you cannot tell, run neither and run `next` again: it waits for a sign of life or the recheck, then asks again (section 8) |
| `stalled <name>: you answered active <n> times in a row, and its child ... showed the engine no progress in between.` | look in that child's own thread: work you saw since the last question, `heartbeat active <name> --note '<what you saw>'`; a pending prompt, resolve it and `heartbeat active`; an error or a confirmed exit, `heartbeat died`; cannot tell, run neither and `next` again; when the recheck asks again and you still cannot tell, `heartbeat died` (section 8) |
| `stopped:` | the run was stopped (section 9): start nothing, report it, quoting the text and the totals line under it, and end the session (exit 7) |
| `ended: <terminal>, outcome <outcome>` | the run is over: write your final report now, quoting this line and the totals line under it (exit 7) |
| `not started yet: <unit> was offered to you earlier ...` | heads a unit you were offered and never started. Under a new claim: start its child again, however often it comes back; a second child that starts is told the unit is taken. When the line says it is the same offer under the same claim: start its child again only if its earlier start is no longer running, since two children of it would both do the work (section 4) |
| `offered <k>: ...` or `start as many of these ...` | how many to start of the spawn lines under it (with `n` left out) |
| `a child finished` | a child settled and ready units wait beyond your `n`: run next again |
| `nothing yet after <wait>` | nothing for you in as long as your harness allows one call: run next again at once; sleeping or checking status in between gains nothing |
| `continue driving` | act on the instructions above, then run `next` again before ending your turn if none of the children you started is running |

`workflow next <run> <n> --json` (or `all`, every ready unit) hands out at once,
without blocking, and prints the JSON array of section 3, for a program that
parses it; never the loop. With `--json` a count is required.

If the run is not ended, nothing is running and `next` hands out nothing and
prints no `blocked` or `waiting` line, the run is blocked: report it and stop.
`workflow status --full <run>` ends with a `nothing ready:` line that says why.

## 3. Next output

`workflow next <run> <n> --json` prints a JSON array. Each element has
these fields.

- `run`: the run id.
- `state`: the qualified state.
- `kind`: `agent`, `human-gate`, `wait`, `manual-verification`, `recovery`, `waiting`, `stopped` or `ended`.
- `attempt`: 1-based attempt number.
- `name`: the unit name of an agent unit. It is the handle of every later call
  about the unit.
- `class`: the unit's qualified state.
- `brief`: the fixed one-line spawn brief (section 4). An agent unit carries no
  `token`, `prompt` or `submit`: its child fetches its prompt itself.
- `output`: the absolute allocated result JSON file under the run's `out/` directory.
- `token`: identifies the attempt of a non-agent instruction.
- `prompt`: the resolved text of a non-agent instruction: the gate's question,
  the effect to check, the holds or the stop.
- `inputs`: optional name to value map (also rendered inside `prompt`).
- `submit`: the exact command line that answers or settles.
- `child`: the child instance id; absent for the run itself.
- `dispatch.model`, `dispatch.effort`, `dispatch.agentType`, `dispatch.tier`, `dispatch.stallAfter`:
  spawn hints for the harness of the caller, each absent when unset. A run
  created under one harness and driven from another gets the caller's agent
  type and tier models. `stallAfter` (nanoseconds) is the state's stall
  threshold, read by the engine; nothing is done with it at spawn. The
  segment's `--model-map` is already applied, at hand-out: a unit handed out
  after a boundary spawns with the new segment's models, and its `handed_out`
  record journals the model and effort it was handed with (`Spawn`; a
  record from before has none, and a replay then keeps the issued dispatch).
- `gitAuthority`: agent only, absent unless the state declares one: `refs`, `allowed`, `forbidden`
  and `section` (the AGENTS.md section naming the same grant). The definition's client sets it; the
  engine only carries it.
- `context`: agent only, always `fresh`: spawn the subagent without the coordinator's
  conversation (section 4). No unit chooses its own context.
- `workspace`: the run's workspace directory.
- `previousRefusal`: agent only, attempt 2 or later: the reasons the state's last
  refused output was refused; the child's prompt says the same. Informational for you.
- `notStarted`: agent only, true when you were offered this unit before and no child of
  it started: children of units a later `next` offered you did, or 5 minutes passed
  since the offer, or the end of a whole wait passed with it unclaimed. Start it now;
  the text output heads it with `not started yet:`.
- `sameClaim`: agent only, with `notStarted`, true when this is the offer you already
  have, shown again under the same claim token (the end of a whole wait). The engine
  tells two children of a unit apart by their claims, so two children of this offer
  would both do the work and neither would be told to stop: start one only if the
  earlier start is no longer running. Absent on a fresh offer under a new token,
  where a second child that starts is told the unit is taken.
- `errLog`: agent only, harnesses whose launch line writes the child's stderr to a file
  (pi): the file this offer's child writes it to. The launch line in the text output
  already redirects to it.
- `prevErrLog`, `prevErrTail`: agent only, with `notStarted`, for those harnesses: the
  earlier offer's stderr file and its last non-empty lines (at most 3), where why that
  spawn did not start is.
- `handoffReceipt`: the shared receipt for all person handoffs in this response
  on an attached run. Show every request to the person, then run the printed
  acknowledgement command. If delivery is lost, the current coordinator's next
  returns the same receipt while those handoffs are current.
- `breakpoint`: present when the run stopped on arrival at a state selected by
  `--halt-before`. Report the state, journal sequence and reason, then end the
  coordinator session. An operator can continue the run with `wf resume`.
- `counts`: `stopped` and `ended` only (a reserved question's too, though the text
  output prints the question alone): the run's subagent totals exactly as `workflow
  status` prints them. Your report quotes it.
- `effect`: manual verification only, the effect id to settle.
- `person`: manual verification only, true when the effect is outside the run (a push, a merge to base) and a person settles it; absent, you settle it yourself.
- `transient`: manual verification only, true when the effect's check failed transiently; it is checked again on the next call.
- `retryAfterSeconds`: wait only, how long the engine backs off. `next`
  never prints a wait: it sleeps through it.
- `question`, `answers`: human gates only.
- `intValues`: optional integer declarations on reserved answers, with effective
  minimum, maximum and proposed default; shown in the person-handoff text.
- `visit`: human gates only; pass it as `answer --visit` (with `--child` for a Map child) so a stale or misdirected answer is rejected (exit 4).
- `delegated`, `delegatedAnswer`: gate metadata recording the starter's delegation. At a reserved gate, follow the person-handoff instruction even when this metadata is present.
- `coordinatorAnswers`: the gate is a `wf dev fanout` fork's target, the one gate whose answer this fork measures: choose the answer yourself, as the person who started the run would, and run the printed answer command with it. Only fanout sets it; every other gate stays the person's.
- `deadline`: timed gates only, when the gate takes its timeout edge; `retryAfterSeconds` counts down to it.
- `terminal`, `outcome`, `cause`: `ended` only, the terminal the run reached, its outcome and why (the last failed observation or effect error on the path into it, else the label of the edge into it). It is the single element `next` returns once the run has ended; `next` then exits 7. Stop.
- `holds`: `waiting` only, one entry per held unit: `child` (absent for the run itself), `name`, `state`, `cause` and `fix`, the whole `retry` command that releases it once its cause is cleared (the prompt says to clear the cause, then prints that command on its own line). It is the single element `next` returns while the run's only remaining work is held units; the run is `blocked`, never `ended`.
- `recovery`: `recovery` only (section 7a): `unit`, `cause`, `restarts` (restarts left for this unit in the run, for a spent budget), for a child that ended without done also `transcript`, `lastTool`, `lastMessage`, `result` (its result file written or missing) and, where your harness can continue a child, `resume` (the harness call carrying the fixer text); and `commands`, the `workflow recover` line of each choice that applies.
- `resume`: `ended` only, when the entry gives one: the command that continues the work as a successor run (`run` on an ended run never reopens it; it starts `<run>.2`, carrying the records over). The successor keeps the ended run's start inputs that the call omits: its asset selection (the selected root and each definition's root, with params derived from assets derived from that selection), snapshots, paged prompts, model map, options, delegations and workspace (a flag the call names wins), so the printed command runs as printed, from any directory. It names the run it continues (`--continue <run>`): params are derived from that run's recorded assets, never this call's default tree, and a run key outside its chain is refused before any run is created or attached.
- `advice`: set on every instruction but `ended` and the holds-only `waiting` once the run has stepped down to detach none mid-run: the bootstrap's `detach: none` line. `next` prints it once after the instructions. Long commands now run inside `next`; give it your longest tool timeout.

## 4. Agent units

All units of one `next` call are independent (Map children are separate units),
so spawn them in parallel in one step.

Each unit is an offer addressed to you, then its brief between two `-----`
lines, addressed to the child (I1):

```
start a child agent for <name> with <call>. Its whole task is the text between the ----- lines, passed verbatim with its command first; the unit is a child's to run, which keeps its work out of your context:
-----
workflow prompt <run> --claim <token> --state-dir <abs>
You are a child agent for one unit of work. Run the command above, then do what it prints.
-----
```

For a run started with `--paged-prompts`, the child's `prompt` response is page
one of an immutable packet. The child fetches each printed continuation in a
separate command, and required indexed references through `wf read`. The engine
tells the child to read every required page and trusts its `done`: neither
`validate` nor `done` counts the pages fetched, and pages have no rate limit or
required order. Status shows fetch coverage, which is not a claim that the
model read the text.
The coordinator still passes the brief unchanged.

`<call>` is your harness's spawn tool with the model, effort and agent type
already in it as that tool's own parameters, and its prompt argument (Codex:
`message`) written as `<the text between the lines>`: pass the brief there,
verbatim:

- Claude Code: `Agent(subagent_type=..., model=..., effort=..., prompt=...)`.
  Any non-fork subagent type gives a fresh context; a fork inherits the parent
  conversation.
- Codex: `spawn_agent(message=..., model=..., reasoning_effort=..., fork_turns="none")`.
  `fork_turns: "none"` gives no parent history (under MultiAgentV2 it defaults
  to `"all"`). Source: https://github.com/openai/codex/blob/rust-v0.167.1/codex-rs/core/src/tools/handlers/multi_agents_spec.rs
  (Codex CLI 0.167.1; the spawn schema is version-dependent).
- agy: `invoke_subagent {"TypeName":"self","Role":"wf-unit","Prompt":<the text between the lines>}`,
  with `"Model":<tier>` added only when the offer's line has it (a tier other
  than `inherit`). Effort and agent type have no agy argument. One call per
  unit; several units may be several `invoke_subagent` calls in one response or
  in several. The engine pairs each call with its result in call order, and a
  child whose result is written is found while a sibling in the same response
  is still launching (a launch takes about 11 s): a sibling still pending does
  not hide a written one.
- opencode: `task(description="run unit", subagent_type="general", prompt=<the text between the lines>)`.
  The `task` tool takes no model or effort. Never pass `background`. Several
  units may be several `task` calls in one response.
- Any other harness (one the engine has no adapter for, run as `generic`): the
  call is in words: spawn a subagent with fresh
  context, on the model it names if any, its whole prompt being the brief.

An unset hint is left out of the call and means the harness default. Never
invent a hint.

The brief is the same shape for every unit, the two lines between the rules
above. Pass it verbatim as the child's prompt. Running its command yourself is
refused where your harness tells your session from a child's (Codex, Hermes,
pi, agy, opencode): a first `prompt` fetch from your own session says "this is a
child's command: start a child agent with the text between the lines next
printed for it". There is nothing to shorten and nothing to add: everything the
child needs comes from that command. `workflow prompt` claims the unit and
prints one page, always under 2 KB: who the child is, the finishing rules (how
to finish with `workflow done <run> <name>` or `workflow failed <run> <name>
--reason <text>`, the blocked class, the escalation rule of section 12, and the
rule to run long commands with the harness's background-task tool rather than
a wait loop), and the path of
the instruction body, `<state-dir>/runs/<run>/prompt/<hash>/prompt.md`. The
child reads the body with its harness's file reader, which pages a large file
itself. The body ends with a `## Finishing` block (the same rules, with the
refused-versus-ran-and-failed rule) and ends by repeating the finishing
commands. Every command the engine prints, there and everywhere else, is one
plain command on its own line, exactly as it is to be run: a `cd` to the run
root, then the command, with absolute paths and no loop, `$(...)`, variable
expansion or `bash -c`. No rule a child must obey is only in the body.

The engine tells the child to read the whole body first and trusts its `done`:
it does not look in the child's transcript for a read, and nothing about the
read gates `validate` or `done`.
Before `done` the child runs `wf validate <run> <name>`: it checks the result
exactly as `done` would (schema, artifacts, checks, route), prints the same
refusals and records nothing, so the child fixes its output and validates
again until it says ok. A refused `done` spends the attempt. A refusal that
says the attempt is still open (the first `done` of an attempt with no file at
the allocated output, or a result path error) tells the child what to
do: it does it and runs the command again, and never re-runs a command that was
accepted. In an agent's shell such a fixable refusal (a `validate` refusal, or
one that keeps the attempt open) ends with "This refusal is yours to fix: fix
what it names and run the same command again. If the same refusal comes back
twice, run your failed command with --reason saying what blocked you and what
you need, so the run can route it". Any other error a child cannot
resolve ends the same way: it runs `failed --reason`, and the run routes the
unit. A `prompt` or `read` that fails after an earlier fetch bound the child's
claim (a later page that cannot be delivered) ends with "run failed with the
same --claim and --reason ...": the child holds the unit. Only a child with
no `failed` to run (its `failed` was itself refused, its attempt is spent and
already recorded, or its fetch bound nothing, so it never took the unit) is
told to tell its coordinator the exact error and end only its child task. The
finishing rules say the same of a spent attempt: do not run `failed`.
Every refusal of a result names what it objects to in the result's own terms:
the field, the value or path, and what to give instead. A result may hold no
path in the run's state directory, with one exception: citing is not
claiming, so a value that is an artifact the engine allocated for a step of
this run whose submission it accepted, or a file inside one (a reviewer
returning the implementer's recorded test log as evidence), is accepted and
journaled as a run-relative `$wf.path` reference, so the journal relocates
with a fork and the citation resolves against the run's own directory. A
citation is a reference, not a read: it resolves to its path string whether
or not the file is there or kept its type, so replay, harvest and calibration
decode it as the run did. A path a submission merely named as an artifact is
not citable. A result that gives a key twice, or two keys differing only in
case, is refused as ambiguous before acceptance. Any other run-owned
path is refused, so is a result that writes the `$wf.` reference form itself,
and an absolute path in the workspace is refused with its relative form.
The prompt and the outputs never pass through the coordinator.

The prompt names the unit's allocated JSON result file under
`<state-dir>/runs/<run>/out/`. The child writes its result there and runs
`done <run> <name>` without `--output`. The optional `--output` must name that
same normalized path; stdin and unrelated files are rejected. The child works
in the run's `workspace` and uses absolute paths under it for task files.

The coordinator never runs `done` or `failed` for a subagent, so outputs stay
out of its context. The subagent reports only its result `class` back.

## 5. Claims and identity: the engine finds its children

A unit is claimed when its child's prompt fetch arrives with the claim token.
Handed out is not claimed, and nothing expires before the claim: there is no
hand-out deadline and no spawn window to race. A handed unit that no child
fetched is offered again only in two cases:

- a `next` from a newer coordinator session (the first coordinator is gone),
  with a degraded `orphan` record ("handed out again"), or
- for a coordinator whose session the engine cannot read (an unknown harness),
  the hand-out window (2 min) passed while no unit of the run is claimed (the
  coordinator never spawned it). A coordinator with a session gets its own
  hand-out again only as `not started yet`, at the end of a wait (above).

A re-offer keeps the unit's name and mints a fresh claim token; the end of a
wait that only shows you your own unstarted units again mints none (above). A
late child from the first offer still claims with its own token. A second child for an
already-claimed unit is refused at `prompt`: "another child already owns this
unit; exit without working".

The engine identifies each child itself. `workflow prompt` reads the child's
own environment through the harness adapter and records the claim; the rest is
found lazily, when the engine next drives the run, at `done` or `failed`, and on
resume. A transcript not flushed yet is searched again later.

- Claude Code: a subagent's shell carries its coordinator's
  `CLAUDE_CODE_SESSION_ID` and no id of its own. The transcript root is
  `CLAUDE_CONFIG_DIR`, else `~/.claude`. The engine searches
  `<root>/projects/*/<session>/subagents/agent-*.jsonl` for the claim token;
  the one file that holds it is the child's transcript, and the agent id is in
  its file name. The coordinator's own transcript, which also holds the token,
  is not under `subagents/`. Claude Code's environment does not tell a
  subagent from its coordinator, so a Claude child's coordinator verbs are not
  refused.
- Codex: the child's `CODEX_THREAD_ID` is its agent id and `CODEX_SESSION_ID`
  is the coordinator's session; a thread unequal to the session is a child.
  The transcript is
  `$CODEX_HOME/sessions/*/*/*/rollout-*-<thread id>.jsonl` (`~/.codex` when
  `CODEX_HOME` is unset). A prompt fetched from the coordinator's own session
  (`CODEX_THREAD_ID` equal to the session) is refused: "this is a child's
  command: start a child agent with the text between the lines next printed
  for it".
- Hermes: a `delegate_task` subagent writes a live transcript log,
  `$HERMES_HOME/cache/delegation/live/<delegation>/task-<n>.log`
  (`~/.hermes` when `HERMES_HOME` is unset), whose goal line carries the brief
  and so the claim token. The one log written since the hand-out that names it
  is the child's transcript; its agent id is `<delegation>/task-<n>`. A brief
  that names two claims vouches for neither. Hermes logs a goal and kickoff cut
  at about 500 characters (`…(+N chars)`), keeps no full copy, and records
  neither the child's session nor its parent's, so a claim command placed after
  other text is not visible and the child's log is not found (its unit then
  runs unverified): pass the brief verbatim, its command first. The log's
  `final | end status=` line is its end marker; a child turn that failed ends
  on it too (`final | end status=failed exit_reason=... error: ...`).
  Hermes's failed-turn notice ("This turn did not complete. ...") goes to its
  session database, not to this log. Only the initial goal and kickoff context
  (including their continuation lines) count for the claim token: a token
  quoted in a later event or tool result is not a claim. The brief starts
  with its exact prompt command, with `--claim` before the state-directory
  flag, so the token survives a truncated kickoff preview; directory advice follows.
  A child has its own `HERMES_SESSION_ID`, and Hermes stamps
  `HERMES_DELEGATED_CHILD_CONTEXT` into every subprocess of a
  `delegate_task` child: with it the caller is a child, without it the
  session's own agent, the coordinator, whose own `prompt` fetch is refused as
  on Codex. Two cases stay unknown: a kanban worker (`HERMES_KANBAN_TASK`
  set), whose terminal carries the marker too and may coordinate, and another
  harness running inside a Hermes terminal (its own agent variables set),
  which shares the outer `HERMES_SESSION_ID` with its children. Hermes keeps
  token usage in its own database, which the engine does not read, so no usage
  is recorded; nor does it assume its children die with their coordinator.
- pi: there is no subagent tool. A child is a separate `pi -p` process the
  coordinator starts in the background with its bash tool, exactly as the
  offer prints it, as one bash call of three lines:

  ```
  SUPER_PI_CHILD='wf-<claim>' SUPER_PI_PARENT="$PI_SESSION_ID" nohup pi -p -ne --session-id 'wf-<claim>' [--model M] [--thinking E] <<'WF_BRIEF' >/dev/null 2>'<state dir>/child-logs/<run>/wf-<claim>.stderr' &
  <brief>
  WF_BRIEF
  ```

  with the brief pasted unchanged as the middle line or lines: a quoted
  here-document, so nothing in it needs quoting. The values around it are
  shell-quoted where they need it, as above. Each part
  is there for a reason. `SUPER_PI_CHILD` puts the child's session id in its
  environment, so its processes can be found (pi retitles its process `pi`).
  `-ne` keeps the coordinator's extensions out of the child. The brief goes on stdin, which print-mode pi reads to its
  end, trims and takes verbatim as its first message: it is never read as an
  option or an `@` file, and the quoted delimiter means the shell expands
  nothing in it. `2>` is
  the file the engine chose for this offer, `<state dir>/child-logs/<run>/wf-<claim>.stderr`,
  created before the offer is printed: why a launch did not start is there. A
  launch whose `2>` path differs from the recorded one is not matched to the
  unit's child. The
  launch may be preceded by one `cd <absolute dir> && `, and then counts only
  when the child's session started in that directory. When
  `next` re-offers a unit that did not start, it names the earlier launch's
  stderr file and quotes its last lines (`prevErrLog`, `prevErrTail`): read
  them before launching again. pi gives every bash command its own session's
  `PI_SESSION_ID`, replacing inherited ones, and passes `SUPER_PI_PARENT`
  through: a caller with `SUPER_PI_PARENT` set is a child (its session is its
  agent id, the coordinator's is the session), without it the session's own
  agent, the coordinator. Sessions are `<root>/sessions/<cwd slug>/<time>_<id>.jsonl`
  under `PI_CODING_AGENT_DIR`, else `~/.pi/agent`; the header names the id and
  when the session began, and the first user message is the prompt the
  process started with, in full. The engine finds a child's session, for its
  liveness and end, only when the
  coordinator's own session file holds a bash call, since the hand-out, in
  exactly the shape above (read literally: background `&`, `-ne`, no option
  but `--session-id`, `--model` and `--thinking`, no message argument, the
  brief as the here-document) whose brief names the claim
  and no other and whose session id is `wf-<claim>`; and exactly one file of that session
  (named and headed by the id, begun at or after the launch) began with that
  brief: every same-id file is checked, and two that match find nothing. A
  sibling's file holding the launch does not count. None of this admits or
  refuses a child's call: a child holding its unit's claim token is served
  whether or not its launch is found.
  The engine reads the records at `<agent dir>/sessions/<one dir>/`. A pi
  session whose records are elsewhere (`--session-dir`, `--session <path>`, a
  `sessionDir` setting or `PI_CODING_AGENT_SESSION_DIR`), one with a relative
  agent dir or session file, and one started with `--no-session` (no records
  at all) run the same; no usage is read from records the engine does not
  find. A brief the launch line cannot carry is
  refused at hand-out, which stops the unit with the cause: a line that is
  exactly `WF_BRIEF` (it would end the here-document early), or a first word
  that begins with `/` and holds no other `/` (pi would read a prompt
  template); a state dir or run id outside `[A-Za-z0-9/._-]` is refused too,
  for the `2>` path. A single quote is carried as is.
  pi writes no end record, so the end is read from the session's last
  message. `aborted` with no tool call ends the child at once (the process
  was interrupted). `stop`, a final `error` and `length` with no tool call end
  it only after the engine's settle (about 15 s) in which the file did not
  change and the child's own pi process (the one carrying
  `SUPER_PI_CHILD=wf-<claim>` whose command line is that pi's) is not
  running: pi may still compact after a stop. A process the child started
  inherits the marker but does not keep the child open, and a scan that
  cannot read a process of the same user waits instead of ending it. Two known
  limits sit inside the cooperative model: the shell's `2>` opens the stderr
  file following symlinks, so a same-user process that replaces a directory
  under the state dir after the engine checked it can redirect that write
 ; and the scan skips an unreadable same-user process that holds
  capabilities, so a child that inherited capabilities (in a container) can
  read as not running. A `context_edit` entry after an `error` or `length` message
  means pi is retrying or recovering: the child has not ended. The ended
  cause names a final `error: ...` or `length: ...`. Usage, read only as
  telemetry (the fanout tool's count, never the budget), is input plus cache
  writes plus output, summed over assistant messages, `usage` entries,
  `compaction` and `branch_summary` entries and tool-result messages; cache
  reads are not counted. A child outlives its coordinator, so a
  stalled child is checked by its processes, which carry
  `SUPER_PI_CHILD=wf-<claim>` in their environment
  (`grep -lz '^SUPER_PI_CHILD=wf-<claim>$' /proc/[0-9]*/environ` lists them):
  one that still runs is working, so answer `active` and leave it running.
  Answer `died` only when the check confirmed it gone: grep read every
  environment and listed none. If grep reported an error (Permission denied,
  exit 2) or the child may run where your session cannot see its processes
  (another PID namespace), you cannot tell: answer neither, and its claim
  stays pending.
  A finished child is continued with
  `SUPER_PI_CHILD=<its id> SUPER_PI_PARENT="$PI_SESSION_ID" nohup pi -p -ne --session-id <its id> -- '<message>' </dev/null >/dev/null 2>&1 &`
  (a resume has no stderr file), run in the child's own working directory:
  pi finds a session id only in the working directory's session folder and
  otherwise starts a new session without saying so, which the engine's
  records then do not show as the child. A resume the engine refuses falls
  back to a fresh hand-out: answer the recovery offer `restart`, which every
  offer prints beside `resume`, and the unit is handed out again.
- agy: a child is a subagent conversation the coordinator starts with
  `invoke_subagent`, inside the coordinator's own agy process. The shell's
  `ANTIGRAVITY_CONVERSATION_ID` is the caller's own conversation id (a child's
  its own) and nothing in the environment names the parent, so only agy's
  records tell a child from its coordinator. The records root is
  `ANTIGRAVITY_APP_DATA_DIR`, else `~/.gemini/antigravity-cli`; a
  conversation's log is
  `<root>/brain/<id>/.system_generated/logs/transcript_full.jsonl`. A caller
  is a child when exactly one parent's listing
  (`brain/<parent>/.system_generated/subagents/<id>.json`) names it and its
  first record is that parent's delivery; it is a coordinator when its own log
  exists, begins with a user input and no listing names it; anything else is
  neither, never treated as a root. The engine finds a child for a claim only
  when (1) the coordinator's own log holds an `invoke_subagent` call, in a
  response created since the hand-out (one-second resolution), whose `Prompt`
  names the claim and no other; (7) that call's result, paired with it in call
  order, names the child; (2) the coordinator's listing names it; (3) the
  child's first record is the coordinator's delivery whose body equals the
  `Prompt` byte for byte (a `<id>/task-<n>` sender is a task message, never the
  coordinator); (4) that record is not older than the launch; (5) exactly one
  child matches; (6) the caller is that child, under the same records root.
  A result is a step after the launching response that is not system-sourced:
  error and system messages (a `task-<n>` message among them) are skipped when
  pairing, and a `run_command` result between launches is an ordinary result.
  The written prefix of a response's results stands while later calls are
  pending; surplus results, or a call whose result cannot be its own, stop that
  response. As for pi, none of this admits or refuses a child's call: the
  claim token does. agy must run in Default agent mode (`"agentMode": ""`): in
  Auto mode (a `-p` run's default since 1.5.7) commands run in agy's terminal
  sandbox, the engine sees only the caller's own conversation, and no child
  is found: its units run unverified. A child has ended when the last step is a response with text and
  no tool call and no backgrounded command (a `RUNNING` result naming
  `<id>/task-<n>`) is still waiting for its `task-<n>` message; never exit
  codes or step status alone. Usage, read only as telemetry, is each
  response's input plus output tokens, cache reads not counted. Children die with the coordinator (one agy process). A stalled
  child is checked with `manage_subagents {"Action":"list"}`; a finished one
  is continued with `send_message {"Recipient":<id>,"Message":...}`.
- opencode: (1.18.x) a child is a session the coordinator starts with the
  `task` tool, inside the coordinator's own opencode process, so children die
  with the coordinator. opencode puts no session id in a command's
  environment: the engine's opencode plugin (`opencode/super-workflow.js` in
  the engine module) exports `OPENCODE_SESSION_ID` (the calling session) and
  `OPENCODE_PARENT_SESSION_ID` (its parent: empty for a root, `unknown` when
  the plugin's lookup failed) into every bash command. Install it by copying
  or linking the file into `~/.config/opencode/plugins/` or a project's
  `.opencode/plugins/`. A caller whose parent is `unknown` may be a child, so
  it gets no session and never drives the run, not even a segment no
  coordinator has bound yet: its driving verbs are refused as below, the
  refusal saying to run the command again; its own child
  verbs work by their claim token, and the plugin looks the parent up again on
  the next command.
  Without the plugin (not installed, `--pure` or `OPENCODE_PURE`, or a
  command run from an opencode terminal tab, which the hook does not cover)
  the shell has only `OPENCODE`: once a session drives the run, its driving
  verbs are refused as a harness mismatch naming `OPENCODE_SESSION_ID` as
  unset. That holds only when no other harness's variables are set: a
  plugin-less opencode started from a Claude Code or Codex tab reads as that
  tab's harness and session, and its children are not told from it, so
  install the plugin: `run` prints a note and journals one degraded
  `harness hint` record when `OPENCODE` is set with no `OPENCODE_SESSION_ID`.
  The plugin clears both variables for a shell with no session (a terminal
  tab), so such a shell never reads as an outer opencode session. The reverse leak is accepted: a Claude Code or Codex
  started from an opencode bash command inherits the plugin's variables and
  reads as opencode (a child when the parent is set), since the environment
  cannot tell it from an opencode started from their tab; start other
  harnesses from their own terminals. The engine reads none of opencode's storage: no child record, end
  marker, usage or transcript. A child is known only through its own verbs,
  and its liveness is the heartbeat and the lease (section 8). One opencode
  session per run: a second root session's `next` takes the run over and
  retires the first session's children, so end the old session before taking
  over. A finished child is continued with the same `task` call plus
  `task_id=<its session id>`; a `task_id` opencode does not know silently
  starts a fresh child, which has no claim, so answer `restart` then.
- Any other harness (generic: one the engine has no adapter for, never agy,
  which has its own entry above): nothing is identified, and nothing is
  journaled for it: the claim token is what binds a child's call.

A child of the run never drives it. Where the environment tells a child from
its coordinator (Codex's thread, Hermes's delegated-child marker, pi's
`SUPER_PI_PARENT`, agy's records: the parent's listing and the first record,
opencode's `OPENCODE_PARENT_SESSION_ID`), every run
verb but a child's own (`prompt`, `validate`, `done`, `failed`,
`status`, `explain`, `journal`), `run` among them, and the named binary's
`next`, is refused (a child is told by any harness's own variables, so a Codex
child in a Hermes terminal, or a Hermes delegate that inherited Codex's
variables, is one):
"you are a child of this run: finish your unit with validate and done; the
coordinator handles the rest". So a child's
`next` is never recorded as a newer coordinator, and never re-offers the live
coordinator's units.

A child's verbs need the unit's claim token, and nothing from the harness's
records. `prompt`, `validate`, `done` and `failed` on a unit are accepted
from a caller holding a token the unit was handed out with: the token binds
the call to its unit. The harness's records (a Claude Code subagent
transcript, a Codex rollout, a Hermes delegation log, a pi session, an agy
conversation) are read for the child's liveness and its end, never
to admit a call: a child whose launch they do not show, or whose records are
missing or unreadable, is served all the same, with no wait for a record to
appear.
What is still refused is a matter of form: no `--claim`; a token the unit was
never handed out with; a second, different token once a first one bound the
unit ("another child already owns this unit; exit without working"); a token
of an offer that expired when the run was stopped and resumed; a first claim
on a stopped run; and a stale or superseded attempt. A call that carries a
token is held to it whoever makes it, a shell with no harness environment
included, and again inside the lock the verb writes under: a unit named with
another unit's token, or with one no hand-out minted, is refused. A `done`
or `failed` repeated with the token that settled the unit (the same class
and reason, for `failed`) is told it is already accepted or recorded,
whichever caller repeats it. One identity check
remains (I2): a first `prompt` fetch from the coordinator's own session,
where the harness tells it from a child's, is refused with "this is a
child's command: start a child agent with the text between the lines next
printed for it". That refusal is journaled as a `child refused` degraded
record and is the refused call's only write; after 20 for one unit from one
session, one `child refusals suppressed` record stands for the rest. The unit
stays offered and comes back to you as never started (section 4). A unit no
child can finish is a person's to answer with `recover` (restart or give up).
Explicit operator repair is available under
`wf dev prompt|validate|done|failed <name>`; these forms refuse known agent
environments. Normal child verbs require `--claim <token>` from the task text;
a supplied name must agree with that token. All printed finishing commands
carry the same claim.

The first tokened prompt fetch also records the caller under the journal lock.
Hermes supplies its own child session, Codex its thread and session, pi its
session and parent, agy its conversation id with the parent from its records,
opencode its session and parent from the plugin's variables;
Claude currently supplies only its shared parent session. A later prompt,
validate, done or failed holding the unit's token from another caller (another
child, another harness than the hand-out's, a child of another coordinator
session, as after a coordinator was resumed or replaced) is served: the token
binds it. Prompt, done and failed journal the mismatch once per unit and cause
as a `child caller mismatch` degraded record; validate always remains
read-only. Transcript resolution never changes the recorded caller.

Caller identity is a record, not a guard. Tokens are readable in shared
journals, so a child that reads another unit's token can act on that unit;
the engine does not provide adversarial isolation.

A run is driven in harness segments (J1). A segment opens at a boundary: the
run's start, a person's resume after a stop, a rewind, or a fork (`dev
rewind`, a snapshot or fanout fork). Its first coordinator call binds it to
that call's harness and records root (where the harness keeps transcripts,
rollouts or delegation logs), with a `coordinator` record carrying the
segment's start seq (a session known from an earlier segment writes one
again); the run's first segment is bound from the start to the harness it was
started under, when the starter's environment showed a known one. The binding
is checked and set under the run's lock before `next` does anything, so of two
first callers the second is checked against the first; `recover`,
`heartbeat` and `started` check it again inside their own lock. Every later
`next`, `recover` or `heartbeat` in the segment must come from an agent of its
harness, with a session. A coordinator session the run already knows, calling
from another records root (a session moved to another profile, so another
`CLAUDE_CONFIG_DIR`, keeps its id), is served: `next` journals one degraded
`records root changed` record naming both roots and goes on, and each unit it
hands out from there records the new root, where its child's records are
read. It retires no child. The record's `Root` is the segment's root from
there on, so a session that moves back is noted again, and the root it left
is another root to everyone else. Another harness, a caller with no session,
or a session the run has not seen under another records root than the
segment's current one (it would replace the segment's coordinator and retire
its children at once) is refused with
"this command is not running in the agent harness this part of the run is
driven from: stop and report this to the person who started the run; do not
start another agent CLI to do this run's units, except through the engine's
child dispatch. A person moves a run to another harness by stopping it and
resuming it from there". The refusal then says what was expected and what the
call shows, so the caller can tell what is missing: the harness the run is
driven from and the one this command's environment is ("driven from claude;
this command's environment is codex's"); that the environment shows no agent
harness, with the variables the driving harness is recognised by; that it
names no session, with the variables that are not set ("not set:
CLAUDE_CODE_SESSION_ID"); or that this is a session the run has not seen under
another records root. It prints names, never a variable's value, a session id
or a records root, and only the names of harnesses this binary has an adapter
for: any other is "another harness" (the caller's) or "a harness this binary
does not know" (the segment's), so a name a caller supplies is never repeated. A child's verbs follow its unit's claim token,
whatever segment the run is in now and whatever harness the caller's
environment names (a caller of another harness than the hand-out's is served
and journaled as a `child caller mismatch`). A child claimed in an
earlier segment finishes under its own, and the new segment's coordinator does
not count it lost for its coordinator's change (only once it goes quiet): a
boundary is a migration, not a crash. A unit that segment offered and no child
claimed expires at the boundary: its old offer's child is refused, before or
after the new segment's coordinator, any session, is offered it again ("this
offer expired when the run was stopped and resumed"). The refusal names no
harness. Each `handed_out` record
names its segment, so every child call is attributed to one through its
unit's hand-out. `workflow status` lists the segments once a run has more
than one: `harness segments: hermes 1-512 (40 units, start), codex 513- (7
units, resume)`, each bound segment with its tier models. A
segment's models are chosen when its first coordinator binds it and journaled
on that `coordinator` record (`Models`: the map in force, the tier -> model
and tier -> effort maps, and where they came from: `start`, `map`, `carried`
or `defaults`); the boundary record (`resumed`, `rewound`) carries the map the
person gave, if any. A journal from before per-segment maps folds as before:
its segments spawn with the start's models. History is never re-resolved
from the live tier table: a replay gives each issue the models its recorded
segment had, and a snapshot fork keeps each source binding's models on the
note that replaces its coordinator record. A rewind or fork with a different
`--model-map` is a different fork (`.rw2`), and the same map again is the
same one. A bad key or effort is refused before a fork writes anything; a
value is decided when the fork's segment binds. `run --run-id`
on a live run refuses a `--model-map` other than the one in force. Printing the bootstrap is not driving, and neither is `next
--adopt`, which only adopts (below). Known residual: where the environment does not tell a coordinator from its
child (Claude Code), a coordinator that runs a unit's command itself is
served. A person's verbs (`stop`, `resume`,
`answer`, `settle`, `retry`) and the views (`status`, `explain`, `journal`)
are not driving verbs. After `wf stop` a child already
running may still fetch its prompt again and finish, and nothing new is
claimed: a handed unit's `prompt` or operator `dev started` is refused, "run <r> is stopped: no new unit
starts until a person resumes it; exit without working".

The run's `tokens` and token budget count only usage a child reports with
`done --usage tokens=N` (or a person attaches with `dev started --usage`); a
child that reports nothing counts zero, and a missing report is never an
error. The child's prompt asks for `--usage tokens=N` on `done` when its
harness shows the count, and to omit it otherwise. A `done --usage` that is
not `tokens=N` never blocks the hand-back: done is accepted, the attempt
counts zero, and a `usage unparsed` degraded note records the value. The engine reads no child's transcript for usage, so the token budget
is a best-effort number; attempt counts, per-step caps and wall time bound a
run on every harness.

The harness is read from each `next` call's environment and journaled with
the session on every `handed_out` record, so a run created under one harness
and driven from another gets the caller's spawn lines and dispatch.

`dev started` is an explicit manual override for a person; no coordinator calls it in the loop. Agent sessions are
refused on both `started` and `dev started`, before any journal mutation; their
attempt starts only through a prompt fetch bound to a child. Environment identity
is cooperative, as with the other operator-only commands:

```
workflow dev started <run> <name> --context fresh --harness <h> --session <id> --agent <id> --transcript <path>
workflow dev started <run> <name> --usage tokens=N
```

It requires both `--session` (the coordinator) and `--agent` (the child), and it
claims a handed unit like a prompt fetch. A unit claimed by operator `dev started` and never
fetched keeps the spawn window (3 min): it is lost if no prompt fetch arrives
by then. Metadata already recorded is kept; a conflicting value is rejected.
`--context fresh|inherited` records the context the child was spawned with.
`--usage tokens=N` attaches an ended child's token usage late; a unit that
already has usage refuses a different count. Never fabricate
an identity or guess a transcript path.

## 6. Human gates (`human-gate`)

When the prompt offers an integer, relay the person's chosen number with
`--number N`; omit that flag to accept the shown proposal. `--value` still names
the answer choice. An out-of-range number leaves the question pending.

Show the human `question`, `answers` (the allowed values) and `prompt`, then
answer:

```
workflow answer <run> --gate <state> --value <v> --visit <visit>
```

Add `--child <id>` for a Map child's gate. Passing `visit` binds the answer to
that arrival at the gate: a stale or misdirected answer is rejected (exit 4), as
is one outside `answers`.

At a reserved gate, every form of coordinator `next` prints only the person-handoff
instruction and exits 7. It repeats while the gate is open, with the question,
choices and a command bound to that child and visit. Ask the person who started
the run, wait for their reply, then relay only what they tell you. Never choose
for them, including when old delegation metadata is present. No other unit is
handed out by that call. JSON consumers receive one `stopped` instruction with
its gate fields. The library simulation API retains `human-gate` instructions.

Answering remains available to the coordinator after the person replies; no
terminal or human-identity guard is imposed. The `gate_answered` record carries
the answering session when present, or no session for sessionless callers.
Policy-answerable gates retain their existing delegation and autonomy rules.

- A timed gate carries a `deadline`: the engine takes its timeout edge then.

## 7. Waits, manual verification, halts and holds

`wait`: the engine is backing off. `next` sleeps through it and returns
when the deadline brings something for the coordinator. Never poll faster.

`manual-verification`: an effect finished uncertain (the tool may or may not
have applied it). A `transient` instruction is an effect whose check failed
transiently; the engine checks it again on the next call, so do nothing.
Otherwise the effect is yours to settle: check it in the world, in the
`workspace` (the files it writes, named in its args; the git state it would
leave; any record it would create), then settle it with the `effect` id and
what you checked:

```
workflow settle <run> --effect <id> --applied --evidence <evidence>
workflow settle <run> --effect <id> --not-applied --evidence <evidence>
```

The instruction's `submit` line is this command, with the run's `--state-dir`
already in it, `<settlement>` in place of the flag and `<evidence>` in place
of the text: replace `<settlement>` with `--applied` or `--not-applied`, and
`<evidence>` with what you read and what it showed, quoted for the shell. The
evidence is kept with the settlement. A command effect settled as applied may
record what was observed: `--exit <code>` and, with it, `--stdout-file <file>`.

If what you can check does not decide it, leave it unsettled and report why:
the run waits there. A `transient` instruction is never settled: run next
again. `--evidence` is required; a settle without it is a usage error.

Only an effect the definition marks outside the run (`person` is true: a push,
a merge to base) needs a person: the line reads `this needs a person`. It is
settled by the run's operator, from a session that is not one of the run's
coordinator sessions, with `--operator <name> --reason <why>` and its
evidence; the engine refuses your session. Ask yours, or report it and end
the session.

Applied continues the run. Not applied is a failed step, never a dead end: a
keyed command (safe to re-run) is issued again once, with nothing spent, even
in a state with no retry policy. After that, or for an unkeyed or
non-idempotent command or an edge effect, the step is blocked and the
definition's route for blocked takes over; a state with none halts with the
exhaustion halt below. Run `workflow settle` yourself or hand it to a subagent
working in the `workspace`; it needs no output file.

A `blocked <state>` line is a halt: nothing moves the run on without a human.
A halt that a Map or Sub settles is not reported; `status` shows it. A halted
instance goes back to ready at its next attempt with `workflow retry <run>`
(with `--child <id>` for a halted Map child). A halted Map child under
ContinueSiblings raises no line; `status` shows it settled with its reason, and
`retry --child` can still revive it while the join is open.

An agent state that spent its retry budget, stalled or hit the lost cap is
offered to you first (section 7a), then takes its declared exhausted edge (or
its stalled edge after a stall); the engine never halts it for a human. A
not-applied effect at a state with no blocked route takes the exhausted edge
when the state has a retry policy.
`workflow retry <run> --stop` answers only the retry-or-stop halt an older
engine journaled; on any other halt, and any `retry` on an ended run, it is
refused (exit 4).

A blocked failure (`workflow failed --class blocked`) takes the state's blocked
route: its own, or the definition's default (`d.OnFailure(wf.Blocked, ...)`),
the cause bound as `{blockedCause}`. Lint rule `agent-blocked-route` keeps every
agent state routed. An agent state that still has no route does not halt for a
human: its unit is offered to you as a recovery, with the cause as its child
stated it (section 7a). It halts, and `workflow retry <run>` resumes it, only
when you give up at a state with no fallback edge, when the unit's two restarts
in the run are spent, or when its retry policy opts out (`NoRecovery`).

When a child can bring a denied action to its coordinator's approval prompt,
it runs `workflow failed --class approval` with the exact command or tool call
quoted in `--reason`. This holds that unit without choosing a route or spending
a retry. `status` and the hold in `next` show the action as quoted data. Review
it and request approval through your normal harness prompt. If the blocker is
cleared, run the printed `retry --approval-hold` command. The number identifies
that specific hold. Go callers use `Executor.ReleaseApproval` with the
instance and the sequence in `HeldUnit.ApprovalHold`. A unit can stay parked
while siblings continue.

## 7a. Recovery offers

A `recovery <unit>:` line hands you a unit that failed, before its failure
route, so you recover it on evidence. Answer every one with one of the
`workflow recover <run> <unit> <choice>` lines it prints; nothing waits on a
human.

- A spent budget, a stall or the lost cap (once per state visit, at most two
  restarts per unit in a run; the line says how many are left): `restart`
  grants one more attempt, a fresh child; `give-up` takes the state's failure
  edge. Restart when the failures look random (a child that died, a flaky
  tool); give up when the same failure repeats.
- A child that ended without `done` or `failed`: the attempt is still open.
  The line carries its transcript, its last tool call and last message, and
  whether its result file was written, so you need not read the whole
  transcript. `resume` (only where your harness can continue a child: run the
  printed harness call, which sends it the fixer text, then the `recover ...
  resume` line) keeps the claim with that child, which picks up where it
  stopped; choose it when the child was close to done. `restart` offers the
  unit again to a fresh child; `give-up` takes its blocked route.

- A failure with no route (a child's `failed --class blocked`, or a blocked
  check, at an agent state with no blocked route; any failure at an agent
  state with no retry policy): the line carries the cause verbatim, which for
  a child's `failed` is what blocked it and what it said it needs. `restart`
  queues a fresh attempt whose prompt shows that cause (the same two restarts
  per unit in a run); `give-up` takes the state's exhausted edge when it
  declares one, and otherwise halts the unit with the cause for a person
  (`blocked <state>:`, section 7). The line says which. Restart when you can
  see the cause was passing or you have supplied what the child needed; give
  up when a fresh child would meet the same thing. An approval hold
  (`failed --class approval`, section 7) is not offered this way: a refused
  permission is the person's.

Every answer is recorded as a `degraded` line (kind `recovery`) with the
choice. A child that died with its coordinator is offered again directly, with
no recovery line.

## 8. Stalls and liveness

A child whose transcript ends on its harness's end marker (Claude Code: the
turn-ending hand-back result or an `end_turn` message; Codex: `task_complete`; agy: a last response with text and
no tool call, no backgrounded command still waiting for its `task-<n>` message)
with no `done` or `failed` has ended without finishing: the engine loses the
attempt as `died` at once, with a degraded `ended without done` record, and the
unit is offered again as the next attempt. It counts toward the lost cap and spends
no retry. A transcript that stops mid-turn has not ended.

The engine decides a unit is stalled; the coordinator only checks. A `stalled
<name>` line is reported at the start of a `next` call. Ask your harness, with
its own tools, whether that subagent is still working (for example, Claude
Code: its Agent call has not returned; Codex: `list_agents` shows its thread
running; agy: `manage_subagents {"Action":"list"}` shows its state other than
idle; opencode has no tool that lists a child's state), without stopping it: a quiet child may be inside one long tool call.
Then run the line it names: `active` when it is working, `died` only when you
confirmed it has exited. A child of an earlier session is yours to answer too,
since the engine never retires a child on elapsed time; but not seeing a child
is no proof it is gone (another session's tools, a process you may not
inspect). When you cannot tell, run neither and wait for fresh evidence: run
`next` again, and it waits for a sign of life from the child or the
question's recheck (one lease length after it asked; `status` lists it as a
`stall recheck` due time), then asks again. Its claim stays pending and
nothing is handed out again meanwhile:

```
workflow heartbeat <run> active <name>
workflow heartbeat <run> died <name>
```

`active` clears the stall and restarts the stall clock; `died` marks the
attempt lost, and the unit is handed out again.

Liveness rule: once the engine has found a claimed unit's transcript, it stats
it (transcript mtime) during every driving call, and a write since the unit's
last sign of life counts as an active heartbeat on its own. A transcript is
written only at message and tool-result boundaries, so the mtime proves a turn
boundary and nothing more: a subagent inside one long, silent tool call can
stall while it works, so check its task status before answering `died`. Without a transcript path, liveness is the heartbeat and the lease alone. A
fetched unit whose lease expires is reported as a stall too, with or without a
transcript, and never lost on its own: `next` returns it as the question "is
this child still running?", and you answer with the two lines it prints,
`heartbeat active` (it is: the lease starts again) or `heartbeat died` (it is
gone: the unit is handed out again at once). The lease is a Map's lease, else
the step's own length (`wf.StallAfter`), else the fetched-unit lease (default 30
min), measured from the unit's last sign of life: its claim, a prompt fetch, a
`heartbeat active`, or a `validate` from the child. Until you answer, the unit
stays claimed and is not handed out again.

An active answer counts at most once per stall question, and only engine
events from the child are progress: a prompt fetch, `started`, `validate`,
`done` or a child's own heartbeat (a transcript write is not). The second
active answer in a row with no such event between changes the question:
`stalled <name>: you answered active <n> times in a row, and its child showed
the engine no progress in between`. Running alone is not enough then; look in
the child's own thread. If you saw it work since the last question (new
output, a command still running), answer `heartbeat active <name> --note
'<what you saw>'`: the note is journaled and the count starts over. If it
waits on a pending prompt, resolve it as your harness allows and answer
`heartbeat active`; for an error or a confirmed exit, answer `heartbeat died`,
and the unit is offered again. If you cannot see its thread or cannot tell,
answer neither: `next` asks again at the recheck. That second question gives
one cannot-tell instruction only: if you still cannot tell, answer `heartbeat
died`. The count is journaled on each active answer (`Unprogressed`), and the
latest observation shows in `status` as the unit's `observed:` (`Observed` in
`--json`).

The child's prompt asks it to run commands expected to take more than a few
minutes with its harness's own background-task tool and to check on them with
that tool, which keeps its transcript moving, rather than a polling or wait
loop: a permission checker refuses those shapes.

Agents' own tool calls never pass through the engine and are not constrained by
it: the engine reads a transcript's modification time and nothing else, and it
limits no tool an agent runs.

### Optional native coordinator Stop hook

A launcher can opt in on Claude Code or Codex. Before installing, verify that
the approved client and the directories that control its executable path are
outside all roots the run can write, including the workspace, state and scratch
directories. Run `wf install-coordinator-stop --hook HARNESS --workspace DIR`
from that installed client and review the native hook configuration.

Run `wf attach-coordinator --initial --state-dir DIR` from a native coordinator
session on its first attachment. Pass `--run-id ID` for a named client. After
operator resume, reattach the same session without `--initial`. For a
multi-definition client, use the run ID as the verb's first positional
argument in place of the named client's `--run-id` flag. If the previous
selection is lost, use `wf attach-coordinator --previous-run ID --previous-state-dir DIR`
with the previous run's address and the new run's normal address. A failed
publication reports a pending selection. Retry the attachment to finish it.

`wf may-end-turn --json` reads the committed decision for the current
attachment. The native harness invokes `wf coordinator-stop` on Stop to recheck
that decision and record any issued continuation. When `next` issues a person
handoff, show every request in the response and run the printed `wf acknowledge-handoff --receipt ID`
command. The acknowledgement records delivery. Only the person can answer a
reserved gate or settle an effect reserved for them.

An attached run reports its automatic continuation count at closeout and in
status. Five issued continuations without durable progress can be
followed by a visible continuation stall. An operator can stop and resume the
run to open a fresh segment, then have the coordinator reattach.

## 9. Stop, resume and a dead coordinator

`workflow stop <run> --reason <r>` stops a run. Anyone may run it: a human, a
budget watcher. It journals `paused`. From then on `next` offers nothing
and returns the `stopped` instruction at once (exit 7); children already
running finish on their own. The stopped instruction tells the coordinator to
report and run nothing further. A coordinator does not clear its own stop: the
run's operator, another session in any harness or a person's shell, runs
`workflow resume <run> --operator <name> --reason <why>`, and the next
coordinator continues from the bootstrap. The resumed record keeps the
operator's name, its session and the reason. `resume` on a stopped run is
refused (exit 4) without `--operator` and `--reason`, and from a session that
has coordinated the run. Nothing asks whether a person is at the terminal. The
refusal names no variable (J3: an agent told what was checked strips it). A
person whose terminal inherited an agent's environment runs resume from a
fresh terminal.

A binary whose definition changed refuses to resume a run started at the old
one (`definition hash differs`) unless the definition declares a `Migration`
from the old hash. `workflow resume <run> --adopt` (also `next ... --adopt`,
which does that and nothing else, from any harness: it hands out no unit,
binds no harness and runs nothing else; it prints `adopted: ...` or `nothing
to adopt: ...` with the coordinator's `next` line, refuses an ended run, and
is not combined with `--until-unit`; the run's coordinator then drives it with
`next`)
carries the run over without one when every live instance's state still
exists under the same name with the same kind and result type, every state it
did work at keeps its types (a terminal's yielded result with its fields too),
each child definition it spawned keeps its input and result types (the seam a
parent reads a settled child across), and the definition's first state is
where an instance that never moved sits. The
adoption is journaled as `migrated` (`Adopted`, with both hashes and the new
shape), so the report shows a run that changed binaries. Otherwise it is
refused, listing each state that vanished or changed: that is when a
`Migration` is needed. Run creation journals the definition's shape for
this; a journal from before it compares state names only.

`workflow dev rewind <run> --to died` forks a run that died into a new run
that carries on from there; the source is left as it was. The point "died" is
the state whose decision ended the run: from the run's transition into its
terminal, down through each Sub or Map child whose settling decided its
parent, to the first state that decided on its own (a gate inside a design
child, not the root's stop). For a run that has not ended it is a halted
state; a run that neither ended nor halted is refused (resume it). The fork's
journal drops only what led into the end: the dying instance's records from
its arrival at that state, each ancestor's from its settling of that child
(descending into a child that failed rather than one that merely settled
last), and what other instances journaled after the first ancestor's
decision. Parallel siblings keep the work they finished; a child whose spawn
is dropped is dropped with all its descendants, and its state spawns it again. Each dropped record
is a `note` in the fork, so the kept ones keep their seqs, and the journal
ends with a `rewound` record naming the source, the last kept seq, the state
and each effect whose applied result was dropped (not undone). The fork's run
dir starts with the source's records, detached effects' results, outputs and
prompt bodies. A run that holds a `rewound` record (every rewind, snapshot
fork, fanout target and manufactured fork, and their forks) runs sandboxed. A fork's tracker has no remote.
A snapshot restore drops each Dolt database's remotes, backups and branch
upstreams from the restored `.beads`, and blanks every remote setting
(`sync.remote`, any remote URL) in its `config.yaml`. The fork's `rewound`
record lists each one under `Omitted`. This is a deliberate divergence from
the source, which is unchanged. The shim adds `--sandbox` for
engine-launched commands. bd adapter calls carry the run's sandboxed
context and run with `--sandbox`, with an environment of PATH and HOME
only. Shell effects and detached commands find the run's own `bd` first on
PATH, in `<run dir>/sandbox-bin`, and a `bd` they launch by any path is
that shim. The shim runs the real bd with `--sandbox`, drops bd's storage
overrides and refuses `--db`, `--database` and `--global`. A process the
engine does not launch (an agent a harness runs, a script that names
another bd) gets no shim; the tracker having no remote is what protects
those. A plain rewind shares its source's workspace and tracker, so only
the shim applies there. A snapshot reads `config.yaml` and `.beads/.env`
itself and never starts bd in the tracker it captures. It refuses a
setting that moves the database, a storage override in `.env`, and YAML
it cannot resolve (flow, anchors, aliases, merge keys, tags) wherever a
storage or remote setting could be. A run ended by a stop with no terminal, or with no halt left
(after a retry), is refused. It prints the fork's id, points the
state dir's `run-id` at it, and names the line that carries on: `next <fork>
--adopt`, which adopts the fixed binary (above), after which the coordinator
that is to drive the fork runs `next`. Rewinding to an
earlier state is not supported yet.

A fork (a rewind, or a snapshot fork such as `dev fanout` makes) is a
crash-restart in a new run. A unit that was live at the fork point has its
child in the source run only: the fork's first call loses it as a `fork
orphan`, with a degraded record, and offers it again at its next attempt. That
loss spends no retry (no turn if it was never started), is never failed as an
open refusal the source child left, and does not count toward the lost cap.
Replay is at-least-once for what a child did outside the journal: in a fork
driven past its target, a reissued unit runs against a world that may already
hold its own earlier attempt's commits, files or tracker writes from before the
capture. Its prompt and checks must tolerate finding them. A checkpoint taken
while children were live is one instant of the world or none: if a branch
ref, a HEAD, a staged index, a working file the capture reads or the tracker
changed during the capture, the offer is recorded as unavailable ("world
changed during capture") and a fanout skips it with that reason. A change
undone within the capture (A to B to A) is not seen. A detached command in flight is never captured.
A capture never stops a run: one that fails for any reason is recorded as
unavailable with why, and the verb that took it goes on. Its linked worktrees
are the run-owned ones that live at that moment: a worktree the run added and
a later step removed (a sweep's `git worktree remove`) is left out, while one
git still registers but whose directory is gone makes that capture
unavailable. A capture records the state it observes, never who removed a
checkout: a person's `git worktree remove` of a run-owned worktree looks the
same as the run's own sweep, and that worktree is left out too. A fork of such a
checkpoint has no checkout there, and its branch is kept. A git repository
or worktree a unit made inside the run's own data (a scratch worktree under
`records/`) is captured as plain files without its `.git` entry (any case
of `.git`). The checkpoint record lists each omission (`Omitted`), and a fork
has those files but no repository there. This holds only for run data: an
adapter whose own checkpoint data holds a `.git` entry makes the capture
unavailable, naming the path. The checkpoint store rolls its packs up as it grows
(a geometric repack once it holds more than 32), so a long run's store stays
a few packs.
`next --until-unit` binds only when no command is in flight anywhere in the
run, since nothing would take that command's result under the bound unit.

`workflow dev replay <run>` checks a fix before any model runs. It replays
the run's journal against the binary's own definition in a fresh in-memory
engine: each read is answered by the run's recorded observation, each effect
by its recorded result, each unit gets the run's recorded submissions (with
their artifacts) and failures, and each gate its recorded answer; nothing
outside the process is touched and the source run is only read. Transitions
are compared by instance, state and visit, so a state added or removed
upstream shows as one difference and the replay carries on. It prints each
difference (the state, the route then and now), whether the definition
passes the state the run died at, and why the replay stopped: the run has no
record to feed it (a unit, read, effect or gate the run never reached), or
the definition's run ended or halted. The loop for a run defect is: fix,
`dev replay` to see the fix take the other route at the point of death,
`dev rewind --to died`, then `next --adopt`. A kept journal replayed in a
test is a regression case.

When a coordinator session dies, its subagents may be gone with it. The new
coordinator starts from `./super next` and its loop as usual. Each `next` from
a newer session than the one a unit was handed to (one whose first `next`
came later; each session's first `next` is journaled as a `coordinator`
record) offers that unit again, with a degraded
`orphan` record; an orphan loss never counts toward the lost cap.

- Claude Code, Codex and agy run children inside the coordinator's process (a
  subagent, a thread, a conversation), so they died with it whatever their last write: the new
  session's first `next` offers them all again at once.
- On any other harness only a child that shows no life within its stall
  threshold is offered again. A child that still shows life finishes and
  submits; a later `next` sweeps it once it goes quiet.

A `heartbeat died` for a child of a replaced session also counts as an orphan.
An older session that comes back never clears a newer one's units. Run `next`
only from the coordinator: a `next` from any other session counts as a new
coordinator. On Claude Code, Codex and agy, while the earlier coordinator's
`next` is running (it holds its session's lock under
`runs/<run>/coordinators/`), that `next` is refused (exit 4): `another
coordinator session of this run is live, and this session is not it`. If that
session is gone, its call ends within its harness's tool limit and next goes
ahead then. A coordinator that crashed holds no lock, so a restart goes ahead at
once and re-offers its children. After a stop and a resume (a harness-segment
boundary) the new coordinator does not count the old one's claimed children
lost for its change: they finish, or are lost once they go quiet.
No coordinator verb is involved. `workflow heartbeat <run> died --session <old>`
still exists as a manual override: it marks every unit claimed by that session
lost in one step.

## 10. Results, outcome classes and exit codes

A child's `done` and `failed` print the result as JSON with a `Class`. Only
rejected and refused change the exit code; everything else exits 0 and the
coordinator's next `next` shows where the run went.

| class | meaning | what to do |
|---|---|---|
| accepted | output taken | nothing |
| rejected | stale or superseded token | the child stops; the engine has moved on |
| refused | output malformed or refused; from `done` the attempt is spent, from `validate` nothing is recorded | validate: the child fixes its output and validates again. done: the child reports and stops; never run done again on that attempt. While retry remains the engine offers a fresh attempt, under a new name, through `next`; once the retry is spent the state takes the failure route. A failed attempt is the engine's to retry; the coordinator never reruns done for a child |
| blocked | the step halted or took its blocked route | nothing; `next` reports a halt |
| unavailable | a tool the step needs is missing | nothing; the engine retries or routes |
| stale | the instruction no longer applies | nothing |
| uncertain | an effect may or may not have applied | a `manual-verification` instruction follows (section 7) |

| code | name | meaning |
|---|---|---|
| exit 0 | ok | done |
| exit 2 | usage | bad flags, unknown subcommand, unknown definition, missing required param, malformed --model-map, lint failure, or a host preflight refusal (stderr `refused: <check>: <cause>` then `fix: <remedy>`) |
| exit 3 | unknown-run | no journal for the run id |
| exit 4 | rejected | stale or superseded token, an answer outside the Enum, or no such open gate or effect |
| exit 5 | refused | output malformed or refused: done spends the attempt (a fresh one is issued while retry remains, else the state takes its failure route); validate records nothing, and the child fixes its output and validates again. No file at the allocated output: the first such done keeps the attempt open, and the child writes the file and runs done again; a second on the same attempt spends it |
| exit 6 | infrastructure | journal or definition-hash error, or a call that became busy after it had already recorded something (it names what, and the command to run next) |
| exit 7 | ended | wf next found the run ended (its single ended instruction names the terminal, outcome and cause) or stopped (its single stopped instruction): the loop ends |
| exit 8 | check-failed | check found structural lint, calibration, simulation or coverage problems |
| exit 9 | busy | another call held the run past this call's lock wait and this call changed nothing: run the same command again |

Exit 2 and exit 3 mean the caller made a mistake (check the command and the run
id), except a host preflight refusal: exit 2 from `run` with a `refused:` line
and a `fix:` line on stderr means the host is not ready, not that the command
is wrong. Report the `fix:` line to the operator; do not retry the command.
Exit 6 is not retried by the coordinator: report it. A call that became busy
after it had already recorded something (a `run` whose run is started, a
`dev rewind` whose fork is published) is exit 6 too: it names what it
recorded in which run and that run's status command, which is never the same
command as it was. A child's `done` and `failed` record their `--escalated`
reports and their result under one lock, so they exit 9 or finish. Exit 9 is a busy run (`<verb>: the run is busy, held by <verb> for unit
<u>, pid N, since T; nothing was changed`): another call still held the run
past this call's wait, this call changed nothing, and the same command is run
again; it is never a stop. `next` itself waits its turn while a child's `done`
holds the run.

A call waiting for the lock waits about one drive step of the call that
holds it, not its whole chain: a drive lets the lock go to a waiting call
between its steps. The longest a driving call can block: the lock wait (2
minutes where the harness reads no limit), plus the longest inline
effect an engine call runs while it holds the lock, which is the effect's own
timeout (the shell timeout, 10 minutes by default for a shell command). `next`
adds its wait (its harness's tool limit less a margin, else 100s) to that; it holds no lock while
it sleeps. Detached effects do not block: they run beside the engine and their
result is journaled when it arrives. The detached runner is Linux-only (it
identifies a launch by its process group and the leader's `/proc` start ticks);
on any other OS a detached step settles Unavailable.
The runner is launched outside the driving call's process tree, as a transient
`systemd-run --user` unit when the probe's unit runs, else by a double fork, so
killing or stopping the tool call does not stop it. The detach modes form a
ladder, systemd, then process (the double fork), then none (inline in `next`):
a launch that dies, a launch that started nothing, or a systemd-run that hangs
(its unit is stopped and its paths fenced, so a late unit either is adopted or
never runs the command) steps the run down one mode for good and re-runs the
command there once, never on the mode that failed. `status` lists each step-down as a `degraded: <state> detach:` line.
Under a sandbox that runs each command in its own PID namespace (Codex's
workspace-write) the probe reports none and the step runs inline from the start.
A run whose probe picks process or none lists one `degraded: <definition> detach:
<cause>: detach <mode> at preflight` line (none for a mode set by
`WF_DETACH_MODE`, a choice); neither kind changes the verdict.
Set the harness's command timeout above these.

## 11. Status

`workflow status <run>` is for a human or a resume, never for the loop. From an
agent's shell (`HERMES_SESSION_ID`, `PI_SESSION_ID`, `CODEX_THREAD_ID`,
`CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_CODE_SESSION_ID`, `OPENCODE` or `ANTIGRAVITY_CONVERSATION_ID`
set) on a running run it
prints one line (H3): `running: R running, nothing for you to do; coordinator:
run next; subagent: carry on`, or, with units handed out and not started or
ready, `running: R running, H handed out not started, K ready; ...`, so a poll
costs a short line and says what to do instead; a subagent sharing the
coordinator's environment is told to carry on with its unit. A stopped,
blocked or ended run prints the whole view there too; `status --full` prints
it anywhere, and `--json` is unchanged. Otherwise it
prints one line per subagent unit (name, state, elapsed time, session, context
mode: `fresh`, `inherited` or `unrecorded`, for units that were claimed) and a
totals line, then the `degraded:`, `due:`, `open effect`, `lock:`, `nothing
ready:` and `delegated:` lines that apply. Unit states: `pending`,
`handed`, `started`, `running`, `stalled`, `done`, `failed`, `lost`, `withdrawn`,
`waiting`, `held` (only in a run an older engine journaled, where a blocked failure held the unit for a human), and `future` (an agent state ahead of the current one on an
unconditional path: not handed out yet; the totals line adds "(+N future
steps not counted)"). `workflow status <run> --json` prints the same data as the RunStatus
JSON (`Phase`, `Outcome`, `Terminal`, `Units`, `Due`, `NothingReadyReason`, and
more), for anything that parses it.

The final report quotes the `ended` line (or the `stopped` line) and the totals
line under it (`The run's totals: subagents <n>: <k> done, <k> handed, ...`,
the `counts` field in JSON): those are the run's units exactly as `workflow
status` counts them, every state included. It gives the `Outcome` (`success`,
`stopped` or `stalled`), the `Terminal` state, the journal path
(`<state-dir>/runs/<run>/`; `workflow journal <run>` prints the records) and the
degraded records.

`status`, `explain` and `journal` read the run with no lock: the records
already written, never a record still being written. They never wait on a
call that holds the run, and they change nothing (a definition migration they
meet is folded in memory, not journaled). On a local filesystem they show
every record written so far, possibly one whose writer has not yet synced it.
Files outside the journal (results, prompts, the lock holder) may be a moment
older or newer than the records. A state directory on NFS can show stale
data.

`workflow explain <run> <unit>` shows the unit's incoming transition, accepted
route, deciding checks and effect records. The unit name remains usable after
it finishes. `workflow journal <run>` emits JSON Lines with numeric `Kind` and
`Class` fields plus readable `KindName` and `ClassName` fields.

### A coordinator verb that ends on an error

A coordinator verb that fails (exit 6, say: a definition this binary does not
run, a failed write) prints the error and ends with "This command failed, and
the error above says why: you may run next again". Nothing is recorded for the
session and nothing is armed: its next call simply runs. A usage error ends
with "Read this command's --help and correct its arguments, then run it
again", and a rejection or refusal with "... Unless it asks for a person, you
may run next again". Only two things are a person's: `workflow stop`, which
stops the run until `workflow resume`, and a call from another agent harness
than the one this part of the run is driven from, whose refusal ends with
"stop and report this to the person who started the run".

Journals written before this hold `session_stopped` and `session_cleared`
records and degraded `stopped session refused` records, from when such an
error stopped the session until a person ran `dev clear-stop`. They still
read and fold, and change nothing: the session they name runs on. `dev
clear-stop` no longer exists.

## 12. Refused or ran and failed

These are the child's rules; its prompt's Finishing block carries them, and the
coordinator only needs to recognise them.

A tool call the harness or an approvals reviewer declined, or an effect that
needs a permission the run lacks (network in a no-network sandbox), is refused.
It is a blocked failure (`--class blocked`) and is never worked around. The
state takes its blocked route, or, with none, the unit is offered to the
coordinator as a recovery with the cause (section 7a). A command that ran and returned an error (EROFS
on a default cache path, a failing test) is ordinary work: fix it inside the
run's own space (a run-local `GOCACHE` or `GOTMPDIR`) and continue, or fail it
`--class transient`. Pointing a cache inside the worktree works around nothing:
nothing was refused. When the sandbox blocks a command the task needs, request
escalation (Codex `require_escalated` and the like); escalating is not working
around a refusal. Record each escalation in the run report (command, reason,
outcome): a child passes `--escalated '<command>: <reason>: <outcome>'` (the
outcome a short phrase without `: `, such as `ran` or `refused by reviewer`) to
its `done` or `failed`, a coordinator that escalated a command itself passes its
own through a person's `dev started <name> --escalated` for the unit it served, and `status` lists
each as a degraded record. If escalation is refused, that is still a blocked
failure (`--class blocked`) unless your coordinator can present the approval
prompt you could not access.

If the harness denied the action without giving you an approval prompt and
your coordinator can present one, report it with `--class approval` and put
the exact command or tool call in `--reason`. Hermes delegated children can
receive this silent denial under smart approvals.

A subagent that dies is answered with `heartbeat ... died` when its stall is
reported; one that gives up runs `workflow failed <run> <name> --reason <r>`
itself. The engine hands the unit out again while retry remains, and its retry
budget is the bound.

One child's failure belongs to that child: never hold or pause sibling
children over it.

## 13. Removed verbs and inspection verbs

The first-generation verbs are removed and refused: `submit`, `fail`, `wait`,
and `next` without a count (the wave alias). Journals written by them still
load. A unit claimed by a wave alias belongs to session `wave`.

Inspection verbs run over the registered definitions and need no run:
`workflow lint`, `workflow graph`, `workflow mermaid` and `workflow hash`.
`workflow version [--json]` prints this binary's build stamp: the source of the
engine and of the workflow (version, revision, modified flag, each marked `vcs`,
`module`, `declared` or unknown), the Go toolchain and target, and the
journal format. Every run records the stamp of the binary that created it in
`run_started`. `status --full` prints it on a `build:` line, and adds `this
binary differs: <what>` when another build reads the run. That line is a note
for people, not a refusal.

The generic CLI verb `workflow check <definition>` runs structural lint,
check-example calibration and deterministic simulation without a model.
For the reference workflow, run `go run ./cmd/wfcheck check feature` from
`docs/proposals/go-api`; the shipped `workflow` binary has no checker fixtures. The definition must register
a checker with known-good fixtures in a separate development checker binary;
shipped binaries do not import the simulator. The CLI runs lint
once before the registered calibration and simulation callback. Findings
exit 8; bad arguments, unknown definitions and missing checker registrations
exit 2. Missing fixtures, simulation findings and uncovered edges fail the
command; the report includes reduced failure scripts and fields the checks
do not constrain.

`workflow dev harvest <run> --into <corpus dir>` extracts accepted agent
results from a kept run for use as calibration and simulation corpus entries.

## 14. The state-dir environment

A state dir may hold a file named `env`. Every `workflow` call on that dir
reads it first and applies it to its own process, so the scripts a run executes
see it whoever started the call, subagents' commands included. Each line is
`K=V` (set) or `-K` (unset); a line that is neither exits 6. The eval
`prepare` step writes it to scrub the coordinator's session and git
variables and to pin HOME and XDG state under the printed state dir. The
coordinator does nothing about the file: it neither reads nor edits it, and it
passes `--state-dir` as usual.
