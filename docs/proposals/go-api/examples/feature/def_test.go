package feature_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

const goal = "Add addition"
const criterion = "2+3 is 5"
const blackBox = "package acceptance_test\nimport (\"testing\";calc \"fixture\")\nfunc TestAPI(t *testing.T) {if calc.Add(2,3)!=5 {t.Fatal(\"sum\")}}\n"

func plan() feature.Plan {
	return feature.Plan{Tasks: []feature.Task{{ID: "add", Title: goal, Files: []string{"calc.go"}, Acceptance: []string{criterion}}}}
}
func rows(id, requirement string, met bool) feature.ReviewResult {
	return feature.ReviewResult{Rows: []feature.ReviewRow{{ID: id, Met: met, Evidence: "TestAPI: calc.go:3 and go test: passed", Tasks: []string{"add"}}}}
}
func design() wftest.Step {
	return wftest.SubmitAt("Design", plan()).WithArtifacts(map[string]string{"spec.md": goal})
}
func designReview(met bool) wftest.Step {
	return wftest.SubmitAt("DesignReview", rows("g1", goal, met))
}
func implement() wftest.Step {
	return wftest.SubmitAt("Implement", feature.Patch{Summary: goal, Head: strings.Repeat("a", 40)})
}
func review(met bool) wftest.Step { return wftest.SubmitAt("Review", rows("c1", criterion, met)) }

const goalEvents = "{\"Action\":\"run\",\"Test\":\"TestAPI\"}\n{\"Action\":\"pass\",\"Test\":\"TestAPI\"}\n"

