package wf_test

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"strings"
	"testing"
)

func TestStatusShowsNonAgentChildFailureReason(t *testing.T) {
	fail := true
	d := retryMapDef(&fail)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.next(run)
	if view := wf.RenderStatus(h.status(run)); !strings.Contains(view, "item 1 broke") {
		t.Fatalf("halted child reason is hidden: %s", view)
	}
}

func TestExplainNamesChosenRouteAndRefusedCheck(t *testing.T) {
	d, _ := simple("explain", wf.PromptText("go"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	in := h.next(run)[0]
	// Wrong JSON type is a refusal, spending the attempt; the unit remains
	// inspectable.
	h.submit(run, in, `42`)
	text, err := h.e.(*wf.Engine).Explain(run, in.Name)
	if err != nil || !strings.Contains(text, "undecodable submission") || !strings.Contains(text, "refused refused") {
		t.Fatalf("explain=%q err=%v", text, err)
	}
	in = h.next(run)[0]
	h.submit(run, in, `{"V":"ok"}`)
	h.next(run)
	text, err = h.e.(*wf.Engine).Explain(run, in.Name)
	if err != nil || !strings.Contains(text, "route: eA -> eDone (done)") {
		t.Fatalf("explain=%q err=%v", text, err)
	}
	if _, err := h.e.(*wf.Engine).Explain(run, "unknown#1"); err == nil {
		t.Fatal("unknown unit explained as clean")
	}
}
