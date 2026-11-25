package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const piBrief = "workflow prompt run-1 --claim 0a1b2c3d4e5f6071 --state-dir /tmp/s\nYou are a child agent for one unit of work. Run the command above, then do what it prints."

func TestPiDetectionAndCaller(t *testing.T) {
	if got := Detect(envMap(map[string]string{"PI_SESSION_ID": "p", "CLAUDECODE": "1", "CODEX_THREAD_ID": "c"})); got != "pi" {
		t.Fatalf("pi run from a Claude or Codex tab is pi; Detect = %q", got)
	}
	if got := Detect(envMap(map[string]string{"PI_SESSION_ID": "p", "HERMES_SESSION_ID": "h"})); got != "hermes" {
		t.Fatalf("Hermes stays first; Detect = %q", got)
	}
	self := For("pi").Caller(envMap(map[string]string{"PI_SESSION_ID": "coord", "PI_CODING_AGENT_DIR": "/agent"}))
	if self.Is != IsSelf || self.Session != "coord" || self.Agent != "" || self.Root != "/agent" {
		t.Fatalf("coordinator caller = %+v", self)
	}
	kid := For("pi").Caller(envMap(map[string]string{"PI_SESSION_ID": "kid", "SUPER_PI_PARENT": "coord", "HOME": "/example/x"}))
	if kid.Is != IsChild || kid.Session != "coord" || kid.Agent != "kid" || kid.Root != "/example/x/.pi/agent" {
		t.Fatalf("child caller = %+v", kid)
	}
	if c := For("pi").Caller(envMap(map[string]string{"SUPER_PI_PARENT": "coord"})); c.Is != IsUnknown || c.Session != "" {
		t.Fatalf("no pi session, no identity: %+v", c)
	}
}

const piToken = "0a1b2c3d4e5f6071"

// A coordinator launch and child session agree on the claim, working
// directory, stderr location, and kickoff brief.
func TestPiRecordedProbe(t *testing.T) {
	const coord, kid, token = "fixture-pi-parent", "wf-1011121314151617", "1011121314151617"
	root := t.TempDir()
	dir := filepath.Join(root, "sessions", "--work--")
	for src, dst := range map[string]string{
		"pi-coordinator.jsonl": "2026-11-19T06-45-48-625Z_" + coord + ".jsonl",
		"pi-child.jsonl":       "2026-11-19T06-45-50-124Z_" + kid + ".jsonl",
		"pi-child-read.jsonl":  "2025-01-01T00-00-00-000Z_wf-0123456789abcdef.jsonl",
	} {
		b, err := os.ReadFile(filepath.Join("testdata", "started", src))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, dst), string(b))
	}
	a := For("pi")
	w := Want{Token: token}
	const errLog = "/nonexistent/child-logs/example/wf-1011121314151617.stderr"
	if _, ok := a.Resolve(root, coord, []Want{{Token: token, ErrLog: errLog}})[Want{Token: token, ErrLog: errLog}]; !ok {
		t.Fatal("the recorded launch writes the engine's stderr path: found")
	}
	if wrong := (Want{Token: token, ErrLog: "/elsewhere/child-logs/probe-run/wf-1011121314151617.stderr"}); len(a.Resolve(root, coord, []Want{wrong})) != 0 {
		t.Fatal("a different recorded stderr path must not find the child")
	}
	got := a.Resolve(root, coord, []Want{w})[w]
	if got.Agent != kid || !strings.HasSuffix(got.Transcript, "_"+kid+".jsonl") {
		t.Fatalf("Resolve = %+v", got)
	}
	if v := a.(SettlingEnd).EndVerdict(got.Transcript); v != EndIfSettled {
		t.Fatalf("EndVerdict = %v: the recorded child stopped with no tool call, so it ends after the settle (EndIfSettled)", v)
	}
	if n, ok := a.(Telemetry).Usage(got.Transcript); !ok || n <= 0 {
		t.Fatalf("Usage = %d, %v", n, ok)
	}
	if a.(Telemetry).CoordinatorTranscript(root, coord) == "" {
		t.Fatal("CoordinatorTranscript")
	}
	readChild := filepath.Join(dir, "2025-01-01T00-00-00-000Z_wf-0123456789abcdef.jsonl")
	body := "/work/fixture/runs/sample/prompt/0123456789abcdef/prompt.md"
	if read, known := a.ReadAny(readChild, []string{body}); !read || !known {
		t.Fatalf("ReadAny = %v, %v", read, known)
	}
	if read, known := a.ReadAny(readChild, []string{"/elsewhere/prompt.md"}); read || !known {
		t.Fatalf("ReadAny(other) = %v, %v", read, known)
	}
	tool, msg := a.(Telemetry).Tail(readChild)
	if !strings.HasPrefix(tool, "read ") || msg != "DONE" {
		t.Fatalf("Tail = %q, %q", tool, msg)
	}
}

// piSession writes a session file named and headed by id (or headed by
// head when given), begun at created: its header, then each record.
func piSession(t *testing.T, root, id string, created time.Time, records ...map[string]any) string {
	t.Helper()
	return piSessionHeaded(t, root, id, id, created, records...)
}

func piSessionHeaded(t *testing.T, root, name, head string, created time.Time, records ...map[string]any) string {
	t.Helper()
	stamp := created.UTC().Format(time.RFC3339Nano)
	lines := []string{`{"type":"session","version":3,"id":"` + head + `","timestamp":"` + stamp + `","cwd":"/work"}`}
	for _, r := range records {
		if _, ok := r["timestamp"]; !ok {
			r["timestamp"] = stamp
		}
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	path := filepath.Join(root, "sessions", "--work--", "2026-11-19T00-00-00-000Z_"+name+".jsonl")
	writeFile(t, path, strings.Join(lines, "\n")+"\n")
	return path
}

func piCommand(command string, at time.Time) map[string]any {
	return map[string]any{"type": "message", "timestamp": at.UTC().Format(time.RFC3339Nano), "message": map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{
		map[string]any{"type": "toolCall", "name": "bash", "arguments": map[string]any{"command": command}}}}}
}

func piKickoff(brief string) map[string]any {
	return map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": brief}}}}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPiCdIsOneLiteralAbsoluteDir(t *testing.T) {
	const line = `SUPER_PI_CHILD=wf-k SUPER_PI_PARENT="$PI_SESSION_ID" nohup pi -p -ne --session-id wf-k -- 'a && b' </dev/null >/dev/null 2>/s/wf-k.stderr &`
	ok := map[string]string{
		line:                          "",
		"cd /work/repo && " + line:    "/work/repo",
		"cd '/work/my repo'&&" + line: "/work/my repo",
		`cd "/work/x/" && ` + line:    "/work/x",
		"cd '/a && b' && " + line:     "/a && b",
		`cd /a\&\&b && ` + line:       "/a&&b",
	}
	for c, want := range ok {
		if dir, rest, good := piCd(c); !good || dir != want || rest != line {
			t.Errorf("piCd(%q) = %q, %q, %v", c, dir, rest, good)
		}
	}
	for _, c := range []string{
		"cd work && " + line,
		"cd /a /b && " + line,
		"cd -P /a && " + line,
		"cd $(mktemp -d) && " + line,
		"cd /a; true && " + line,
		"cd /a || true && " + line,
		"cdx /a && " + line,
		"cd /a #&& " + line,
		"cd /a # x && " + line,
		"cd /a#b && " + line,
		"cd /a\n# && " + line,
	} {
		if dir, rest, good := piCd(c); good {
			t.Errorf("piCd accepted %q: %q, %q", c, dir, rest)
		}
	}
}
