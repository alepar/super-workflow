package wf

import (
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

func TestGateAnswerReportsRecordedSession(t *testing.T) {
	for _, session := range []string{"coordinator-session", ""} {
		t.Run(session, func(t *testing.T) {
			e, j, run := reservedGateFixture(t, harness.Caller{Session: session}, Reserved)
			if err := e.Answer(run, "reservedQuestion", "yes"); err != nil {
				t.Fatal(err)
			}
			wantSession := session
			if wantSession == "" {
				wantSession = "unknown session"
			}
			want := "answered by " + wantSession
			result, err := e.Wait(run, 0, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, ev := range result.Events {
				if ev.Kind != EventGateAnswered {
					continue
				}
				found = true
				if ev.Detail != "yes" || ev.Line() != "gate answered reservedQuestion yes ("+want+")" {
					t.Fatalf("event: %+v; line: %q", ev, ev.Line())
				}
			}
			if !found {
				t.Fatal("missing gate answer event")
			}
			for _, rec := range reservedRecords(t, j, run) {
				if rec.Kind != JournalGateAnswered {
					continue
				}
				var b strings.Builder
				if err := explainRecord(&b, rec); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(b.String(), "yes ("+want+")\n") {
					t.Fatalf("explanation: %s", b.String())
				}
			}
		})
	}
}
