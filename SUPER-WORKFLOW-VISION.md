# Super-workflow

In many tasks, the result of one step determines what an AI agent should do next. A prompt can describe the steps, but when a conversation is shortened or a new session begins, the agent can lose track of what has already happened. We address this problem by keeping a record of the work outside the conversation and using it to guide the next step, even when a different agent takes over.

Super-workflow gives an agent access to a workflow through a local **command-line interface (CLI)**. The agent calls `next` to find out what to do. After the work is done, the agent reports the result through the CLI. Behind that interface is an **agentic graph**: a description of the steps in a task and how the result of one step determines which steps can follow. An author can define a graph for a single question or reuse it for many runs. The goal is for the same definition to work across the tools that host agents.

## When to define a step

A graph is useful when an author can say in advance how a result should change the course of the work. If a test fails, the worker responsible for the repair needs the error. If it passes, the change can go to review. Inside the repair step, the worker may need to read the code, try a change and test it several times. The author can define when the repair is ready for review while leaving the investigation to the worker.

How much of the procedure should the author specify? How many agents should carry it out? StateFlow’s authors found that explicit stages improved results and reduced model cost on SQL and simulated household tasks compared with a model choosing its next action in a loop. Changing the examples given to the loop narrowed the SQL gap for GPT-3.5 and widened it for GPT-4. A separate study of teams found that dividing financial research into independent parts could help, while dividing a planning task in which each action changed the next one could hurt. These results suggest a practical test. Keep closely related decisions with one worker. Add a step when it produces something another worker can use, calls for independent review, or gives an interrupted run a useful place to resume. [StateFlow](https://arxiv.org/abs/2403.11322v5) · [Towards a Science of Scaling Agent Systems](https://arxiv.org/abs/2512.08296v3)

## How a run works

The author writes the graph in Go. Some steps need an agent to interpret evidence or make a judgment. Others can run a command, check a file or combine earlier results. For each step, the author specifies what information it needs, what it should return and how to use the answer. This lets the engine give the agent instructions for the work at hand. [Definition and five-minute tour](docs/proposals/go-api/README.md)

The agent driving the run is called the coordinator. Its `next` command can return several independent assignments when the host has room for them. Each worker claims an assignment through the CLI, reads its instructions and returns its result in the form the author specified. The worker can use `validate` to catch a missing field and correct it without spending an attempt. Once a result is accepted, the graph determines whether it goes to review, triggers a command or completes the job. [Coordinator protocol](docs/proposals/go-api/COORDINATOR.md)

The engine keeps a journal so that a new session can continue the same run from its recorded workspace and state directory. The journal tells the coordinator which results the run accepted. For work that changes another system, recovery can also require checking that system. Suppose the journal records an intent to publish a file and the process then stops. The file might already be at its destination. The graph can direct the next worker to check before publishing again. [Resumable pipeline](docs/proposals/go-api/examples/resumable/def.go)

## What belongs in the engine

The workflow trusts the worker’s account of what it did. The engine checks that the result belongs to the right assignment and has the required form. The worker can depart from ordinary guidance and explain why. For example, resolving a conflict may require editing a line just outside the conflict itself. A reviewer can inspect that choice. The firm boundaries concern the run protocol, decisions the user reserved and actions that affect systems outside the run.

We decide where a feature belongs by asking whether a second workflow would need the same behavior. Both a research team and a software team need to know which assignments are still open after a restart. The engine can keep that record for both. The research team decides how much evidence supports a claim, while the software team decides how many rounds of repair a change warrants. Those decisions belong in their workflow definitions. A rule can stay with its first workflow until another needs it.

The environment that runs an agent is its *harness*. The harness starts workers and determines how many can run at once. An adapter connects its public operations to the workflow. Workers on different harnesses can therefore use the same CLI to claim work and report progress, even when their hosts start or resume them in different ways.

## What different workflows need

**Deep research.** A research question can split into several questions that workers investigate independently. Their results come together in a shared record that connects claims to sources. Combining the results may reveal a gap. A proposed research graph could send a worker to look for contrary evidence or ask the writer to explain what remains unknown. The engine tracks which investigations returned. The research workflow decides whether their evidence answers the question.

**Kinship research.** Two searches can lead to different copies of the same historical record. Counting the copies as independent evidence could lead a researcher to accept the wrong relationship. The ex-USSR kinship research skill records people, sources, claims and unsuccessful searches separately. A graph based on this skill could bring leads together for comparison, reopen a disputed relationship when new evidence arrives, and continue other searches while an archive request awaits permission. The rules for identifying people and contacting archives would belong to this workflow. [Kinship research skill](https://github.com/DenisSergeevitch/ex-ussr-kinship-research)

**Software development.** A workflow can take an idea through design,
implementation, review and repair. Workers can make independent changes in
separate worktrees. The workflow tests and reviews the combined result, then
reports what passed, what remains open, and whether a person needs to make a
decision. The [feature example](docs/proposals/go-api/examples/feature/README.md)
covers these steps and shows how to reserve the merge decision.

**A product experiment.** A workflow could also take an idea through development, deploy it, run an A/B experiment and make a launch decision. Its owner would authorize those actions and specify how long to measure the result, what evidence would justify launch and what to do if the result were inconclusive. Deployment would need a way to check what happened after an interruption. The experiment would need a way to wait for observations before deciding whether to launch or roll back.

## How a run recovers

Recovery starts with what the worker observed. A failed test command might mean that the code is wrong or that a dependency was unavailable. Those cases need different responses. If a worker stops midway through its assignment, the coordinator can resume it with a specific instruction, start a fresh attempt or follow the workflow’s failure route. A unit awaiting permission through the harness can stay on hold without consuming a retry while independent work continues. A check the run could not perform remains unverified. [Recovery protocol](docs/proposals/go-api/COORDINATOR.md)

A retry is useful when another attempt could change the result. A workflow can limit repeated attempts, route the problem to a different step, or finish with a report of what was accomplished and what remains. It asks a person when that person reserved the decision. After an explicit stop, only a person may authorize resumption. The engine records the operator and the reason given for resuming. That record does not establish who was present. · [Retry and fallback examples](docs/proposals/go-api/examples/README.md)

## Out of scope, and why

**Policing workers and judging their conclusions.** A rule that forbids all edits outside a conflict can prevent a legitimate repair. A rule that counts citations can accept a weak claim supported by several copies of one source. The engine can check the structure of a result. The workflow can send the result to a reviewer who understands the work.

**Dependence on private harness records.** A transcript file may be readable on one host and absent on another. Using its last modification time to decide whether a worker is still active would give the same run different behavior on the two hosts. Some checks still use transcripts. The goal is for workers to report enough through the CLI that the engine can tell when to continue on any host. An operator investigating a problem can read the transcript when the host provides one.

**Routine supervision and domain policy.** Requiring a person to settle every disagreement would rule out unattended work. The author defines how to recover, what evidence is enough to finish and which decisions require a person. A family-history investigation and a software release can then use the same engine and apply different standards.

**Access control and hosted services.** A claim token connects a result to an assignment. Receiving an assignment does not give a worker permission to deploy a change or contact an archive. The host remains responsible for its sandbox, and the engine leaves credential management and command permissions to the systems that control them. A local run requires a workspace and a state directory. Running a shared service for many customers would add separate responsibilities for isolation and availability. A workflow may still use an external service when its owner has authorized it.

## How we know it works

The first test is how much effort it takes to write a new workflow. A human or an agent should be able to define a one-off job with the existing steps and adapters, inspect its routes, and simulate what happens when a step fails. If each new subject requires a change to the engine, the framework is too costly to reuse. [Tour and simulations](docs/proposals/go-api/examples/tour/sim_test.go)

The second test is whether the chosen harness and model can deliver the requested work without supervisor intervention. An evaluation records the revisions used, the requested outcome, what the run delivered and any help it received. Repairing an evaluation environment can reveal problems in later steps. A fresh run must succeed without that help to show the workflow can finish unattended. The same standard applies to a research report, a branch ready to merge or an authorized launch decision.

## Protocols

- The [coordinator protocol](docs/proposals/go-api/COORDINATOR.md) defines
  the commands that drive a run.
- The [authoring skill](skills/super-workflow/SKILL.md) explains how to
  define, build, and drive a workflow.

## Essential reading

Start with ReAct and StateFlow for two ways to decide what happens next. The remaining papers cover how to represent a workflow, how to discover one, and when the work benefits from a team. Dates and citation counts are from Semantic Scholar, checked November 21, 2026.

1. **ReAct** (Yao et al.). A single agent chooses an action, observes the result and uses it to decide what to do next. This is the baseline to understand before adding steps or workers. [ReAct: Synergizing Reasoning and Acting in Language Models](https://arxiv.org/abs/2210.03629) · 2022-10-06 · [14,214 indexed citations](https://www.semanticscholar.org/paper/99832586d55f540f603637e458a292406a0ed75d)
2. **StateFlow** (Wu et al.). The author defines stages for a task and gives the model a different job in each stage. The SQL experiments compare that design with a single agent following a loop, including what happens when the loop gets better instructions. [StateFlow: Enhancing LLM Task-Solving through State-Driven Workflows](https://arxiv.org/abs/2403.11322v5) · 2024-03-17 · [93 indexed citations](https://www.semanticscholar.org/paper/b8cc74d8c2a009cc99a8b285503d86be80d840d9)
3. **Learning to Construct Practical Agentic Systems** (Kumar et al.). The authors give the same model the same tools and compare a fixed workflow with a ReAct loop that chooses its next tool after each result. The fixed workflow scored higher on financial questions, while the loop scored higher on date reasoning. The authors tuned the fixed workflows on examples and used a default prompt for ReAct. [Table 1 and experimental setup](https://arxiv.org/html/2606.00189#S5.T1) · 2026-05-29 · [0 indexed citations](https://www.semanticscholar.org/paper/022b958ebb8bb5727fb8ec449943a4835dba68b3)
4. **GPTSwarm** (Zhuge et al.). The authors represent operations as nodes and the information passed between them as edges. They test how changing the instructions at a node or changing who receives its output affects the result. [Language Agents as Optimizable Graphs](https://arxiv.org/abs/2402.16823) · 2024-02-26 · [97 indexed citations](https://www.semanticscholar.org/paper/4580fa51ab097a56b98ed1c820f6547c145d6ca4)
5. **AFlow** (Zhang et al.). AFlow generates candidate workflows as code and tests them on examples. It illustrates a distinction that matters for authors: the search for a good procedure can be complex even when the chosen procedure is simple to run. [AFlow: Automating Agentic Workflow Generation](https://arxiv.org/abs/2410.10762) · 2024-10-14 · [555 indexed citations](https://www.semanticscholar.org/paper/53132ea6c107479d4557631299d3ed525109b464)
6. **Automated Design of Agentic Systems** (Hu, Lu and Clune). An agent writes code for new agents, drawing on an archive of earlier designs and their test scores. The authors also test whether a design that works on one task transfers to another. [Paper (PDF)](https://arxiv.org/pdf/2408.08435) · 2024-08-15 · [540 indexed citations](https://www.semanticscholar.org/paper/c9537f656e7d9713fd4108ce7bf512290f48e562)
7. **Towards a Science of Scaling Agent Systems** (Kim et al.). The authors compare one agent with several ways of organizing a team under common compute budgets. Teams helped on financial research that could be split up and struggled on planning tasks where each action changed the next decision. [Read the study](https://arxiv.org/abs/2512.08296v3) · 2025-12-09 · [187 indexed citations](https://www.semanticscholar.org/paper/628dcc7046ea462b5e770a0f070c5b0988aaeee0)
