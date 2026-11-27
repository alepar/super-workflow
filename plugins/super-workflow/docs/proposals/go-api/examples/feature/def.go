// Package feature is a small, complete design-code-review workflow to copy.
package feature

import (
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	Setup            struct{}
	Design           struct{}
	DesignReview     struct{}
	PrepareTasks     struct{}
	Tasks            struct{}
	Verify           struct{}
	Suite            struct{}
	Quality          struct{}
	Snapshot         struct{}
	GoalCheck        struct{}
	RunGoalCheck     struct{}
	Fix              struct{}
	Report           struct{}
	Approve          struct{}
	BaseMerge        struct{}
	ResolveBaseMerge struct{}
	ResolveTaskMerge struct{}
	Recovery         struct{}
	Done             struct{}
	Kept             struct{}
	Stopped          struct{}
	StartTask        struct{}
	Implement        struct{}
	Check            struct{}
	Review           struct{}
	Merge            struct{}
	TaskDone         struct{}
	TaskStopped      struct{}
)

type Verdict string

const (
	OK      Verdict = "ok"
	Revise  Verdict = "revise"
	FixCode Verdict = "fix"
)

type Answer string

const (
	MergeBase Answer = "merge"
	Keep      Answer = "keep"
)

type Task struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Files      []string `json:"files"`
	Acceptance []string `json:"acceptance"`
}
type Plan struct {
	Tasks []Task `json:"tasks"`
}
type Patch struct {
	Summary string `json:"summary"`
	Head    string `json:"head"`
}
type Summary struct {
	ReportHead string
	Built      []string
	Verdict    string
	Degraded   []string
}

type verifiedTree struct {
	VerifiedHead          string
	PassedBefore, Repairs int
}
type verificationSnapshot struct {
	IgnoredFiles string
	BuildRetries int
}

// GoalTest has no executable output; the agent supplies only the allocated artifact.
type GoalTest struct{}

type layout struct {
	Workdir, Integration                   wf.RunPath
	Branch, BaseBranch, BaseSHA, GoalTable string
}
type taskInput struct {
	Task
	Workdir, Integration                      wf.RunPath
	TaskBranch, BaseSHA, Suite, CriteriaTable string
	Spec                                      taskSpecRef
}
type taskSpecRef struct {
	Path wf.RunPath
	Hash string
}
type taskResult struct {
	ID, Summary string
	Degraded    []string
}

