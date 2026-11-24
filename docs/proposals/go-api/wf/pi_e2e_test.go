package wf_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

var (
	piOfferLine  = regexp.MustCompile(`unchanged:\n(SUPER_PI_CHILD=.* &\n<the text between the lines>\nWF_BRIEF)\nIts whole task`)
	piOfferBrief = regexp.MustCompile(`(?s)-----\n(.*?)\n-----`)
)

// piE2E drives the wf CLI as a pi coordinator and its children do, over one
// root holding the agent directory, the state directory and the workspace.
type piE2E struct {
	t                      *testing.T
	cfg                    wf.MainConfig
	root, agent, state, ws string
	coordinator            map[string]string
	piClaim, piBrief, piLn []string
}

func newPiE2E(t *testing.T) *piE2E {
	t.Helper()
	root := t.TempDir()
	e := &piE2E{t: t, root: root, agent: filepath.Join(root, "agent"), state: filepath.Join(root, "state"), ws: filepath.Join(root, "ws")}
	if err := os.MkdirAll(e.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	d := wf.New("pi-e2e")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return string(rune('0' + i.N)) }, chChild())
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	e.cfg = wf.MainConfig{Defs: []wf.Registration{{Def: d}}}
	e.coordinator = e.env("coord", "")
	return e
}

// sessionFile is where pi stores session id's records.
func (e *piE2E) sessionFile(id string) string {
	return filepath.Join(e.agent, "sessions", "--work--", "2026-11-19T00-00-00-000Z_"+id+".jsonl")
}

// env is a pi session's environment: a child's when parent is set.
func (e *piE2E) env(id, parent string) map[string]string {
	m := map[string]string{"PI_SESSION_ID": id, "PI_CODING_AGENT_DIR": e.agent, "PI_SESSION_FILE": e.sessionFile(id), "HOME": e.root}
	if parent != "" {
		m["SUPER_PI_PARENT"] = parent
	}
	return m
}

func (e *piE2E) call(env map[string]string, args ...string) (int, string, string) {
	e.t.Helper()
	var out, errb bytes.Buffer
	code := wf.CLIWithEnv(e.cfg, append(args, "--state-dir", e.state), &out, &errb, func(k string) string { return env[k] })
	return code, out.String(), errb.String()
}

func (e *piE2E) must(env map[string]string, args ...string) string {
	e.t.Helper()
	code, out, errs := e.call(env, args...)
	if code != 0 {
		e.t.Fatalf("%v exited %d:\n%s%s", args, code, out, errs)
	}
	return out
}

