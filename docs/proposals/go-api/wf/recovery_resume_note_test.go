package wf_test

import (
	"strings"
	"testing"
	"time"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A recovery offer whose harness has a ResumeNote prints it right after the
// resume call; any other harness's offer reads as before.
func TestRecoveryResumeNoteBesideTheResumeLine(t *testing.T) {
	mt := time.Date(2026, 11, 19, 7, 0, 0, 0, time.UTC)

	t.Run("pi", func(t *testing.T) {
		now := mt.Add(time.Second)
		s := newSettleRun(t, "pi", "pi-final-error.jsonl", func() time.Time { return now }, mt)
		if got := s.pass(); len(got) != 0 {
			t.Fatalf("offered before the settle: %+v", got)
		}
		now = mt.Add(settle)
		got := s.pass()
		if len(got) != 1 || got[0].Recovery == nil || got[0].Recovery.Resume == "" {
			t.Fatalf("offers = %+v, want one with a resume line", got)
		}
		v, note := got[0].Recovery, hn.For("pi").(hn.ResumeNote).ResumeNote()
		want := "\n  " + v.Resume + "\n" + note + "\nthen run:\n  " + v.Commands[wf.RecoveryResume]
		if !strings.Contains(got[0].Prompt, want) {
			t.Errorf("prompt = %q\nwant it to hold %q", got[0].Prompt, want)
		}
		for _, w := range []string{"own working directory", "new session", "restart line below"} {
			if !strings.Contains(got[0].Prompt, w) {
				t.Errorf("prompt does not say %q", w)
			}
		}
	})

	t.Run("claude unchanged", func(t *testing.T) {
		now := mt.Add(time.Second)
		s := newSettleRun(t, "claude", "claude-handback.jsonl", func() time.Time { return now }, mt)
		got := s.pass()
		if len(got) != 1 || got[0].Recovery == nil || got[0].Recovery.Resume == "" {
			t.Fatalf("offers = %+v, want one with a resume line", got)
		}
		v := got[0].Recovery
		want := "\nTo resume, make this harness call first:\n  " + v.Resume + "\nthen run:\n  " + v.Commands[wf.RecoveryResume] + "\nTo restart, run:\n  "
		if !strings.Contains(got[0].Prompt, want) {
			t.Errorf("prompt = %q\nwant the resume block as before: %q", got[0].Prompt, want)
		}
		if strings.Contains(got[0].Prompt, "working directory") {
			t.Errorf("a claude offer carries a resume note: %q", got[0].Prompt)
		}
	})
}

func TestCodexRecoveryOfferUsesClaimAgent(t *testing.T) {
	mt := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	s := newSettleRun(t, "codex", "codex-task-complete.jsonl", func() time.Time { return mt.Add(settle) }, mt)
	got := s.pass()
	if len(got) != 1 || got[0].Recovery == nil {
		t.Fatalf("offers = %+v, want one Codex recovery offer", got)
	}
	recovery := got[0].Recovery
	const prefix = `collaboration.followup_task({"target":"wf-kid","message":`
	if !strings.HasPrefix(recovery.Resume, prefix) || !strings.Contains(recovery.Resume, "--claim") {
		t.Errorf("resume line = %q, want follow-up to the claimed thread with the finishing instructions", recovery.Resume)
	}
	if recovery.Commands[wf.RecoveryResume] == "" || recovery.Commands[wf.RecoveryRestart] == "" {
		t.Errorf("commands = %v, want resume and restart", recovery.Commands)
	}
	resumeBlock := "\n  " + recovery.Resume + "\n" + hn.For("codex").(hn.ResumeNote).ResumeNote() +
		"\nthen run:\n  " + recovery.Commands[wf.RecoveryResume]
	if !strings.Contains(got[0].Prompt, resumeBlock) {
		t.Errorf("offer prompt does not print the harness call and note before the resume command: %q", got[0].Prompt)
	}
}

// Codex acknowledges dispatch before the child writes or calls the engine.
// Recording resume spends the offer; an unchanged transcript then reaches
// the ordinary stall question, where confirmed death re-offers the unit.
func TestCodexSilentResumeReoffersThroughStall(t *testing.T) {
	mt := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	now := mt.Add(time.Second)
	s := newSettleRun(t, "codex", "codex-task-complete.jsonl", func() time.Time { return now }, mt)
	if _, err := s.e.PromptClaim(s.run, s.unit.Name, claimToken(t, s.unit)); err != nil {
		t.Fatal(err)
	}
	offers := s.pass()
	if len(offers) != 1 || offers[0].Recovery == nil || offers[0].Recovery.Resume == "" {
		t.Fatalf("offers = %+v, want one Codex recovery offer with resume", offers)
	}
	text := offers[0].Prompt
	for _, want := range []string{"If the call is refused", "keep running next", "stall question", "heartbeat died"} {
		if !strings.Contains(text, want) {
			t.Errorf("recovery prompt lacks %q: %s", want, text)
		}
	}
	if err := s.e.Recover(s.run, s.unit.Name, wf.RecoveryResume); err != nil {
		t.Fatal(err)
	}
	if got := s.pass(); len(got) != 0 {
		t.Fatalf("silent child immediately reopened recovery: %+v", got)
	}
	if err := s.e.Recover(s.run, s.unit.Name, wf.RecoveryRestart); err == nil || !strings.Contains(err.Error(), "has no open recovery offer") {
		t.Fatalf("restart after resume = %v, want no open recovery offer", err)
	}
	// No tool turns, new transcript writes or child calls take place.
	now = now.Add(time.Hour)
	next, err := s.e.NextWait(s.run, 0, s.co, time.Millisecond)
	if err != nil || len(next.Stalled) != 1 || next.Stalled[0].Name != s.unit.Name {
		t.Fatalf("next after silence = %+v, %v; want the stalled child", next, err)
	}
	if q := wf.RenderNext(next, s.co.Harness, func(verb string, args ...string) string {
		return strings.Join(append([]string{"wf", verb}, args...), " ")
	}); !strings.Contains(q, "heartbeat died "+s.unit.Name) {
		t.Fatalf("stall question lacks its died command: %s", q)
	}
	if err := s.e.HeartbeatAs(s.run, wf.HeartbeatTarget{Name: s.unit.Name}, false, s.co); err != nil {
		t.Fatal(err)
	}
	again, err := s.e.HandOutAs(s.run, 1, s.co)
	if err != nil || len(again) != 1 || again[0].Kind != wf.InstructionAgent || again[0].Attempt != s.unit.Attempt+1 ||
		claimToken(t, again[0]) == claimToken(t, s.unit) {
		t.Fatalf("next after confirmed death = %+v, %v; want a fresh attempt and claim", again, err)
	}
}
