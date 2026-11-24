package wf_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	wf "github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The coordinator documents are prose, so this file keeps them honest: every
// subcommand, flag, exit code and next output field they name must exist in
// the CLI's own tables and the Instruction type.

var coordinatorDocs = []string{"../COORDINATOR.md"}

// The engine identifies each child itself (plan section A): the claim at the
// prompt fetch, the per-harness lookup and started as
// a manual override. COORDINATOR.md states each rule and never asks
// the coordinator to report a child's identity.
func TestDocsEngineFindsChildren(t *testing.T) {
	docs := readDocs(t)
	text := norm(docs["../COORDINATOR.md"])
	for _, want := range []string{
		"a unit is claimed when its child's prompt fetch arrives with the claim token",
		"handed out is not claimed, and nothing expires before the claim",
		"the hand-out window (2 min) passed while no unit of the run is claimed",
		"a `next` from a newer coordinator session",
		"a late child from the first offer still claims with its own token",
		"another child already owns this unit; exit without working",
		"the engine identifies each child itself",
		"claude_code_session_id", "claude_config_dir", "~/.claude",
		"<root>/projects/*/<session>/subagents/agent-*.jsonl",
		"codex_thread_id` is its agent id", "codex_session_id` is the coordinator's session",
		"$codex_home/sessions/*/*/*/rollout-*-<thread id>.jsonl",
		"this is a child's command: start a child agent with the text between the lines next printed for it",
		"journaled with the session on every `handed_out` record",
		"no coordinator calls it in the loop", "keeps the spawn window", "--usage tokens=n",
		"never fabricate an identity or guess a transcript path",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("COORDINATOR.md lacks %q", want)
		}
	}
	// The removed claim guidance: hook fields, the session_meta lookup and
	// started after every spawn.
	for path, text := range docs {
		for _, gone := range []string{"session_meta", "subagentstart", "agent_transcript_path", "after each spawn", "--since"} {
			if strings.Contains(norm(text), gone) {
				t.Errorf("%s still names %q", path, gone)
			}
		}
	}
}

// norm folds whitespace and case, so a phrase matches across line breaks.
func norm(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func readDocs(t *testing.T) map[string]string {
	t.Helper()
	docs := map[string]string{}
	for _, p := range coordinatorDocs {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		docs[p] = string(b)
	}
	return docs
}

var (
	// subcommandRe matches a verb after the multi-definition binary's name
	// (`workflow next`) or after the single-registration binary's `wf` group
	// (`./super wf next`).
	subcommandRe = regexp.MustCompile(`\b(?:workflow|wf) ([A-Za-z][A-Za-z-]*)`)
	flagRe       = regexp.MustCompile(`--[a-z][a-z-]*`)
	exitRowRe    = regexp.MustCompile(`(?m)^\|\s*exit (\d+)\s*\|\s*([a-z-]+)\s*\|\s*([^|]*?)\s*\|`)
	exitMention  = regexp.MustCompile(`\bexit (\d+)\b`)
	backtickRe   = regexp.MustCompile("`([^`]+)`")
)

func TestDocsNameRealSubcommandsAndFlags(t *testing.T) {
	flagsOf := map[string]map[string]bool{}
	for _, s := range wf.Subcommands {
		set := map[string]bool{}
		for _, f := range s.Flags {
			for _, name := range flagRe.FindAllString(f, -1) {
				set[name] = true
			}
		}
		flagsOf[s.Name] = set
	}
	// `workflow dev <verb>`: the dev group's flags, all of its verbs.
	dev := map[string]bool{}
	for _, s := range wf.DevSubcommands {
		for _, f := range s.Flags {
			for _, name := range flagRe.FindAllString(f, -1) {
				dev[name] = true
			}
		}
	}
	flagsOf["dev"] = dev
	for path, text := range readDocs(t) {
		seen := map[string]bool{}
		for i, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "> ") {
				continue // a quote of the engine's own text (the rules for the run), not a command
			}
			locs := subcommandRe.FindAllStringSubmatchIndex(line, -1)
			for n, loc := range locs {
				sub := line[loc[2]:loc[3]]
				seen[sub] = true
				set, ok := flagsOf[sub]
				if !ok {
					t.Errorf("%s:%d: names `workflow %s`, which is not in wf.Subcommands", path, i+1, sub)
					continue
				}
				end := len(line)
				if n+1 < len(locs) {
					end = locs[n+1][0]
				}
				for _, f := range flagRe.FindAllString(line[loc[1]:end], -1) {
					if !set[f] {
						t.Errorf("%s:%d: `workflow %s` names %s, which that subcommand does not take", path, i+1, sub, f)
					}
				}
			}
		}
		// The loop's verbs and their answers, in the protocol document.
		for _, want := range []string{"next", "prompt", "heartbeat", "answer", "settle", "retry", "stop"} {
			if !seen[want] {
				t.Errorf("%s never names `workflow %s`", path, want)
			}
		}
		// The removed verbs are named only in prose, never as a command.
		for _, gone := range []string{"submit", "fail", "wait"} {
			if seen[gone] {
				t.Errorf("%s names the removed verb `workflow %s`", path, gone)
			}
		}
	}
}

