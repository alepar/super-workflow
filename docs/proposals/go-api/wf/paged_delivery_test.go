package wf_test

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	ha "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Removing the page bounds breaks this test. Pages are fetched back to back,
// with no wait between them: delivery has no rate limit.
func TestPagedDeliveryEnvelope(t *testing.T) {
	d, _ := simple("paged", wf.PromptText("First constraint.\n"+strings.Repeat("界é long instruction. ", 1400)+"\nLast constraint."))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	req := wf.PromptPageRequest{Page: 1}
	first, err := h.e.(*wf.Engine).PromptPage(run, ins.Name, "", ha.Caller{}, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Pages < 2 {
		t.Fatalf("not paginated: %+v", first)
	}
	all := first.Delivered
	page := first
	for n := 1; n <= first.Pages; n++ {
		if n > 1 {
			page, err = h.e.(*wf.Engine).PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: n, Code: page.NextCode})
			if err != nil {
				t.Fatal(err)
			}
			all += page.Delivered
		}
		if len(page.Delivered) > 3072 || strings.Count(page.Delivered, "\n") > 150 {
			t.Fatalf("oversized page %d: %d bytes", n, len(page.Delivered))
		}
		for _, line := range strings.Split(page.Delivered, "\n") {
			if utf8.RuneCountInString(line) > 100 {
				t.Fatalf("wide line: %q", line)
			}
		}
	}
	if !strings.Contains(all, "First constraint.") || !strings.Contains(all, "Last constraint.") {
		t.Fatal("constraints lost")
	}
	if _, err = h.e.(*wf.Engine).PromptPage(run, ins.Name, "", ha.Caller{}, req); err != nil {
		t.Fatalf("idempotent re-fetch: %v", err)
	}
}

func TestPagedDeliveryLargeInputReference(t *testing.T) {
	value := strings.Repeat("reference constraint. ", 1800)
	d, _ := simple("paged-reference", wf.PromptText("Use {{spec}}.", wf.Bound("spec", value)))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	page, err := h.e.(*wf.Engine).PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Delivered, "required reference") {
		t.Fatal("large input was inlined instead of indexed")
	}
}

func TestPagedDeliveryNoWholePromptBypass(t *testing.T) {
	d, _ := simple("paged-whole", wf.PromptText(strings.Repeat("constraint ", 2000)))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	f, err := h.e.(*wf.Engine).PromptFetchWhole(run, ins.Name, "", ha.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Prompt) > 0 || len(f.Delivered) > 3072 {
		t.Fatal("whole prompt bypasses paged delivery")
	}
}

func TestPagedDeliveryCLIOptIn(t *testing.T) {
	state := t.TempDir()
	code, out, er := cliCall(clConfig(), "run", "agent-cli", "--state-dir", state, "--paged-prompts", "--run-id", "paged-cli")
	if code != 0 {
		t.Fatalf("opt in: %d %s", code, er)
	}
	run := strings.TrimSpace(out)
	code, out, er = cliCall(clConfig(), "next", run, "1", "--state-dir", state, "--json")
	if code != 0 {
		t.Fatalf("next: %d %s", code, er)
	}
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(out), &ins); err != nil {
		t.Fatal(err)
	}
	if len(ins) == 0 {
		t.Fatal("no instruction")
	}
	code, out, er = cliCall(clConfig(), "prompt", run, ins[0].Name, "--state-dir", state, "--page", "1")
	if code != 0 {
		t.Fatalf("prompt: %d %s", code, er)
	}
	if !strings.Contains(out, "Required prompt page") {
		t.Fatalf("not paged: %s", out)
	}
}

