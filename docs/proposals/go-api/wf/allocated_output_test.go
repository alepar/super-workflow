package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Exercise allocation and completion through persisted CLI state with actual
// mapped identities, rather than calling the path encoder alone.
func TestAllocatedOutputLongMapKeys(t *testing.T) {
	for _, key := range []string{strings.Repeat(".", 86), strings.Repeat("界", 30)} {
		t.Run(key, func(t *testing.T) {
			e := newCLIEnv(t)
			d := wf.New("long-allocated")
			keys := []string{key, key + "x"}
			m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(0, 1), func(i chItem) string { return keys[i.N] }, chChild())
			ok := wf.NewTerminal[chMerged](d, wf.Success)
			bad := wf.NewTerminal[chPartial](d, wf.Stopped)
			m.Parallel(2).Join(wf.AllSettled(), m.To(ok, "met"), m.To(bad, "partial"))
			cfg := clConfig()
			cfg.Defs = append(cfg.Defs, wf.Registration{Def: d})
			call := func(args ...string) (int, string, string) {
				var out, er bytes.Buffer
				code := fixtureCLI(cfg, args, &out, &er)
				return code, out.String(), er.String()
			}
			must := func(args ...string) string {
				t.Helper()
				code, out, er := call(args...)
				if code != 0 {
					t.Fatalf("%v exited %d: %s", args, code, er)
				}
				return out
			}
			run := wf.RunID(strings.TrimSpace(must("run", d.Name(), "--run-id", "long", "--state-dir", e.state, "--workspace", e.ws)))
			var ins []wf.Instruction
			if err := json.Unmarshal([]byte(must("next", string(run), "2", "--state-dir", e.state, "--json")), &ins); err != nil {
				t.Fatal(err)
			}
			if len(ins) != 2 {
				t.Fatalf("handed out %d mapped units: %+v", len(ins), ins)
			}
			if ins[0].Output == ins[1].Output {
				t.Fatal("distinct long keys share an output")
			}
			for i, in := range ins {
				if in.Name != "chFan/"+keys[i]+"/chWork#1" {
					t.Fatalf("logical name changed: %q", in.Name)
				}
				must("dev", "started", string(run), in.Name, "--session", "s", "--agent", "a", "--state-dir", e.state)
				writeAllocated(t, in.Output, `{"OK":true}`)
				prompt := withBody(t, must("prompt", string(run), in.Name, "--state-dir", e.state))
				if !strings.Contains(prompt, in.Output) {
					t.Fatalf("resume changed output: %s", prompt)
				}
				if i == 0 {
					must("heartbeat", string(run), "died", in.Name, "--state-dir", e.state)
					var reissued []wf.Instruction
					if err := json.Unmarshal([]byte(must("next", string(run), "1", "--state-dir", e.state, "--json")), &reissued); err != nil {
						t.Fatal(err)
					}
					if len(reissued) != 1 || reissued[0].Name != "chFan/"+key+"/chWork#2" || reissued[0].Output == in.Output || reissued[0].Output == ins[1].Output {
						t.Fatalf("reissue lost identity: %+v", reissued)
					}
					in = reissued[0]
					must("dev", "started", string(run), in.Name, "--session", "s", "--agent", "b", "--state-dir", e.state)
					if code, _, er := call("validate", string(run), in.Name, "--state-dir", e.state); code != 5 || !strings.Contains(er, "no output file at "+in.Output) {
						t.Fatalf("reissue consumed old result: %d %s", code, er)
					}
					writeAllocated(t, in.Output, `{"OK":true}`)
				}
				must("done", string(run), in.Name, "--state-dir", e.state)
			}
			var next []wf.Instruction
			code, nextOut, er := call("next", string(run), "1", "--state-dir", e.state, "--json")
			if code != 0 && code != 7 {
				t.Fatalf("next exited %d: %s", code, er)
			}
			if err := json.Unmarshal([]byte(nextOut), &next); err != nil {
				t.Fatal(err)
			}
			if next = withoutEnded(next); len(next) != 0 {
				t.Fatalf("completed units redispatched: %+v", next)
			}
		})
	}
}