func TestDocsExitCodesMatchTable(t *testing.T) {
	want := map[int]string{}
	for _, e := range wf.ExitCodes {
		want[e.Code] = e.Name
	}
	for path, text := range readDocs(t) {
		for _, m := range exitMention.FindAllStringSubmatch(text, -1) {
			n, _ := strconv.Atoi(m[1])
			if _, ok := want[n]; !ok {
				t.Errorf("%s names exit %d, which is not in wf.ExitCodes", path, n)
			}
		}
	}
	// COORDINATOR.md carries the whole table, one row per code.
	text := readDocs(t)["../COORDINATOR.md"]
	got := map[int]string{}
	gotMeaning := map[int]string{}
	for _, m := range exitRowRe.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[1])
		got[n] = m[2]
		gotMeaning[n] = m[3]
	}
	for _, e := range wf.ExitCodes {
		if gotMeaning[e.Code] != e.Meaning {
			t.Errorf("COORDINATOR.md exit %d meaning = %q, want wf.ExitCodes %q", e.Code, gotMeaning[e.Code], e.Meaning)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("COORDINATOR.md exit rows = %v, want wf.ExitCodes %v", got, want)
	}
}

// jsonTags returns the json tag names of a struct type.
func jsonTags(typ reflect.Type) map[string]bool {
	tags := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			tags[name] = true
		}
	}
	return tags
}