// Definition reads top to bottom: design, fan-out, suite, report, approval.
// Review caps are two revisions AFTER the first attempt. Retry budgets cover
// invalid submissions separately. Every agent is dispatched in fresh context.
func Definition() *wf.Def {
	d := wf.New("feature").Version(6)
	goal, suiteParam := d.Param("goal"), d.Param("suite")
	checkParam := d.Param("goal-check-command")
	d.RunKey(goal)
	verdicts := wf.NewEnum(d, "design-verdict", OK, Revise)
	answers := wf.NewEnum(d, "answer", MergeBase, Keep)
	green := d.Counter("acceptance-passed")
	repairs := d.Counter("integration-repairs")
	buildRetries := d.Counter("goal-test-build-retries")
	location := shell.Observe(shell.Run("printf", "%s\n", "{run}"))
	baseBranch := shell.Observe(shell.Run("git", "branch", "--show-current").In("{workspace}"))
	baseHead, baseStatus := git.Head("{workspace}"), git.Status("{workspace}")
	setup := wf.NewCompute[Setup, layout](d, func(c wf.Ctx) (layout, error) {
		if len(requirements(c.Param(goal))) == 0 {
			return layout{}, errors.New("goal must contain at least one nonempty requirement")
		}
		if strings.TrimSpace(c.Param(checkParam)) == "" {
			return layout{}, errors.New("goal-check-command is required: pass go test -count=1 -json (CLI registration supplies this default)")
		}
		loc, err := wf.Observed(c, location)
		if err != nil {
			return layout{}, err
		}
		branch, err := wf.Observed(c, baseBranch)
		if err != nil {
			return layout{}, err
		}
		head, err := wf.Observed(c, baseHead)
		if err != nil {
			return layout{}, err
		}
		status, err := wf.Observed(c, baseStatus)
		if err != nil {
			return layout{}, err
		}
		run := strings.TrimSpace(loc.Stdout)
		if run == "" || strings.Contains(run, "\n") {
			return layout{}, errors.New("missing run identifier")
		}
		if strings.TrimSpace(branch.Stdout) == "" {
			return layout{}, errors.New("HEAD is detached; check out a base branch")
		}
		if !status.Clean {
			return layout{}, errors.New("base checkout is dirty; commit or stash changes first")
		}
		path := wf.RunPath(filepath.Join("worktrees", "integration"))
		return layout{path, path, "feature/" + run, strings.TrimSpace(branch.Stdout), head, requirementTable(requirements(c.Param(goal)))}, nil
	}).Observes(location, baseBranch, baseHead, baseStatus)
	design := wf.NewAgent[Design, Plan](d)
	spec := wf.NewArtifact[string](design, "spec.md")
	review := wf.NewAgent[DesignReview, ReviewResult](d)
	prepare := wf.NewCompute[PrepareTasks, []taskInput](d, func(c wf.Ctx) ([]taskInput, error) {
		l := wf.ComputedOf(c, setup)
		ref := wf.RefOf(c, spec)
		path, err := wf.RunPathOf(c, ref.Path)
		if err != nil {
			return nil, err
		}
		var items []taskInput
		for _, t := range wf.OutputOf(c, design).Tasks {
			items = append(items, taskInput{t, wf.RunPath(filepath.Join(filepath.Dir(string(l.Integration)), "task-"+t.ID)), l.Integration, l.Branch + "-" + t.ID, l.BaseSHA, c.Param(suiteParam), requirementTable(criteria(t.Acceptance)), taskSpecRef{path, ref.Hash}})
		}
		return items, nil
	})
	tasks := wf.NewMap[Tasks, taskInput, string, taskResult](d, wf.FromCompute(prepare, func(v []taskInput) []taskInput { return v }), func(t taskInput) string { return t.ID }, taskDefinition())
	cleanIntegration := git.Status("{integration}")
	ignoredFiles := shell.Observe(shell.Run("bash", "-o", "pipefail", "-c", ignoredFingerprint).In("{integration}"))
	verify := wf.NewCompute[Verify, verifiedTree](d, func(c wf.Ctx) (verifiedTree, error) {
		h, err := wf.Observed(c, git.Head("{integration}"))
		if err != nil {
			return verifiedTree{}, err
		}
		status, err := wf.Observed(c, cleanIntegration)
		if err != nil {
			return verifiedTree{}, err
		}
		if !status.Clean {
			return verifiedTree{}, errors.New("integration must be clean before verification")
		}
		return verifiedTree{h, c.Count(green), c.Count(repairs)}, nil
	}).Observes(git.Head("{integration}"), cleanIntegration)

	suite := shell.Command[Suite](d, shell.Run("sh", "-c", "{suite}", "feature-suite").In("{integration}")).Key("feature", "{run}", "suite").Idempotent()
	quality := shell.Command[Quality](d, shell.Run("sh", "-c", "files=$(gofmt -l .) && test -z \"$files\" && go vet ./...").In("{integration}")).Key("feature", "{run}", "quality").Idempotent()
	// Capture ignored build output after the suite, immediately before acceptance.
	snapshot := wf.NewCompute[Snapshot, verificationSnapshot](d, func(c wf.Ctx) (verificationSnapshot, error) {
		files, err := wf.Observed(c, ignoredFiles)
		if err != nil {
			return verificationSnapshot{}, err
		}
		if files.Exit != 0 {
			return verificationSnapshot{}, errors.New("cannot fingerprint ignored files (ignored Go source is forbidden)")
		}
		return verificationSnapshot{files.Stdout, c.Count(buildRetries)}, nil
	}).Observes(ignoredFiles)
	accept := wf.NewAgent[GoalCheck, GoalTest](d)
	blackbox := wf.NewArtifact[string](accept, "goal_test.go")
	acceptance := shell.Command[RunGoalCheck](d, shell.Run("sh", "-c", goalCheckScript, "feature-goal-check", "{goal-check-command}", wf.ArtifactPath(blackbox)).In("{integration}")).Key("feature", "{run}", "acceptance").Idempotent()
	wf.Consumes(acceptance, blackbox)
	fix := wf.NewAgent[Fix, Patch](d)
	report := wf.NewCompute[Report, Summary](d, func(c wf.Ctx) (Summary, error) {
		h, err := wf.Observed(c, git.Head("{integration}"))
		if err != nil {
			return Summary{}, err
		}
		s := Summary{Verdict: "blocked", ReportHead: h}
		joined := wf.Joined(c, tasks)
		for _, item := range joined.Items {
			if item.Terminal == "TaskDone" {
				s.Built = append(s.Built, item.Result.ID+": "+item.Result.Summary)
			}
			s.Degraded = append(s.Degraded, item.Result.Degraded...)
		}
		sort.Strings(s.Built)
		sort.Strings(s.Degraded)
		s.Degraded = slices.Compact(s.Degraded)
		s.Degraded = append(s.Degraded, unmet(wf.OutputOf(c, review))...)
		if c.Count(repairs) > 0 {
			s.Degraded = append(s.Degraded, "unreviewed integration repair commits; human review required")
		}
		if v := wf.ComputedOf(c, verify); c.Count(green) > v.PassedBefore && c.Count(repairs) == v.Repairs {
			status, err := wf.Observed(c, cleanIntegration)
			if err != nil {
				return Summary{}, err
			}
			files, err := wf.Observed(c, ignoredFiles)
			if err != nil {
				return Summary{}, err
			}
			if !status.Clean || files.Exit != 0 || files.Stdout != wf.ComputedOf(c, snapshot).IgnoredFiles || h != wf.ComputedOf(c, verify).VerifiedHead {
				s.Degraded = append(s.Degraded, "integration changed during acceptance; verification does not cover the committed tree")
				return s, nil
			}
			s.Verdict = "ready"
			if len(s.Degraded) > 0 {
				s.Verdict = "degraded"
			}
		}
		return s, nil
	}).Observes(git.Head("{integration}"), cleanIntegration, ignoredFiles)
	baseMerge := wf.NewCompute[BaseMerge, struct{}](d, func(wf.Ctx) (struct{}, error) { return struct{}{}, nil })
	resolveBase := wf.NewAgent[ResolveBaseMerge, Patch](d)
	approve := wf.NewGate[Approve, Answer](d, "Inspect Report in the journal and the integration branch. Merge into the original base, or keep for later?", answers, wf.Reserved)
	done, kept, stopped := wf.NewTerminal[Done](d, wf.Success), wf.NewTerminal[Kept](d, wf.Stopped), wf.NewTerminal[Stopped](d, wf.Stopped)
	recovery := recoveryGate(d, stopped)
	failures(d, recovery)

	setup.Retry(wf.Retry(wf.Const(2), setup.To(stopped, "setup unavailable").Unreachable("Refused takes the precondition route; other failure classes take definition defaults before retry exhaustion")).OnClass(wf.Refused, setup.To(stopped, "setup precondition failed: see refusal cause")))
	begin := setup.To(design, "isolated integration workspace").With(git.WorktreeAdd(wf.KeyOf("feature", "{run}", "integration"), "{integration}", "{branch}", "{baseSHA}"))
	setup.Then(begin)
	design.Spec(wf.PromptText("Design {{goal}}. Inspect the repo read-only. Return only independent tasks with id, title, files, acceptance (a nonempty list of criteria); IDs use lowercase letters, digits and hyphens. Tasks must be independently implementable from the current base. Write the spec to the allocated spec.md artifact; do not return its path in the result. Prior review: {{review}}.", goal, wf.As("review", wf.Out(review))), wf.Retry(wf.Const(2), design.To(stopped, "design unavailable")).OnClass(wf.Unavailable, design.To(recovery, "default unavailable route").Unreachable("inline prompt with no adapter observations or external edge effects cannot become unavailable")), wf.Checkf("nonempty independent task contracts", checkPlan).
		Accepts(Plan{Tasks: []Task{{ID: "add", Title: "Add addition", Files: []string{"calc.go"}, Acceptance: []string{"2+3 is 5"}}}}).
		Refuses(Plan{}, "tasks are required"))
	designed := design.To(review, "review the design")
	design.Then(designed)
	wf.MaxVisits(wf.Const(3), design.To(prepare, "design revision cap: retain open reasons"))
	review.Spec(wf.PromptText("{{requirements}}\n\nReview the plan against the requirements above. Return one row per listed id: id, met, evidence, tasks (covering task IDs). Do not copy requirement text into the result. Plan: {{plan}}. Spec: {{spec}}. Check exact public API names, exports and signatures, truthful comments/docs, external-package tests, independence and acceptance coverage. A met row must name valid covering tasks. Evidence must cite the spec or task contract; explain gaps in failed rows. Do not edit files.", wf.Bound("requirements", "{goalTable}"), wf.As("plan", wf.Out(design)), wf.As("spec", spec)), wf.Retry(wf.Const(2), review.To(stopped, "design review unavailable")).OnClass(wf.Unavailable, review.To(recovery, "default unavailable route").Unreachable("inline prompt with no adapter observations or external edge effects; artifact input errors are blocked")), wf.Checkf("complete requirement evidence", func(r ReviewResult, c wf.Ctx) error {
		return checkRows(r, requirements(c.Param(goal)), wf.OutputOf(c, design).Tasks)
	}).Accepts(ReviewResult{Rows: []ReviewRow{{ID: "g1", Met: true, Evidence: "spec.md: addition contract", Tasks: []string{"add"}}}}).
		Refuses(ReviewResult{Rows: []ReviewRow{{ID: "g1", Met: true}}}, `row "g1": met design row requires at least one covering task ID`))
	wf.Consumes(review, spec)
	review.RouteCases(wf.Cases[DesignReview, ReviewResult](func(r ReviewResult) Verdict { return verdict(r, Revise) }, verdicts, wf.On(OK, review.To(prepare, "approved")), wf.On(Revise, review.To(design, "revise"))))
	prepared := prepare.To(tasks, "dispatch independent tasks")
	prepare.Then(prepared)
	prepare.Retry(wf.Retry(wf.Const(2), prepare.To(stopped, "cannot prepare tasks").Unreachable("pure transformation of accepted plan and setup data always returns items without error")).OnClass(wf.Blocked, prepare.To(recovery, "default blocked route").Unreachable("typed accepted plan and setup data plus allocated artifact reference require no fallible external operation")).OnClass(wf.Unavailable, prepare.To(recovery, "default unavailable route").Unreachable("pure transformation has no adapter observations or external edge effects")))
	tasks.Join(wf.AllOK(), tasks.To(verify, "all tasks merged"), tasks.To(report, "task failed: not ready"))
	tasks.OnClass(wf.Blocked, tasks.To(recovery, "default blocked route").Unreachable("accepted plan has unique typed task keys and prepared typed input; AllOK joins child outcomes without a fallible predicate"))
	tasks.OnClass(wf.Unavailable, tasks.To(recovery, "default unavailable route").Unreachable("prepared typed task input has no external source observation; AllOK routes failed children through the partial-failure edge"))
	verified := verify.To(suite, "pin clean integration commit for verification")
	verify.Then(verified)
	verify.Retry(wf.Retry(wf.Const(1), verify.To(report, "cannot verify integration")).OnClass(wf.Blocked, verify.To(recovery, "default blocked route").Unreachable("declared observation errors are refused or unavailable; compute and route have no blocked operation")))
	suite.Retry(wf.Retry(wf.Const(2), suite.To(report, "suite unavailable")).OnClass(wf.Blocked, suite.To(recovery, "default blocked route").Unreachable("command has Retry: execution errors are refused or unavailable and nonzero exits use normal routes")))
	suiteOK, suiteRed := suite.To(quality, "suite green"), suite.To(fix, "suite red")
	suite.RouteExit(suiteOK, suiteRed)
	quality.Retry(wf.Retry(wf.Const(2), quality.To(report, "quality checks unavailable")).OnClass(wf.Blocked, quality.To(recovery, "default blocked route").Unreachable("command has Retry: execution errors are refused or unavailable and nonzero exits use normal routes")))
	qualityOK, qualityRed := quality.To(snapshot, "format and vet clean"), quality.To(fix, "format or vet failed")
	quality.RouteExit(qualityOK, qualityRed)
	captured := snapshot.To(accept, "fingerprint ignored files before independent acceptance")
	snapshot.Then(captured)
	snapshot.Retry(wf.Retry(wf.Const(1), snapshot.To(report, "cannot inspect acceptance checkout")).OnClass(wf.Blocked, snapshot.To(recovery, "default blocked route").Unreachable("declared observation errors are refused or unavailable; compute and route have no blocked operation")))
	accept.Spec(wf.PromptText("{{requirements}}\n\nWrite an independent black-box test of these goal requirements in the integrated checkout. Write the test at the allocated goal_test.go path, outside the checkout, and return an empty JSON object. Use an external test package (package <name>_test) that imports this module and calls its public API; the file is compiled standalone, so internal-package names are not in scope. Test public functions, methods and zero values as appropriate. The engine runs the configured test command on that artifact and requires at least one executed passing test. Do not change checkout files. You have only the requirements and checkout, not the design or earlier reviews. Previous engine test result (including compiler diagnostics): {{check}}. If the test did not build, correct the test file at its newly allocated path; do not change product code.", wf.Bound("requirements", "{goalTable}"), wf.As("check", wf.Out(acceptance))), wf.Retry(wf.Const(2), accept.To(report, "goal test unavailable")), wf.Checkf("unchanged checkout", func(_ GoalTest, c wf.Ctx) error {
		h, err := wf.Observed(c, git.Head("{integration}"))
		if err != nil {
			return err
		}
		status, err := wf.Observed(c, cleanIntegration)
		if err != nil {
			return err
		}
		files, err := wf.Observed(c, ignoredFiles)
		if err != nil {
			return err
		}
		if !status.Clean || files.Exit != 0 || files.Stdout != wf.ComputedOf(c, snapshot).IgnoredFiles || h != wf.ComputedOf(c, verify).VerifiedHead {
			return errors.New("acceptance must not edit or commit integration")
		}
		return nil
	}).Observes(git.Head("{integration}"), cleanIntegration, ignoredFiles).
		Accepts(GoalTest{}).
		Refuses(GoalTest{}, "acceptance must not edit or commit integration", wf.ExampleObserved(cleanIntegration, git.StatusResult{Clean: false})))
	// At most two authoring visits per version: initial plus two product repairs.
	wf.MaxVisits(wf.Const(6), accept.To(report, "goal-test authoring cap"))
	preparedTest := accept.To(acceptance, "run the configured test command")
	accept.Then(preparedTest)
	acceptance.Retry(wf.Retry(wf.Const(2), acceptance.To(report, "black-box check unavailable")).OnClass(wf.Blocked, acceptance.To(recovery, "default blocked route").Unreachable("command has Retry: execution errors are refused or unavailable and nonzero exits use normal routes")))
	blackGreen, blackRed := acceptance.To(report, "independent acceptance passed").With(green.Inc()), acceptance.To(fix, "black-box test failed")
	testBuildRed := acceptance.To(accept, "goal test did not build: correct the test").With(buildRetries.Inc())
	persistentBuildRed := acceptance.To(fix, "goal test still does not build: public API may be missing or unexported")
	acceptance.Route(func(r wf.RunResult, c wf.Ctx) wf.Edge[RunGoalCheck] {
		if goalTestBuildFailed(r) {
			if c.Count(buildRetries) > wf.ComputedOf(c, snapshot).BuildRetries {
				return persistentBuildRed
			}
			return testBuildRed
		}
		if goalTestsPassed(r) {
			return blackGreen
		}
		return blackRed
	}, blackGreen, blackRed, testBuildRed, persistentBuildRed)
	fix.Spec(wf.PromptText("Fix integration failures in {{dir}} for {{goal}}. Run {{suite}}, gofmt -l ., and go vet ./... to see current diagnostics. Acceptance artifact: {{test}}. Engine rerun output (exit, stdout and stderr): {{check}}. If the goal test still does not build after its author retried, the public API may be missing or unexported; inspect the compiler output and the required API. Rerun the artifact for diagnostics; a nonzero exit or zero executed tests means verification failed. Make the smallest repair, format Go files, rerun checks and commit. Return summary and full HEAD. Do not modify the original checkout. Never copy the allocated goal_test.go artifact into the product or add/change a file with that name in your commits.", wf.Bound("dir", "{integration}"), goal, suiteParam, wf.As("test", blackbox), wf.As("check", wf.Out(acceptance))), wf.Retry(wf.Const(2), fix.To(report, "repair unavailable")), headCheck("{integration}"), goalTestStaysExternal())
	fix.GitAuthority(wf.GitAuthority{Refs: []string{"{branch}"}, Allowed: []string{"commit"}, Forbidden: []string{"push", "merge into base"}})
	repaired := fix.To(verify, "verify repair").With(repairs.Inc())
	fix.Then(repaired)
	wf.MaxVisits(wf.Const(2), fix.To(report, "suite repair cap"))
	report.Retry(wf.Retry(wf.Const(1), report.To(stopped, "report unavailable")).OnClass(wf.Blocked, report.To(recovery, "default blocked route").Unreachable("observation errors are refused or unavailable; blocked summary is normal output, not a failure class")))
	ready, blocked := report.To(approve, "human reviews readiness"), report.To(stopped, "not ready").Ends("required tasks or verification did not complete; keep integration for inspection")
	report.Route(func(s Summary, _ wf.Ctx) wf.Edge[Report] {
		if s.Verdict == "blocked" {
			return blocked
		}
		return ready
	}, ready, blocked)
	approve.Answers(wf.On(MergeBase, approve.To(baseMerge, "human approved pinned base merge")), wf.On(Keep, approve.To(kept, "keep integration branch").Ends(wf.Stopped, "retain the integration branch without merging"))).Timeout(24*time.Hour, approve.To(kept, "approval expired: keep branch"))
	baseMerge.Retry(wf.Retry(wf.Const(1), baseMerge.To(recovery, "base merge unavailable").Unreachable("compute always succeeds; merge edge failures take Blocked, Unavailable or Uncertain class routes")).OnClass(wf.Blocked, baseMerge.To(resolveBase, "resolve base merge failure")))
	baseMerged := baseMerge.To(done, "merge approved pinned commit").With(git.MergeInto(wf.KeyOf("feature", "{run}", "base"), wf.PathTemplate("{workspace}"), "{reportHead}", "{baseBranch}", false))
	baseMerge.Then(baseMerged)
	resolveBase.Spec(wf.PromptText("The approved base merge failed: {{cause}}. Work only in {{dir}} on {{branch}}. Merge the current original base branch {{base}} into this integration branch, resolve conflicts with the smallest change, run {{suite}}, and commit. Do not modify the original checkout, merge into base, or push. Never add or change goal_test.go. Return summary and clean full HEAD. The engine reruns all verification and obtains fresh human approval before merging the new commit.", wf.Bound("cause", "{blockedCause}"), wf.Bound("dir", "{integration}"), wf.Bound("branch", "{branch}"), wf.Bound("base", "{baseBranch}"), suiteParam), wf.Retry(wf.Const(2), resolveBase.To(recovery, "base conflict resolution unavailable")), headCheck("{integration}"), goalTestStaysExternal())
	resolveBase.GitAuthority(wf.GitAuthority{Refs: []string{"{branch}"}, Allowed: []string{"merge base into integration", "commit"}, Forbidden: []string{"push", "merge into base"}})
	resolvedBase := resolveBase.To(verify, "reverify resolved integration before fresh approval").With(repairs.Inc())
	resolveBase.Then(resolvedBase)
	wf.MaxVisits(wf.Const(2), resolveBase.To(recovery, "base conflict resolution cap"))
	return d
}

