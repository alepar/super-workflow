package wftest

import (
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A done or failed holding its unit's claim token settles the unit whoever
// calls it and whatever the harness's records show: the bound child with its
// records gone, the coordinator's own session, or an unrelated agent. Nothing
// is refused; a caller that is not the first fetcher is journaled as a child
// caller mismatch.
func TestClaimTokenAdmitsWithoutChildRecord(t *testing.T) {
	for _, h := range []string{"codex", "hermes"} {
		for _, verb := range []string{"done", "failed"} {
			for _, identity := range []string{"coordinator", "child", "unrelated"} {
				for _, retainRecords := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/retain=%t", h, verb, identity, retainRecords), func(t *testing.T) {
						c, err := newProtocolCase(t.TempDir(), h)
						if err != nil {
							t.Fatal(err)
						}
						if err := c.authorize(verb); err != nil {
							t.Fatal(err)
						}
						if !retainRecords {
							if err := os.RemoveAll(c.records); err != nil {
								t.Fatal(err)
							}
						}
						env := c.coordinatorEnv
						if identity != "coordinator" {
							env = maps.Clone(c.childEnv)
						}
						if identity == "unrelated" {
							if h == "codex" {
								env["CODEX_THREAD_ID"] = "unrelated-thread"
							} else {
								env["HERMES_SESSION_ID"] = "unrelated-session"
							}
						}
						if code, out, stderr := c.invoke(verb, env); code != 0 {
							t.Fatalf("exit %d: %s%s", code, out, stderr)
						}
						_, journal, _ := c.call(map[string]string{}, "journal", "protocol-run")
						if strings.Contains(journal, string(wf.DegradedChildRefused)) {
							t.Errorf("the journal holds a refusal:\n%s", journal)
						}
						if noted := strings.Contains(journal, string(wf.DegradedChildCaller)); noted != (identity != "child") {
							t.Errorf("child caller mismatch noted = %v for the %s caller:\n%s", noted, identity, journal)
						}
					})
				}
			}
		}
	}
}