func accept() wftest.Step {
	return wftest.SubmitAt("GoalCheck", feature.GoalTest{}).WithArtifacts(map[string]string{"goal_test.go": blackBox})
}
func failGoalCheck(w *wftest.World, once bool) {
	if once {
		wftest.ExitOnce(w, 1, "sh", "-c")
	} else {
		wftest.Exit(w, 1, "sh", "-c")
	}
	// The broad goal wrapper prefix also matches the earlier suite commands.
	wftest.Exit(w, 0, "sh", "-c", "go test ./...")
	wftest.Exit(w, 0, "sh", "-c", "files=$(gofmt -l .) && test -z \"$files\" && go vet ./...")
}
func fix() wftest.Step {
	return wftest.SubmitAt("Fix", feature.Patch{Summary: "repair", Head: strings.Repeat("a", 40)})
}
func happy() []wftest.Step {
	return []wftest.Step{design(), designReview(true), implement(), review(true), accept()}
}
func world(t *testing.T) *wftest.World { return checkoutWorld(t, true, "main") }
func checkoutWorld(t *testing.T, clean bool, branch string) *wftest.World {
	t.Helper()
	w := wftest.NewWorld()
	wftest.Stdout(w, goalEvents, "sh", "-c")
	wftest.OnRead(w, func(d wf.ObservationDesc) (json.RawMessage, bool) {
		var v any
		switch d.Adapter + "." + d.Op {
		case "git.head":
			v = strings.Repeat("a", 40)
		case "git.status":
			v = map[string]any{"Clean": clean}
		case "shell.run":
			b, _ := json.Marshal(d.Args)
			if strings.Contains(string(b), "ls-files") {
				v = wf.RunResult{}
			} else if strings.Contains(string(b), "list") {
				v = wf.RunResult{Stdout: "fixture\n"}
			} else if strings.Contains(string(b), "printf") {
				v = wf.RunResult{Stdout: "run\n"}
			} else {
				v = wf.RunResult{Stdout: branch + "\n"}
			}
		default:
			return nil, false
		}
		b, _ := json.Marshal(v)
		return b, true
	})
	return w
}
func run(t *testing.T, w *wftest.World, steps ...wftest.Step) *wftest.Trace {
	t.Helper()
	return wftest.New(feature.Definition(), wftest.WithTB(t), wftest.WithWorld(w), wftest.WithParams(map[string]string{"goal-check-command": "go test -count=1 -json", "goal": goal, "suite": "go test ./..."})).Run(steps...)
}
func report(t *testing.T, tr *wftest.Trace) feature.Summary {
	t.Helper()
	for _, r := range tr.Journal() {
		if r.Kind == wf.JournalAccepted && r.State == "Report" {
			var a wf.AcceptedData
			var s feature.Summary
			if err := json.Unmarshal(r.Data, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(a.Output, &s); err != nil {
				t.Fatal(err)
			}
			return s
		}
	}
	t.Fatalf("missing report\n%s", tr)
	return feature.Summary{}
}
func refused(t *testing.T, tr *wftest.Trace, state string) {
	t.Helper()
	// A refused submission spends its attempt (C5): a refused record marked
	// Submitted.
	for _, r := range tr.Journal() {
		var d wf.RefusedData
		if r.Kind == wf.JournalRefused && r.State == state && json.Unmarshal(r.Data, &d) == nil && d.Submitted {
			return
		}
	}
	t.Fatalf("no refusal at %s\n%s", state, tr)
}

func TestDefinitionLint(t *testing.T) {
	if ps := feature.Definition().Lint(); len(ps) > 0 {
		t.Fatal(ps)
	}
}

func TestApprovedMergeJournalCanRelocate(t *testing.T) {
	workspace := t.TempDir()
	tr := wftest.New(feature.Definition(), wftest.WithTB(t), wftest.WithWorkspace(workspace), wftest.WithWorld(world(t)), wftest.WithParams(map[string]string{"goal-check-command": "go test -count=1 -json", "goal": goal, "suite": "go test ./..."})).Run(append(happy(), wftest.AnswerAt("Approve", "merge"))...)
	if tr.Terminal() != "Done" {
		t.Fatalf("expected approved merge to finish: %s", tr)
	}
	if err := wf.CheckJournalRelocation(tr.Journal(), wf.PathRoots{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
}
func TestHappyPathWaitsForHuman(t *testing.T) {
	w := world(t)
	tr := run(t, w, happy()...)
	s := report(t, tr)
	if tr.Terminal() != "" || s.Verdict != "ready" || len(s.Built) != 1 || len(s.Degraded) != 0 {
		t.Fatalf("%+v\n%s", s, tr)
	}
	for _, e := range wftest.Applied(w) {
		if e.Op == "merge" && e.Args["into"] != nil {
			t.Fatal("merged base without approval")
		}
	}
}
func TestApprovalMergesPinnedCommitOrKeepsBase(t *testing.T) {
	for _, answer := range []string{"merge", "keep"} {
		t.Run(answer, func(t *testing.T) {
			w := world(t)
			tr := run(t, w, append(happy(), wftest.AnswerAt("Approve", answer))...)
			want := "Kept"
			if answer == "merge" {
				want = "Done"
			}
			if tr.Terminal() != want {
				t.Fatalf("%s", tr)
			}
			merged := false
			for _, e := range wftest.Applied(w) {
				if e.Op == "merge" && e.Args["into"] != nil {
					merged = true
					if e.Args["commit"] != strings.Repeat("a", 40) {
						t.Fatalf("mutable approved ref: %+v", e)
					}
				}
			}
			if merged != (answer == "merge") {
				t.Fatal("incorrect base merge")
			}
		})
	}
}

func TestReviewRequiresEveryIDExactlyOnce(t *testing.T) {
	bads := []feature.ReviewResult{{}, rows("wrong", goal, true), rows("g1", goal, true)}
	bads[2].Rows = append(bads[2].Rows, bads[2].Rows[0])
	for i, bad := range bads {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			tr := run(t, world(t), design(), wftest.SubmitAt("DesignReview", bad), designReview(true), implement(), review(true), accept())
			refused(t, tr, "DesignReview")
			if report(t, tr).Verdict != "ready" {
				t.Fatal(tr)
			}
		})
	}
	tr := run(t, world(t), design(), designReview(true), implement(), wftest.SubmitAt("Review", feature.ReviewResult{}), review(true), accept())
	refused(t, tr, "Review")
}
func TestDesignCoverageNamesARealTask(t *testing.T) {
	bad := rows("g1", goal, true)
	bad.Rows[0].Tasks = []string{"missing"}
	tr := run(t, world(t), design(), wftest.SubmitAt("DesignReview", bad), designReview(true), implement(), review(true), accept())
	refused(t, tr, "DesignReview")
}
func TestReviewVerdictComesFromRows(t *testing.T) {
	tr := run(t, world(t), design(), designReview(false), design(), designReview(true), implement(), review(false), implement(), review(true), accept())
	if report(t, tr).Verdict != "ready" {
		t.Fatal("row-based revision failed")
	}
}
func TestDesignCapRetainsEvidence(t *testing.T) {
	var steps []wftest.Step
	for i := 0; i < 3; i++ {
		steps = append(steps, design(), designReview(false))
	}
	steps = append(steps, implement(), review(true), accept())
	s := report(t, run(t, world(t), steps...))
	if s.Verdict != "degraded" || len(s.Degraded) == 0 {
		t.Fatalf("%+v", s)
	}
}
func TestCodeReviewFixesAreBounded(t *testing.T) {
	steps := []wftest.Step{design(), designReview(true)}
	for i := 0; i < 3; i++ {
		steps = append(steps, implement(), review(false))
	}
	w := world(t)
	tr := run(t, w, steps...)
	if tr.Terminal() != "Stopped" || report(t, tr).Verdict != "blocked" {
		t.Fatalf("%s", tr)
	}
	for _, e := range wftest.Applied(w) {
		if e.Op == "merge" {
			t.Fatal("unapproved task merged")
		}
	}
}
func TestGoalCheckDoesNotReadSource(t *testing.T) {
	tr := run(t, world(t), append(happy()[:4], wftest.SubmitAt("GoalCheck", feature.GoalTest{}).WithArtifacts(map[string]string{"goal_test.go": "the engine executes the artifact; no source validator"}))...)
	if report(t, tr).Verdict != "ready" {
		t.Fatal(tr)
	}
}

func TestGoalCheckCannotOverrideEngineCommand(t *testing.T) {
	w := world(t)
	step := wftest.SubmitAt("GoalCheck", map[string]string{"command": "true; echo agent-controlled", "goalCheckCommand": "true", "goal-check-command": "true"}).WithArtifacts(map[string]string{"goal_test.go": blackBox})
	tr := run(t, w, append(happy()[:4], step)...)
	if report(t, tr).Verdict != "ready" {
		t.Fatal(tr)
	}
	found := false
	for _, e := range wftest.Applied(w) {
		if e.Adapter != "shell" || e.Op != "run" {
			continue
		}
		raw, _ := json.Marshal(e.Args["argv"])
		var argv []string
		if err := json.Unmarshal(raw, &argv); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "feature-goal-check") {
			continue
		}
		found = true
		if strings.Contains(string(raw), "agent-controlled") {
			t.Fatalf("agent replaced command: %v", argv)
		}
		if len(argv) < 6 || argv[0] != "sh" || argv[1] != "-c" || argv[len(argv)-2] != "go test -count=1 -json" || !strings.HasSuffix(argv[len(argv)-1], "goal_test.go") {
			t.Fatalf("engine command/artifact missing: %v", argv)
		}
	}
	if !found {
		t.Fatal("no engine goal-check command")
	}
}