// nextOutputFields returns the field names under the "next output" heading of
// a document, up to the following heading: the backticked names that open a
// list item, before its first colon.
func nextOutputFields(text string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			in = strings.Contains(strings.ToLower(line), "next output")
			continue
		}
		if !in || !strings.HasPrefix(line, "- ") {
			continue
		}
		head, _, _ := strings.Cut(line, ":")
		for _, m := range backtickRe.FindAllStringSubmatch(head, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

func TestDocsNextFieldsExist(t *testing.T) {
	top := jsonTags(reflect.TypeOf(wf.Instruction{}))
	dispatch := jsonTags(reflect.TypeOf(wf.DispatchHints{}))
	// The JSON array is next without --wait, documented in COORDINATOR.md;
	// the prompt's loop never sees it.
	path := "../COORDINATOR.md"
	{
		fields := nextOutputFields(readDocs(t)[path])
		named := map[string]bool{}
		for _, f := range fields {
			named[f] = true
			if head, tail, dotted := strings.Cut(f, "."); dotted {
				if head != "dispatch" || !dispatch[tail] {
					t.Errorf("%s: next output field %q is not a dispatch hint field", path, f)
				}
				continue
			}
			if !top[f] {
				t.Errorf("%s: next output field %q is not a json tag of wf.Instruction", path, f)
			}
		}
		// Every field of the format is documented, so a new Instruction field
		// cannot ship unexplained.
		var missing []string
		for _, f := range fields {
			if head, _, dotted := strings.Cut(f, "."); dotted {
				named[head] = true
			}
		}
		for tag := range top {
			if !named[tag] {
				missing = append(missing, tag)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: next output section does not name %v", path, missing)
		}
		for tag := range dispatch {
			if !named["dispatch."+tag] {
				t.Errorf("%s: next output section does not name dispatch.%s", path, tag)
			}
		}
	}
}

func TestDocsCoverEveryInstructionKind(t *testing.T) {
	kinds := []wf.InstructionKind{wf.InstructionAgent, wf.InstructionHumanGate, wf.InstructionWait, wf.InstructionManualVerification, wf.InstructionWaiting, wf.InstructionStopped, wf.InstructionEnded}
	// COORDINATOR.md documents the JSON form; the prompt sees next --wait
	// lines, which TestEveryNextLineHasCoordinatorAction covers.
	path, text := "../COORDINATOR.md", readDocs(t)["../COORDINATOR.md"]
	{
		for _, k := range kinds {
			if !strings.Contains(text, "`"+string(k)+"`") {
				t.Errorf("%s never names the instruction kind `%s`", path, k)
			}
		}
		for _, class := range []wf.OutcomeClass{wf.Rejected, wf.Refused, wf.Blocked, wf.Unavailable, wf.Stale, wf.Uncertain} {
			if !strings.Contains(text, class.String()) {
				t.Errorf("%s never names the outcome class %s", path, class)
			}
		}
		for _, w := range []string{"parallel", "dispatch.model", "dispatch.effort", "dispatch.agentType", "retryAfterSeconds"} {
			if !strings.Contains(text, w) {
				t.Errorf("%s never mentions %s", path, w)
			}
		}
	}
}

func TestDocsWorkspaceSettleAndFail(t *testing.T) {
	for path, text := range readDocs(t) {
		for _, phrase := range []string{"workspace", "absolute", "workflow settle"} {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s lacks %q", path, phrase)
			}
		}
	}
}

func TestDocsStopOnBlockedRun(t *testing.T) {
	doc := strings.Join(strings.Fields(readDocs(t)["../COORDINATOR.md"]), " ")
	if !strings.Contains(doc, "the run is blocked: report it and stop") {
		t.Error("COORDINATOR.md no longer carries the blocked run stop rule")
	}
}

// The protocol document says that a manual-verification instruction's settle line
// takes a settlement and that a result file is the allocated one under out/;
// COORDINATOR.md also says done takes no --output.
func TestDocsSettleLineAndResultLocation(t *testing.T) {
	for path, text := range readDocs(t) {
		text = strings.Join(strings.Fields(text), " ")
		wants := []string{"<settlement>", "out/", "allocated"}
		if path == "../COORDINATOR.md" {
			wants = append(wants, "without `--output`")
		}
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
	}
}

// TestDocsRefusalMatchesEngine drives the engine's refusal path and holds
// COORDINATOR.md to what it does: a refusal spends the attempt (C5); with
// retry left the engine issues a fresh attempt; with the retry spent the state
// halts for a retry-or-stop answer and stop takes its failure route; either
// way the spent token is settled and a resubmit is rejected. (Submit is the
// library call behind done; the CLI verb is gone.)
func TestDocsRefusalMatchesEngine(t *testing.T) {
	d := outOfEnumDef(false) // Retry Const(2), exhausted edge to eC
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	first := h.next(run)[0]
	if res := h.submit(run, first, `{"Verdict":"maybe"}`); res.Class != wf.Refused {
		t.Fatalf("first submit = %+v, want refused with the attempt spent", res)
	}
	again := h.next(run)
	if len(again) != 1 || again[0].State != first.State || again[0].Attempt != 2 || again[0].Token == first.Token {
		t.Fatalf("after a refusal with retry left next = %+v, want a fresh attempt 2 of %s", again, first.State)
	}
	if res := h.submit(run, first, `{"Verdict":"pass"}`); res.Class != wf.Rejected {
		t.Fatalf("resubmitting the refused token = %+v, want rejected", res)
	}
	if res := h.submit(run, again[0], `{"Verdict":"nope"}`); res.Class != wf.Refused {
		t.Fatalf("second submit = %+v, want refused", res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eC") // the failure route, no third attempt
	if res := h.submit(run, again[0], `{"Verdict":"pass"}`); res.Class != wf.Rejected {
		t.Fatalf("resubmitting the spent token = %+v, want rejected", res)
	}

	docs := readDocs(t)
	doc := norm(docs["../COORDINATOR.md"])
	for _, w := range []string{
		"never run done again on that attempt", "a fresh attempt", "while retry remains", "the failure route",
		"the child fixes its output and validates again", "a failed attempt is the engine's to retry", "never reruns done for a child",
	} {
		if !strings.Contains(doc, w) {
			t.Errorf("COORDINATOR.md lacks %q", w)
		}
	}
	for path, text := range docs {
		if strings.Contains(norm(text), "the retry budget is spent and a fresh attempt is issued") {
			t.Errorf("%s still says a spent retry issues a fresh attempt", path)
		}
	}
}

// verbsNamed is every verb a document names after `workflow` or `wf`.
func verbsNamed(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range subcommandRe.FindAllStringSubmatch(text, -1) {
		out[m[1]] = true
	}
	return out
}

// The reverse check: every row of wf.Subcommands is named in COORDINATOR.md,
// so a new verb cannot ship undocumented.
func TestEverySubcommandNamedInDocs(t *testing.T) {
	named := verbsNamed(readDocs(t)["../COORDINATOR.md"])
	for _, s := range wf.Subcommands {
		if !named[s.Name] {
			t.Errorf("COORDINATOR.md never names the verb `%s` (wf.Subcommands)", s.Name)
		}
	}
}

// The reverse check for the exit codes: each code and its name appear in
// COORDINATOR.md.
func TestEveryExitCodeNamedInDocs(t *testing.T) {
	text := readDocs(t)["../COORDINATOR.md"]
	for _, e := range wf.ExitCodes {
		if !strings.Contains(text, "exit "+strconv.Itoa(e.Code)) {
			t.Errorf("COORDINATOR.md never names exit %d (%s)", e.Code, e.Name)
		}
		if !strings.Contains(text, "| "+e.Name+" |") {
			t.Errorf("COORDINATOR.md has no table row for the exit code name %q", e.Name)
		}
	}
}

// The paged hand-out fields of an instruction exist, and COORDINATOR.md
// describes them in its next output section.
func TestDocsNameThePagedInstructionFields(t *testing.T) {
	top := jsonTags(reflect.TypeOf(wf.Instruction{}))
	fields := nextOutputFields(readDocs(t)["../COORDINATOR.md"])
	for _, tag := range []string{"name", "class", "brief", "visit", "delegated", "deadline", "transient", "holds"} {
		if !top[tag] {
			t.Errorf("wf.Instruction has no json tag %q", tag)
		}
		if !slices.Contains(fields, tag) {
			t.Errorf("COORDINATOR.md: next output section does not name %q", tag)
		}
	}
}

// The stable phrases the two liveness statements are matched on. The test
// reads COORDINATOR.md with whitespace folded and case ignored:
//
//	"without a transcript path"       the fallback sentence: with no transcript
//	                                   path, liveness is the heartbeat and the lease only
//	"never pass through the engine"    agents' own tool calls are the engine's
//	"are not constrained"              business in no way
func TestDocsLivenessFallbackAndAgentsUnconstrained(t *testing.T) {
	text := norm(readDocs(t)["../COORDINATOR.md"])
	i := strings.Index(text, "without a transcript path")
	if i < 0 {
		t.Error("COORDINATOR.md lacks the liveness fallback statement (\"without a transcript path\")")
	} else if tail := text[i:min(len(text), i+200)]; !strings.Contains(tail, "heartbeat") {
		t.Errorf("COORDINATOR.md: the liveness fallback does not name the heartbeat within 200 bytes: %q", tail)
	}
	j := strings.Index(text, "never pass through the engine")
	if j < 0 {
		t.Error("COORDINATOR.md lacks \"never pass through the engine\"")
	} else if !strings.Contains(text[j:min(len(text), j+200)], "are not constrained") {
		t.Error("COORDINATOR.md: agents' tool calls are not stated as unconstrained")
	}
}

// The loop is next --wait and nothing else (plan C1, C2a, C2b, C2c, C2d),
// matched on stable phrases; the loop line is the bootstrap's own.
func TestDocsNextWaitLoopRules(t *testing.T) {
	docs := readDocs(t)
	boot := norm(wf.Bootstrap(wf.RunStatus{Run: "run"}, "./super", "/state"))
	const loopLine = "as many as your harness allows"
	if !strings.Contains(boot, loopLine) {
		t.Fatalf("the bootstrap no longer has the loop line %q: update the documents with it", loopLine)
	}
	if !strings.Contains(boot, "report when next prints the ended instruction or asks for a person, and quote it") {
		t.Error("the bootstrap lost its report rule; COORDINATOR.md quotes it")
	}
	for path, wants := range map[string][]string{
		"../COORDINATOR.md": {
			"./super next", "is the only call in the loop", "within your harness's own limit",
			"other native work already running in this session", "100 s where the adapter",
			"a child finishing is not an event when nothing new became ready",
			"never call `status` in the loop", "the brief is the same shape for every unit",
			"workflow prompt <run> --claim <token> --state-dir <abs>",
			"pass it verbatim as the child's prompt", "## finishing",
			"report when `next` prints the ended instruction or asks for a person, and quote it",
			"workflow stop <run> --reason <r>", "anyone may run it", "children already running finish on their own",
			"a coordinator does not clear its own stop", "degraded `orphan` record", "no coordinator verb is involved",
			"died --session", "transcript mtime", "the longest a driving call can block", "2 minutes where the harness reads no limit", "a busy run is never a stop",
			"shell timeout", "detached effects do not block", "with `--json` a count is required", "not started yet: start these too", "you keep no list",
			"model tiers are hints", "degraded `tiers` record", "--harness generic --model-map",
			"`generic` always fits",
		},
	} {
		wants = append(wants, loopLine)
		for _, w := range wants {
			if !strings.Contains(norm(docs[path]), strings.ToLower(w)) {
				t.Errorf("%s lacks %q", path, w)
			}
		}
		// The engine has no cap, and the wave-era loop is gone: no document
		// prescribes a slot count, a wait cursor or a hand-out deadline.
		for _, gone := range []string{"slot", "stored budget", "suggest: wf next", "units: r ready", "540s"} {
			if strings.Contains(norm(docs[path]), gone) {
				t.Errorf("%s still names %q", path, gone)
			}
		}
	}
}

// nextLineHeads renders one NextResult of every kind RenderNext prints and
// returns each line's head, the text before its first colon: `spawn <name>`,
// `blocked <state>`, `ended` and so on. A kind of line added to RenderNext is
// caught here with no test edit, as long as this result exercises it.
func nextLineHeads(t *testing.T) []string {
	t.Helper()
	ins := []wf.Instruction{
		{Kind: wf.InstructionAgent, Name: "<name>", Brief: "run: x"},
		{Kind: wf.InstructionHumanGate, State: "<state>", Prompt: "q"},
		{Kind: wf.InstructionManualVerification, State: "<state>", Prompt: "q"},
		{Kind: wf.InstructionWaiting, Prompt: "p"},
		{Kind: wf.InstructionStopped, Prompt: "p"},
		{Kind: wf.InstructionEnded, Terminal: "<terminal>", Outcome: wf.Success},
	}
	l := func(verb string, args ...string) string { return "workflow " + verb + " " + strings.Join(args, " ") }
	var heads []string
	for _, r := range []wf.NextResult{
		{Instructions: ins, More: 3},
		{Instructions: ins[:1]},
		{Blocked: &wf.BlockedView{State: "<state>", Reason: "r"}},
		{Stalled: []wf.StalledView{{Name: "<name>"}}},
		{Timeout: true},
	} {
		inBrief := false
		for _, line := range strings.Split(wf.RenderNext(r, "claude", l), "\n") {
			if line == "-----" { // a brief between its lines is the child's (I1)
				inBrief = !inBrief
				continue
			}
			if line == "" || strings.HasPrefix(line, " ") || inBrief {
				continue
			}
			head, _, _ := strings.Cut(line, ":")
			if strings.HasSuffix(head, " more are ready") {
				head = "<n> more are ready" // the count varies
			}
			if strings.HasPrefix(head, "start a child agent for <name> with ") {
				head = "start a child agent for <name>" // the call is the harness's
			}
			if strings.HasPrefix(head, "nothing yet after ") {
				head = "nothing yet after <wait>" // the wait varies with the harness (H2)
			}
			heads = append(heads, head)
		}
	}
	if len(heads) < 9 {
		t.Fatalf("RenderNext printed %d line heads (%v), want at least 9: the render is broken", len(heads), heads)
	}
	return heads
}

// namesHead reports whether text has a backticked span that is the head
// alone or the head followed by a colon or a space.
func namesHead(text, head string) bool {
	for _, m := range backtickRe.FindAllStringSubmatch(text, -1) {
		if m[1] == head || strings.HasPrefix(m[1], head+":") || strings.HasPrefix(m[1], head+" ") {
			return true
		}
	}
	return false
}

// Every kind of line next --wait prints has a stated coordinator action: a
// row of the line table in COORDINATOR.md. The generated bootstrap tells the
// coordinator to follow the printed instructions.
func TestEveryNextLineHasCoordinatorAction(t *testing.T) {
	docs := readDocs(t)
	var rows []string
	inTable := false
	for _, line := range strings.Split(docs["../COORDINATOR.md"], "\n") {
		trim := strings.TrimSpace(line)
		if !inTable {
			inTable = strings.HasPrefix(trim, "| line | do |")
			continue
		}
		if trim == "" {
			break
		}
		rows = append(rows, trim)
	}
	for _, head := range nextLineHeads(t) {
		covered := false
		for _, row := range rows {
			cells := strings.Split(strings.Trim(row, "|"), "|")
			if len(cells) >= 2 && namesHead(cells[0], head) && strings.TrimSpace(cells[1]) != "" {
				covered = true
			}
		}
		if !covered {
			t.Errorf("COORDINATOR.md: the next line table has no action for %q", head)
		}
	}
}

func TestDocsDetachedRunnerLinuxOnlyAndQuotedLines(t *testing.T) {
	for path, text := range readDocs(t) {
		if !strings.Contains(norm(text), "already shell-quoted") {
			t.Errorf("%s lacks %q", path, "already shell-quoted")
		}
	}
	if !strings.Contains(norm(readDocs(t)["../COORDINATOR.md"]), "detached runner is linux-only") {
		t.Errorf("COORDINATOR.md lacks %q", "detached runner is linux-only")
	}
}

// unitStatesFromSource is every UnitStatus.State value the engine can emit:
// the UnitState enum names, plus each string literal assigned to a State field
// in engine_status.go and status_view.go and each literal case clause in
// status_view.go (status_view.go spells "future" that way). A state added to
// either place is caught with no test edit.
func unitStatesFromSource(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for i := 1; ; i++ {
		name := wf.UnitState(i).String()
		if strings.HasPrefix(name, "UnitState(") {
			break
		}
		seen[name] = true
	}
	lit := func(e ast.Expr) {
		if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			s, err := strconv.Unquote(bl.Value)
			if err != nil {
				t.Fatalf("unquoting %s: %v", bl.Value, err)
			}
			seen[s] = true
		}
	}
	for _, file := range []string{"engine_status.go", "status_view.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && id.Name == "State" {
					lit(n.Value)
				}
			case *ast.CaseClause:
				if file == "status_view.go" {
					for _, e := range n.List {
						lit(e)
					}
				}
			}
			return true
		})
	}
	var states []string
	for s := range seen {
		states = append(states, s)
	}
	sort.Strings(states)
	if len(states) < 10 {
		t.Fatalf("found %d unit states in source (%v), want at least 10: the parse is broken", len(states), states)
	}
	return states
}

