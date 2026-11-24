package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The fixtures under testdata/ended/pi-*.jsonl are built from the documented
// pi 1.5.2 session format (spec "pi facts"), not recorded: a real re-record
// is the orchestrator's smoke run.

func endLines(t *testing.T, name string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "ended", name))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			out = append(out, []byte(l))
		}
	}
	return out
}

func inlineLines(ss ...string) [][]byte {
	var out [][]byte
	for _, s := range ss {
		out = append(out, []byte(s))
	}
	return out
}

func TestPiEndReader(t *testing.T) {
	asst := func(stop string) string {
		return `{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"` + stop + `","content":[{"type":"text","text":"x"}]}}`
	}
	call := `{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","name":"bash","arguments":{"command":"ls"}}]}}`
	note := `{"type":"custom_message","customType":"workspace-extension:note"}`
	for _, c := range []struct {
		name  string
		lines [][]byte
		want  EndVerdict
	}{
		// Old rule (Ended): true at the error prefix of the retry fixture and at its full file.
		{"retry then stop: full file", endLines(t, "pi-retry-then-stop.jsonl"), EndIfSettled},
		{"retry then stop: prefix at the context_edit", endLines(t, "pi-retry-then-stop.jsonl")[:4], EndNo},
		{"retry then stop: prefix at the error", endLines(t, "pi-retry-then-stop.jsonl")[:3], EndIfSettled},
		{"final error", endLines(t, "pi-final-error.jsonl"), EndIfSettled},
		{"final error then late compaction", endLines(t, "pi-final-error-late-compaction.jsonl"), EndIfSettled},
		// Old rule: ended at once.
		{"stop then late compaction", endLines(t, "pi-stop-late-compaction.jsonl"), EndIfSettled},
		// Old rule: length was ended at once, also at the full file.
		{"length recovery: full file", endLines(t, "pi-length-recovery.jsonl"), EndNo},
		{"length recovery: prefix at the context_edit", endLines(t, "pi-length-recovery.jsonl")[:4], EndNo},
		{"length recovery: prefix at the length message", endLines(t, "pi-length-recovery.jsonl")[:3], EndIfSettled},
		{"aborted", endLines(t, "pi-aborted.jsonl"), EndYes},
		// Pins the default: the old rule did not end on deferred either.
		{"deferred", endLines(t, "pi-deferred.jsonl"), EndNo},
		// Moved from TestPiEnded. Old answer true.
		{"stop", inlineLines(asst("stop")), EndIfSettled},
		{"stop then an extension note", inlineLines(asst("stop"), note), EndIfSettled},
		{"inline aborted", inlineLines(asst("aborted")), EndYes},
		// Old answer true.
		{"inline error", inlineLines(asst("error")), EndIfSettled},
		{"waiting on its tool call", inlineLines(call), EndNo},
		{"a tool result last", inlineLines(call, `{"type":"message","message":{"role":"toolResult","content":[]}}`), EndNo},
		{"a new user turn", inlineLines(asst("stop"), `{"type":"message","message":{"role":"user","content":[]}}`), EndNo},
		{"pending", inlineLines(asst("pending")), EndNo},
		{"no stop reason", inlineLines(asst("")), EndNo},
		{"a last line that is not JSON after a final error", inlineLines(asst("error"), `{"type":"message","id":"a2","mess`), EndNo},
		{"a context_edit targeting another id", inlineLines(asst("error"), `{"type":"context_edit","id":"e1","targetId":"zz"}`), EndIfSettled},
		{"a context_edit targeting the error", inlineLines(asst("error"), `{"type":"context_edit","id":"e1","targetId":"a1"}`), EndNo},
		{"an error with no id is never targeted", inlineLines(`{"type":"message","message":{"role":"assistant","stopReason":"error","content":[]}}`, `{"type":"context_edit","targetId":""}`), EndIfSettled},
		{"no message at all", inlineLines(`{"type":"session","id":"x"}`), EndNo},
		{"no lines", nil, EndNo},
	} {
		if got := piEnd(c.lines); got != c.want {
			t.Errorf("%s: piEnd = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestPiEndedOnlyAtOnce(t *testing.T) {
	p := For("pi")
	for _, c := range []struct {
		file string
		want bool
	}{
		{"pi-aborted.jsonl", true},
		{"pi-retry-then-stop.jsonl", false}, // the old rule said true
		{"pi-final-error.jsonl", false},
		{"pi-final-error-late-compaction.jsonl", false},
		{"pi-stop-late-compaction.jsonl", false}, // the old rule said true
		{"pi-length-recovery.jsonl", false},
		{"pi-deferred.jsonl", false},
		{"missing.jsonl", false},
	} {
		if got := p.Ended(filepath.Join("testdata", "ended", c.file)); got != c.want {
			t.Errorf("Ended(%s) = %v, want %v", c.file, got, c.want)
		}
	}
	se, ok := p.(SettlingEnd)
	if !ok {
		t.Fatal("pi must implement SettlingEnd")
	}
	if v := se.EndVerdict(filepath.Join("testdata", "ended", "pi-final-error.jsonl")); v != EndIfSettled {
		t.Errorf("EndVerdict of a file = %d, want EndIfSettled", v)
	}
	if v := se.EndVerdict(filepath.Join("testdata", "ended", "missing.jsonl")); v != EndNo {
		t.Errorf("EndVerdict of a missing file = %d, want EndNo", v)
	}
	for _, name := range []string{"claude", "codex", "generic"} {
		if _, ok := For(name).(SettlingEnd); ok {
			t.Errorf("%s must not implement SettlingEnd", name)
		}
	}
}

func TestPiTailNamesTheFinalStop(t *testing.T) {
	p := For("pi")
	_, msg := p.(Telemetry).Tail(filepath.Join("testdata", "ended", "pi-final-error.jsonl"))
	if msg != "error: No API key found for openrouter." {
		t.Errorf("final error: Tail msg = %q", msg)
	}
	// Only the first line of a multi-line errorMessage.
	_, msg = p.(Telemetry).Tail(filepath.Join("testdata", "ended", "pi-retry-then-stop.jsonl"))
	if msg != "done" {
		t.Errorf("a later successful stop replaces the error: Tail msg = %q", msg)
	}
	dir := t.TempDir()
	multi := filepath.Join(dir, "multi.jsonl")
	os.WriteFile(multi, []byte(`{"type":"message","message":{"role":"assistant","stopReason":"error","errorMessage":"429 rate limited\nretry later","content":[]}}`+"\n"), 0o644)
	if _, msg = p.(Telemetry).Tail(multi); msg != "error: 429 rate limited" {
		t.Errorf("multi-line error: Tail msg = %q", msg)
	}
	length := filepath.Join(dir, "length.jsonl")
	lines := endLines(t, "pi-length-recovery.jsonl")[:3]
	os.WriteFile(length, []byte(string(lines[0])+"\n"+string(lines[1])+"\n"+string(lines[2])+"\n"), 0o644)
	if _, msg = p.(Telemetry).Tail(length); msg != "length: output cut at the token limit" {
		t.Errorf("length: Tail msg = %q", msg)
	}
}

func TestPiTailClearsARecoveredStop(t *testing.T) {
	p := For("pi")
	dir := t.TempDir()
	const (
		user   = `{"type":"message","id":"u1","message":{"role":"user","content":[{"type":"text","text":"go"}]}}`
		said   = `{"type":"message","id":"a0","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"text","text":"checking the tree"},{"type":"toolCall","name":"bash","arguments":{"command":"pwd"}}]}}`
		failed = `{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"error","errorMessage":"429 rate limited","content":[]}}`
		cut    = `{"type":"message","id":"a1","message":{"role":"assistant","stopReason":"length","content":[]}}`
		edit   = `{"type":"context_edit","id":"e1","targetId":"a1","replacement":null}`
		call   = `{"type":"message","id":"a2","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","name":"bash","arguments":{"command":"ls"}}]}}`
	)
	for i, c := range []struct {
		name           string
		lines          []string
		wantTool, want string
	}{
		{"error, context_edit, then a tool-call-only turn", []string{user, failed, edit, call}, `bash {"command":"ls"}`, ""},
		{"the text before the error comes back", []string{user, said, failed, edit, call}, `bash {"command":"ls"}`, "checking the tree"},
		{"length, context_edit, then a tool-call-only turn", []string{user, cut, edit, call}, `bash {"command":"ls"}`, ""},
		{"an error still last is still named", []string{user, said, failed}, `bash {"command":"pwd"}`, "error: 429 rate limited"},
	} {
		path := filepath.Join(dir, "case"+strconv.Itoa(i)+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(c.lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		tool, msg := p.(Telemetry).Tail(path)
		if tool != c.wantTool || msg != c.want {
			t.Errorf("%s: Tail = %q, %q; want %q, %q", c.name, tool, msg, c.wantTool, c.want)
		}
	}
}

// fakeProc is a /proc with one directory per put: environ, cmdline (NUL
// separated args) and a status with no capability, each written when given.
type fakeProc string

func (p fakeProc) put(t *testing.T, pid, env string, args ...string) string {
	t.Helper()
	dir := filepath.Join(string(p), pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"status": "Name:\tx\nCapPrm:\t0000000000000000\n"}
	if env != "" {
		files["environ"] = env
	}
	if len(args) > 0 {
		files["cmdline"] = strings.Join(args, "\x00") + "\x00"
	}
	for f, body := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPiChildLiveFakeProc(t *testing.T) {
	proc := fakeProc(t.TempDir())
	launched := []string{"nohup", "pi", "-p", "-ne", "--session-id", "wf-x", "--", "b"}
	proc.put(t, "1", "A=1\x00SUPER_PI_CHILD=wf-x\x00", launched...)
	proc.put(t, "2", "SUPER_PI_CHILD=wf-xy\x00", "pi")
	proc.put(t, "3", "") // a pid with no environ: it exited mid-scan
	proc.put(t, "self", "SUPER_PI_CHILD=wf-self\x00", "pi")
	proc.put(t, "4", "SUPER_PI_CHILD=wf-d\x00", "bash", "-c", "sleep 99")         // a descendant of wf-d's pi
	proc.put(t, "5", "SUPER_PI_CHILD=wf-r\x00", "pi", "", "", "")                 // wf-r's pi after its retitle
	proc.put(t, "6", "SUPER_PI_CHILD=wf-s\x00", "pi", "--session-id", "wf-other") // a pi of another session
	proc.put(t, "7", "SUPER_PI_CHILD=wf-g\x00")                                   // its command line exited mid-scan
	for child, want := range map[string]bool{
		"wf-x":    true,  // the launched process, by its arguments
		"wf-r":    true,  // the launched process, retitled
		"wf-y":    false, // no holder
		"wf-self": false, // a non-numeric entry is skipped
		"wf-d":    false, // only a descendant holds the marker
		"wf-s":    false, // the holder names another session
		"wf-g":    false,
	} {
		if live, err := piChildLive(string(proc), child); live != want || err != nil {
			t.Errorf("%s = %v, %v; want %v, nil", child, live, err, want)
		}
	}
	// wf-x is a prefix of the entry wf-xy holds; querying the prefix of
	// wf-x's own id finds nothing either.
	only := fakeProc(t.TempDir())
	only.put(t, "1", "SUPER_PI_CHILD=wf-xy\x00", "nohup", "pi", "--session-id", "wf-xy")
	if live, err := piChildLive(string(only), "wf-x"); live || err != nil {
		t.Errorf("a holder of wf-xy queried for wf-x = %v, %v; want not live", live, err)
	}
	if live, err := piChildLive(string(proc), "wf-"); live || err != nil {
		t.Errorf("wf- = %v, %v; want not live", live, err)
	}
	if _, err := piChildLive(filepath.Join(string(proc), "missing"), "wf-x"); err == nil {
		t.Error("a proc that cannot be listed is an error")
	}
}

// A same-user process whose environment cannot be read could be the child:
// the scan is incomplete, so the answer is unknown (an error), never dead.
// One holding capabilities, or another uid's, cannot be the launched pi.
func TestPiChildLiveIncompleteVisibilityIsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	proc := fakeProc(t.TempDir())
	hidden := proc.put(t, "8", "SUPER_PI_CHILD=wf-x\x00", "pi")
	if err := os.Chmod(filepath.Join(hidden, "environ"), 0); err != nil {
		t.Fatal(err)
	}
	if live, err := piChildLive(string(proc), "wf-x"); live || err == nil {
		t.Fatalf("an unreadable same-uid environ = %v, %v; want unknown (an error)", live, err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "status"), []byte("CapPrm:\t0000000800000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if live, err := piChildLive(string(proc), "wf-x"); live || err != nil {
		t.Errorf("an unreadable process holding capabilities = %v, %v; want skipped", live, err)
	}

	proc = fakeProc(t.TempDir())
	cmd := proc.put(t, "9", "SUPER_PI_CHILD=wf-x\x00", "pi")
	if err := os.Chmod(filepath.Join(cmd, "cmdline"), 0); err != nil {
		t.Fatal(err)
	}
	if live, err := piChildLive(string(proc), "wf-x"); live || err == nil {
		t.Errorf("a marker holder whose command line cannot be read = %v, %v; want unknown", live, err)
	}
}

func TestPiChildLiveRealScan(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	id := "wf-x" + strconv.Itoa(os.Getpid())
	// The shell stands in for the launched pi (its arguments hold
	// --session-id <id>); the sleep is a descendant that inherits the marker.
	cmd := exec.Command("sh", "-c", "sleep 30; true", "sh", "--session-id", id)
	cmd.Env = append(os.Environ(), piChildMarker(id))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	defer func() { syscall.Kill(-pgid, syscall.SIGKILL); cmd.Wait() }()
	p := For("pi").(SettlingEnd)
	if live, err := p.ChildLive(id); err != nil || !live {
		t.Fatalf("running: ChildLive = %v, %v; want true, nil", live, err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	if live, err := p.ChildLive(id); err != nil || live {
		t.Fatalf("exited, its descendant still running: ChildLive = %v, %v; want false, nil", live, err)
	}
}

func TestPiMarkerIsWhatChildLiveMatches(t *testing.T) {
	const claim = "0a1b2c3d4e5f6071"
	id := "wf-" + claim
	spawn := pi{}.SpawnLine(Spawn{Brief: "b", Claim: claim, ErrLog: "/s/" + id + ".stderr"})
	resume := pi{}.ResumeLine(id, "m")
	marker := piChildMarker(id)
	for name, line := range map[string]string{"SpawnLine": spawn, "ResumeLine": resume} {
		if !strings.HasPrefix(line, marker+" ") {
			t.Errorf("%s = %q, want it to start with %q", name, line, marker+" ")
		}
	}
	proc := fakeProc(t.TempDir())
	first, _, _ := strings.Cut(spawn, " ")
	proc.put(t, "7", "X=1\x00"+first+"\x00", "pi")
	if live, err := piChildLive(string(proc), id); !live || err != nil {
		t.Errorf("the entry SpawnLine emits must be the one ChildLive matches: %v, %v", live, err)
	}
}