func TestPagedDeliveryReferenceCoverageAndOriginals(t *testing.T) {
	value := `{"number":9007199254740993,"body":"` + strings.Repeat("rare constraint ", 1600) + `"}`
	d, _ := simple("paged-originals", wf.PromptText("Use {{spec}}.", wf.Bound("spec", value)))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	engine := h.e.(*wf.Engine)
	page, err := engine.PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	var record wf.PromptFetchedData
	if err = json.Unmarshal(h.kind(run, wf.JournalPromptFetched)[0].Data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Packet.Originals["spec"] != value {
		t.Fatal("original input changed")
	}
	for n := 2; n <= page.Pages; n++ {
		h.now = h.now.Add(time.Second)
		page, err = engine.PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: n, Code: page.NextCode})
		if err != nil {
			t.Fatal(err)
		}
	}
	for id, ref := range record.Packet.References {
		code := ""
		all := ""
		for n := 1; n <= len(ref.Pages); n++ {
			h.now = h.now.Add(time.Second)
			p, e := engine.PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Reference: id, Page: n, Code: code})
			if e != nil {
				t.Fatal(e)
			}
			code = p.NextCode
			all += p.Delivered
		}
		if !strings.Contains(all, "9007199254740993") {
			t.Fatal("JSON number changed during readable rendering")
		}
	}
	status, err := h.e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wf.RenderStatus(status), "required pages fetched") {
		t.Fatal("status lacks honest coverage")
	}
}

func TestPagedDeliveryOptionalReference(t *testing.T) {
	d, _ := simple("paged-optional", wf.PromptText("Consult {{spec}} if useful.", wf.Reference(wf.Bound("spec", strings.Repeat("optional material ", 1500)), false)))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	page, err := h.e.(*wf.Engine).PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Delivered, "optional reference") {
		t.Fatal("optional reference presented as required")
	}
	var record wf.PromptFetchedData
	if err = json.Unmarshal(h.kind(run, wf.JournalPromptFetched)[0].Data, &record); err != nil {
		t.Fatal(err)
	}
	for _, ref := range record.Packet.References {
		for _, body := range ref.Pages {
			if strings.Contains(body, "Required prompt page") || strings.Contains(body, "End of required pages") {
				t.Fatal("optional page falsely requires fetching")
			}
		}
	}

	for n := 2; n <= page.Pages; n++ {
		h.now = h.now.Add(time.Second)
		page, err = h.e.(*wf.Engine).PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: n, Code: page.NextCode})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A done is trusted with pages still unfetched: the child is told to read
// every page, and neither validate nor done counts its fetches. Pages need no
// order and no continuation code. A page out of range, an unknown reference
// and an unoffered token are still refused, the packet is the same after a
// reopen, and status still shows how many pages were served.
func TestPagedDeliveryDoneAcceptedWithPagesUnfetched(t *testing.T) {
	d, _ := simple("paged-replay", wf.PromptText(strings.Repeat("constraint ", 1200)))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	e := h.e.(*wf.Engine)
	page, err := e.PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.Pages < 4 {
		t.Fatalf("want at least 4 pages, got %d", page.Pages)
	}
	for _, req := range []wf.PromptPageRequest{{Page: 999}, {Reference: "wrong", Page: 1}} {
		if _, err = e.PromptPage(run, ins.Name, "", ha.Caller{}, req); err == nil {
			t.Fatalf("invalid request accepted: %+v", req)
		}
	}
	if _, err = e.PromptPage(run, ins.Name, "unoffered-token", ha.Caller{}, wf.PromptPageRequest{Page: 1}); err == nil {
		t.Fatal("wrong child token accepted")
	}
	// Page 3 before page 2, with no continuation code and no wait.
	if _, err = e.PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: 3}); err != nil {
		t.Fatalf("out-of-order fetch: %v", err)
	}
	restarted := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Clock: func() time.Time { return h.now }}).(*wf.Engine)
	again, err := restarted.PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: 1})
	if err != nil || again.Delivered != page.Delivered {
		t.Fatalf("packet not immutable across reopen: %v", err)
	}
	if res, err := restarted.Validate(run, ins.Name, wf.Submission{Output: json.RawMessage(`{"V":"ok"}`)}); err != nil || res.Class != wf.Accepted {
		t.Fatalf("validate with pages unfetched = %+v, %v; want accepted", res, err)
	}
	if res, err := restarted.Done(run, ins.Name, wf.Submission{Output: json.RawMessage(`{"V":"ok"}`)}); err != nil || res.Class != wf.Accepted {
		t.Fatalf("done with pages unfetched = %+v, %v; want accepted", res, err)
	}
	status, err := restarted.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	if want := "2 of " + strconv.Itoa(page.Pages) + " required pages fetched"; !strings.Contains(wf.RenderStatus(status), want) {
		t.Fatalf("status lacks %q:\n%s", want, wf.RenderStatus(status))
	}
}