func taskDefinition() wf.Child[taskInput, taskResult] {
	d := wf.New("feature-task")
	integrationRed := d.Counter("integration-build-red")
	verdicts := wf.NewEnum(d, "code-verdict", OK, FixCode)
	start := wf.NewCompute[StartTask, struct{}](d, func(wf.Ctx) (struct{}, error) { return struct{}{}, nil })
	implement := wf.NewAgent[Implement, Patch](d)
	check := shell.Command[Check](d, shell.Run("sh", "-c", "{suite}").In("{workdir}")).Key("feature", "{run}", "{id}", "check").Idempotent()
	review := wf.NewAgent[Review, ReviewResult](d)
	resolve := wf.NewAgent[ResolveTaskMerge, Patch](d)
	merge := shell.Command[Merge](d, shell.Run("sh", "-c", "{suite}").In("{integration}")).Key("feature", "{run}", "{id}", "build").Idempotent()
	done, stopped := wf.NewTerminal[TaskDone](d, wf.Success), wf.NewTerminal[TaskStopped](d, wf.Stopped)
	recovery := recoveryGate(d, stopped)
	failures(d, recovery)
	wf.Hold(merge, d.Lock("feature-merge", wf.LockRunKey))
	start.Retry(wf.Retry(wf.Const(1), start.To(stopped, "task setup failed").Unreachable("compute always succeeds; worktree edge failures take definition class routes")))
	begin := start.To(implement, "isolated task workspace").With(git.WorktreeAdd(wf.KeyOf("feature", "{run}", "{id}", "workspace"), "{workdir}", "{taskBranch}", "{baseSHA}"))
	start.Then(begin)
	implement.Spec(wf.PromptText("Implement only this task: {{item}}. Work in its workdir. Read the spec reference. Write tests for its acceptance; run the supplied suite for build and tests. Commit on taskBranch and return summary and full HEAD. Prior review: {{review}}. Do not merge or push.", wf.ChildInput(), wf.As("review", wf.Out(review))), wf.Retry(wf.Const(2), implement.To(stopped, "implementation unavailable")), headCheck("{workdir}"))
	implement.GitAuthority(wf.GitAuthority{Refs: []string{"{taskBranch}"}, Allowed: []string{"commit"}, Forbidden: []string{"push", "merge"}})
	patched := implement.To(check, "check committed task")
	implement.Then(patched)
	wf.MaxVisits(wf.Const(3), implement.To(stopped, "task repair cap"))
	check.Retry(wf.Retry(wf.Const(2), check.To(stopped, "check unavailable")).OnClass(wf.Blocked, check.To(recovery, "default blocked route").Unreachable("command has Retry: execution errors are refused or unavailable and nonzero exits use normal routes")))
	checked, repair := check.To(review, "tests pass"), check.To(implement, "tests fail: rerun for diagnostics")
	check.RouteExit(checked, repair)
	review.Spec(wf.PromptText("{{criteria}}\n\nReview the implementation against the criteria above. Return one row per listed id: id, met, evidence (specific code/test locations and results). Do not copy criterion text into the result. Task: {{item}}. Implementation: {{patch}}. Latest conflict resolution (if any): {{resolution}}. Inspect the baseSHA diff and the spec. Check public names, exports, signatures, truthful comments/docs and tests using the API from outside the package. Mark failures met=false with concrete repair evidence. Do not change files.", wf.Bound("criteria", "{criteriaTable}"), wf.ChildInput(), wf.As("patch", wf.Out(implement)), wf.As("resolution", wf.Out(resolve))), wf.Retry(wf.Const(2), review.To(stopped, "review unavailable")).OnClass(wf.Blocked, review.To(resolve, "resolve task merge failure")), wf.Checkf("complete criterion evidence", func(r ReviewResult, c wf.Ctx) error {
		return checkRows(r, criteria(wf.InputOf[taskInput](c).Acceptance), nil)
	}).Accepts(ReviewResult{Rows: []ReviewRow{{ID: "c1", Met: true, Evidence: "go test: addition passes"}}}).
		Refuses(ReviewResult{}, "review row IDs: missing [\"c1\"]; unknown []; duplicate []; expected IDs:\n| ID | Requirement |\n| --- | --- |\n| c1 | 2+3 is 5 |\n"))
	// Incoming effects acquire Merge's lock; the build stays under the same lock.
	merged := review.To(merge, "merge reviewed commit under lock").With(git.Merge(wf.KeyOf("feature", "{run}", "{id}", "merge"), "{integration}", "{head}", "{taskBranch}", false))
	review.RouteCases(wf.Cases[Review, ReviewResult](func(r ReviewResult) Verdict { return verdict(r, FixCode) }, verdicts, wf.On(OK, merged), wf.On(FixCode, review.To(implement, "fix review findings"))))
	resolve.Spec(wf.PromptText("The reviewed task merge failed: {{cause}}. Task: {{item}}. In the task workdir, merge the current integration branch into taskBranch and resolve conflicts while preserving the task contract and the integrated work. Run the supplied suite, commit, and return summary and clean full HEAD. Do not edit the integration checkout, merge into it, or push. The engine reruns tests and independent review before retrying the merge.", wf.Bound("cause", "{blockedCause}"), wf.ChildInput()), wf.Retry(wf.Const(2), resolve.To(recovery, "task conflict resolution unavailable")), headCheck("{workdir}"))
	resolve.GitAuthority(wf.GitAuthority{Refs: []string{"{taskBranch}"}, Allowed: []string{"merge integration into task", "commit"}, Forbidden: []string{"push", "merge into integration"}})
	resolved := resolve.To(check, "test and review conflict resolution")
	resolve.Then(resolved)
	wf.MaxVisits(wf.Const(2), resolve.To(recovery, "task conflict resolution cap"))
	merge.Retry(wf.Retry(wf.Const(2), merge.To(stopped, "merge build unavailable")).OnClass(wf.Blocked, merge.To(recovery, "default blocked route").Unreachable("command has Retry: execution errors are refused or unavailable and nonzero exits use normal routes")))
	mergedOK, mergedRed := merge.To(done, "merged and checked"), merge.To(done, "merge kept: parent suite owns integration repair").With(integrationRed.Inc())
	merge.RouteExit(mergedOK, mergedRed)
	wf.Yield(done, func(c wf.Ctx) taskResult {
		r := taskResult{ID: wf.InputOf[taskInput](c).ID, Summary: wf.OutputOf(c, implement).Summary}
		if c.Count(integrationRed) > 0 {
			r.Degraded = []string{"integration build red after merge; merged tasks retained for parent suite repair"}
		}
		return r
	})
	wf.Yield(stopped, func(c wf.Ctx) taskResult {
		return taskResult{ID: wf.InputOf[taskInput](c).ID, Degraded: []string{"task " + wf.InputOf[taskInput](c).ID + " did not complete"}}
	})
	return wf.Typed[taskInput, taskResult](d)
}

// Operational failures hand back to a person after applicable backoff; invalid
// submissions keep each state's retry policy. No failed effect is best-effort.
func failures(d *wf.Def, stop wf.State) {
	d.OnFailures(stop, wf.Blocked, wf.Unavailable, wf.Uncertain)
}

// Operational failures preserve the checkout for a person; they never claim success.
func recoveryGate(d *wf.Def, stop wf.State) *wf.Gate[Recovery, Answer] {
	answers := wf.NewEnum(d, "recovery-answer", Keep)
	gate := wf.NewGate[Recovery, Answer](d, "Recovery needs a person. Inspect the latest blocked/failure record in the journal for the Git cause and affected path. For worktree creation failures, resolve stale paths, worktree locks or disk space before starting a new run. For exhausted merge repair, inspect the retained branches and resolve the conflict manually. Keep the branches and stop this run?", answers, wf.Reserved)
	gate.Answers(wf.On(Keep, gate.To(stop, "person retained branches for recovery").Ends(wf.Stopped, "retain branches for manual recovery"))).Timeout(24*time.Hour, gate.To(stop, "recovery expired: branches retained"))
	return gate
}