// COORDINATOR.md section 10 lists the unit states wf status prints; the list
// matches the states the engine and the status view can emit, both ways.
func TestDocsNameEveryUnitState(t *testing.T) {
	text := readDocs(t)["../COORDINATOR.md"]
	i := strings.Index(text, "Unit states:")
	if i < 0 {
		t.Fatal("COORDINATOR.md has no sentence starting `Unit states:`")
	}
	// the sentence ends at the first period before whitespace (a period inside
	// the parenthesis would end it early, so the list stays one sentence)
	sentence := text[i:]
	if loc := regexp.MustCompile(`\.\s`).FindStringIndex(sentence); loc != nil {
		sentence = sentence[:loc[0]]
	}
	documented := map[string]bool{}
	for _, m := range backtickRe.FindAllStringSubmatch(sentence, -1) {
		documented[m[1]] = true
	}
	source := unitStatesFromSource(t)
	for _, s := range source {
		if !documented[s] {
			t.Errorf("unit state %q can be emitted but the `Unit states:` sentence in COORDINATOR.md omits it", s)
		}
	}
	for s := range documented {
		if !slices.Contains(source, s) {
			t.Errorf("`Unit states:` in COORDINATOR.md names %q, which no source emits", s)
		}
	}
}

// D1, D3: COORDINATOR.md carries the refusal rules, the sandbox escalation
// rule, the fix-and-rerun rule and the sibling rule. It never keeps
// the old run-each-command-once or never-escalate wording. The child meets
// these rules, so its fetched prompt's Finishing block carries them too (C2).
func TestDocsRefusalRules(t *testing.T) {
	flat := strings.Join(strings.Fields(readDocs(t)["../COORDINATOR.md"]), " ")
	for _, want := range []string{
		"is refused", "is never worked around", "A command that ran and returned an error", "--class transient",
		"Pointing a cache inside the worktree works around nothing",
		"When the sandbox blocks a command the task needs, request escalation",
		"Record each escalation in the run report (command, reason, outcome)",
		"`--escalated '<command>: <reason>: <outcome>'`", "--escalated` for the unit it served",
		"If escalation is refused, that is still a blocked failure (`--class blocked`) unless your coordinator can present the approval prompt you could not access.",
		"never re-runs a command that was accepted",
		"One child's failure belongs to that child: never hold or pause sibling children over it.",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("COORDINATOR.md lacks %q", want)
		}
	}
	for path, text := range readDocs(t) {
		flat := strings.Join(strings.Fields(text), " ")
		for _, bad := range []string{"running each command once", "each run once", "Never request sandbox escalation"} {
			if strings.Contains(flat, bad) {
				t.Errorf("%s keeps %q", path, bad)
			}
		}
	}
	// What COORDINATOR.md says the Finishing block holds, the engine prints.
	fetched := norm(childPrompt(t))
	for _, want := range []string{
		"## finishing", "never re-run a command that was accepted", "validate",
		"--class blocked", "never work around it", "a command that ran and failed",
		"request escalation", "--escalated '<command>: <reason>: <outcome>'",
		"if escalation is refused, that is still blocked unless your coordinator can present the approval prompt you could not access",
		"background-task tool", "rather than a polling or wait loop",
	} {
		if !strings.Contains(fetched, want) {
			t.Errorf("the fetched prompt lacks %q, which COORDINATOR.md says its Finishing block carries", want)
		}
	}
}

// claimRe finds the claim token in a brief.
var claimRe = regexp.MustCompile(`--claim '?([0-9a-f]+)`)

// childPrompt hands out one unit and fetches its prompt by the claim token in
// its brief, as the child would.
func childPrompt(t *testing.T) string {
	t.Helper()
	d := outOfEnumDef(false)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOut(run, 1)
	if err != nil || len(ins) != 1 {
		t.Fatalf("HandOut = %v, %v; want one unit", ins, err)
	}
	m := claimRe.FindStringSubmatch(ins[0].Brief)
	if m == nil {
		t.Fatalf("brief %q carries no --claim token", ins[0].Brief)
	}
	if _, ok := wf.BriefCommand(ins[0].Brief); !ok {
		t.Errorf("brief %q is not the one fixed line COORDINATOR.md documents", ins[0].Brief)
	}
	f, err := e.PromptFetch(run, "", m[1])
	if err != nil {
		t.Fatalf("PromptFetch by claim: %v", err)
	}
	return f.Delivered
}
