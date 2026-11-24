package feature_test

import (
	"encoding/json"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	gitadapter "github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Real adapters catch bindings, artifacts and merges the fake World cannot.
func TestRealGitWorkflow(t *testing.T) {
	for _, mode := range []string{"happy", "task-merge", "base-merge", "interaction", "unexported", "format", "vet", "accept-dirty", "accept-commit", "test-dirty", "accept-ignored", "suite-ignored", "accept-ignored-removed", "accept-ignored-content", "test-ignored-content", "suite-ignored-go", "methods", "empty-test", "internal-retry", "assertion", "fix-copy-goal", "fix-copy-nested-goal", "gcd-unexported", "fix-type-goal"} {
		t.Run(mode, func(t *testing.T) { realGitWorkflow(t, mode) })
	}
}
func realGitWorkflow(t *testing.T, mode string) {
	interaction := mode == "interaction"
	mergeRecovery := mode == "task-merge" || mode == "base-merge"
	runGoal := goal
	if mode == "gcd-unexported" {
		runGoal = "Expose GCD(a, b int) int returning the greatest common divisor"
	}
	if mode == "methods" {
		runGoal = "Support zero-value Stack methods"
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo, records := t.TempDir(), t.TempDir()
	gitRun(t, repo, "init", "-b", "main")
	gitRun(t, repo, "config", "commit.gpgsign", "false")
	gitRun(t, repo, "config", "core.hooksPath", os.DevNull)
	gitRun(t, repo, "config", "user.name", "Test")
	gitRun(t, repo, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module fixture\n\ngo 1.23\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.go\ncoverage.out\n-\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "README", "go.mod", ".gitignore")
	if mode == "fix-type-goal" {
		if err := os.WriteFile(filepath.Join(repo, "goal_test.go"), []byte("package calc\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitRun(t, repo, "add", "goal_test.go")
	}
	gitRun(t, repo, "commit", "-m", "fixture")
	original := gitRun(t, repo, "rev-parse", "HEAD")
	adapters := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{gitadapter.Runtime(gitadapter.Config{}), shell.Runtime(shell.Config{})} {
		if err := adapters.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	d := feature.Definition()
	journal := wf.MemJournal()
	e := wf.NewEngine(journal, adapters, wf.EngineOptions{Workspace: repo, RunDir: func(id wf.RunID) string { return filepath.Join(records, string(id)) }})
	suite := "test -f calc.go"
	if mode == "suite-ignored" || mode == "accept-ignored-removed" || mode == "accept-ignored-content" || mode == "test-ignored-content" {
		suite = "printf sentinel > ./-; printf coverage > coverage.out; test -f calc.go"
	}
	if mode == "suite-ignored-go" {
		suite = "printf 'package calc\\n' > ignored.go; test -f calc.go"
	}
	suiteLog := filepath.Join(records, "suite-heads")
	if mergeRecovery {
		suite = "git rev-parse HEAD >> " + strconv.Quote(suiteLog) + "; test -f calc.go"
	}
	plan := feature.Plan{Tasks: []feature.Task{{ID: "add", Title: "Add addition", Files: []string{"calc.go"}, Acceptance: []string{"2+3 is 5"}}}}
	if interaction {
		// Both task checkouts pass. The combined tree deterministically fails
		// until the parent repair adds its marker; no fake suite divergence.
		suite = "test ! -f calc.go || test ! -f sub.go || test -f repaired"
		plan.Tasks = append(plan.Tasks, feature.Task{ID: "sub", Title: "subtraction", Files: []string{"sub.go"}, Acceptance: []string{"3-2 is 1"}})
	}
	params := map[string]string{"goal-check-command": "go test -count=1 -json", "goal": runGoal, "suite": suite}
	if mode == "methods" {
		params["goal-check-command"] = "go test -json -count=1 -timeout=30s"
	}
	id, err := e.Run(d, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	gate := false
	repairs := 0
	goalChecks := 0
	goalArtifact := ""
	resolvedHead, taskHead := "", ""
	approvals, taskReviews := 0, 0
	for i := 0; i < 15; i++ {
		instructions, err := e.Next(id, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(instructions) == 0 {
			break
		}
		for _, in := range instructions {
			if in.Kind == wf.InstructionEnded {
				continue
			}
			if in.Kind == wf.InstructionHumanGate {
				gate = true
				if mergeRecovery && in.State != "Approve" {
					t.Fatalf("merge recovery unexpectedly needs a person: %+v", in)
				}
				approvals++
				if mode == "base-merge" && approvals == 1 {
					// A concurrent base change invalidates the previously verified merge.
					writeRecoveryFile(t, repo, "README", "base change\n")
					gitRun(t, repo, "add", "README")
					gitRun(t, repo, "commit", "-m", "concurrent base change")
					original = gitRun(t, repo, "rev-parse", "HEAD")
				}
				if mode == "base-merge" && approvals == 2 {
					if goalChecks != 2 || resolvedHead == "" {
						t.Fatalf("fresh approval without reverified resolution: checks=%d head=%s", goalChecks, resolvedHead)
					}
					assertRecoverySuiteHead(t, suiteLog, resolvedHead)
				}
				if gitRun(t, repo, "rev-parse", "HEAD") != original {
					t.Fatal("base changed before approval")
				}
				if err := e.AnswerVisit(id, wf.GateAnswer{Gate: in.State, Child: in.Child, Visit: in.Visit, Value: "merge"}); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if in.Kind != wf.InstructionAgent {
				t.Fatalf("unexpected instruction: %+v", in)
			}
			var output any
			artifacts := map[string]string{}
			switch {
			case in.State == "Design":
				output = plan
				for _, line := range strings.Split(in.Prompt, "\n") {
					if strings.HasPrefix(line, "- spec.md: ") {
						artifacts["spec.md"] = strings.TrimPrefix(line, "- spec.md: ")
					}
				}
				if artifacts["spec.md"] == "" {
					t.Fatalf("no allocated spec: %s", in.Prompt)
				}
				if err := os.MkdirAll(filepath.Dir(artifacts["spec.md"]), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(artifacts["spec.md"], []byte("PRIVATE-DESIGN-CONTEXT: Add addition"), 0600); err != nil {
					t.Fatal(err)
				}
			case in.State == "DesignReview":
				output = rows("g1", goal, true)
			case strings.HasSuffix(in.State, "/Implement"):
				if in.Workspace == repo {
					t.Fatal("implementer was placed in original checkout")
				}
				file, body := "calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n"
				if mode == "assertion" || mode == "fix-copy-goal" || mode == "fix-copy-nested-goal" || mode == "fix-type-goal" {
					body = strings.Replace(body, "return a + b", "return a - b", 1)
				}
				if mode == "gcd-unexported" {
					body += "func gcd(a, b int) int { for b != 0 { a, b = b, a % b }; return a }\n"
					formatted, err := format.Source([]byte(body))
					if err != nil {
						t.Fatal(err)
					}
					body = string(formatted)
				}
				if mode == "unexported" {
					body = strings.Replace(body, "func Add(", "func add(", 1)
				}
				if mode == "format" {
					body = "package calc\nfunc Add(a,b int)int{return a+b}\n"
				}
				if mode == "vet" {
					body = "package calc\n\nimport \"fmt\"\n\nfunc Add(a, b int) int { return a + b }\nfunc unused() { fmt.Printf(\"%d\", \"bad\") }\n"
				}
				if strings.Contains(in.Child, "[sub]") {
					file, body = "sub.go", "package calc\n\nfunc Sub(a, b int) int { return a - b }\n"
				}
				if mode == "methods" {
					body += `type Stack struct { values []int }
func(s *Stack)Len()int{return len(s.values)}
func(s *Stack)Push(v int){s.values=append(s.values,v)}
func(s *Stack)Pop()(int,bool){if len(s.values)==0{return 0,false};v:=s.values[len(s.values)-1];s.values=s.values[:len(s.values)-1];return v,true}
`
					formatted, err := format.Source([]byte(body))
					if err != nil {
						t.Fatal(err)
					}
					body = string(formatted)
				}
				if err := os.WriteFile(filepath.Join(in.Workspace, file), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				if mergeRecovery {
					writeRecoveryFile(t, in.Workspace, "README", "task change\n")
					gitRun(t, in.Workspace, "add", "README")
				}
				gitRun(t, in.Workspace, "add", file)
				gitRun(t, in.Workspace, "commit", "-m", "add addition")
				taskHead = gitRun(t, in.Workspace, "rev-parse", "HEAD")
				output = feature.Patch{Summary: "Added addition", Head: taskHead}
			case strings.HasSuffix(in.State, "/ResolveTaskMerge") || in.State == "ResolveBaseMerge":
				if gitRun(t, repo, "rev-parse", "HEAD") != original {
					t.Fatal("failed merge modified the original base")
				}
				other := "main"
				if mode == "task-merge" {
					integration := filepath.Join(filepath.Dir(in.Workspace), "integration")
					other = gitRun(t, integration, "rev-parse", "HEAD")
				}
				cmd := exec.Command("git", "-C", in.Workspace, "merge", "--no-edit", other)
				out, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(out), "CONFLICT") {
					t.Fatalf("expected real conflict: %s %v", out, err)
				}
				writeRecoveryFile(t, in.Workspace, "README", "task change and concurrent change\n")
				gitRun(t, in.Workspace, "add", "README")
				gitRun(t, in.Workspace, "commit", "-m", "resolve conflict")
				resolvedHead = gitRun(t, in.Workspace, "rev-parse", "HEAD")
				if resolvedHead == taskHead {
					t.Fatal("resolution did not create a new head")
				}
				output = feature.Patch{Summary: "resolved conflict", Head: resolvedHead}
			case in.State == "GoalCheck":
				goalChecks++
				if mode == "internal-retry" && goalChecks == 2 && !strings.Contains(in.Prompt, "undefined: Add") {
					t.Fatalf("authoring retry lost compiler diagnostics: %s", in.Prompt)
				}
				if strings.Contains(in.Prompt, "PRIVATE-DESIGN-CONTEXT") {
					t.Fatal("acceptance inherited designer narrative")
				}
				output = rows("g1", goal, true)
				for _, line := range strings.Split(in.Prompt, "\n") {
					if path, ok := strings.CutPrefix(line, "- goal_test.go: "); ok {
						artifacts["goal_test.go"] = path
					}
				}
				goalArtifact = artifacts["goal_test.go"]
				rel, err := filepath.Rel(in.Workspace, goalArtifact)
				if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
					t.Fatalf("acceptance artifact is inside product checkout: %q (%v)", goalArtifact, err)
				}
				body := "package acceptance_test\nimport (\n \"testing\"\n calc \"fixture\"\n)\nfunc TestAPI(t *testing.T) {if calc.Add(2,3)!=5 {t.Fatal(\"wrong sum\")}}\n"
				if mode == "accept-dirty" || mode == "accept-commit" || mode == "accept-ignored" {
					if err := os.WriteFile(filepath.Join(in.Workspace, map[bool]string{true: "ignored.go", false: "calc.go"}[mode == "accept-ignored"]), []byte("package calc\n\nfunc Add(a, b int) int { return a - b }\n"), 0600); err != nil {
						t.Fatal(err)
					}
					if mode == "accept-commit" {
						gitRun(t, in.Workspace, "add", "calc.go")
						gitRun(t, in.Workspace, "commit", "-m", "unverified acceptance edit")
					}
				}
				if mode == "accept-ignored-removed" {
					if err := os.Remove(filepath.Join(in.Workspace, "coverage.out")); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "accept-ignored-content" {
					if err := os.WriteFile(filepath.Join(in.Workspace, "coverage.out"), []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "suite-ignored-go" {
					if err := os.WriteFile(filepath.Join(in.Workspace, "ignored.go"), []byte("package calc\nfunc Mul(a,b int) int {return a*b}\n"), 0600); err != nil {
						t.Fatal(err)
					}
					body = strings.ReplaceAll(body, "calc.Add(2,3)!=5", "calc.Mul(2,3)!=6")
				}
				if mode == "test-dirty" || mode == "test-ignored-content" {
					body = "package acceptance_test\nimport (\"os\";\"testing\";calc \"fixture\")\nfunc TestAPI(t *testing.T) {if calc.Add(2,3)!=5 {t.Fatal(\"sum\")};if err:=os.WriteFile(" + strconv.Quote(filepath.Join(in.Workspace, "calc.go")) + ",[]byte(\"package calc\\nfunc Add(a,b int) int {return a-b}\\n\"),0600);err!=nil {t.Fatal(err)}}\n"
				}
				if mode == "test-ignored-content" {
					body = strings.ReplaceAll(body, strconv.Quote(filepath.Join(in.Workspace, "calc.go")), strconv.Quote(filepath.Join(in.Workspace, "coverage.out")))
				}
				if mode == "methods" {
					body = zeroValueStackCheck
				}
				if mode == "gcd-unexported" {
					body = strings.ReplaceAll(body, "calc.Add(2,3)!=5", "calc.GCD(54,24)!=6")
				}
				if mode == "internal-retry" && goalChecks == 1 {
					body = "package calc\nimport \"testing\"\nfunc TestAPI(t *testing.T) {if Add(2,3)!=5 {t.Fatal(\"wrong sum\")}}\n"
				}
				if mode == "empty-test" && repairs == 0 {
					body = "package acceptance_test\n"
				}
				output = feature.GoalTest{}
				if err := os.WriteFile(artifacts["goal_test.go"], []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			case in.State == "Fix":
				repairs++
				if mode == "unexported" || mode == "gcd-unexported" {
					api := "Add"
					if mode == "gcd-unexported" {
						api = "GCD"
					}
					if goalChecks != 2 || !strings.Contains(in.Prompt, "undefined: calc."+api) {
						t.Fatalf("persistent build failure lost compiler evidence after %d GoalChecks: %s", goalChecks, in.Prompt)
					}
				}
				if mode == "happy" || mode == "internal-retry" {
					t.Fatalf("unexpected product repair for %s", mode)
				}
				if err := os.WriteFile(filepath.Join(in.Workspace, "repaired"), []byte("fixed\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if !interaction {
					body := "package calc\n\nfunc Add(a, b int) int { return a + b }\n"
					if mode == "gcd-unexported" {
						body += "func GCD(a, b int) int { for b != 0 { a, b = b, a % b }; return a }\n"
						formatted, err := format.Source([]byte(body))
						if err != nil {
							t.Fatal(err)
						}
						body = string(formatted)
					}
					if err := os.WriteFile(filepath.Join(in.Workspace, "calc.go"), []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				gitRun(t, in.Workspace, "add", "repaired", "calc.go")
				if mode == "fix-copy-goal" || mode == "fix-copy-nested-goal" {
					copied := "goal_test.go"
					if mode == "fix-copy-nested-goal" {
						copied = filepath.Join("copied", "goal_test.go")
						if err := os.MkdirAll(filepath.Join(in.Workspace, "copied"), 0700); err != nil {
							t.Fatal(err)
						}
					}
					body, err := os.ReadFile(goalArtifact)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(in.Workspace, copied), body, 0600); err != nil {
						t.Fatal(err)
					}
					gitRun(t, in.Workspace, "add", copied)
				}
				if mode == "fix-type-goal" {
					path := filepath.Join(in.Workspace, "goal_test.go")
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("calc.go", path); err != nil {
						t.Fatal(err)
					}
					gitRun(t, in.Workspace, "add", "goal_test.go")
				}
				gitRun(t, in.Workspace, "commit", "-m", "repair interaction")
				output = feature.Patch{Summary: "repaired integration", Head: gitRun(t, in.Workspace, "rev-parse", "HEAD")}
			case strings.HasSuffix(in.State, "/Review"):
				taskReviews++
				if mode == "task-merge" && taskReviews == 1 {
					integration := filepath.Join(filepath.Dir(in.Workspace), "integration")
					writeRecoveryFile(t, integration, "README", "integration change\n")
					gitRun(t, integration, "add", "README")
					gitRun(t, integration, "commit", "-m", "concurrent integration change")
				}
				if mode == "task-merge" && taskReviews == 2 {
					if gitRun(t, in.Workspace, "rev-parse", "HEAD") != resolvedHead || !strings.Contains(in.Prompt, resolvedHead) {
						t.Fatal("review did not receive the resolved commit")
					}
					assertRecoverySuiteHead(t, suiteLog, resolvedHead)
				}
				output = rows("c1", criterion, true)
				if strings.Contains(in.Child, "[sub]") {
					output = rows("c1", "3-2 is 1", true)
				}
			default:
				t.Fatalf("unexpected agent %s", in.State)
			}
			raw, _ := json.Marshal(output)
			result, err := e.Submit(id, wf.Submission{Token: in.Token, Attempt: in.Attempt, Child: in.Child, Output: raw, Artifacts: artifacts})
			if in.State == "Fix" && (mode == "fix-copy-goal" || mode == "fix-copy-nested-goal" || mode == "fix-type-goal") {
				if err != nil || result.Class != wf.Refused {
					t.Fatalf("repair committed acceptance test: %+v %v", result, err)
				}
				if gate || gitRun(t, repo, "rev-parse", "HEAD") != original {
					t.Fatal("repair with copied acceptance test reached approval")
				}
				return
			}
			if in.State == "GoalCheck" && (mode == "accept-dirty" || mode == "accept-commit" || mode == "accept-ignored" || mode == "accept-ignored-removed" || mode == "accept-ignored-content" || mode == "suite-ignored-go") {
				if err != nil || result.Class != wf.Refused {
					t.Fatalf("changed acceptance checkout accepted: %+v %v", result, err)
				}
				if gate || gitRun(t, repo, "rev-parse", "HEAD") != original {
					t.Fatal("changed checkout reached approval")
				}
				return
			}
			if err != nil || result.Class != wf.Accepted {
				t.Fatalf("submit %s: %+v %v", in.State, result, err)
			}
		}
	}
	st, err := e.Status(id)
	if (mode == "unexported" || mode == "gcd-unexported") && (goalChecks != 3 || repairs != 1) {
		t.Fatalf("persistent build failure: goalChecks=%d repairs=%d", goalChecks, repairs)
	}
	if mode == "internal-retry" && (goalChecks != 2 || repairs != 0) {
		t.Fatalf("authoring retry: goalChecks=%d repairs=%d", goalChecks, repairs)
	}
	if mode == "test-dirty" || mode == "test-ignored-content" || mode == "suite-ignored-go" {
		if err != nil || st.Terminal != "Stopped" || gate {
			t.Fatalf("mutating test reached readiness: %+v %v", st, err)
		}
		if gitRun(t, repo, "rev-parse", "HEAD") != original {
			t.Fatal("mutating acceptance changed base")
		}
		return
	}
	if err != nil || st.Terminal != "Done" || !gate {
		t.Fatalf("status=%+v gate=%v err=%v", st, gate, err)
	}
	if mergeRecovery {
		wantReviews, wantApprovals := 2, 1
		if mode == "base-merge" {
			wantReviews, wantApprovals = 1, 2
		}
		if taskReviews != wantReviews || approvals != wantApprovals || resolvedHead == "" {
			t.Fatalf("recovery counts: reviews=%d approvals=%d resolved=%s", taskReviews, approvals, resolvedHead)
		}
		gitRun(t, repo, "merge-base", "--is-ancestor", resolvedHead, "HEAD")
		if got := gitRun(t, repo, "show", "HEAD:README"); got != "task change and concurrent change" {
			t.Fatalf("merged stale content: %s", got)
		}
	}
	if mode == "internal-retry" {
		if summary := realGitSummary(t, journal, id); summary.Verdict != "ready" {
			t.Fatalf("corrected authoring report: %+v", summary)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "calc.go")); err != nil {
		t.Fatal("approved task missing from base:", err)
	}
	if !mergeRecovery && mode != "happy" && mode != "suite-ignored" && mode != "methods" && mode != "internal-retry" {
		if repairs != 1 {
			t.Fatalf("repair calls=%d", repairs)
		}
		unlock, err := journal.Lock(id)
		if err != nil {
			t.Fatal(err)
		}
		records, err := journal.Records(id)
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, rec := range records {
			if rec.State == "Report" && rec.Kind == wf.JournalAccepted {
				var a wf.AcceptedData
				var summary feature.Summary
				_ = json.Unmarshal(rec.Data, &a)
				_ = json.Unmarshal(a.Output, &summary)
				if summary.Verdict != "degraded" || len(summary.Built) != len(plan.Tasks) || !strings.Contains(strings.Join(summary.Degraded, " "), "unreviewed integration repair") {
					t.Fatalf("%+v", summary)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("missing integration report")
		}
	}

	second, err := e.Run(d, map[string]string{"goal-check-command": "go test -count=1 -json", "goal": runGoal, "suite": "test -f calc.go"}, nil)
	if err != nil || second == id {
		t.Fatalf("successor=%s error=%v", second, err)
	}
	ins, err := e.Next(second, "")
	if err != nil || len(ins) != 1 || ins[0].State != "Design" {
		t.Fatalf("successor did not reach Design: %+v err=%v", ins, err)
	}

}

func realGitSummary(t *testing.T, journal wf.Journal, id wf.RunID) feature.Summary {
	t.Helper()
	unlock, err := journal.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	records, err := journal.Records(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.State == "Report" && rec.Kind == wf.JournalAccepted {
			var accepted wf.AcceptedData
			var summary feature.Summary
			if err := json.Unmarshal(rec.Data, &accepted); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(accepted.Output, &summary); err != nil {
				t.Fatal(err)
			}
			return summary
		}
	}
	t.Fatal("missing integration report")
	return feature.Summary{}
}

// Adapted from rung4b run 3, which passed Go tests but was refused by the old
// source-inspection rule because new(tc.Stack) and s.Len are not pkg.F calls.
const zeroValueStackCheck = `package fixture_test
import ("testing"; tc "fixture")
func Test_g2_ZeroValueStackIsUsable(t *testing.T) {
 s:=new(tc.Stack)
 if n:=s.Len(); n!=0 {t.Errorf("new(tc.Stack).Len() = %d, want 0",n)}
 s.Push(42)
 if n:=s.Len(); n!=1 {t.Errorf("Len after Push = %d, want 1",n)}
 if v,ok:=s.Pop();v!=42||!ok {t.Errorf("Pop = (%d,%t), want (42,true)",v,ok)}
 var zero tc.Stack
 if zero.Len()!=0 {t.Fatal("zero-value Stack not empty")}
 literal:=tc.Stack{}
 literal.Push(7)
 if v,ok:=literal.Pop();v!=7||!ok {t.Fatal("composite Stack not usable")}
}
`

func writeRecoveryFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertRecoverySuiteHead(t *testing.T, path, head string) {
	t.Helper()
	log, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if head == "" || !strings.Contains(string(log), head+"\n") {
		t.Fatalf("resolved head %s was not tested: %s", head, log)
	}
}
