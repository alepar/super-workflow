package harness_test

import (
	"bufio"
	"os/exec"
	"strings"
	"testing"
)

func TestWorkflowDependenciesDoNotEmbedHarnessFixtures(t *testing.T) {
	const module = "github.com/alepar/super-workflow/docs/proposals/go-api"
	const listing = "{{if .EmbedFiles}}{{.ImportPath}} {{join .EmbedFiles \" \"}}{{end}}"
	out, err := exec.Command("go", "list", "-deps", "-f", listing, module+"/wf", module+"/wftest").CombinedOutput()
	if err != nil {
		t.Fatalf("list workflow and checker dependencies: %v\n%s", err, out)
	}
	lines := bufio.NewScanner(strings.NewReader(string(out)))
	for lines.Scan() {
		pkg, files, _ := strings.Cut(lines.Text(), " ")
		if strings.HasPrefix(pkg, module+"/") && strings.Contains(files, "testdata/") {
			t.Errorf("%s embeds test fixtures in its regular build", pkg)
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
}