// failed needs no page fetched.
func TestPagedDeliveryFailedWithNoPageFetched(t *testing.T) {
	d, _ := simple("paged-failed", wf.PromptText(strings.Repeat("constraint ", 1200)))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Failed(run, ins.Name, "cannot retrieve inputs"); err != nil {
		t.Fatalf("failed with no page fetched: %v", err)
	}
}

func TestPagedDeliveryStartedMetadata(t *testing.T) {
	d, _ := simple("paged-metadata", wf.PromptText("Work."))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, wf.Claim{Session: "session"}); err != nil {
		t.Fatal(err)
	}
	var hand wf.HandedOutData
	if err := json.Unmarshal(h.kind(run, wf.JournalHandedOut)[0].Data, &hand); err != nil {
		t.Fatal(err)
	}
	_, err := h.e.(*wf.Engine).PromptPage(run, ins.Name, hand.Claim, ha.Caller{FromEnv: true, Harness: "generic", Session: "session", Agent: "child", Root: t.TempDir()}, wf.PromptPageRequest{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	attached := metadataAttached(t, h, run)
	if len(attached) == 0 || attached[0].Root == "" || attached[0].Agent != "child" {
		t.Fatal("child fetch did not attach missing metadata")
	}
}

func TestPagedDeliveryReadCLIReference(t *testing.T) {
	def, _ := simple("read-cli", wf.PromptText("Use {{spec}}", wf.Reference(wf.Bound("spec", "unique reference content"), false)))
	def.OnFailure(wf.Blocked, wf.NewTerminal[eOther](def, wf.Stopped))
	state := t.TempDir()
	mustSingle(t, def, "run", "--state-dir", state, "--run-id", "r1", "--paged-prompts")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil {
		t.Fatal(err)
	}
	out := mustSingle(t, def, "wf", "prompt", ins[0].Name, "--state-dir", state)
	match := regexp.MustCompile(`optional reference spec \(([0-9a-f]+)\)`).FindStringSubmatch(out)
	if len(match) != 2 {
		t.Fatalf("no reference ID: %s", out)
	}
	time.Sleep(510 * time.Millisecond)
	out = mustSingle(t, def, "wf", "read", match[1], "--page", "1", "--state-dir", state)
	if !strings.Contains(out, "unique reference content") || !strings.Contains(out, "Optional input page") {
		t.Fatalf("wrong reference delivery: %s", out)
	}
}

func TestPagedDeliveryBindsCallerAndCarriesToken(t *testing.T) {
	d, _ := simple("paged-guard", wf.PromptText(strings.Repeat("instruction ", 1600), wf.Reference(wf.Bound("spec", "reference"), false)))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	token := claimToken(t, ins)
	who := ha.Caller{FromEnv: true, Is: ha.IsChild, Harness: "codex", Session: "parent", Agent: "owner"}
	e := h.e.(*wf.Engine)
	page, err := e.PromptPage(run, "", token, who, wf.PromptPageRequest{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Delivered, "--claim "+token) {
		t.Fatal("continuation lost its claim token")
	}
	sibling := who
	sibling.Agent = "sibling"
	// A sibling holding the token is served; the first fetcher stays the
	// recorded caller and the mismatch is noted once.
	if _, err = e.PromptPage(run, "", token, sibling, wf.PromptPageRequest{Page: 1}); err != nil {
		t.Fatalf("sibling re-fetch holding the token = %v, want served", err)
	}
	var record wf.PromptFetchedData
	if err = json.Unmarshal(h.kind(run, wf.JournalPromptFetched)[0].Data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Caller == nil || record.Caller.Agent != "owner" {
		t.Fatal("first fetch did not bind caller")
	}
	for _, p := range record.Packet.Pages {
		if strings.Contains(p, "Next (one page per command):") && !strings.Contains(p, "--claim "+token) {
			t.Fatal("a continuation lost its token")
		}
	}
	for _, ref := range record.Packet.References {
		for _, p := range ref.Pages {
			if strings.Contains(p, "Next (one page per command):") && !strings.Contains(p, "--claim "+token) {
				t.Fatal("a reference continuation lost its token")
			}
		}
	}
	h.now = h.now.Add(time.Second)
	if _, err = e.PromptPage(run, "", token, who, wf.PromptPageRequest{Page: 2, Code: page.NextCode}); err != nil {
		t.Fatal(err)
	}
	if got := h.kind(run, wf.JournalDegraded); len(got) != 1 || !strings.Contains(string(got[0].Data), string(wf.DegradedChildCaller)) {
		t.Fatalf("degraded records %+v, want one child caller mismatch", got)
	}
}

func TestPagedPromptShapeExcludesFetchToken(t *testing.T) {
	d, _ := simple("paged-shape", wf.PromptText("Use {{spec}}", wf.Reference(wf.Bound("spec", "fixed input"), true)))
	var hashes []string
	for range 2 {
		h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
		run, ins := handOneUnit(t, h, d)
		if _, err := h.e.(*wf.Engine).PromptPage(run, "", claimToken(t, ins), ha.Caller{}, wf.PromptPageRequest{Page: 1}); err != nil {
			t.Fatal(err)
		}
		var record wf.PromptFetchedData
		if err := json.Unmarshal(h.kind(run, wf.JournalPromptFetched)[0].Data, &record); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, record.PromptShapeHash)
	}
	if hashes[0] != hashes[1] {
		t.Fatalf("identical input has run-specific shape hashes: %v", hashes)
	}
}

func TestPagedDeliveryCommandsUseRunWorkspace(t *testing.T) {
	root := t.TempDir()
	d, _ := simple("paged-workspace", wf.PromptText("Use {{spec}}. "+strings.Repeat("instruction ", 500), wf.Reference(wf.Bound("spec", "reference text"), true)))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true, Workspace: root,
		VerbLine: func(_ wf.RunID, verb string, _ ...string) string { return "echo " + verb },
	})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	page, err := h.e.(*wf.Engine).PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(page.Delivered, " \\\n", " ")
	for _, verb := range []string{"prompt", "read", "validate", "done", "failed"} {
		if !strings.Contains(text, "cd "+root+" && echo "+verb) {
			t.Errorf("%s lacks stable workspace: %s", verb, text)
		}
	}
}

