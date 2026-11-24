package feature

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// ReviewResult has no agent-selected verdict. The engine derives it from every row.
type ReviewResult struct {
	Rows []ReviewRow `json:"rows"`
}
type ReviewRow struct {
	ID       string   `json:"id"`
	Met      bool     `json:"met"`
	Evidence string   `json:"evidence"`
	Tasks    []string `json:"tasks,omitempty"`
}
type requirement struct{ id, text string }

// Registration enables @file goals while journaling the original goal text.
func Registration() wf.Registration {
	return wf.Registration{Def: Definition(), Params: []wf.ParamSpec{
		{Name: "goal", Required: true, Value: "<text|@file>", File: true},
		{Name: "suite", Required: true, Value: "<command>"},
		{Name: "goal-check-command", Value: defaultGoalCheckCommand},
	}}
}
func requirements(goal string) []requirement {
	var out []requirement
	for _, line := range strings.Split(goal, "\n") {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if strings.TrimSpace(line) != "" {
			out = append(out, requirement{fmt.Sprintf("g%d", len(out)+1), line})
		}
	}
	return out
}
func criteria(items []string) []requirement {
	out := make([]requirement, len(items))
	for i, item := range items {
		out[i] = requirement{fmt.Sprintf("c%d", i+1), item}
	}
	return out
}

// requirementTable keeps the engine-assigned inventory visible to reviewers.
func requirementTable(items []requirement) string {
	escape := strings.NewReplacer("\r\n", "<br>", "\r", "<br>", "\n", "<br>", "|", "\\|")
	var table strings.Builder
	table.WriteString("| ID | Requirement |\n| --- | --- |\n")
	for _, item := range items {
		fmt.Fprintf(&table, "| %s | %s |\n", escape.Replace(item.id), escape.Replace(item.text))
	}
	return table.String()
}

func checkRows(r ReviewResult, expected []requirement, tasks []Task) error {
	if len(expected) == 0 {
		return errors.New("empty requirement inventory: provide at least one nonempty requirement")
	}
	want := map[string]bool{}
	for _, item := range expected {
		want[item.id] = true
	}
	seen := map[string]bool{}
	var missing, unknown, duplicate []string
	for _, row := range r.Rows {
		if seen[row.ID] {
			duplicate = append(duplicate, row.ID)
		}
		if !want[row.ID] {
			unknown = append(unknown, row.ID)
		}
		seen[row.ID] = true
	}
	for _, item := range expected {
		if !seen[item.id] {
			missing = append(missing, item.id)
		}
	}
	if len(missing)+len(unknown)+len(duplicate) > 0 {
		return fmt.Errorf("review row IDs: missing %q; unknown %q; duplicate %q; expected IDs:\n%s", missing, unknown, duplicate, requirementTable(expected))
	}
	if tasks != nil {
		validTasks := map[string]bool{}
		for _, task := range tasks {
			validTasks[task.ID] = true
		}
		for _, row := range r.Rows {
			if row.Met && len(row.Tasks) == 0 {
				return fmt.Errorf("row %q: met design row requires at least one covering task ID", row.ID)
			}
			for _, id := range row.Tasks {
				if !validTasks[id] {
					return fmt.Errorf("row %q: unknown covering task %q", row.ID, id)
				}
			}
		}
	}
	return nil
}
func verdict(r ReviewResult, fail Verdict) Verdict {
	for _, row := range r.Rows {
		if !row.Met {
			return fail
		}
	}
	return OK
}
func unmet(r ReviewResult) []string {
	var reasons []string
	for _, row := range r.Rows {
		if !row.Met {
			reasons = append(reasons, row.ID+": "+row.Evidence)
		}
	}
	return reasons
}

var taskID = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func checkPlan(p Plan, _ wf.Ctx) error {
	if len(p.Tasks) == 0 {
		return errors.New("tasks are required")
	}
	ids, files := map[string]bool{}, map[string]bool{}
	for _, t := range p.Tasks {
		if !taskID.MatchString(t.ID) || ids[t.ID] || strings.TrimSpace(t.Title) == "" || len(t.Acceptance) == 0 || len(t.Files) == 0 {
			return fmt.Errorf("invalid or duplicate task %q", t.ID)
		}
		for _, criterion := range t.Acceptance {
			if strings.TrimSpace(criterion) == "" {
				return fmt.Errorf("empty criterion in %q", t.ID)
			}
		}
		ids[t.ID] = true
		for _, f := range t.Files {
			if !filepath.IsLocal(f) {
				return fmt.Errorf("tasks must own disjoint local files: %q", f)
			}
			clean := filepath.Clean(f)
			for other := range files {
				if clean == other || clean == "." || other == "." || strings.HasPrefix(clean, other+string(filepath.Separator)) || strings.HasPrefix(other, clean+string(filepath.Separator)) {
					return fmt.Errorf("task paths overlap: %q and %q", f, other)
				}
			}
			files[clean] = true
		}
	}
	return nil
}
func headCheck(dir string) wf.Check[Patch] {
	head, status := git.Head(dir), git.Status(dir)
	return wf.Checkf("committed clean HEAD", func(p Patch, c wf.Ctx) error {
		h, err := wf.Observed(c, head)
		if err != nil {
			return err
		}
		s, err := wf.Observed(c, status)
		if err != nil {
			return err
		}
		if p.Head != h || len(p.Head) != 40 || !s.Clean || strings.TrimSpace(p.Summary) == "" {
			return errors.New("return the clean committed HEAD and a summary")
		}
		return nil
	}).Observes(head, status).
		Accepts(Patch{Summary: "Add addition", Head: strings.Repeat("a", 40)}, wf.ExampleObserved(head, strings.Repeat("a", 40)), wf.ExampleObserved(status, git.StatusResult{Clean: true})).
		Refuses(Patch{}, "return the clean committed HEAD and a summary")
}