func TestGoalCheckZeroTestsCannotReportReady(t *testing.T) {
	w := world(t)
	wftest.Stdout(w, "{\"Action\":\"pass\",\"Package\":\"fixture\"}\n", "sh", "-c")
	tr := run(t, w, append(happy(), fix(), accept(), fix(), accept())...)
	if tr.Terminal() != "Stopped" || report(t, tr).Verdict != "blocked" {
		t.Fatal(tr)
	}
}

func TestFailedAcceptanceEntersFixAndMustBeRechecked(t *testing.T) {
	steps := append(happy()[:4], accept(), fix(), accept())
	w := world(t)
	failGoalCheck(w, true)
	s := report(t, run(t, w, steps...))
	if s.Verdict != "degraded" || !strings.Contains(strings.Join(s.Degraded, " "), "unreviewed integration repair") {
		t.Fatalf("%+v", s)
	}
}
func TestBlackBoxCommandFailureOverridesClaimedPass(t *testing.T) {
	w := world(t)
	failGoalCheck(w, true)
	steps := append(happy(), fix(), accept())
	s := report(t, run(t, w, steps...))
	if s.Verdict != "degraded" {
		t.Fatalf("%+v", s)
	}
}
func TestAcceptanceFailureIsBounded(t *testing.T) {
	w := world(t)
	failGoalCheck(w, false)
	steps := append(happy()[:4], accept(), fix(), accept(), fix(), accept())
	tr := run(t, w, steps...)
	if tr.Terminal() != "Stopped" || report(t, tr).Verdict != "blocked" {
		t.Fatalf("%s", tr)
	}
}
func TestSuiteRepairIsBounded(t *testing.T) {
	w := world(t)
	wftest.Exit(w, 1, "sh", "-c", "go test ./...", "feature-suite")
	tr := run(t, w, append(happy()[:4], fix(), fix())...)
	if tr.Terminal() != "Stopped" || report(t, tr).Verdict != "blocked" {
		t.Fatalf("%s", tr)
	}
}
func TestMergedRedReachesIntegrationRepair(t *testing.T) {
	w := world(t)
	wftest.ExitOnce(w, 1, "sh", "-c", "go test ./...", "feature-suite")
	wftest.ExitOnce(w, 1, "sh", "-c", "go test ./...")
	wftest.ExitOnce(w, 0, "sh", "-c", "go test ./...")
	tr := run(t, w, append(happy()[:4], fix(), accept())...)
	s := report(t, tr)
	if s.Verdict != "degraded" || len(s.Built) != 1 || !strings.Contains(strings.Join(s.Degraded, " "), "integration build") {
		t.Fatalf("%+v", s)
	}
}
func TestChildCheckRedRepairsBeforeReview(t *testing.T) {
	w := world(t)
	wftest.ExitOnce(w, 1, "sh", "-c", "go test ./...")
	tr := run(t, w, design(), designReview(true), implement(), implement(), review(true), accept())
	if report(t, tr).Verdict != "ready" {
		t.Fatal("check repair failed")
	}
}
func TestTaskMergeConflictRepairsAndRechecks(t *testing.T) {
	w := world(t)
	failed := false
	wftest.FailApplyIf(w, func(e wf.ExternalEffect) string {
		if e.Adapter == "git" && e.Op == "merge" && !failed {
			failed = true
			return "merge conflict; aborted"
		}
		return ""
	})
	tr := run(t, w, design(), designReview(true), implement(), review(true), wftest.SubmitAt("ResolveTaskMerge", feature.Patch{Summary: "resolved", Head: strings.Repeat("a", 40)}), review(true), accept())
	if report(t, tr).Verdict != "ready" {
		t.Fatal(tr)
	}
	if !strings.Contains(strings.Join(tr.Route(), " "), "feature-task/Review feature-task/ResolveTaskMerge feature-task/Check feature-task/Review feature-task/Merge") {
		t.Fatal(tr.Route())
	}
}