// A Registration with PagedPrompts pages its runs by default, and
// --paged-prompts=false opts one run out.
func TestPagedDeliveryRegistrationDefault(t *testing.T) {
	cfg := clConfig()
	cfg.Defs = append(cfg.Defs, wf.Registration{Def: clAgentDef("paged-reg"), PagedPrompts: true})
	for _, tc := range []struct {
		flags []string
		paged bool
	}{{nil, true}, {[]string{"--paged-prompts=false"}, false}, {[]string{"--paged-prompts"}, true}} {
		state := t.TempDir()
		code, out, er := cliCall(cfg, append([]string{"run", "paged-reg", "--state-dir", state, "--run-id", "reg"}, tc.flags...)...)
		if code != 0 {
			t.Fatalf("run %v: %d %s", tc.flags, code, er)
		}
		run := strings.TrimSpace(out)
		code, out, er = cliCall(cfg, "next", run, "1", "--state-dir", state, "--json")
		var ins []wf.Instruction
		if code != 0 || json.Unmarshal([]byte(out), &ins) != nil || len(ins) == 0 {
			t.Fatalf("next %v: %d %s %s", tc.flags, code, out, er)
		}
		code, out, er = cliCall(cfg, "prompt", run, ins[0].Name, "--state-dir", state, "--page", "1")
		if paged := code == 0 && strings.Contains(out, "Required prompt page"); paged != tc.paged {
			t.Errorf("run %v: paged %v (exit %d, %s), want %v", tc.flags, paged, code, er, tc.paged)
		}
	}
}
