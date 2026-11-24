package wf_test

import (
	"strings"
	"testing"

	hw "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	wf "github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// piBullet is the pi bullet of COORDINATOR.md's section 5: from "- pi:" to
// the next "- opencode:" bullet.
func piBullet(t *testing.T) string {
	t.Helper()
	text := readDocs(t)["../COORDINATOR.md"]
	i := strings.Index(text, "\n- pi: ")
	j := strings.Index(text, "\n- opencode: ")
	if i < 0 || j < i {
		t.Fatalf("COORDINATOR.md has no pi bullet before the opencode bullet (pi at %d, opencode at %d)", i, j)
	}
	return text[i:j]
}

// The doc prints the launch line SpawnLine emits, with the optional flags
// bracketed; it fails whenever the emitted line changes. The placeholders
// pass through SpawnLine unchanged: the claim and the brief are written as
// is, and the effort E is not the GLM off case.
func TestDocsPiLaunchLineIsSpawnLine(t *testing.T) {
	line := hw.For("pi").(interface {
		SpawnLine(hw.Spawn) string
	}).SpawnLine(hw.Spawn{
		Brief: "<brief>", Claim: "<claim>", Model: "M", Effort: "E",
		ErrLog: "<state dir>/child-logs/<run>/wf-<claim>.stderr",
	})
	if !strings.Contains(line, " --model M --thinking E") {
		t.Fatalf("SpawnLine did not pass the placeholders through: %s", line)
	}
	want := strings.Replace(line, " --model M --thinking E", " [--model M] [--thinking E]", 1)
	if !strings.Contains(piBullet(t), "  ```\n  "+strings.ReplaceAll(want, "\n", "\n  ")+"\n  ```\n") {
		t.Errorf("COORDINATOR.md's pi bullet lacks the emitted launch line in a fenced block:\n%s", want)
	}
}

// The rules of section 5's pi bullet, each by the words it is stated in.
func TestDocsPiRules(t *testing.T) {
	text := norm(piBullet(t))
	for _, want := range []string{
		"SUPER_PI_CHILD", "aborted", "context_edit", "leave it running", "cannot tell", "pending",
		"child-logs", "PI_CODING_AGENT_SESSION_DIR", "here-document", "`WF_BRIEF`",
		"`usage`", "compaction", "branch_summary", "tool-result", "cache reads are not counted",
		"working directory", "fresh hand-out",
		"grep -lz '^SUPER_PI_CHILD=wf-<claim>$' /proc/[0-9]*/environ",
		"settle",
	} {
		if !strings.Contains(text, norm(want)) {
			t.Errorf("COORDINATOR.md's pi bullet lacks %q", want)
		}
	}
	for _, gone := range []string{"SIGTERM", "SIGKILL", "records elsewhere", "model note", "context unverified", "[--thinking e] '<brief>'", "[--thinking e] -- '<brief>'", "stopped (`stop`, `length`, `error`, `aborted`)"} {
		if strings.Contains(text, norm(gone)) {
			t.Errorf("COORDINATOR.md's pi bullet still holds the old text %q", gone)
		}
	}
}

// The bootstrap's pi line says the text is pasted unchanged into the
// here-document, and no longer that it is single-quoted after --.
func TestPiSpawnAdviceDescribesTheLine(t *testing.T) {
	boot := wf.Bootstrap(wf.RunStatus{Run: "run", Harness: "pi"}, "./super", "/state")
	if !strings.Contains(boot, "pasted unchanged in place of <the text between the lines>") || !strings.Contains(boot, "here-document") {
		t.Errorf("pi bootstrap does not describe the pasted brief:\n%s", boot)
	}
	if strings.Contains(boot, "single-quoted") {
		t.Errorf("pi bootstrap still says the text is single-quoted:\n%s", boot)
	}
	if strings.Contains(boot, "'") && strings.Contains(piLine(boot), "'") {
		t.Errorf("pi advice line holds an apostrophe: %s", piLine(boot))
	}
}

func piLine(boot string) string {
	for _, l := range strings.Split(boot, "\n") {
		if strings.HasPrefix(l, "pi: ") {
			return l
		}
	}
	return ""
}
