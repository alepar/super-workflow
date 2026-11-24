package wf_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestCoordinatorErrorEndsWithActionWithoutChangingState(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"marked", []string{"next", "missing", "--state-dir", t.TempDir()}, map[string]string{"HERMES_SESSION_ID": "coordinator"}, "This command failed, and the error above says why: you may run next again."},
		{"bad-flags", []string{"next", "missing", "--not-a-flag"}, map[string]string{"CODEX_THREAD_ID": "coordinator"}, "Read this command's --help and correct its arguments, then run it again."},
		{"person", []string{"next", "missing", "--state-dir", t.TempDir()}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := wf.CLIWithEnv(wf.MainConfig{}, tc.args, &out, &stderr, func(k string) string { return tc.env[k] })
			if code == 0 {
				t.Fatal("expected failure")
			}
			// An error no person has to clear names no stop: the coordinator
			// is told what to do and that it may go on.
			got := strings.TrimSpace(stderr.String())
			if strings.Contains(got, "stop and report") || strings.Contains(got, "work around") {
				t.Fatalf("error guidance tells the coordinator to stop: %q", got)
			}
			if tc.want == "" && strings.Contains(got, "run next again") || !strings.HasSuffix(got, tc.want) {
				t.Fatalf("error guidance: %q, want it to end %q", got, tc.want)
			}
		})
	}
}
