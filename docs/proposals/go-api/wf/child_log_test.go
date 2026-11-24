package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func plainVerb(v string, a ...string) string { return v }

// piRender is how next prints ins to a coordinator of harnessName.
func piRender(ins []wf.Instruction, harnessName string, relisted bool) string {
	return wf.RenderNext(wf.NextResult{Instructions: ins, Harness: harnessName, Relisted: relisted}, harnessName, plainVerb)
}

func errLogHarness(t *testing.T, state string) (*harness, wf.RunID) {
	t.Helper()
	return quoteHarness(t, "tidy the repo", wf.EngineOptions{StateRoot: state})
}

func handedOutErrLogs(t *testing.T, h *harness, run wf.RunID) []string {
	t.Helper()
	var out []string
	for _, r := range h.kind(run, wf.JournalHandedOut) {
		var d wf.HandedOutData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Spawn == nil {
			out = append(out, "")
			continue
		}
		out = append(out, d.Spawn.ErrLog)
	}
	return out
}

func TestPiOfferCarriesTheErrLog(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	h, run := errLogHarness(t, state)
	ins := quoteHandOut(t, h, run, "pi")
	if len(ins) != 1 {
		t.Fatalf("pi offer = %+v", ins)
	}
	want := filepath.Join(state, "child-logs", string(run), "wf-"+claimToken(t, ins[0])+".stderr")
	if ins[0].ErrLog != want {
		t.Fatalf("Instruction.ErrLog = %q, want %q", ins[0].ErrLog, want)
	}
	if st, err := os.Stat(filepath.Dir(want)); err != nil || !st.IsDir() {
		t.Errorf("the child log directory was not created: %v", err)
	}
	if got := handedOutErrLogs(t, h, run); len(got) != 1 || got[0] != want {
		t.Errorf("journaled Spawn.ErrLog = %q, want %q", got, want)
	}
	if out := piRender(ins, "pi", false); !strings.Contains(out, "2>"+want+" &") {
		t.Errorf("the pi offer does not redirect stderr to the recorded file:\n%s", out)
	}

	// A claude coordinator gets no path, no directory, no journaled file.
	state = filepath.Join(t.TempDir(), "state")
	h, run = errLogHarness(t, state)
	ins = quoteHandOut(t, h, run, "claude")
	if len(ins) != 1 || ins[0].ErrLog != "" {
		t.Fatalf("claude offer = %+v, want no ErrLog", ins)
	}
	if _, err := os.Stat(filepath.Join(state, "child-logs")); !os.IsNotExist(err) {
		t.Errorf("a claude hand-out created child-logs: %v", err)
	}
	if got := handedOutErrLogs(t, h, run); len(got) != 1 || got[0] != "" {
		t.Errorf("journaled Spawn.ErrLog for claude = %q, want none", got)
	}
	if out := piRender(ins, "claude", false); strings.Contains(out, "child-logs") {
		t.Errorf("a claude offer names child-logs:\n%s", out)
	}

	// A library engine with no state dir records none, even for pi.
	h, run = quoteHarness(t, "tidy the repo", wf.EngineOptions{})
	ins = quoteHandOut(t, h, run, "pi")
	if len(ins) != 1 || ins[0].ErrLog != "" {
		t.Fatalf("pi offer without a state dir = %+v, want no ErrLog", ins)
	}
}

