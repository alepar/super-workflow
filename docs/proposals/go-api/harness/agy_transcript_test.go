package harness

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	agyFixture = "testdata/agy/probe"
	agyEdgeDir = "testdata/agy/edge"

	agyCoord1  = "00000000-0000-4000-8000-000000000002"
	agyChild1A = "00000000-0000-4000-8000-000000000001"
	agyChild1B = "00000000-0000-4000-8000-000000000003"
	agyCoord2  = "00000000-0000-4000-8000-000000000005"
	agyChild2A = "00000000-0000-4000-8000-000000000004"
	agyChild2B = "00000000-0000-4000-8000-000000000006"
)

// agyIsolate points the records root and HOME at the fixture root and a temp
// dir, so no agy test can resolve the real home directory.
func agyIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", agyFixture)
	t.Setenv("HOME", t.TempDir())
}

func agyFix(id string) string { return agyTranscript(agyFixture, id) }
func agyEdge(name string) string {
	return filepath.Join(agyEdgeDir, name+".jsonl")
}

// agyHead writes the first n lines of the transcript at path to a temp file.
func agyHead(t *testing.T, path string, n int) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(b), "\n")
	out := filepath.Join(t.TempDir(), "cut.jsonl")
	if err := os.WriteFile(out, []byte(strings.Join(lines[:n], "")), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func runScan(t *testing.T, dir string) (int, string) {
	t.Helper()
	out, err := exec.Command("bash", filepath.Join("..", "scripts", "agy-scan-secrets.sh"), dir).CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("scan: %v", err)
	return 0, ""
}

func agyWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgyScanSecrets(t *testing.T) {
	agyIsolate(t)
	if code, out := runScan(t, filepath.Join("testdata", "agy")); code != 0 {
		t.Fatalf("the agy fixtures must scan clean: %d %s", code, out)
	}
	// Planted values are built at run time so no credential-shaped literal sits in the source.
	csrf := "ANTIGRAVITY_CSRF_TOKEN=" + strings.Repeat("ab12", 8)
	for name, body := range map[string]string{
		"csrf":    `{"content":"` + csrf + `\r\n"}`,
		"access":  "ya29." + strings.Repeat("x", 30),
		"refresh": `{"refresh_token": "` + "1//0" + strings.Repeat("y", 30) + `"}`,
		"apikey":  "AIza" + strings.Repeat("z", 35),
	} {
		dir := t.TempDir()
		agyWrite(t, filepath.Join(dir, "f.jsonl"), []byte(body))
		code, out := runScan(t, dir)
		if code != 1 || !strings.Contains(out, "f.jsonl") || strings.Contains(out, strings.Repeat("ab12", 8)) {
			t.Errorf("%s: exit %d, out %q: want 1, the file named, the value never printed", name, code, out)
		}
	}
	dir := t.TempDir()
	agyWrite(t, filepath.Join(dir, "antigravity-oauth-token"), nil)
	if code, _ := runScan(t, dir); code != 1 {
		t.Error("a credential file must fail the scan")
	}
	dir = t.TempDir()
	agyWrite(t, filepath.Join(dir, "ok.jsonl"), []byte(`ANTIGRAVITY_CSRF_TOKEN=<redacted>\r\n`))
	if code, out := runScan(t, dir); code != 0 {
		t.Errorf("a redacted value is clean: %d %s", code, out)
	}
}

func TestAgyRecordsLayout(t *testing.T) {
	agyIsolate(t)
	d, h := t.TempDir(), t.TempDir()
	if got := agyRoot(envMap(map[string]string{"ANTIGRAVITY_APP_DATA_DIR": d, "HOME": "/h"})); got != d {
		t.Errorf("app data dir wins: %q", got)
	}
	if got, want := agyRoot(envMap(map[string]string{"HOME": h})), filepath.Join(h, ".gemini", "antigravity-cli"); got != want {
		t.Errorf("home fallback: %q want %q", got, want)
	}
	if got := agyRoot(envMap(nil)); got != "" {
		t.Errorf("no root without either variable: %q", got)
	}
	g := filepath.Join("r", "brain", "c", ".system_generated")
	if got, want := agyTranscript("r", "c"), filepath.Join(g, "logs", "transcript_full.jsonl"); got != want {
		t.Errorf("transcript %q want %q", got, want)
	}
	if got, want := agyListing("r", "p", "c"), filepath.Join("r", "brain", "p", ".system_generated", "subagents", "c.json"); got != want {
		t.Errorf("listing %q want %q", got, want)
	}
	if got, want := agyMessagesDir("r", "c"), filepath.Join(g, "messages"); got != want {
		t.Errorf("messages %q want %q", got, want)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../x", `a\b`, "a\x00b"} {
		if agyTranscript("r", bad) != "" || agyMessagesDir("r", bad) != "" || agyListing("r", "p", bad) != "" || agyListing("r", bad, "c") != "" {
			t.Errorf("invalid id %q must give no path", bad)
		}
	}
	if agyTranscript("", "c") != "" || agyMessagesDir("", "c") != "" || agyListing("", "p", "c") != "" {
		t.Error("an empty root gives no path")
	}
	for child, want := range map[string][]string{
		agyChild1A: {agyCoord1}, agyChild2A: {agyCoord2}, agyCoord1: nil, "../x": nil,
	} {
		if got := agyListingParents(agyFixture, child); !reflect.DeepEqual(got, want) {
			t.Errorf("listing parents of %s: %v want %v", child, got, want)
		}
	}
}

func TestAgyPathsOnlyInRecords(t *testing.T) {
	agyIsolate(t)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	lits := []string{`".system_generated"`, `"brain"`, `"transcript_full.jsonl"`, `"subagents"`, `"antigravity-cli"`}
	for _, f := range files {
		if !strings.HasPrefix(f, "agy") || strings.HasSuffix(f, "_test.go") || f == "agy_transcript.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lits {
			if strings.Contains(string(b), l) {
				t.Errorf("%s assembles an agy path (%s): only agy_transcript.go may", f, l)
			}
		}
	}
}