func TestPersistentTaskMergeConflictNeedsPerson(t *testing.T) {
	w := world(t)
	wftest.FailApply(w, "git", "merge", "merge conflict; aborted")
	steps := happy()[:4]
	for i := 0; i < 2; i++ {
		steps = append(steps, wftest.SubmitAt("ResolveTaskMerge", feature.Patch{Summary: "resolved", Head: strings.Repeat("a", 40)}), review(true))
	}
	steps = append(steps, wftest.AnswerAt("Recovery", "keep"))
	tr := run(t, w, steps...)
	if tr.Terminal() != "Stopped" {
		t.Fatal(tr)
	}
}

func TestBaseMergeConflictRequiresReverificationAndFreshApproval(t *testing.T) {
	w := world(t)
	failed := false
	wftest.FailApplyIf(w, func(e wf.ExternalEffect) string {
		if e.Adapter == "git" && e.Op == "merge" && e.Args["into"] != nil && !failed {
			failed = true
			return "base merge conflict; aborted"
		}
		return ""
	})
	steps := append(happy(), wftest.AnswerAt("Approve", "merge"), wftest.SubmitAt("ResolveBaseMerge", feature.Patch{Summary: "resolved", Head: strings.Repeat("a", 40)}), accept(), wftest.AnswerAt("Approve", "merge"))
	tr := run(t, w, steps...)
	if tr.Terminal() != "Done" {
		t.Fatal(tr)
	}
	if !strings.HasSuffix(strings.Join(tr.Route(), " "), "Approve BaseMerge ResolveBaseMerge Verify Suite Quality Snapshot GoalCheck RunGoalCheck Report Approve BaseMerge Done") {
		t.Fatal(tr.Route())
	}
}