// pi sessions the way pi writes them: a header, then records.
func writePiSession(t *testing.T, root, id string, created time.Time, records ...map[string]any) {
	t.Helper()
	stamp := created.UTC().Format(time.RFC3339Nano)
	lines := []string{`{"type":"session","version":3,"id":"` + id + `","timestamp":"` + stamp + `","cwd":"/work"}`}
	for _, r := range records {
		if _, ok := r["timestamp"]; !ok {
			r["timestamp"] = stamp
		}
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	p := filepath.Join(root, "sessions", "--work--", "2026-11-19T00-00-00-000Z_"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func piUser(text string) map[string]any {
	return map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}
}

func piBash(command string, at time.Time) map[string]any {
	return map[string]any{"type": "message", "timestamp": at.UTC().Format(time.RFC3339Nano), "message": map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{
		map[string]any{"type": "toolCall", "name": "bash", "arguments": map[string]any{"command": command}}}}}
}

var renderedPiLine = regexp.MustCompile(`unchanged:\n(SUPER_PI_CHILD=.* &\n<the text between the lines>\nWF_BRIEF)\nIts whole task`)

// piLaunchCase hands a unit to a pi coordinator whose records are at root,
// writes the coordinator's launch (rewrite changes the line) and the child's
// session, and returns whether the adapter finds the launch and what the
// child's prompt fetch says.
func piLaunchCase(t *testing.T, rewrite func(line, state string) string) (found bool, fetch error) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	h, run := errLogHarness(t, state)
	root := t.TempDir()
	e := h.e.(*wf.Engine)
	all, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "pi", Session: "coord", Root: root})
	ins := agentsOf(all)
	if err != nil || len(ins) != 1 {
		t.Fatalf("pi offer = %+v, %v", all, err)
	}
	claim := claimToken(t, ins[0])
	m := renderedPiLine.FindStringSubmatch(piRender(ins, "pi", false))
	if m == nil {
		t.Fatalf("no launch line in the rendered offer:\n%s", piRender(ins, "pi", false))
	}
	line := strings.Replace(m[1], "<the text between the lines>", ins[0].Brief, 1)
	if rewrite != nil {
		line = rewrite(line, state)
	}
	kid := "wf-" + claim
	writePiSession(t, root, "coord", h.now, piUser("run the workflow"), piBash(line, h.now.Add(10*time.Second)))
	writePiSession(t, root, kid, h.now.Add(11*time.Second), piUser(ins[0].Brief))
	want := hn.Want{Token: claim, After: h.now.Add(-time.Minute)}
	_, found = hn.For("pi").Resolve(root, "coord", []hn.Want{want})[want]
	_, fetch = e.PromptFetchAs(run, ins[0].Name, claim, hn.Caller{Harness: "pi", Session: "coord", Agent: kid, Root: root, Is: hn.IsChild, FromEnv: true})
	return found, fetch
}

func TestRenderedPiOfferPassesTheLaunchGrammar(t *testing.T) {
	found, fetch := piLaunchCase(t, nil)
	if !found {
		t.Error("the rendered pi line does not pass piLaunch")
	}
	if fetch != nil {
		t.Errorf("the exact launch was refused by the engine: %v", fetch)
	}
}

// A coordinator that rewrote the stderr directory of its launch left records
// the adapter does not match to the claim (TestResolveWithRecordedErrLog).
// The child's fetch is served all the same: its token admits it.
func TestRewrittenErrLogDirectoryIsServed(t *testing.T) {
	_, fetch := piLaunchCase(t, func(line, state string) string {
		out := strings.Replace(line, filepath.Join(state, "child-logs"), "/elsewhere/child-logs", 1)
		if out == line {
			t.Fatalf("test bug: the line does not hold the state dir:\n%s", line)
		}
		return out
	})
	if fetch != nil {
		t.Errorf("a child whose launch wrote stderr elsewhere was refused: %v", fetch)
	}
}

func TestStateDirWithASpaceRefusesPiOnly(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state dir")
	h, run := errLogHarness(t, state)
	if ins := quoteHandOut(t, h, run, "pi"); len(ins) != 0 {
		t.Fatalf("a pi hand-out with a space in the state dir was offered: %+v", ins)
	}
	if n := len(h.kind(run, wf.JournalHandedOut)); n != 0 {
		t.Fatalf("%d handed_out records for a refused path", n)
	}
	rs := refusedRecords(t, h, run)
	if len(rs) != 1 || !rs[0].Halt || len(rs[0].Reasons) != 1 || !strings.Contains(rs[0].Reasons[0], filepath.Join(state, "child-logs", string(run))) || !strings.Contains(rs[0].Reasons[0], "hand-out of ") {
		t.Fatalf("refused records = %+v, want one halt naming the stderr path", rs)
	}
	if _, err := os.Stat(filepath.Join(state, "child-logs")); !os.IsNotExist(err) {
		t.Errorf("a refused hand-out created the child log directory: %v", err)
	}

	h, run = errLogHarness(t, state)
	ins := quoteHandOut(t, h, run, "claude")
	if len(ins) != 1 || ins[0].ErrLog != "" {
		t.Fatalf("claude on the same state dir = %+v, want offered normally", ins)
	}
	if _, err := os.Stat(filepath.Join(state, "child-logs")); !os.IsNotExist(err) {
		t.Errorf("a claude hand-out created child-logs: %v", err)
	}
}