func TestAgyUsage(t *testing.T) {
	agyIsolate(t)
	for _, c := range []struct {
		name, path string
		want       int
		ok         bool
	}{
		{"coordinator 1", agyFix(agyCoord1), 1106, true},
		{"coordinator 2", agyFix(agyCoord2), 1258, true},
		{"child 1A", agyFix(agyChild1A), 378, true},
		{"child 1B", agyFix(agyChild1B), 384, true},
		{"child 2A", agyFix(agyChild2A), 1100, true},
		{"child 2B", agyFix(agyChild2B), 384, true},
		{"cache reads not counted", agyEdge("trailing-call"), 120, true},
		{"no usage", agyEdge("no-usage"), 0, false},
		// truncated: the one whole response counts input 100 + output 20 (cache
		// reads left out); the half-written step 3 line counts nothing.
		{"truncated last line", agyEdge("truncated"), 120, true},
		{"missing", filepath.Join(t.TempDir(), "none.jsonl"), 0, false},
	} {
		got, ok := For("agy").(Telemetry).Usage(c.path)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: Usage = %d, %v; want %d, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestAgyEnded(t *testing.T) {
	agyIsolate(t)
	for _, id := range []string{agyCoord1, agyChild1A, agyChild1B, agyCoord2, agyChild2A, agyChild2B} {
		if !For("agy").Ended(agyFix(id)) {
			t.Errorf("fixture %s ends", id)
		}
	}
	for name, want := range map[string]bool{
		"trailing-call": false, "tool-result": false, "user-last": false,
		"error-last": false, "truncated": false, "error-message": true,
		// a RUNNING task more than tailWindow before the end, never completed
		"running-far-back": false,
	} {
		if got := For("agy").Ended(agyEdge(name)); got != want {
			t.Errorf("edge %s: Ended = %v, want %v", name, got, want)
		}
	}
	for name, c := range map[string]struct {
		path string
		n    int
	}{
		"2A cut after step 6 (task-4 RUNNING)":      {agyFix(agyChild2A), 7},
		"2A cut after step 7 (a message is last)":   {agyFix(agyChild2A), 8},
		"coordinator 1 cut after step 15 (task-11)": {agyFix(agyCoord1), 16},
	} {
		if For("agy").Ended(agyHead(t, c.path, c.n)) {
			t.Errorf("%s: must not end", name)
		}
	}
	if For("agy").Ended(filepath.Join(t.TempDir(), "none.jsonl")) {
		t.Error("a missing file does not end")
	}
}

// TestAgyEndedTailAgreesWithFullRead: Ended equals a full read for every
// probe transcript, every edge fixture and every line prefix of a probe
// transcript, and a decided tail verdict equals the full read's.
func TestAgyEndedTailAgreesWithFullRead(t *testing.T) {
	agyIsolate(t)
	var paths []string
	for _, id := range []string{agyCoord1, agyChild1A, agyChild1B, agyCoord2, agyChild2A, agyChild2B} {
		p := agyFix(id)
		paths = append(paths, p)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= strings.Count(string(b), "\n"); n++ {
			paths = append(paths, agyHead(t, p, n))
		}
	}
	edges, err := filepath.Glob(filepath.Join(agyEdgeDir, "*.jsonl"))
	if err != nil || len(edges) == 0 {
		t.Fatalf("edge fixtures: %v, %v", edges, err)
	}
	paths = append(paths, edges...)
	for _, p := range paths {
		full := agyEndedFull(p)
		if got := For("agy").Ended(p); got != full {
			t.Errorf("%s: Ended = %v, full read = %v", p, got, full)
		}
		if ended, decided := agyEndedTail(p); decided && ended != full {
			t.Errorf("%s: decided tail = %v, full read = %v", p, ended, full)
		}
	}
}

// agyBig writes a log of the given lines to a temp file and returns its path.
func agyBig(t *testing.T, lines ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func agyLineJSON(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func agyStepLine(t *testing.T, idx int, typ, status, content string, calls ...map[string]any) string {
	m := map[string]any{"step_index": idx, "source": "MODEL", "type": typ, "status": status, "content": content}
	if len(calls) > 0 {
		m["tool_calls"] = calls
	}
	return agyLineJSON(t, m)
}

// TestAgyEndedFallsBackOnlyWhenTheTailCannotAnswer: each case holds Ended,
// the full read and the tail's (ended, decided) apart.
func TestAgyEndedFallsBackOnlyWhenTheTailCannotAnswer(t *testing.T) {
	agyIsolate(t)
	b, err := os.ReadFile(agyEdge("trailing-call"))
	if err != nil {
		t.Fatal(err)
	}
	step0 := strings.SplitN(string(b), "\n", 2)[0]
	const task = "aaaaaaaa-0000-4000-8000-000000000001/task-4"
	sentence := strings.Repeat("All quiet on the northern front. ", 120)
	filler := func(from, n int) []string {
		var out []string
		for i := 0; i < n; i++ {
			out = append(out, agyStepLine(t, from+i, agyResponse, "DONE", sentence))
		}
		return out
	}
	running := agyStepLine(t, 1, agyGeneric, "RUNNING", "Tool is running as a background task with task id: "+task)
	msg := agyStepLine(t, 100, agySystem, "DONE",
		agyWrapPrefix+"timestamp=2026-11-19T09:00:00Z sender="+task+" priority=MESSAGE_PRIORITY_HIGH content=done"+agyWrapSuffix)
	final := agyStepLine(t, 101, agyResponse, "DONE", "All done.")
	withCall := agyStepLine(t, 101, agyResponse, "DONE", "", map[string]any{"name": "run_command", "args": map[string]any{}})
	huge := agyStepLine(t, 1, agyResponse, "DONE", strings.Repeat("x", tailWindow+10))

	cat := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	one := func(s ...string) []string { return s }
	cases := []struct {
		name           string
		path           string
		ended, decided bool
	}{
		{"committed far-back RUNNING fixture", agyEdge("running-far-back"), false, false},
		{"far-back RUNNING answered by its message", agyBig(t, cat(one(step0, running), filler(2, 98), one(msg, final))...), true, false},
		{"far-back RUNNING, last a tool call", agyBig(t, cat(one(step0, running), filler(2, 98), one(withCall))...), false, true},
		{"small log with RUNNING and no message", agyBig(t, step0, running, final), false, true},
		{"no RUNNING, content-only last", agyBig(t, cat(one(step0), filler(1, 99), one(final))...), true, false},
		{"last response larger than the window", agyBig(t, step0, huge), true, false},
		{"half-written last line", agyBig(t, cat(one(step0), filler(1, 99), one(`{"step_index":100,"type":"PLAN`))...), false, true},
	}
	for _, c := range cases {
		full := agyEndedFull(c.path)
		if full != c.ended {
			t.Errorf("%s: full read = %v, want %v", c.name, full, c.ended)
		}
		if got := For("agy").Ended(c.path); got != c.ended {
			t.Errorf("%s: Ended = %v, want %v", c.name, got, c.ended)
		}
		ended, decided := agyEndedTail(c.path)
		if decided != c.decided || (decided && ended != c.ended) {
			t.Errorf("%s: tail = (%v, %v), want decided=%v ended=%v", c.name, ended, decided, c.decided, c.ended)
		}
	}
}

func TestAgyTail(t *testing.T) {
	agyIsolate(t)
	tool, msg := For("agy").(Telemetry).Tail(agyFix(agyCoord1))
	if !strings.HasPrefix(tool, "run_command {") || !strings.Contains(tool, "probe-sleep.sh") {
		t.Errorf("coordinator 1 last tool: %q", tool)
	}
	if !strings.HasPrefix(msg, "### Step 3 Result") {
		t.Errorf("coordinator 1 last message: %q", msg)
	}
	tool, msg = For("agy").(Telemetry).Tail(agyEdge("trailing-call"))
	if !strings.HasPrefix(tool, "run_command ") || !strings.Contains(tool, "cat /work/body.md") || msg != "" {
		t.Errorf("trailing call: %q, %q", tool, msg)
	}
	// truncated: the half-written last line is skipped; the response before it is last.
	if tool, msg = For("agy").(Telemetry).Tail(agyEdge("truncated")); tool != "" || msg != "All done." {
		t.Errorf("truncated: %q, %q", tool, msg)
	}
	if tool, msg = For("agy").(Telemetry).Tail(filepath.Join(t.TempDir(), "none.jsonl")); tool != "" || msg != "" {
		t.Errorf("missing: %q, %q", tool, msg)
	}
	long := filepath.Join(t.TempDir(), "long.jsonl")
	agyWrite(t, long, []byte(`{"step_index":0,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"`+strings.Repeat("word\\n", 400)+`"}`+"\n"))
	if _, msg = For("agy").(Telemetry).Tail(long); strings.Contains(msg, "\n") || !strings.HasSuffix(msg, "…") {
		t.Errorf("a long response is cut to one line: %q", msg)
	}
}

func TestAgyReadAny(t *testing.T) {
	agyIsolate(t)
	probe2 := "/work/fixture/probe-4.sh"
	for _, c := range []struct {
		name, path  string
		paths       []string
		read, known bool
	}{
		// run_command is no read tool: a command line naming the path is no read.
		{"run_command names it", agyEdge("trailing-call"), []string{"/work/body.md"}, false, true},
		{"run_command names another", agyEdge("trailing-call"), []string{"/elsewhere.md"}, false, true},
		{"read_file path", agyEdge("read-file"), []string{"/work/run/prompt.md"}, true, true},
		{"read_file another path", agyEdge("read-file"), []string{"/work/run/other.md"}, false, true},
		{"read_file relative want", agyEdge("read-file"), []string{"prompt.md"}, false, true},
		{"no tool calls", agyEdge("user-last"), []string{"/work/body.md"}, false, false},
		{"truncated has no tool calls", agyEdge("truncated"), []string{"/work/body.md"}, false, false},
		{"missing", filepath.Join(t.TempDir(), "none.jsonl"), []string{"/work/body.md"}, false, false},
		// The child ran the script; it never read it.
		{"fixture script", agyFix(agyChild2A), []string{probe2}, false, true},
	} {
		read, known := For("agy").ReadAny(c.path, c.paths)
		if read != c.read || known != c.known {
			t.Errorf("%s: ReadAny = %v, %v; want %v, %v", c.name, read, known, c.read, c.known)
		}
	}
}

// TestAgyReadAnyOnlyReadTools: only a file-read tool's own path argument is
// a read; the path in any other call's argument, or in another argument of a
// read tool, gives no credit.
func TestAgyReadAnyOnlyReadTools(t *testing.T) {
	agyIsolate(t)
	const body = "/work/run/prompt.md"
	call := func(name, args string) string {
		return `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-19T09:00:01Z","tool_calls":[{"name":"` + name + `","args":` + args + `}]}`
	}
	for _, c := range []struct {
		name, line  string
		read, known bool
	}{
		{"read_file Path", call("read_file", `{"Path":"/work/run/prompt.md","StartLine":1}`), true, true},
		{"read_file unclean Path", call("read_file", `{"Path":"/work/run/../run/prompt.md"}`), true, true},
		{"view_file AbsolutePath", call("view_file", `{"AbsolutePath":"/work/run/prompt.md"}`), true, true},
		{"read_file relative Path", call("read_file", `{"Path":"run/prompt.md"}`), false, true},
		{"read_file Path prefix", call("read_file", `{"Path":"/work/run/prompt.md.bak"}`), false, true},
		{"read_file other argument", call("read_file", `{"Path":"/work/x.md","Note":"/work/run/prompt.md"}`), false, true},
		{"view_file wrong key", call("view_file", `{"Path":"/work/run/prompt.md"}`), false, true},
		{"run_command echo", call("run_command", `{"CommandLine":"echo /work/run/prompt.md"}`), false, true},
		{"run_command cat", call("run_command", `{"CommandLine":"cat /work/run/prompt.md"}`), false, true},
		{"write_file content", call("write_file", `{"Path":"/work/out.md","Content":"see /work/run/prompt.md"}`), false, true},
		{"write_file path", call("write_file", `{"Path":"/work/run/prompt.md","Content":"x"}`), false, true},
		{"send_message", call("send_message", `{"Recipient":"r","Message":"/work/run/prompt.md"}`), false, true},
	} {
		p := filepath.Join(t.TempDir(), "t.jsonl")
		agyWrite(t, p, []byte(c.line+"\n"))
		read, known := For("agy").ReadAny(p, []string{body})
		if read != c.read || known != c.known {
			t.Errorf("%s: ReadAny = %v, %v; want %v, %v", c.name, read, known, c.read, c.known)
		}
	}
}

func TestAgyCoordinatorTranscript(t *testing.T) {
	agyIsolate(t)
	for _, c := range []struct{ name, root, id, want string }{
		{"known", agyFixture, agyCoord1, agyFix(agyCoord1)},
		{"unknown id", agyFixture, "00000000-0000-4000-8000-000000000000", ""},
		{"invalid id", agyFixture, "../x", ""},
		{"empty root", "", agyCoord1, ""},
		{"temp root", t.TempDir(), agyCoord1, ""},
	} {
		if got := For("agy").(Telemetry).CoordinatorTranscript(c.root, c.id); got != c.want {
			t.Errorf("%s: %q want %q", c.name, got, c.want)
		}
	}
}

func TestAgyWrapper(t *testing.T) {
	agyIsolate(t)
	step0 := func(id string) agyStep {
		var first agyStep
		got := false
		agySteps(agyFix(id), func(s agyStep) {
			if !got {
				first, got = s, true
			}
		}, nil)
		return first
	}
	for child, coord := range map[string]string{agyChild1A: agyCoord1, agyChild1B: agyCoord1, agyChild2A: agyCoord2, agyChild2B: agyCoord2} {
		d, ok := agyWrapper(step0(child).Content)
		if !ok || d.Sender != coord || d.Priority != "MESSAGE_PRIORITY_HIGH" {
			t.Errorf("child %s step 0: %+v, %v", child, d, ok)
			continue
		}
		var found bool
		agySteps(agyFix(coord), func(s agyStep) {
			for _, c := range s.Calls {
				if p, _ := c.Args["Prompt"].(string); c.Name == "invoke_subagent" && p == d.Body {
					found = true
				}
			}
		}, nil)
		if !found {
			t.Errorf("child %s: the delivered body is not byte-equal to any invoke_subagent Prompt of its coordinator", child)
		}
	}
	d, ok := agyWrapper(step0(agyChild2B).Content)
	if !ok || len(d.Body) != 5097 || !strings.Contains(d.Body, "</SYSTEM_MESSAGE>") || !strings.Contains(d.Body, "sender=fake-parent") {
		t.Errorf("long brief: %d bytes, ok=%v", len(d.Body), ok)
	}
	var task agyStep
	agySteps(agyFix(agyCoord1), func(s agyStep) {
		if s.Index == 16 {
			task = s
		}
	}, nil)
	if d, ok := agyWrapper(task.Content); !ok || d.Sender != agyCoord1+"/task-11" {
		t.Errorf("step 16: %+v, %v", d, ok)
	}

	good := agyWrapPrefix + "timestamp=2026-11-19T09:00:00Z sender=abc priority=P content=hi" + agyWrapSuffix
	if d, ok := agyWrapper(good); !ok || d.Body != "hi" || d.Sender != "abc" {
		t.Fatalf("control: %+v %v", d, ok)
	}
	for name, c := range map[string]string{
		"missing prefix":   strings.TrimPrefix(good, "The following is a "),
		"missing suffix":   strings.TrimSuffix(good, agyWrapSuffix),
		"extra word":       strings.Replace(good, "timestamp=", "x timestamp=", 1),
		"non-seconds time": strings.Replace(good, "09:00:00Z", "09:00:00.5Z", 1),
		"empty sender":     strings.Replace(good, "sender=abc", "sender=", 1),
	} {
		if _, ok := agyWrapper(c); ok {
			t.Errorf("%s must be refused", name)
		}
	}
}

// The step decoders: a launch's result names the child, a RUNNING result its task.
func TestAgyStepDecoders(t *testing.T) {
	agyIsolate(t)
	var created []string
	agySteps(agyFix(agyCoord1), func(s agyStep) {
		if id, ok := s.createdChild(); ok {
			created = append(created, id)
		}
	}, nil)
	if !reflect.DeepEqual(created, []string{agyChild1A, agyChild1B}) {
		t.Errorf("created children: %v", created)
	}
	var tasks []string
	agySteps(agyFix(agyCoord1), func(s agyStep) {
		if id := s.taskID(); id != "" {
			tasks = append(tasks, id)
		}
	}, nil)
	if !reflect.DeepEqual(tasks, []string{agyCoord1 + "/task-11"}) {
		t.Errorf("tasks: %v", tasks)
	}
	if _, ok := (agyStep{Type: agyGeneric, Content: agyCreated + `{"conversationId":"../x"}`}).createdChild(); ok {
		t.Error("an invalid conversation id is never a created child")
	}
}