func TestWorktreeFailureNeedsPerson(t *testing.T) {
	for _, task := range []bool{false, true} {
		t.Run(fmt.Sprint(task), func(t *testing.T) {
			w := world(t)
			n := 0
			wftest.FailApplyIf(w, func(e wf.ExternalEffect) string {
				if e.Adapter == "git" && e.Op == "worktree_add" {
					n++
					if (!task && n == 1) || (task && n == 2) {
						return "worktree path exists: remove stale checkout before restarting"
					}
				}
				return ""
			})
			var steps []wftest.Step
			if task {
				steps = append(steps, design(), designReview(true))
			}
			steps = append(steps, wftest.AnswerAt("Recovery", "keep"))
			tr := run(t, w, steps...)
			if tr.Terminal() != "Stopped" {
				t.Fatal(tr)
			}
		})
	}
}
func TestInvalidPlanIsRefusedBeforeTasks(t *testing.T) {
	for _, files := range [][]string{nil, {"../escape"}, {"pkg", "pkg/a.go"}, {"pkg/a.go", "pkg"}} {
		p := plan()
		p.Tasks[0].Files = files
		tr := run(t, world(t), append([]wftest.Step{wftest.SubmitAt("Design", p).WithArtifacts(map[string]string{"spec.md": goal})}, happy()...)...)
		refused(t, tr, "Design")
	}
}
func TestPagedMapJoinsBothTasks(t *testing.T) {
	for _, cap := range []int{1, 2} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			p := plan()
			p.Tasks = append(p.Tasks, feature.Task{ID: "second", Title: "second", Files: []string{"other.go"}, Acceptance: []string{criterion}})
			steps := []wftest.Step{wftest.SubmitAt("Design", p).WithArtifacts(map[string]string{"spec.md": goal}), designReview(true)}
			if cap == 1 {
				for i := 0; i < 2; i++ {
					steps = append(steps, implement(), review(true))
				}
			} else {
				steps = append(steps, implement(), implement(), review(true), review(true))
			}
			steps = append(steps, accept())
			tr := wftest.New(feature.Definition(), wftest.WithTB(t), wftest.WithWorld(world(t)), wftest.Cap(cap), wftest.WithParams(map[string]string{"goal-check-command": "go test -count=1 -json", "goal": goal, "suite": "true"})).Run(steps...)
			if s := report(t, tr); s.Verdict != "ready" || len(s.Built) != 2 {
				t.Fatalf("%+v", s)
			}
		})
	}
}