func TestNotStartedPointsAtTheEarlierErrLog(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	h, run := errLogHarness(t, state)
	first := quoteHandOut(t, h, run, "pi")
	if len(first) != 1 {
		t.Fatalf("first offer = %+v", first)
	}
	p1 := first[0].ErrLog
	long := strings.Repeat("x", 250)
	if err := os.WriteFile(p1, []byte("one\n\ntwo\n"+long+"\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(10 * time.Minute)
	again := quoteHandOut(t, h, run, "pi")
	if len(again) != 1 || !again[0].NotStarted {
		t.Fatalf("re-offer = %+v, want never started", again)
	}
	if again[0].PrevErrLog != p1 || again[0].ErrLog == p1 || again[0].ErrLog == "" {
		t.Fatalf("PrevErrLog = %q, ErrLog = %q; want the earlier %q and a new path", again[0].PrevErrLog, again[0].ErrLog, p1)
	}
	out := piRender(again, "pi", false)
	cut := strings.Repeat("x", 197) + "…"
	for _, want := range []string{hn.For("pi").(hn.StartLog).StartLog(p1), "its last lines:\n  two\n  " + cut + "\n  four\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the re-offer does not hold %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "one\n") {
		t.Errorf("the excerpt holds more than the last three lines:\n%s", out)
	}

	// With the file missing, the sentence alone.
	state = filepath.Join(t.TempDir(), "state")
	h, run = errLogHarness(t, state)
	first = quoteHandOut(t, h, run, "pi")
	h.now = h.now.Add(10 * time.Minute)
	again = quoteHandOut(t, h, run, "pi")
	if len(again) != 1 || again[0].PrevErrLog != first[0].ErrLog || len(again[0].PrevErrTail) != 0 {
		t.Fatalf("re-offer without a stderr file = %+v", again)
	}
	out = piRender(again, "pi", false)
	if !strings.Contains(out, hn.For("pi").(hn.StartLog).StartLog(first[0].ErrLog)) || strings.Contains(out, "its last lines") {
		t.Errorf("want the sentence alone:\n%s", out)
	}
}

func TestRelistSetsNotStarted(t *testing.T) {
	for _, name := range []string{"pi", "claude"} {
		state := filepath.Join(t.TempDir(), "state")
		h, run := errLogHarness(t, state)
		first := quoteHandOut(t, h, run, name)
		if len(first) != 1 {
			t.Fatalf("%s first offer = %+v", name, first)
		}
		if first[0].ErrLog != "" {
			if err := os.WriteFile(first[0].ErrLog, []byte("boom\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		co := wf.Coordinator{Harness: name, Session: "coord"}
		res, err := h.e.(*wf.Engine).NextWait(run, wf.NextAuto, co, 50*time.Millisecond)
		got := agentsOf(res.Instructions)
		if err != nil || !res.Relisted || len(got) != 1 || !got[0].NotStarted {
			t.Fatalf("%s: wait end = %+v, %v; want the unit listed again as never started", name, res, err)
		}
		out := wf.RenderNext(res, name, plainVerb)
		if !strings.Contains(out, strings.SplitN(wf.NextNotStarted, "%s", 2)[0]) {
			t.Errorf("%s: the relisted offer lacks the not-started wording:\n%s", name, out)
		}
		if name == "pi" {
			if got[0].PrevErrLog != first[0].ErrLog || !strings.Contains(out, hn.For("pi").(hn.StartLog).StartLog(first[0].ErrLog)) || !strings.Contains(out, "its last lines:\n  boom\n") {
				t.Errorf("pi relist = %+v; want the earlier file named with its last line:\n%s", got[0], out)
			}
			continue
		}
		if got[0].PrevErrLog != "" || strings.Contains(out, "stderr") || strings.Contains(out, "did not start is in") {
			t.Errorf("claude relist carries a stderr pointer: %+v\n%s", got[0], out)
		}
	}
}

// A pi offer is sized with its stderr path, and when it is offered again its
// not-started sentence and a full excerpt: with OutBytes just over one such
// offer, two never-started units offer one and report the other as More.
func TestOfferSizingCountsTheStderrPointer(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	d := fanDef("proto-size", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{StateRoot: state})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "pi", Session: "coord"}
	first, err := e.HandOutAs(run, 2, co)
	if err != nil || len(agentsOf(first)) != 2 {
		t.Fatalf("first hand-out = %+v, %v", first, err)
	}
	// Fill each recorded file with the worst-case excerpt.
	line := strings.Repeat("y", 250)
	for _, in := range agentsOf(first) {
		if err := os.WriteFile(in.ErrLog, []byte(line+"\n"+line+"\n"+line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.now = h.now.Add(10 * time.Minute)
	sized := func(limit int) (offered, more int) {
		c := co
		c.OutBytes = limit
		res, err := e.NextWait(run, 2, c, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		out := piRender(res.Instructions, "pi", false)
		if len(res.Instructions) > 0 && len(out) > limit && len(agentsOf(res.Instructions)) > 1 {
			t.Errorf("limit %d: %d offers printed %d bytes", limit, len(res.Instructions), len(out))
		}
		return len(agentsOf(res.Instructions)), res.More
	}
	// Measure one real never-started offer, then bracket the limit around two.
	c := co
	res, err := e.NextWait(run, 2, c, time.Second)
	if err != nil || len(agentsOf(res.Instructions)) != 2 {
		t.Fatalf("unlimited re-offer = %+v, %v", res, err)
	}
	one := len(piRender(res.Instructions[:1], "pi", false))
	both := len(piRender(res.Instructions, "pi", false))
	h.now = h.now.Add(10 * time.Minute)
	if n, more := sized(both + outputReserve); n != 2 || more != 0 {
		t.Errorf("limit with room for both: offered %d, more %d; want 2, 0", n, more)
	}
	h.now = h.now.Add(10 * time.Minute)
	if n, more := sized(one + outputReserve); n != 1 || more != 1 {
		t.Errorf("limit with room for one: offered %d, more %d; want 1, 1", n, more)
	}
}

// outputReserve mirrors the engine's reserve for the offer header and advice
// (outputReserveBytes), plus slack for the worst-case sizing sample being a
// little larger than the real offer.
const outputReserve = 2048 + 1024

// A child log directory that is a symlink refuses the hand-out: the stderr
// file would land, and be read, outside the state dir.
func TestSymlinkedChildLogDirRefusesTheHandOut(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	h, run := errLogHarness(t, state)
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(state, "child-logs")); err != nil {
		t.Fatal(err)
	}
	if ins := quoteHandOut(t, h, run, "pi"); len(ins) != 0 {
		t.Fatalf("a pi hand-out through a symlinked child-logs was offered: %+v", ins)
	}
	if n := len(h.kind(run, wf.JournalHandedOut)); n != 0 {
		t.Fatalf("%d handed_out records for a refused hand-out", n)
	}
	rs := refusedRecords(t, h, run)
	if len(rs) != 1 || !rs[0].Halt || len(rs[0].Reasons) != 1 || !strings.Contains(rs[0].Reasons[0], "not a real directory") || !strings.Contains(rs[0].Reasons[0], filepath.Join(state, "child-logs")) {
		t.Fatalf("refused records = %+v, want one halt naming the child-logs path", rs)
	}
}

// The re-offer quotes no file a symlink points at, and escapes the control
// characters of the lines it quotes.
func TestNotStartedExcerptFollowsNoLinkAndEscapes(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	h, run := errLogHarness(t, state)
	first := quoteHandOut(t, h, run, "pi")
	if len(first) != 1 {
		t.Fatalf("first offer = %+v", first)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, first[0].ErrLog); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(10 * time.Minute)
	again := quoteHandOut(t, h, run, "pi")
	if len(again) != 1 || !again[0].NotStarted || again[0].PrevErrLog != first[0].ErrLog || len(again[0].PrevErrTail) != 0 {
		t.Fatalf("re-offer over a symlinked stderr file = %+v, want no excerpt", again)
	}
	if out := piRender(again, "pi", false); strings.Contains(out, "secret line") {
		t.Errorf("the re-offer quotes the symlink's target:\n%s", out)
	}

	if err := os.WriteFile(again[0].ErrLog, []byte("\x1b]0;title\x07\x1b[2Jcleared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(10 * time.Minute)
	third := quoteHandOut(t, h, run, "pi")
	if len(third) != 1 || third[0].PrevErrLog != again[0].ErrLog {
		t.Fatalf("third offer = %+v", third)
	}
	out := piRender(third, "pi", false)
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("the re-offer holds a raw control character:\n%q", out)
	}
	if want := `  \x1b]0;title\x07\x1b[2Jcleared` + "\n"; !strings.Contains(out, want) {
		t.Errorf("the re-offer does not hold %q:\n%s", want, out)
	}
	// Rendering escapes a tail that reached it raw, too.
	third[0].PrevErrTail = []string{"raw\x1bline"}
	if out := piRender(third, "pi", false); !strings.Contains(out, `  raw\x1bline`+"\n") {
		t.Errorf("a raw tail line was not escaped when rendered:\n%s", out)
	}
}
