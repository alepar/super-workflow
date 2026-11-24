package wf

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

func TestChildErrLogLayout(t *testing.T) {
	c := &cycle{e: &Engine{opts: EngineOptions{StateRoot: "/s"}}, run: "r1"}
	if got, want := c.childErrLog("0a1b"), "/s/child-logs/r1/wf-0a1b.stderr"; got != want {
		t.Errorf("childErrLog = %q, want %q", got, want)
	}
	c = &cycle{e: &Engine{}, run: "r1"}
	if got := c.childErrLog("0a1b"); got != "" {
		t.Errorf("childErrLog with no state root = %q, want empty", got)
	}
}

func TestChildErrLogMatchesPiSessionID(t *testing.T) {
	const claim = "0a1b2c3d4e5f6071"
	c := &cycle{e: &Engine{opts: EngineOptions{StateRoot: "/s"}}, run: "r1"}
	line := harness.For("pi").SpawnLine(harness.Spawn{Brief: "b", Claim: claim, ErrLog: "/x/y.stderr"})
	m := regexp.MustCompile(`--session-id (\S+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no --session-id in %q", line)
	}
	if got, want := filepath.Base(c.childErrLog(claim)), m[1]+".stderr"; got != want {
		t.Errorf("helper base name = %q, pi's session id plus .stderr = %q", got, want)
	}
}

func TestErrLogPathOK(t *testing.T) {
	for p, want := range map[string]bool{
		"/s/child-logs/r/wf-0a.stderr": true,
		"/s s/child-logs/r/x.stderr":   false,
		"rel/x.stderr":                 false,
		"/s/$x":                        false,
		"/s/'x":                        false,
		"/s/é":                         false,
		"":                             false,
	} {
		if got := errLogPathOK(p); got != want {
			t.Errorf("errLogPathOK(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestErrLogExcerpt(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	long := strings.Repeat("x", 250)
	got := errLogExcerpt(write("a", "line1\n\nline2\n"+long+"\n   \nline4\n"))
	if len(got) != 3 || got[0] != "line2" || got[2] != "line4" {
		t.Fatalf("excerpt = %q, want line2, a cut line, line4", got)
	}
	if want := strings.Repeat("x", 197) + "…"; got[1] != want || len(got[1]) != 200 {
		t.Errorf("cut line = %q (%d bytes), want 197 x and …", got[1], len(got[1]))
	}
	if got := errLogExcerpt(write("empty", "")); got != nil {
		t.Errorf("empty file excerpt = %q, want nil", got)
	}
	if got := errLogExcerpt(write("blank", "\n  \n")); got != nil {
		t.Errorf("blank-lines file excerpt = %q, want nil", got)
	}
	if got := errLogExcerpt(filepath.Join(dir, "missing")); got != nil {
		t.Errorf("missing file excerpt = %q, want nil", got)
	}
	// Multi-byte runes at the cut are never split, at every offset.
	for pad := 0; pad < 4; pad++ {
		l := strings.Repeat("x", 190+pad) + strings.Repeat("é", 20)
		cut := errLogExcerpt(write("m", l+"\n"))
		if len(cut) != 1 || len(cut[0]) > 200 || !utf8.ValidString(cut[0]) || !strings.HasSuffix(cut[0], "…") {
			t.Errorf("pad %d: excerpt %q (%d bytes), want valid UTF-8 of at most 200 bytes ending in …", pad, cut, len(cut[0]))
		}
	}
}

// The adapter's launch matching takes the recorded ErrLog: a child whose
// launch wrote stderr to another directory does not resolve, the exact one
// does.
func TestResolveWithRecordedErrLog(t *testing.T) {
	const claim, coord = "0a1b2c3d4e5f6071", "coord"
	kid := "wf-" + claim
	root := t.TempDir()
	at := time.Now().Add(-time.Minute).UTC()
	recorded := "/s/child-logs/r1/wf-" + claim + ".stderr"
	elsewhere := "/elsewhere/child-logs/r1/wf-" + claim + ".stderr"
	brief := "wf prompt --claim " + claim + "\n"
	pi := harness.For("pi")
	cmd := func(errLog string) map[string]any {
		return map[string]any{"type": "message", "timestamp": at.Add(10 * time.Second).Format(time.RFC3339Nano), "message": map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{
			map[string]any{"type": "toolCall", "name": "bash", "arguments": map[string]any{"command": pi.SpawnLine(harness.Spawn{Brief: brief, Claim: claim, ErrLog: errLog})}}}}}
	}
	kick := func(text string) map[string]any {
		return map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	session := func(id string, created time.Time, rs ...map[string]any) {
		lines := []string{`{"type":"session","version":3,"id":"` + id + `","timestamp":"` + created.Format(time.RFC3339Nano) + `","cwd":"/work"}`}
		for _, r := range rs {
			if _, ok := r["timestamp"]; !ok {
				r["timestamp"] = created.Format(time.RFC3339Nano)
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
	session(coord, at, kick("run the workflow"), cmd(elsewhere))
	session(kid, at.Add(11*time.Second), kick(brief))
	// A wanted child of the same claim resolves only with the recorded path.
	w := harness.Want{Token: claim, After: at, ErrLog: recorded}
	if _, ok := pi.Resolve(root, coord, []harness.Want{w})[w]; ok {
		t.Error("Resolve found a child launched with another stderr directory")
	}
	w.ErrLog = elsewhere
	if _, ok := pi.Resolve(root, coord, []harness.Want{w})[w]; !ok {
		t.Error("Resolve did not find the child launched with the recorded stderr path")
	}
}

// The excerpt follows no link: a symlinked stderr file, a file in a
// symlinked directory and a FIFO give no excerpt (and the FIFO no hang).
func TestErrLogExcerptFollowsNoLink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("secret line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(dir, "logs")
	if err := os.Mkdir(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(logs, "wf-a.stderr")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if got := errLogExcerpt(link); got != nil {
		t.Errorf("a symlinked stderr file was followed: %q", got)
	}
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "wf-b.stderr"), []byte("hidden\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if got := errLogExcerpt(filepath.Join(dir, "linked", "wf-b.stderr")); got != nil {
		t.Errorf("a file in a symlinked directory was read: %q", got)
	}
	fifo := filepath.Join(logs, "wf-c.stderr")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := errLogExcerpt(fifo); got != nil {
		t.Errorf("a FIFO gave an excerpt: %q", got)
	}
}

// Only the tail of a large stderr file is read, and the partial line it
// starts inside is dropped.
func TestErrLogTailIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big")
	body := "HEAD-MARK\n" + strings.Repeat("filler line\n", 100000) + "almost\nlast\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	b, ok := errLogTail(p, errLogTailBytes)
	if !ok || len(b) > errLogTailBytes || len(b) == 0 {
		t.Fatalf("errLogTail = %d bytes, %v; want at most %d", len(b), ok, errLogTailBytes)
	}
	if strings.Contains(string(b), "HEAD-MARK") || !strings.HasPrefix(string(b), "filler line\n") || !strings.HasSuffix(string(b), "almost\nlast\n") {
		t.Errorf("errLogTail = %q...%q, want whole lines up to the end", string(b)[:24], string(b)[len(b)-24:])
	}
	if got := errLogExcerpt(p); len(got) != 3 || got[2] != "last" || got[1] != "almost" {
		t.Errorf("excerpt of a large file = %q", got)
	}
	small := filepath.Join(t.TempDir(), "small")
	if err := os.WriteFile(small, []byte("one\ntwo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, ok := errLogTail(small, errLogTailBytes); !ok || string(b) != "one\ntwo" {
		t.Errorf("a file under the limit = %q, %v; want it whole", b, ok)
	}
}

func TestEscapeControl(t *testing.T) {
	in := "a\x1b[31mb\x07\x7f\u009bc\xffd\té"
	want := `a\x1b[31mb\x07\x7f\u009bc\xffd\x09é`
	if got := escapeControl(in); got != want {
		t.Errorf("escapeControl(%q) = %q, want %q", in, got, want)
	}
	p := filepath.Join(t.TempDir(), "esc")
	if err := os.WriteFile(p, []byte("\x1b[2Jcleared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := errLogExcerpt(p); len(got) != 1 || got[0] != `\x1b[2Jcleared` {
		t.Errorf("excerpt = %q, want the escape written out", got)
	}
}

func TestMakeChildLogDirRefusesALink(t *testing.T) {
	state := t.TempDir()
	if err := makeChildLogDir(state, filepath.Join(state, "child-logs", "r1")); err != nil {
		t.Fatalf("a fresh child log dir: %v", err)
	}
	if err := makeChildLogDir(state, filepath.Join(state, "child-logs", "r1")); err != nil {
		t.Fatalf("an existing child log dir: %v", err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(state, "child-logs", "r2")); err != nil {
		t.Fatal(err)
	}
	if err := makeChildLogDir(state, filepath.Join(state, "child-logs", "r2")); !errors.Is(err, errNotRealDir) {
		t.Errorf("a symlinked run dir = %v, want errNotRealDir", err)
	}
	if err := os.WriteFile(filepath.Join(state, "child-logs", "r3"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := makeChildLogDir(state, filepath.Join(state, "child-logs", "r3")); !errors.Is(err, errNotRealDir) {
		t.Errorf("a file in the dir's place = %v, want errNotRealDir", err)
	}
	if err := makeChildLogDir(state, "/elsewhere/child-logs/r1"); err == nil {
		t.Error("a dir outside the state dir was accepted")
	}
}