// Direct engine simulation exposes time and journals without adding a clock step to the DSL.
func TestApprovalTimeoutKeepsBase(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w := world(t)
	a := wf.NewAdapters()
	if err := a.Register(w); err != nil {
		t.Fatal(err)
	}
	d := feature.Definition()
	dir := t.TempDir()
	e := wf.NewEngine(wf.MemJournal(), a, wf.EngineOptions{Clock: func() time.Time { return now }, RunDir: func(wf.RunID) string { return dir }})
	id, err := e.Run(d, map[string]string{"goal-check-command": "go test -count=1 -json", "goal": goal, "suite": "true"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []any{plan(), rows("g1", goal, true), feature.Patch{Summary: goal, Head: strings.Repeat("a", 40)}, rows("c1", criterion, true), feature.GoalTest{}} {
		ins, err := e.Next(id, "")
		if err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionAgent {
			t.Fatalf("%+v %v", ins, err)
		}
		in := ins[0]
		arts := map[string]string{}
		for _, name := range []string{"spec.md", "goal_test.go"} {
			for _, line := range strings.Split(in.Prompt, "\n") {
				if p, ok := strings.CutPrefix(line, "- "+name+": "); ok {
					arts[name] = p
					body := goal
					if name == "goal_test.go" {
						body = blackBox
					}
					if err := os.WriteFile(p, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		raw, _ := json.Marshal(out)
		res, err := e.Submit(id, wf.Submission{Token: in.Token, Attempt: in.Attempt, Child: in.Child, Output: raw, Artifacts: arts})
		if err != nil || res.Class != wf.Accepted {
			t.Fatalf("%+v %v", res, err)
		}
	}
	ins, err := e.Next(id, "")
	if err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate {
		t.Fatalf("%+v %v", ins, err)
	}
	now = now.Add(23 * time.Hour)
	ins, err = e.Next(id, "")
	if err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate {
		t.Fatalf("expired early %+v %v", ins, err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := e.Next(id, ""); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status(id)
	if err != nil || st.Terminal != "Kept" {
		t.Fatalf("%+v %v", st, err)
	}
	for _, eff := range wftest.Applied(w) {
		if eff.Op == "merge" && eff.Args["into"] != nil {
			t.Fatal("timed out gate merged")
		}
	}
}
func TestInvalidSetupStopsWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name, branch, cause, goal, command string
		clean                              bool
	}{{"dirty", "main", "base checkout is dirty", goal, "go test -count=1 -json", false}, {"detached", "", "HEAD is detached", goal, "go test -count=1 -json", true}, {"empty-goal", "main", "goal must contain at least one nonempty requirement", "", "go test -count=1 -json", true}, {"blank-goal", "main", "goal must contain at least one nonempty requirement", " \t\r\n", "go test -count=1 -json", true}, {"missing-command", "main", "goal-check-command is required", goal, "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			w := checkoutWorld(t, tc.clean, tc.branch)
			a := wf.NewAdapters()
			if err := a.Register(w); err != nil {
				t.Fatal(err)
			}
			j := wf.MemJournal()
			dir := t.TempDir()
			d := feature.Definition()
			e := wf.NewEngine(j, a, wf.EngineOptions{RunDir: func(wf.RunID) string { return dir }})
			id, err := e.Run(d, map[string]string{"goal": tc.goal, "suite": "true", "goal-check-command": tc.command}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Next(id, ""); err != nil {
				t.Fatal(err)
			}
			st, err := e.Status(id)
			if err != nil || st.Terminal != "Stopped" {
				t.Fatalf("%+v %v", st, err)
			}
			unlock, err := j.Lock(id)
			if err != nil {
				t.Fatal(err)
			}
			records, err := j.Records(id)
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, r := range records {
				if r.Kind == wf.JournalRefused {
					n++
					var f wf.RefusedData
					_ = json.Unmarshal(r.Data, &f)
					if f.Spent || !strings.Contains(strings.Join(f.Reasons, " "), tc.cause) {
						t.Fatalf("%+v", f)
					}
				}
			}
			if n != 1 || len(wftest.Applied(w)) != 0 {
				t.Fatalf("refusals=%d effects=%v", n, wftest.Applied(w))
			}
		})
	}
}

func TestMultilineGoalRequiresEveryID(t *testing.T) {
	multiline := "Export Add  \r\n\r\n  Return the sum\t\r\n"
	p := plan()
	p.Tasks[0].Acceptance = []string{"Export Add", "Return the sum"}
	full := rows("g1", "Export Add", true)
	full.Rows = append(full.Rows, feature.ReviewRow{ID: "g2", Met: true, Evidence: "TestAPI: calc.go Add signature and result", Tasks: []string{"add"}})
	taskRows := rows("c1", "Export Add", true)
	taskRows.Rows = append(taskRows.Rows, feature.ReviewRow{ID: "c2", Met: true, Evidence: "external TestAddition"})
	tr := wftest.New(feature.Definition(), wftest.WithTB(t), wftest.WithWorld(world(t)), wftest.WithParams(map[string]string{"goal-check-command": "go test -count=1 -json", "goal": multiline, "suite": "true"})).Run(
		wftest.SubmitAt("Design", p).WithArtifacts(map[string]string{"spec.md": goal}),
		wftest.SubmitAt("DesignReview", rows("g1", "Export Add", true)),
		wftest.SubmitAt("DesignReview", full), implement(),
		wftest.SubmitAt("Review", rows("c1", "Export Add", true)),
		wftest.SubmitAt("Review", taskRows),
		accept(),
	)
	for _, state := range []string{"DesignReview", "Review"} {
		refused(t, tr, state)
	}
	if report(t, tr).Verdict != "ready" {
		t.Fatal(tr)
	}
}

func TestAcceptanceRerunDiagnosticsReachFix(t *testing.T) {
	w := world(t)
	failGoalCheck(w, true)
	wftest.Stdout(w, "undefined: calc.Add", "sh", "-c")
	tr := run(t, w, happy()...)
	found := false
	for _, rec := range tr.Journal() {
		if rec.Kind != wf.JournalInstructionIssued || rec.State != "Fix" {
			continue
		}
		var in wf.InstructionIssuedData
		if err := json.Unmarshal(rec.Data, &in); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(in.Prompt, "undefined: calc.Add") || !strings.Contains(in.Prompt, "goal_test.go") {
			t.Fatalf("missing diagnostics/artifact: %s", in.Prompt)
		}
		found = true
	}
	if !found {
		t.Fatalf("no Fix prompt: %s", tr)
	}
}
func TestSetupUnavailableBacksOff(t *testing.T) {
	w := world(t)
	repo := t.TempDir()
	wftest.Unavailable(w, git.Head(repo))
	a := wf.NewAdapters()
	if err := a.Register(w); err != nil {
		t.Fatal(err)
	}
	j := wf.MemJournal()
	dir := t.TempDir()
	e := wf.NewEngine(j, a, wf.EngineOptions{Workspace: repo, RunDir: func(wf.RunID) string { return dir }})
	id, err := e.Run(feature.Definition(), map[string]string{"goal-check-command": "go test -count=1 -json", "goal": goal, "suite": "true"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(id, ""); err != nil {
		t.Fatal(err)
	}
	unlock, err := j.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	records, err := j.Records(id)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range records {
		if rec.State == "Setup" && rec.Kind == wf.JournalBackoffScheduled {
			found = true
			if rec.Class != wf.Unavailable {
				t.Fatalf("observation failure class=%s", rec.Class)
			}
		}
	}
	if !found {
		t.Fatal("no unavailable backoff scheduled")
	}
}
func TestIntegrationRedQualifierAppearsOnce(t *testing.T) {
	p := plan()
	p.Tasks = append(p.Tasks, feature.Task{ID: "second", Title: "second", Files: []string{"other.go"}, Acceptance: []string{criterion}})
	w := world(t)
	for _, code := range []int{1, 0, 1, 0} {
		wftest.ExitOnce(w, code, "sh", "-c", "true")
	}
	wftest.Exit(w, 0, "sh", "-c", "true", "feature-suite")
	tr := wftest.New(feature.Definition(), wftest.WithTB(t), wftest.WithWorld(w), wftest.Cap(1), wftest.WithParams(map[string]string{"goal-check-command": "go test -count=1 -json", "goal": goal, "suite": "true"})).Run(
		wftest.SubmitAt("Design", p).WithArtifacts(map[string]string{"spec.md": goal}),
		designReview(true), implement(), review(true), implement(), review(true), accept())
	s := report(t, tr)
	if len(s.Built) != 2 || len(s.Degraded) != 1 || !strings.Contains(s.Degraded[0], "integration build red") {
		t.Fatalf("%+v", s)
	}
}

func TestReviewInventoriesPrecedeSchemas(t *testing.T) {
	tr := run(t, world(t), happy()...)
	seen := map[string]bool{}
	for _, rec := range tr.Journal() {
		if rec.Kind != wf.JournalInstructionIssued {
			continue
		}
		if rec.State != "DesignReview" && rec.State != "Review" && rec.State != "GoalCheck" {
			continue
		}
		var in wf.InstructionIssuedData
		if err := json.Unmarshal(rec.Data, &in); err != nil {
			t.Fatal(err)
		}
		rules, body, ok := strings.Cut(in.Prompt, "\n\n")
		if !ok || !strings.HasPrefix(rules, "Rules for this run. ") {
			t.Fatalf("%s prompt does not open with the rules: %s", rec.State, in.Prompt)
		}
		if !strings.HasPrefix(body, "| ID | Requirement |\n") {
			t.Fatalf("%s inventory is not first after the rules: %s", rec.State, in.Prompt)
		}
		id, text := "g1", goal
		if rec.State == "Review" {
			id, text = "c1", criterion
		}
		if !strings.Contains(in.Prompt, "| "+id+" | "+text+" |") {
			t.Fatalf("%s missing inventory: %s", rec.State, in.Prompt)
		}
		seen[rec.State] = true
	}
	if len(seen) != 3 {
		t.Fatalf("review prompts seen: %v", seen)
	}
}
func TestUnknownReviewIDExplainsInventory(t *testing.T) {
	tr := run(t, world(t), design(), wftest.SubmitAt("DesignReview", rows("wrong", "paraphrased", true)), designReview(true))
	for _, rec := range tr.Journal() {
		if rec.State != "DesignReview" || rec.Kind != wf.JournalRefused {
			continue
		}
		text := string(rec.Data)
		for _, want := range []string{"missing", "g1", "unknown", "wrong", goal} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q: %s", want, text)
			}
		}
		return
	}
	t.Fatal("no ID refusal")
}

func TestGoalTestBuildFailureRetriesAuthorBeforeBoundedRepair(t *testing.T) {
	w := world(t)
	failGoalCheck(w, false)
	wftest.Stdout(w, "{\"Action\":\"output\",\"Output\":\"undefined: GCD\\n\"}\n{\"Action\":\"fail\",\"Package\":\"command-line-arguments\",\"FailedBuild\":\"command-line-arguments\"}\n", "sh", "-c")
	tr := run(t, w, append(happy(), accept(), fix(), accept(), accept(), fix(), accept(), accept())...)
	if tr.Terminal() != "Stopped" {
		t.Fatal(tr)
	}
	issued, fixes := 0, 0
	for _, rec := range tr.Journal() {
		if rec.Kind != wf.JournalInstructionIssued {
			continue
		}
		if rec.State == "Fix" {
			fixes++
			if issued%2 != 0 {
				t.Fatal("product repair before author retry")
			}
			if !strings.Contains(string(rec.Data), "public API may be missing or unexported") {
				t.Fatal("missing API diagnostic")
			}
		}
		if rec.State != "GoalCheck" {
			continue
		}
		issued++
		if issued > 1 && !strings.Contains(string(rec.Data), "undefined: GCD") {
			t.Fatalf("compiler output missing: %s", rec.Data)
		}
	}
	if issued != 6 || fixes != 2 {
		t.Fatalf("goal-test attempts=%d fixes=%d", issued, fixes)
	}
}

func TestFixRefusesAllocatedGoalTestInCommit(t *testing.T) {
	for _, path := range []string{"goal_test.go", "checks/goal_test.go"} {
		t.Run(path, func(t *testing.T) {
			w := world(t)
			failGoalCheck(w, true)
			wftest.WrapRead(w, func(next func(wf.ObservationDesc) (json.RawMessage, bool)) func(wf.ObservationDesc) (json.RawMessage, bool) {
				return func(d wf.ObservationDesc) (json.RawMessage, bool) {
					raw, _ := json.Marshal(d.Args)
					if d.Adapter == "shell" && strings.Contains(string(raw), "--name-only") {
						out := path + "\x00"
						b, _ := json.Marshal(wf.RunResult{Stdout: out})
						return b, true
					}
					return next(d)
				}
			})
			tr := run(t, w, append(happy(), fix())...)
			refused(t, tr, "Fix")
			for _, rec := range tr.Journal() {
				if rec.State == "Fix" && rec.Kind == wf.JournalAccepted {
					t.Fatal("contaminated repair was accepted")
				}
			}
		})
	}

}

func TestBaseConflictCannotReuseEarlierAcceptance(t *testing.T) {
	w := world(t)
	wftest.FailApplyIf(w, func(e wf.ExternalEffect) string {
		if e.Adapter == "git" && e.Op == "merge" && e.Args["into"] != nil {
			return "base conflict"
		}
		return ""
	})
	steps := append(happy(), wftest.AnswerAt("Approve", "merge"), wftest.SubmitAt("ResolveBaseMerge", feature.Patch{Summary: "resolved", Head: strings.Repeat("a", 40)}))
	// Verification of the repaired tree fails at GoalCheck until its retry budget
	// is spent. The earlier acceptance must not make this tree ready.
	for i := 0; i < 2; i++ {
		steps = append(steps, wftest.SubmitAt("GoalCheck", "invalid schema"))
	}
	tr := run(t, w, steps...)
	if tr.Terminal() != "Stopped" {
		t.Fatalf("unverified repair reused earlier acceptance: %s", tr)
	}
}

func TestUnavailableSetupEventuallyNeedsPerson(t *testing.T) {
	w := world(t)
	repo := t.TempDir()
	wftest.Unavailable(w, git.Head(repo))
	a := wf.NewAdapters()
	if err := a.Register(w); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	e := wf.NewEngine(wf.MemJournal(), a, wf.EngineOptions{Workspace: repo, Clock: func() time.Time { return now }, RunDir: func(wf.RunID) string { return dir }})
	id, err := e.Run(feature.Definition(), map[string]string{"goal": goal, "suite": "true", "goal-check-command": "go test -count=1 -json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		ins, err := e.Next(id, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range ins {
			if in.Kind == wf.InstructionHumanGate && in.State == "Recovery" {
				return
			}
		}
		now = now.Add(time.Hour)
	}
	t.Fatal("unavailable setup stopped without person recovery")
}
