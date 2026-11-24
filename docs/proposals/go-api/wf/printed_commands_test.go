package wf_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// cmdRecorder builds the engine's command lines as the named CLI does (an
// absolute binary and state dir, both needing quotes) and records each one,
// so a printed text can be held to them: a line naming the binary is one
// recorded command, whole, after the run root's cd.
type cmdRecorder struct {
	bin, state, ws string
	mu             sync.Mutex
	built          map[string]bool
}

func newCmdRecorder(t *testing.T) *cmdRecorder {
	root := t.TempDir()
	r := &cmdRecorder{bin: filepath.Join(root, "bin dir", "super"), state: filepath.Join(root, "state dir"), ws: filepath.Join(root, "work space"), built: map[string]bool{}}
	if err := os.MkdirAll(r.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *cmdRecorder) line(run wf.RunID, verb string, args ...string) string {
	s := wf.WfLine(r.bin, r.state, run, verb, args...)
	r.mu.Lock()
	r.built[s] = true
	r.mu.Unlock()
	return s
}

// l is a printed command as next's renderer receives it (resultLines).
func (r *cmdRecorder) l(run wf.RunID) func(string, ...string) string {
	return func(verb string, args ...string) string { return wf.WorkspaceCommand(r.ws, r.line(run, verb, args...)) }
}

func (r *cmdRecorder) opts(o wf.EngineOptions) wf.EngineOptions {
	o.Workspace = r.ws
	o.VerbLine = r.line
	o.SubmitLine = func(run wf.RunID, token string) string {
		return r.line(run, "submit", "--token", token, "--output", "<file>")
	}
	o.AnswerLine = func(run wf.RunID, gate string) string {
		return r.line(run, "answer", "--gate", gate, "--value", "<value>")
	}
	o.SettleLine = func(run wf.RunID, effect int64) string {
		return r.line(run, "settle", "--effect", fmt.Sprint(effect), "<settlement>")
	}
	o.ResumeLine = func(started wf.RunStartedData) string { return r.line("", "run", started.Def) }
	return o
}

// gateValue and gateVisit are what an answer command adds to the line the
// engine built: its <value> filled, the gate's instance and visit, and for a
// numeric answer its --number slot.
var (
	gateValue = regexp.MustCompile(`--value ('[^']*'|[^ ]+)`)
	gateVisit = regexp.MustCompile(` --child ('[^']*'|[^ ]+) --visit [0-9]+( --number <integer>)?$`)
)

// check holds every line of text that names the binary to one recorded
// command, whole: indentation, then "cd <run root> && ", then the command
// and nothing else. exempt lines (a harness call carrying a command as its
// message) are skipped.
func (r *cmdRecorder) check(t *testing.T, where, text string, exempt ...string) int {
	t.Helper()
	prefix := wf.WorkspaceCommand(r.ws, "")
	seen := 0
	for _, raw := range strings.Split(text, "\n") {
		if !strings.Contains(raw, r.bin) && !strings.Contains(raw, strings.ReplaceAll(r.bin, " ", "\\ ")) {
			continue
		}
		line := strings.TrimLeft(raw, " ")
		skip := false
		for _, e := range exempt {
			skip = skip || line == strings.TrimLeft(e, " ")
		}
		if skip {
			continue
		}
		seen++
		cmd, ok := strings.CutPrefix(line, "cd "+quoteLike(r.ws)+" && ")
		if !ok {
			t.Errorf("%s: a line names the binary but is not \"cd <run root> && <command>\" alone: %q", where, raw)
			continue
		}
		_ = prefix
		r.mu.Lock()
		whole := r.built[cmd] || r.built[gateValue.ReplaceAllString(gateVisit.ReplaceAllString(cmd, ""), "--value <value>")]
		r.mu.Unlock()
		if !whole {
			t.Errorf("%s: a printed line is not exactly one command the engine built (prose before or after it, or two on a line): %q", where, raw)
		}
	}
	return seen
}

// quoteLike is how the engine quotes the run root (shellQuote).
func quoteLike(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(wf.WorkspaceCommand(s, "x"), "cd "), " && x")
}

