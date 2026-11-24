package harness

import (
	"strings"
	"testing"
)

func TestPiChecksBriefsByThePredicate(t *testing.T) {
	bc, ok := For("pi").(BriefChecker)
	if !ok {
		t.Fatal("pi does not implement BriefChecker")
	}
	for _, brief := range []string{
		"/example/x/workflow prompt r --claim c\nmore",
		"cd /w && /bin/wf prompt r --claim c",
		"cd /w && wf prompt 'a b'", // a here-document carries a single quote
		"@file prompt",             // stdin is never read as an @file
		"-x prompt",                // nor as an option
	} {
		if err := bc.CheckBrief(brief); err != nil {
			t.Errorf("CheckBrief(%q) = %v, want nil", brief, err)
		}
	}
	for _, brief := range []string{
		"/review --claim c",
		"x --claim c\nWF_BRIEF\nrm -rf /",
	} {
		why := piBriefProblem(brief)
		if why == "" {
			t.Fatalf("test brief %q is accepted by the predicate", brief)
		}
		err := bc.CheckBrief(brief)
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("CheckBrief(%q) = %v, want an error holding %q", brief, err, why)
		}
	}
}

func TestOtherHarnessesHaveNoBriefChecker(t *testing.T) {
	for _, name := range []string{"claude", "codex", "generic"} {
		if _, ok := For(name).(BriefChecker); ok {
			t.Errorf("%s implements BriefChecker", name)
		}
	}
}