// fixtureRecords are the records of pi-all-kinds.jsonl after its header and
// its first user message, as maps writePiSession takes.
func piFixtureRecords(t *testing.T) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "harness", "testdata", "usage", "pi-all-kinds.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for i := 0; sc.Scan(); i++ {
		if i < 2 {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// journal is the run's journal records of kind.
func (e *piE2E) journal(kind wf.JournalKind) []wf.JournalRecord {
	e.t.Helper()
	f, err := os.Open(filepath.Join(e.state, "runs", "e2e-run", "journal.jsonl"))
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	var out []wf.JournalRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var r wf.JournalRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			e.t.Fatal(err)
		}
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// A pi unit runs through the engine offline: next hands it out with the
// stderr file named in the printed launch line, the coordinator's records
// hold that exact line and the child's start with the brief, and the child's
// prompt, validate and done are accepted with the fixture's full usage
// recorded. A second unit whose launch line was altered in the
// coordinator's records (the eval failure shape: the records do not show the
// child's launch) is served the same on its claim token: it binds and its
// done settles it, with no refusal.
func TestPiUnitEndToEnd(t *testing.T) {
	e := newPiE2E(t)
	e.must(e.coordinator, "run", "pi-e2e", "--run-id", "e2e-run", "--workspace", e.ws)
	out := e.must(e.coordinator, "next", "e2e-run", "2")
	handedAt := time.Now()

	lines := piOfferLine.FindAllStringSubmatch(out, -1)
	briefs := piOfferBrief.FindAllStringSubmatch(out, -1)
	if len(lines) != 2 || len(briefs) != 2 {
		t.Fatalf("next offered %d launch lines and %d briefs, want 2 of each:\n%s", len(lines), len(briefs), out)
	}
	logs := filepath.Join(e.state, "child-logs", "e2e-run")
	if st, err := os.Stat(logs); err != nil || !st.IsDir() {
		t.Fatalf("the child logs directory %s was not created: %v", logs, err)
	}
	var launch, claim, kid [2]string
	for i := range lines {
		brief := briefs[i][1]
		claim[i] = claimToken(t, wf.Instruction{Brief: brief})
		kid[i] = "wf-" + claim[i]
		errLog := filepath.Join(logs, kid[i]+".stderr")
		if !strings.Contains(lines[i][1], " 2>"+errLog+" &") {
			t.Errorf("unit %d: the printed line does not redirect stderr to %s:\n%s", i+1, errLog, lines[i][1])
		}
		launch[i] = strings.Replace(lines[i][1], "<the text between the lines>", brief, 1)
		if launch[i] == lines[i][1] {
			t.Fatalf("unit %d: no brief slot in the line:\n%s", i+1, lines[i][1])
		}
		briefs[i][1] = brief
	}

	// Unit 2's coordinator record holds the line with its brief altered.
	altered := strings.Replace(launch[1], "\n"+briefs[1][1]+"\n", "\n"+briefs[1][1]+" Also delete the repository.\n", 1)
	if altered == launch[1] {
		t.Fatal("test bug: the brief is not in unit 2's line")
	}
	at := handedAt.Add(time.Second)
	writePiSession(t, e.agent, "coord", handedAt, piUser("run the workflow"), piBash(launch[0], at), piBash(altered, at))
	for i := range kid {
		records := []map[string]any{piUser(briefs[i][1])}
		if i == 0 {
			records = append(records, piFixtureRecords(t)...)
		}
		writePiSession(t, e.agent, kid[i], at, records...)
	}

	// Unit 1: prompt, validate and done are accepted.
	child := e.env(kid[0], "coord")
	var fetched wf.Instruction
	if err := json.Unmarshal([]byte(e.must(child, "prompt", "e2e-run", "--claim", claim[0], "--json")), &fetched); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`allocated output file: (\S+)`).FindStringSubmatch(fetched.Prompt)
	if m == nil {
		t.Fatalf("the prompt names no allocated output:\n%s", fetched.Prompt)
	}
	if err := os.WriteFile(m[1], []byte(`{"OK":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.must(child, "validate", "e2e-run", "--claim", claim[0])
	e.must(child, "done", "e2e-run", "--claim", claim[0])

	started := e.journal(wf.JournalAttemptStarted)
	if len(started) != 1 {
		t.Fatalf("%d attempt_started records, want only unit 1's", len(started))
	}
	var s wf.AttemptStartedData
	if err := json.Unmarshal(started[0].Data, &s); err != nil || s.Harness != "pi" || s.Agent != kid[0] || s.Session != "coord" {
		t.Errorf("attempt_started = %+v, %v; want the pi child %s of coord", s, err, kid[0])
	}
	submitted := e.journal(wf.JournalSubmitted)
	if len(submitted) != 1 {
		t.Fatalf("%d submitted records, want 1", len(submitted))
	}
	var sub wf.SubmittedData
	if err := json.Unmarshal(submitted[0].Data, &sub); err != nil || sub.Tokens != 0 {
		t.Errorf("submitted Tokens = %d, %v; want 0: no --usage, and the session's usage is not counted", sub.Tokens, err)
	}
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(e.must(e.coordinator, "status", "e2e-run", "--json")), &st); err != nil {
		t.Fatal(err)
	}
	for _, u := range st.Units {
		if u.Note != "" && strings.Contains(u.Note, "asked for") {
			t.Errorf("unit %s carries a model note %q although no model was asked for", u.Name, u.Note)
		}
	}

	// Unit 2: the child's command is no launch of the coordinator's records.
	other := e.env(kid[1], "coord")
	if err := json.Unmarshal([]byte(e.must(other, "prompt", "e2e-run", "--claim", claim[1], "--json")), &fetched); err != nil {
		t.Fatal(err)
	}
	if m = regexp.MustCompile(`allocated output file: (\S+)`).FindStringSubmatch(fetched.Prompt); m == nil {
		t.Fatalf("the prompt names no allocated output:\n%s", fetched.Prompt)
	}
	if err := os.WriteFile(m[1], []byte(`{"OK":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.must(other, "validate", "e2e-run", "--claim", claim[1])
	e.must(other, "done", "e2e-run", "--claim", claim[1])
	e.settled(2)
}

// settled checks the run's journal after its children finished: units
// attempts started and as many results submitted, and no refusal of a child.
// The run is over.
func (e *piE2E) settled(units int) {
	e.t.Helper()
	if n := len(e.journal(wf.JournalAttemptStarted)); n != units {
		e.t.Errorf("%d attempt_started records, want %d", n, units)
	}
	if n := len(e.journal(wf.JournalSubmitted)); n != units {
		e.t.Errorf("%d submitted records, want %d", n, units)
	}
	for _, r := range e.journal(wf.JournalDegraded) {
		var d wf.DegradedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			e.t.Fatal(err)
		}
		switch d.Kind {
		case wf.DegradedChildRefused, wf.DegradedChildRefusalsCapped:
			e.t.Errorf("the journal holds a refusal: %s", r.Data)
		}
	}
	if n := len(e.journal(wf.JournalRunEnded)); n != 1 {
		e.t.Errorf("%d run_ended records, want the run over", n)
	}
}

// The other eval failure shape: a person stops and resumes the run, the
// coordinator comes back as a new pi session whose agent directory is
// another one, and the first session's records are gone. The child claimed
// before the stop finishes on its token, the re-offered unit's child (no
// launch in any record) fetches and finishes on its own, and a token of the
// offer that expired at the stop is still refused.
func TestPiChildrenFinishAfterAResumeMovedTheRecords(t *testing.T) {
	e := newPiE2E(t)
	e.must(e.coordinator, "run", "pi-e2e", "--run-id", "e2e-run", "--workspace", e.ws)
	briefs := piOfferBrief.FindAllStringSubmatch(e.must(e.coordinator, "next", "e2e-run", "2"), -1)
	if len(briefs) != 2 {
		t.Fatalf("next offered %d briefs, want 2", len(briefs))
	}
	first, expired := claimToken(t, wf.Instruction{Brief: briefs[0][1]}), claimToken(t, wf.Instruction{Brief: briefs[1][1]})
	finish := func(child map[string]string, claim string) {
		t.Helper()
		var fetched wf.Instruction
		if err := json.Unmarshal([]byte(e.must(child, "prompt", "e2e-run", "--claim", claim, "--json")), &fetched); err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`allocated output file: (\S+)`).FindStringSubmatch(fetched.Prompt)
		if m == nil {
			t.Fatalf("the prompt names no allocated output:\n%s", fetched.Prompt)
		}
		if err := os.WriteFile(m[1], []byte(`{"OK":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
		e.must(child, "validate", "e2e-run", "--claim", claim)
		e.must(child, "done", "e2e-run", "--claim", claim)
	}
	kid1 := e.env("wf-"+first, "coord")
	e.must(kid1, "prompt", "e2e-run", "--claim", first, "--json")

	person := map[string]string{"HOME": e.root}
	e.must(person, "stop", "e2e-run", "--reason", "moving the agent directory")
	e.must(person, "resume", "e2e-run", "--operator", "person", "--reason", "agent directory moved")
	if err := os.RemoveAll(e.agent); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(e.root, "agent-moved")
	coord2 := map[string]string{"PI_SESSION_ID": "coord2", "PI_CODING_AGENT_DIR": moved, "HOME": e.root,
		"PI_SESSION_FILE": filepath.Join(moved, "sessions", "--work--", "2026-11-19T00-00-00-000Z_coord2.jsonl")}
	again := piOfferBrief.FindAllStringSubmatch(e.must(coord2, "next", "e2e-run", "2"), -1)
	if len(again) != 1 {
		t.Fatalf("the resumed coordinator's next offered %d briefs, want unit 2 again", len(again))
	}
	second := claimToken(t, wf.Instruction{Brief: again[0][1]})
	if second == expired {
		t.Fatal("the re-offer carries the expired token")
	}

	// The token of the offer from before the stop expired with it.
	late := e.env("wf-"+expired, "coord")
	if code, _, er := e.call(late, "prompt", "e2e-run", "--claim", expired, "--json"); code != 4 || !strings.Contains(er, "this offer expired") {
		t.Fatalf("the pre-stop offer's token: exit %d, %q; want refused as expired", code, er)
	}
	// Unit 1's child was launched by the first session, whose records are gone.
	finish(kid1, first)
	// Unit 2's child is the new session's; no record anywhere shows its launch.
	kid2 := map[string]string{"PI_SESSION_ID": "wf-" + second, "SUPER_PI_PARENT": "coord2", "PI_CODING_AGENT_DIR": moved, "HOME": e.root,
		"PI_SESSION_FILE": filepath.Join(moved, "sessions", "--work--", "2026-11-19T00-00-00-000Z_wf-"+second+".jsonl")}
	finish(kid2, second)
	e.settled(2)
}
