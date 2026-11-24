package feature

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"reflect"
	"strings"
	"testing"
)

func TestRowsReportIDProblemsWithInventory(t *testing.T) {
	expected := []requirement{{"g1", "Export Add"}, {"g2", "  Return the sum"}}
	for _, tc := range []struct {
		name     string
		rows     []ReviewRow
		problems []string
	}{
		{"missing", []ReviewRow{{ID: "g1"}}, []string{"missing", "g2"}},
		{"unknown", []ReviewRow{{ID: "g1"}, {ID: "g2"}, {ID: "other"}}, []string{"unknown", "other"}},
		{"duplicate", []ReviewRow{{ID: "g1"}, {ID: "g2"}, {ID: "g1"}}, []string{"duplicate", "g1"}},
		{"combined", []ReviewRow{{ID: "g1"}, {ID: "g1"}, {ID: "other"}}, []string{"missing", "g2", "unknown", "other", "duplicate", "g1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRows(ReviewResult{Rows: tc.rows}, expected, nil)
			if err == nil {
				t.Fatal("invalid IDs accepted")
			}
			for _, part := range append(tc.problems, "g1", "Export Add", "g2", "  Return the sum") {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("diagnostic %q missing %q", err, part)
				}
			}
		})
	}
}

func TestRowsDoNotValidateEvidenceProse(t *testing.T) {
	for _, evidence := range []string{"", "unrelated wording", "Export Add"} {
		rows := ReviewResult{Rows: []ReviewRow{{ID: "g1", Met: true, Evidence: evidence, Tasks: []string{"add"}}}}
		if err := checkRows(rows, requirements("Export Add"), []Task{{ID: "add"}}); err != nil {
			t.Fatalf("evidence %q rejected: %v", evidence, err)
		}
	}
}

func TestRowsValidateTaskReferences(t *testing.T) {
	rows := ReviewResult{Rows: []ReviewRow{{ID: "g1", Tasks: []string{"add"}}}}
	if err := checkRows(rows, requirements("Export Add"), []Task{{ID: "add"}}); err != nil {
		t.Fatal(err)
	}
	rows.Rows[0].Tasks = []string{"missing"}
	err := checkRows(rows, requirements("Export Add"), []Task{{ID: "add"}})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unknown task diagnostic: %v", err)
	}
}

func TestGoalLineEndingNormalization(t *testing.T) {
	got := requirements("Export Add  \r\n\r\n  Return the sum\t\r\n")
	want := []requirement{{"g1", "Export Add"}, {"g2", "  Return the sum"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	got = criteria([]string{"Export Add", "  Return the sum"})
	want = []requirement{{"c1", "Export Add"}, {"c2", "  Return the sum"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
}

func TestRequirementTableEscapesCellBoundaries(t *testing.T) {
	got := requirementTable([]requirement{{"g1", "Choose A | B"}, {"g2", "First\r\nsecond\nthird\rfourth"}})
	for _, part := range []string{"| ID | Requirement |", "| g1 | Choose A \\| B |", "| g2 | First<br>second<br>third<br>fourth |"} {
		if !strings.Contains(got, part) {
			t.Errorf("table %q missing %q", got, part)
		}
	}
	if strings.Count(got, "\n") != 4 {
		t.Errorf("cell newlines escaped incorrectly: %q", got)
	}
}

func TestRowsRejectEmptyInventory(t *testing.T) {
	if err := checkRows(ReviewResult{}, nil, nil); err == nil || !strings.Contains(err.Error(), "empty requirement inventory") {
		t.Fatalf("empty inventory: %v", err)
	}
}
func TestMetDesignRowRequiresCoveringTask(t *testing.T) {
	rows := ReviewResult{Rows: []ReviewRow{{ID: "g1", Met: true}}}
	if err := checkRows(rows, requirements("Export Add"), []Task{{ID: "add"}}); err == nil {
		t.Fatal("met design row without covering task accepted")
	}
	rows.Rows[0].Met = false
	if err := checkRows(rows, requirements("Export Add"), []Task{{ID: "add"}}); err != nil {
		t.Fatal(err)
	}
}

func TestGoalTestsRequireExecutionEvidence(t *testing.T) {
	for _, output := range []string{"", "[no tests to run]", `{"Action":"pass","Package":"command-line-arguments"}`, `{"Action":"run","Test":"TestAPI"}`, "{\"Action\":\"run\",\"Test\":\"TestAPI\"}\n{\"Action\":\"skip\",\"Test\":\"TestAPI\"}"} {
		if goalTestsPassed(wf.RunResult{Stdout: output}) {
			t.Fatalf("accepted missing execution: %s", output)
		}
	}
	output := "{\"Action\":\"run\",\"Test\":\"TestAPI\"}\n{\"Action\":\"pass\",\"Test\":\"TestAPI\"}"
	if !goalTestsPassed(wf.RunResult{Stdout: output}) {
		t.Fatal("executed passing test refused")
	}
	if goalTestsPassed(wf.RunResult{Exit: 1, Stdout: output}) {
		t.Fatal("failed command accepted")
	}
}

func TestGoalBuildFailureUsesTypedCommandEvidence(t *testing.T) {
	for _, tc := range []struct {
		stdout      string
		exit        int
		buildFailed bool
	}{
		{`{"Action":"fail","FailedBuild":"command-line-arguments"}`, 1, true},
		{`{"Action":"build-fail","ImportPath":"fixture"}`, 1, true},
		{`{"Action":"fail","Test":"TestAPI"}`, 1, false},
		{`{"Action":"output","Output":"build failed"}`, 1, false},
		{`{"Action":"fail","FailedBuild":"fixture"}`, 0, false},
		{`not JSON`, 1, false},
	} {
		if got := goalTestBuildFailed(wf.RunResult{Exit: tc.exit, Stdout: tc.stdout}); got != tc.buildFailed {
			t.Errorf("exit=%d stdout=%s: got %t", tc.exit, tc.stdout, got)
		}
	}
}

func TestPlanRequiresTasksWithoutAnArtifactPath(t *testing.T) {
	p := Plan{Tasks: []Task{{ID: "add", Title: "Addition", Files: []string{"calc.go"}, Acceptance: []string{"2+3 is 5"}}}}
	if err := checkPlan(p, nil); err != nil {
		t.Fatalf("task-only result rejected: %v", err)
	}
	if _, exists := reflect.TypeFor[Plan]().FieldByName("Spec"); exists {
		t.Fatal("Plan duplicates the engine-owned spec artifact")
	}
}
