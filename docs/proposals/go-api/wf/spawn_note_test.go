package wf_test

import (
	"strings"
	"testing"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// sw-zwao: a Codex offer of native spawns says, once, what Codex's
// "agent thread limit reached" refusal means and that the coordinator runs
// next again rather than ending its turn; a re-offer says it too. Other
// harnesses, and an offer with no native spawn, print no such line.
func TestCodexOfferCarriesSpawnRefusalNote(t *testing.T) {
	l := func(verb string, args ...string) string { return "wf " + verb + " " + strings.Join(args, " ") }
	note := hn.For("codex").(hn.SpawnNote).SpawnNote()
	for _, want := range []string{`"agent thread limit reached"`, "do not end your turn", "Run next again", "not started yet"} {
		if !strings.Contains(note, want) {
			t.Errorf("codex spawn note %q lacks %q", note, want)
		}
	}
	unit := wf.Instruction{Kind: wf.InstructionAgent, Name: "a#1", Brief: "Run: wf prompt --claim t", Dispatch: wf.DispatchHints{Model: "m"}}
	two := []wf.Instruction{unit, unit}
	two[1].Name = "a#2"
	offer := wf.RenderNext(wf.NextResult{Instructions: two, Offer: &wf.OfferView{Limit: 3, Source: "max_threads in test", Busy: 1}}, "codex", l)
	if strings.Count(offer, note+"\n") != 1 {
		t.Errorf("codex offer printed the note %d times, want once:\n%s", strings.Count(offer, note), offer)
	}
	if i, j := strings.Index(offer, note), strings.Index(offer, "start a child agent for a#1"); i < 0 || j < i {
		t.Errorf("note does not precede the units:\n%s", offer)
	}
	again := unit
	again.NotStarted, again.SameClaim = true, true
	if relisted := wf.RenderNext(wf.NextResult{Instructions: []wf.Instruction{again}, Relisted: true}, "codex", l); !strings.Contains(relisted, note) {
		t.Errorf("codex re-offer lacks the note:\n%s", relisted)
	}
	if claude := wf.RenderNext(wf.NextResult{Instructions: two, Offer: &wf.OfferView{}}, "claude", l); strings.Contains(claude, "agent thread limit") {
		t.Errorf("claude offer carries the codex note:\n%s", claude)
	}
	headless := unit
	headless.Spawn = "wf spawn a#1"
	if got := wf.RenderNext(wf.NextResult{Instructions: []wf.Instruction{headless}}, "codex", l); strings.Contains(got, note) {
		t.Errorf("headless-only offer carries the note:\n%s", got)
	}
}
