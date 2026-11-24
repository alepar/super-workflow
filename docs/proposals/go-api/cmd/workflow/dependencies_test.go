package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestShippedWorkflowExcludesSimulator(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("list workflow dependencies: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "testing" || strings.HasSuffix(dep, "/wftest") {
			t.Errorf("shipped workflow imports checker dependency %s", dep)
		}
	}
}