func writeAllocated(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func allocatedNext(t *testing.T, e *cliEnv, run wf.RunID) wf.Instruction {
	t.Helper()
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(e.must("next", string(run), "1", "--state-dir", e.state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("next: %v: %+v", err, ins)
	}
	return ins[0]
}

func TestAllocatedOutputSurvivesResumeAndReissue(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	first := allocatedNext(t, e, run)
	e.must("dev", "started", string(run), first.Name, "--session", "s", "--agent", "a", "--state-dir", e.state)
	writeAllocated(t, first.Output, `{"OK":true}`)
	for i := 0; i < 2; i++ {
		// Every CLI call builds a fresh engine from the persisted journal.
		prompt := withBody(t, e.must("prompt", string(run), first.Name, "--state-dir", e.state))
		if !strings.Contains(prompt, first.Output) {
			t.Fatalf("resumed prompt changed output: %s", prompt)
		}
	}
	e.must("heartbeat", string(run), "died", first.Name, "--state-dir", e.state)
	second := allocatedNext(t, e, run)
	if second.Output == first.Output || second.Name == first.Name {
		t.Fatalf("replacement reused predecessor: %+v", second)
	}
	e.must("dev", "started", string(run), second.Name, "--session", "s", "--agent", "b", "--state-dir", e.state)
	code, _, er := e.cli("validate", string(run), second.Name, "--state-dir", e.state)
	if code != 5 || !strings.Contains(er, "no output file at "+second.Output) {
		t.Fatalf("replacement consumed predecessor output: %d %s", code, er)
	}
	if code, _, er := e.cli("done", string(run), first.Name, "--state-dir", e.state); code != 4 {
		t.Fatalf("retired result: %d %s", code, er)
	}
	writeAllocated(t, second.Output, `{"OK":true}`)
	// A lexically normalized spelling of the same allocated file is accepted.
	e.must("done", string(run), second.Name, "--output", filepath.Dir(second.Output)+"/./"+filepath.Base(second.Output), "--state-dir", e.state)
}

func TestAllocatedUnitOutputPath(t *testing.T) {
	e := newCLIEnv(t)
	e.state = filepath.Join(e.state, "state with spaces")
	run := e.start("agent-cli")
	in := allocatedNext(t, e, run)
	p := in.Output
	root := filepath.Join(e.state, "runs", string(run), "out")
	if !filepath.IsAbs(p) || !strings.HasPrefix(p, root+string(filepath.Separator)) {
		t.Fatalf("output = %q, want absolute path under %s", p, root)
	}
	if _, err := os.Stat(filepath.Dir(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("placeholder output: %v", err)
	}
	firstPrompt := withBody(t, e.must("prompt", string(run), in.Name, "--state-dir", e.state))
	if !strings.Contains(firstPrompt, "Write your result as JSON to this allocated output file: "+p) {
		t.Errorf("first prompt lacks allocated output wording: %s", firstPrompt)
	}
	if !strings.Contains(firstPrompt, "Then run this exact command:\n") || strings.Contains(firstPrompt, "--output") || strings.Contains(firstPrompt, "outside any other git worktree") {
		t.Errorf("first prompt has incompatible completion instructions: %s", firstPrompt)
	}
	// The fetch above claimed the unit for this environment's harness (A1).
	e.must("dev", "started", string(run), in.Name, "--harness", "claude", "--session", "s", "--agent", "a", "--state-dir", e.state)
	prompt := withBody(t, e.must("prompt", string(run), in.Name, "--state-dir", e.state))
	if !strings.Contains(prompt, p) {
		t.Fatalf("prompt lacks allocated output: %s", prompt)
	}
	if err := os.WriteFile(p, []byte(`{"OK":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.must("done", string(run), in.Name, "--state-dir", e.state)
}

// A wrong --output is never read. While nothing is at the allocated path it
// is the missing-output mistake (D2): validate's refusal names the allocated
// path and records nothing; once the allocated file exists it is a
// usage error.
func TestDoneRejectsUnrelatedOutputBeforeRead(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	in := allocatedNext(t, e, run)
	e.must("dev", "started", string(run), in.Name, "--harness", "test", "--session", "s", "--agent", "a", "--state-dir", e.state)
	unrelated := writeAllocated(t, filepath.Join(e.ws, "elsewhere.json"), `{"OK":true}`)
	for _, output := range []string{"-", unrelated} {
		code, _, er := e.cli("validate", string(run), in.Name, "--output", output, "--state-dir", e.state)
		if code != 5 || !strings.Contains(er, "no output file at "+in.Output) {
			t.Fatalf("%q with no allocated output: exit %d: %s", output, code, er)
		}
	}
	writeAllocated(t, in.Output, `{"OK":true}`)
	for _, output := range []string{"-", unrelated, filepath.Join(e.ws, "absent")} {
		code, _, er := e.cli("done", string(run), in.Name, "--output", output, "--state-dir", e.state)
		if code != 2 || !strings.Contains(er, "must name allocated output") {
			t.Fatalf("%q: exit %d: %s", output, code, er)
		}
	}
	code, _, er := e.cli("done", string(run), "unknown", "--output", filepath.Join(e.ws, "absent"), "--state-dir", e.state)
	if code != 4 || !strings.Contains(er, "no unit") {
		t.Fatalf("exit %d: %s", code, er)
	}
}

func TestDoneReadsAllocatedOutput(t *testing.T) {
	e := newCLIEnv(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	e.state, err = filepath.Rel(cwd, filepath.Join(e.state, "relative state with spaces"))
	if err != nil {
		t.Fatal(err)
	}
	run := e.start("agent-cli")
	in := allocatedNext(t, e, run)
	if !filepath.IsAbs(in.Output) {
		t.Fatalf("relative output: %s", in.Output)
	}
	e.must("dev", "started", string(run), in.Name, "--session", "s", "--agent", "a", "--state-dir", e.state)
	// A directory at the file path is unreadable as a result even as root.
	if err := os.Mkdir(in.Output, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, er := e.cli("done", string(run), in.Name, "--state-dir", e.state); code != 2 || !strings.Contains(er, "reading the output") {
		t.Fatalf("unreadable result: %d %s", code, er)
	}
	if err := os.Remove(in.Output); err != nil {
		t.Fatal(err)
	}
	writeAllocated(t, in.Output, `malformed JSON`)
	// The refusal spends the attempt (C5).
	if code, _, er := e.cli("done", string(run), in.Name, "--state-dir", e.state); code != 5 || !strings.Contains(er, "this attempt is spent") {
		t.Fatalf("malformed result: %d %s", code, er)
	}
	replacement := allocatedNext(t, e, run)
	if replacement.Output == in.Output {
		t.Fatal("refusal reused result path")
	}
	e.must("dev", "started", string(run), replacement.Name, "--session", "s", "--agent", "b", "--state-dir", e.state)
	writeAllocated(t, replacement.Output, `{"OK":true}`)
	e.must("done", string(run), replacement.Name, "--state-dir", e.state)
}

// Exercise the CLI's FileJournal with a Map inside a revisited Sub. Names and
// output files must originate in the engine, including the child agent revisit.
type eiHop struct{}

func TestAllocatedNestedMappedOutput(t *testing.T) {
	keys := []string{"..", "%2E%2E", ".", "%2E", `..\work`, "a/b", "a%2Fb", "", "%EMPTY", "a b", "雪", "[x]#1"}
	child := wf.New("allocated-item")
	a := wf.NewAgent[eiLoop, clAnswer](child)
	end := wf.NewTerminal[eiEnd](child, wf.Success)
	child.OnFailure(wf.Blocked, end)
	a.Spec(wf.PromptText("Again?"), wf.Retry[eiLoop](wf.Const(2), a.To(end, "gave up")))
	// An agent has no edge to itself in a served definition: the revisit goes
	// through a compute hop.
	hop := wf.NewCompute[eiHop, struct{}](child, func(wf.Ctx) (struct{}, error) { return struct{}{}, nil })
	back := hop.To(a, "back")
	hop.Route(func(struct{}, wf.Ctx) wf.Edge[eiHop] { return back }, back)
	again, done := a.To(hop, "again"), a.To(end, "done")
	a.Route(func(o clAnswer, _ wf.Ctx) wf.Edge[eiLoop] {
		if o.OK {
			return again
		}
		return done
	}, again, done)
	wf.Yield(end, func(wf.Ctx) chRes { return chRes{} })
	mapped := wf.New("allocated-map")
	m := wf.NewMap[chFan, string, string, chRes](mapped, wf.FromValues(keys...), func(k string) string { return k }, wf.Typed[string, chRes](child))
	merged := wf.NewTerminal[chMerged](mapped, wf.Success)
	partial := wf.NewTerminal[chPartial](mapped, wf.Stopped)
	m.Parallel(1).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	wf.Yield(merged, func(wf.Ctx) chRes { return chRes{} })
	wf.Yield(partial, func(wf.Ctx) chRes { return chRes{} })
	def := wf.New("allocated-nested")
	sub := wf.NewSub[chSubRun, chItem, chRes](def, wf.Typed[chItem, chRes](mapped), func(wf.Ctx) chItem { return chItem{} })
	fin := wf.NewTerminal[chFin](def, wf.Success)
	wf.MaxVisits(wf.Const(2), sub.To(fin, "cap"))
	sub.Exits(wf.OnExit[chMerged](sub.To(sub, "again")), wf.OnExit[chPartial](sub.To(fin, "partial")))
	d := newDrv(t, def)
	d.start("nested")
	seen := map[string]string{}
	next := func() wf.Instruction {
		t.Helper()
		ins := d.handOut("1")
		if len(ins) != 1 {
			t.Fatalf("next = %+v, want one nested mapped unit", ins)
		}
		in := ins[0]
		if !strings.Contains(in.Child, "chSubRun#") || !strings.Contains(in.Child, "chFan[") {
			t.Fatalf("unit is not inside Map inside Sub: %+v", in)
		}
		root := filepath.Join(d.state, "runs", "nested", "out")
		rel, err := filepath.Rel(root, in.Output)
		if err != nil || !filepath.IsAbs(in.Output) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Fatalf("output escaped %s: %+v (%v)", root, in, err)
		}
		if old, ok := seen[in.Output]; ok {
			t.Fatalf("%s aliases %s at %s", in.Name, old, in.Output)
		}
		seen[in.Output] = in.Name
		if _, err := os.Stat(in.Output); !os.IsNotExist(err) {
			t.Fatalf("placeholder: %v", err)
		}
		for i := 0; i < 2; i++ {
			if prompt := d.must("prompt", in.Name); !strings.Contains(prompt, in.Output) {
				t.Fatalf("reread changed allocated path: %s", prompt)
			}
		}
		d.must("dev", "started", in.Name, "--session", "s", "--agent", in.Name)
		return in
	}
	rejectStale := func(old wf.Instruction) {
		t.Helper()
		// A directory is an unreadable result even as root. The retired-name
		// error must win over the read error, and no submission may be journaled.
		if err := os.Rename(old.Output, old.Output+".retained"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(old.Output, 0o755); err != nil {
			t.Fatal(err)
		}
		before := d.must("journal")
		code, _, er := d.wf("done", old.Name)
		if code != 4 || !strings.Contains(er, "retired") {
			t.Fatalf("stale completion = %d %s, want retired before read", code, er)
		}
		if after := d.must("journal"); after != before {
			t.Fatal("stale completion mutated journal")
		}
		if raw, err := os.ReadFile(old.Output + ".retained"); err != nil || string(raw) != `{"OK":true}` {
			t.Fatalf("predecessor result was not retained: %q, %v", raw, err)
		}
	}
	for visit := 0; visit < 2; visit++ {
		for range keys {
			first := next()
			if visit == 0 && len(seen) == 1 {
				writeAllocated(t, first.Output, `{"OK":true}`)
				d.must("heartbeat", "died", first.Name)
				replacement := next()
				if replacement.Name == first.Name || replacement.Output == first.Output || replacement.Attempt != first.Attempt+1 {
					t.Fatalf("loss reused identity: %+v -> %+v", first, replacement)
				}
				rejectStale(first)
				first = replacement
			}
			writeAllocated(t, first.Output, `{"OK":true}`)
			d.must("done", first.Name)
			second := next()
			if second.Name == first.Name || second.Output == first.Output {
				t.Fatalf("revisit reused identity: %+v -> %+v", first, second)
			}
			rejectStale(first)
			// The active name cannot be paired with the predecessor's file.
			if code, _, er := d.wf("validate", second.Name, "--output", first.Output); code != 5 || !strings.Contains(er, "no output file at "+second.Output) {
				t.Fatalf("stale path = %d %s", code, er)
			}
			writeAllocated(t, second.Output, `{"OK":false}`)
			d.must("done", second.Name)
		}
	}
	if st := d.status(); st.Phase != wf.RunEnded || st.Terminal != "chFin" || st.Outcome != wf.Success {
		t.Fatalf("final status = %+v", st)
	}
}

// D2: the first done of an attempt with nothing at the allocated output is
// refused with exit 5 naming the path and what to write, and the attempt
// stays open: one submission_refused is journaled, no refused, and the same
// done succeeds once the file is written. validate names the same path and
// records nothing.
func TestDoneMissingOutputIsRefusal(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	in := allocatedNext(t, e, run)
	e.must("dev", "started", string(run), in.Name, "--session", "s", "--agent", "a", "--state-dir", e.state)
	e.must("prompt", string(run), in.Name, "--state-dir", e.state)
	if code, _, er := e.cli("validate", string(run), in.Name, "--state-dir", e.state); code != 5 || !strings.Contains(er, "validate: refused: no output file at "+in.Output) {
		t.Fatalf("validate with no output = %d %s, want exit 5 naming the path", code, er)
	}
	before := e.must("journal", string(run), "--state-dir", e.state)
	code, _, er := e.cli("done", string(run), in.Name, "--state-dir", e.state)
	if code != 5 || !strings.Contains(er, "done: refused: no output file at "+in.Output) ||
		!strings.Contains(er, "done: your attempt is still open: write your result JSON to that file, then run done again") ||
		strings.Contains(er, "this attempt is spent") {
		t.Fatalf("done with no output = %d %s, want exit 5 naming the path with the attempt open", code, er)
	}
	after := e.must("journal", string(run), "--state-dir", e.state)
	if added := strings.TrimPrefix(after, before); strings.Count(added, `"submission_refused"`) != 1 || strings.Count(added, "\n") != 1 {
		t.Fatalf("the missing-output refusal journaled, want one submission_refused and nothing else:\n%s", added)
	}
	writeAllocated(t, in.Output, `{"OK":true}`)
	var res wf.SubmitResult
	if code, o, er := e.cli("done", string(run), in.Name, "--state-dir", e.state); code != 0 ||
		json.Unmarshal([]byte(o), &res) != nil || res.Class != wf.Accepted {
		t.Fatalf("the second done, with the output written = %d %q %s, want accepted on the same attempt", code, o, er)
	}
}

// A child that keeps running done without writing its output is bounded by
// the run, whatever its transcript shows: the first missing-output refusal
// leaves the attempt open, the second on the same attempt spends it, and the
// state takes its retry route. With the retry budget spent the unit is
// offered for recovery as retry exhausted, and giving up takes the state's
// exhausted edge.
func TestRepeatedMissingOutputSpendsTheAttempt(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	for attempt := 1; attempt <= 2; attempt++ {
		in := allocatedNext(t, e, run)
		if in.Kind != wf.InstructionAgent || in.Attempt != attempt {
			t.Fatalf("next = %+v, want attempt %d", in, attempt)
		}
		e.must("dev", "started", string(run), in.Name, "--session", "s", "--agent", "a", "--state-dir", e.state)
		e.must("prompt", string(run), in.Name, "--state-dir", e.state)
		code, _, er := e.cli("done", string(run), in.Name, "--state-dir", e.state)
		if code != 5 || !strings.Contains(er, "your attempt is still open") || strings.Contains(er, "this attempt is spent") {
			t.Fatalf("attempt %d, first done with no output = %d %s, want exit 5 with the attempt open", attempt, code, er)
		}
		code, o, er := e.cli("done", string(run), in.Name, "--state-dir", e.state)
		var res wf.SubmitResult
		if code != 5 || !strings.Contains(er, "done: refused: no output file at "+in.Output) || !strings.Contains(er, "this attempt is spent") ||
			strings.Contains(er, "your attempt is still open") || json.Unmarshal([]byte(o), &res) != nil || res.Class != wf.Refused {
			t.Fatalf("attempt %d, second done with no output = %d %q %s, want exit 5 with the attempt spent", attempt, code, o, er)
		}
		if code, _, _ := e.cli("done", string(run), in.Name, "--state-dir", e.state); code == 0 || code == 5 {
			t.Fatalf("attempt %d, a third done on the spent attempt exited %d, want it rejected", attempt, code)
		}
	}
	offer := allocatedNext(t, e, run)
	if offer.Kind != wf.InstructionRecovery || !strings.Contains(offer.Prompt, "retry exhausted: no output file at ") {
		t.Fatalf("after two spent attempts next = %+v, want the retry-exhausted recovery offer", offer)
	}
	e.must("recover", string(run), offer.Name, "give-up", "--state-dir", e.state)
	if j := e.must("journal", string(run), "--state-dir", e.state); !strings.Contains(j, `"Terminal":"clGaveUp"`) {
		t.Fatalf("giving up did not take the exhausted edge:\n%s", j)
	}
}

// A journal that holds missing-output refusals replays against the same
// definition with no drift: a first missing-output done belongs to its own
// attempt and changes no other attempt's recorded action, and an attempt
// spent by two missing-output dones is replayed as spent.
func TestMissingOutputJournalsReplayClean(t *testing.T) {
	for _, c := range []string{"wrong type, then missing, then ok", "missing, then ok", "missing twice, then a retry ok"} {
		t.Run(c, func(t *testing.T) {
			e := newCLIEnv(t)
			run := e.start("agent-cli")
			begin := func(attempt int) wf.Instruction {
				in := allocatedNext(t, e, run)
				if in.Kind != wf.InstructionAgent || in.Attempt != attempt {
					t.Fatalf("next = %+v, want attempt %d", in, attempt)
				}
				e.must("dev", "started", string(run), in.Name, "--session", "s", "--agent", "a", "--state-dir", e.state)
				e.must("prompt", string(run), in.Name, "--state-dir", e.state)
				return in
			}
			done := func(in wf.Instruction, want int) {
				if code, _, er := e.cli("done", string(run), in.Name, "--state-dir", e.state); code != want {
					t.Fatalf("done %s = %d %s, want exit %d", in.Name, code, er, want)
				}
			}
			in := begin(1)
			switch c {
			case "wrong type, then missing, then ok":
				writeAllocated(t, in.Output, `{"OK":"wrong"}`)
				done(in, 5) // refused: the attempt is spent
				in = begin(2)
				done(in, 5) // no file: the attempt stays open
			case "missing, then ok":
				done(in, 5)
			case "missing twice, then a retry ok":
				done(in, 5)
				done(in, 5) // the attempt is spent
				in = begin(2)
			}
			writeAllocated(t, in.Output, `{"OK":true}`)
			done(in, 0)
			e.next(run)
			rep := e.must("dev", "replay", string(run), "--state-dir", e.state)
			for _, want := range []string{"; 0 differ\n", "\n0 stale recordings", "replay stopped (ended)"} {
				if !strings.Contains(rep, want) {
					t.Fatalf("replay against the same definition lacks %q:\n%s", want, rep)
				}
			}
		})
	}
}