// NUL-delimited paths and -- preserve arbitrary filenames. pipefail propagates
// git/list/read failures; a partial fingerprint must never count as a snapshot.
// Prefix ./ so a filename of - cannot consume stdin instead of its own contents.
const ignoredFingerprint = `git ls-files --others --ignored --exclude-standard -z |
while IFS= read -r -d '' path; do
 case "$path" in *.go) printf 'ignored Go source is not allowed: %s\n' "$path" >&2; exit 1 ;; esac
 sha256sum -- "./$path" || exit
done`

// The run owner supplies whitespace-separated argv, not shell syntax. Disable
// globbing before splitting it and append the engine-allocated artifact as one
// argument. No agent field enters the command.
const defaultGoalCheckCommand = "go test -count=1 -json"

const goalCheckScript = `set -f
command=$1
set -- $command "$2"
exec "$@"`

// Execution evidence comes only from the command's Go test JSON stream.
func goalTestsPassed(r wf.RunResult) bool {
	if r.Exit != 0 {
		return false
	}
	ran, passed := map[string]bool{}, map[string]bool{}
	dec := json.NewDecoder(strings.NewReader(r.Stdout))
	for {
		var event struct{ Action, Test string }
		if err := dec.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return false
		}
		if event.Test == "" {
			continue
		}
		switch event.Action {
		case "run":
			ran[event.Test] = true
		case "pass":
			passed[event.Test] = true
		case "fail", "skip":
			return false
		}
	}
	if len(ran) == 0 {
		return false
	}
	for name := range ran {
		if !passed[name] {
			return false
		}
	}
	return true
}

// Go test reports the package that could not compile in a typed FailedBuild
// field. Give the test author a retry before escalating persistent failures
// to product repair for a potentially missing public API.
func goalTestBuildFailed(r wf.RunResult) bool {
	if r.Exit == 0 {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(r.Stdout))
	failed := false
	for {
		var event struct{ Action, FailedBuild string }
		if err := dec.Decode(&event); err == io.EOF {
			return failed
		} else if err != nil {
			return false
		}
		if event.FailedBuild != "" || event.Action == "build-fail" {
			failed = true
		}
	}
}

func goalTestStaysExternal() wf.Check[Patch] {
	// Compare with the commit pinned before repair, covering every commit the
	// repair made. NUL paths avoid Git's quoting of unusual filenames.
	changed := shell.Observe(shell.Run("git", "diff", "--name-only", "--diff-filter=ACMRT", "-z", "{verifiedHead}", "HEAD", "--").In("{integration}"))
	return wf.Checkf("goal test stays outside product", func(_ Patch, c wf.Ctx) error {
		r, err := wf.Observed(c, changed)
		if err != nil {
			return err
		}
		if r.Exit != 0 {
			return fmt.Errorf("cannot inspect repair commit: %s", r.Stderr)
		}
		for _, path := range strings.Split(r.Stdout, "\x00") {
			if filepath.Base(path) == "goal_test.go" {
				return fmt.Errorf("repair must not add or change allocated goal test %q; remove it from the repair commits and keep it at its allocated path", path)
			}
		}
		return nil
	}).Observes(changed).
		Accepts(Patch{}, wf.ExampleObserved(changed, wf.RunResult{Stdout: "calc.go\x00calc_test.go\x00"})).
		Refuses(Patch{}, `repair must not add or change allocated goal test "goal_test.go"; remove it from the repair commits and keep it at its allocated path`, wf.ExampleObserved(changed, wf.RunResult{Stdout: "goal_test.go\x00"})).
		Refuses(Patch{}, `repair must not add or change allocated goal test "checks/goal_test.go"; remove it from the repair commits and keep it at its allocated path`, wf.ExampleObserved(changed, wf.RunResult{Stdout: "checks/goal_test.go\x00"}))
}