func recordedHarness(t *testing.T, r *cmdRecorder, d *wf.Def, o wf.EngineOptions) *harness {
	t.Helper()
	o.RunDir = func(run wf.RunID) string { return filepath.Join(r.state, "runs", string(run)) }
	return newHarnessOpts(t, d, nil, r.opts(o))
}

// Every path that prints a command for an agent prints it alone on its line:
// next with its offers and briefs, a child's prompt page and body, status,
// the ended run's resume, paged delivery, a recovery, a reserved and a
// person's gate, a blocked run, a stalled child and a run
// waiting on its holds.
func TestEveryPrintedCommandIsAWholeLine(t *testing.T) {
	co := wf.Coordinator{Harness: "claude", Session: "sess"}
	t.Run("unit", func(t *testing.T) {
		r := newCmdRecorder(t)
		d := sizedAgentDef("pc-unit", "Do the task.")
		d.Describe("read the repo and write the answer")
		h := recordedHarness(t, r, d, wf.EngineOptions{})
		run := h.run(d, nil)
		e := h.e.(*wf.Engine)
		nr, err := e.NextWait(run, 1, co, 0)
		if err != nil || len(nr.Instructions) != 1 {
			t.Fatalf("next = %+v, %v", nr, err)
		}
		in := nr.Instructions[0]
		seen := r.check(t, "next", wf.RenderNext(nr, "claude", r.l(run)))
		f, err := e.PromptFetch(run, in.Name, claimToken(t, in))
		if err != nil {
			t.Fatal(err)
		}
		seen += r.check(t, "prompt page", f.Delivered)
		seen += r.check(t, "prompt body", f.Prompt)
		seen += r.check(t, "status", wf.RenderStatus(h.status(run)))
		res, err := e.Done(run, in.Name, wf.Submission{Output: []byte(`{"OK":true}`)})
		if err != nil {
			t.Fatalf("submit = %+v, %v", res, err)
		}
		end, err := e.NextWait(run, 1, co, 0)
		if err != nil {
			t.Fatal(err)
		}
		ended := wf.RenderNext(end, "claude", r.l(run))
		if !strings.Contains(ended, "To resume this run later") {
			t.Fatalf("the ended run prints no resume command:\n%s\nsubmit: %+v", ended, res)
		}
		seen += r.check(t, "ended", ended)
		seen += r.check(t, "ended status", wf.RenderStatus(h.status(run)))
		if seen < 8 {
			t.Fatalf("only %d command lines checked", seen)
		}
	})
	t.Run("paged", func(t *testing.T) {
		r := newCmdRecorder(t)
		d := sizedAgentDef("pc-paged", "First constraint.\n"+strings.Repeat("long instruction. ", 600)+"\nLast constraint.")
		h := recordedHarness(t, r, d, wf.EngineOptions{PagedPrompts: true})
		run, ins := handOneUnit(t, h, d)
		if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
			t.Fatal(err)
		}
		e := h.e.(*wf.Engine)
		page, err := e.PromptPage(run, ins.Name, "", hn.Caller{}, wf.PromptPageRequest{Page: 1})
		if err != nil {
			t.Fatal(err)
		}
		seen := r.check(t, "page 1", page.Delivered)
		for n := 2; n <= page.Pages; n++ {
			h.now = h.now.Add(time.Second)
			if page, err = e.PromptPage(run, ins.Name, "", hn.Caller{}, wf.PromptPageRequest{Page: n, Code: page.NextCode}); err != nil {
				t.Fatal(err)
			}
			seen += r.check(t, fmt.Sprintf("page %d", n), page.Delivered)
			if strings.Contains(page.Delivered, " \\\n") {
				t.Errorf("page %d continues a command with a backslash", n)
			}
		}
		if seen < 4 {
			t.Fatalf("only %d command lines checked across %d pages", seen, page.Pages)
		}
	})
	t.Run("recovery", func(t *testing.T) {
		r := newCmdRecorder(t)
		d := clAgentDef("pc-recovery")
		h := recordedHarness(t, r, d, wf.EngineOptions{})
		h.manualRecovery = true
		run, _, again := endedChild(t, h, d, "claude-handback.jsonl")
		if len(again) != 1 || again[0].Recovery == nil {
			t.Fatalf("no recovery offer: %+v", again)
		}
		v := again[0].Recovery
		text := wf.RenderNext(wf.NextResult{Instructions: again}, "claude", r.l(run))
		for _, want := range []string{v.Commands[wf.RecoveryResume], v.Commands[wf.RecoveryRestart], v.Commands[wf.RecoveryGiveUp]} {
			if !strings.Contains(text, "\n  "+want+"\n") && !strings.Contains(text, "\n    "+want+"\n") {
				t.Errorf("recovery does not print %q on its own line:\n%s", want, text)
			}
		}
		// The resume answer's harness call carries the child's prompt
		// command as its message: one tool call, not a shell line.
		r.check(t, "recovery", text, v.Resume)
		r.check(t, "recovery status", wf.RenderStatus(h.status(run)))
	})
	t.Run("gates", func(t *testing.T) {
		for _, res := range []wf.Reservation{wf.Reserved, wf.PolicyAnswerable} {
			r := newCmdRecorder(t)
			d := wf.New(fmt.Sprintf("pc-gate-%d", res))
			ans := wf.NewEnum(d, "pc-ans", "a b'c", "no")
			g := wf.NewGate[clGate](d, "Proceed?", ans, res)
			yes := wf.NewTerminal[clYes](d, wf.Success)
			no := wf.NewTerminal[clNoEnd](d, wf.Stopped)
			g.Answers(wf.On("a b'c", g.To(yes, "yes")), wf.On("no", g.To(no, "no")))
			h := recordedHarness(t, r, d, wf.EngineOptions{})
			run := h.run(d, nil)
			nr, err := h.e.(*wf.Engine).NextWait(run, 1, co, 0)
			if err != nil {
				t.Fatal(err)
			}
			if n := r.check(t, fmt.Sprintf("gate %d", res), wf.RenderNext(nr, "claude", r.l(run))); n != 1 {
				t.Errorf("gate %d: %d answer command lines, want 1:\n%s", res, n, wf.RenderNext(nr, "claude", r.l(run)))
			}
		}
	})
	t.Run("blocked stalled waiting", func(t *testing.T) {
		r := newCmdRecorder(t)
		const run = wf.RunID("pc-held")
		l := r.l(run)
		nr := wf.NextResult{
			Blocked: &wf.BlockedView{State: "Work", Child: "c1", Reason: "refused"},
			Stalled: []wf.StalledView{{Name: "Work#1", Since: time.Unix(0, 0)}},
			Instructions: []wf.Instruction{
				{Kind: wf.InstructionWaiting, Prompt: "The run is waiting on a human.\nheld: Work#3 (Work): cause\n  clear the cause, then run:\n  " + l("retry", "--child", "c3")},
			},
		}
		if n := r.check(t, "blocked, stalled, waiting", wf.RenderNext(nr, "claude", l)); n != 5 {
			t.Errorf("%d command lines, want 5 (two retries, two heartbeats, one hold):\n%s", n, wf.RenderNext(nr, "claude", l))
		}
		st := wf.RunStatus{Run: run, Phase: wf.RunBlocked, Fix: []string{l("retry", "--child", "c1"), l("retry", "--child", "c1", "--stop")}, FixNotes: []string{"one more attempt", "its fallback edge: gave up"},
			Recoveries: []wf.RecoveryView{{Unit: "Work#1", Cause: "ended", Commands: map[string]string{wf.RecoveryRestart: l("recover", "Work#1", wf.RecoveryRestart), wf.RecoveryGiveUp: l("recover", "Work#1", wf.RecoveryGiveUp)}}}}
		if n := r.check(t, "status", wf.RenderStatus(st)); n != 4 {
			t.Errorf("%d status command lines, want 4:\n%s", n, wf.RenderStatus(st))
		}
		if !strings.Contains(wf.RenderStatus(st), "fix, its fallback edge: gave up:\n  "+st.Fix[1]+"\n") {
			t.Errorf("status does not name the fallback edge above its command:\n%s", wf.RenderStatus(st))
		}
	})
}
