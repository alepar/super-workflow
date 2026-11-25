package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

// hostileCall parses a line of the form Name(k="v", k="v") as a coordinator
// reads it into a tool call: each value is the quoted token after its
// key, read as a JSON string.
func hostileCall(line, name string) (map[string]string, error) {
	rest, ok := strings.CutPrefix(line, name+"(")
	if !ok {
		return nil, fmt.Errorf("line does not start with %s(", name)
	}
	out := map[string]string{}
	for {
		k, after, ok := strings.Cut(rest, "=")
		if !ok {
			return nil, fmt.Errorf("no key=value in %q", rest)
		}
		tok, err := strconv.QuotedPrefix(after)
		if err != nil {
			return nil, fmt.Errorf("value of %s is not one quoted token: %v", k, err)
		}
		var v string
		if err := json.Unmarshal([]byte(tok), &v); err != nil {
			return nil, fmt.Errorf("value of %s, %s, is not a JSON string: %v", k, tok, err)
		}
		out[k] = v
		rest = after[len(tok):]
		if rest == ")" {
			return out, nil
		}
		if rest, ok = strings.CutPrefix(rest, ", "); !ok {
			return nil, fmt.Errorf("unexpected text after %s: %q", k, rest)
		}
	}
}

// Claude's and Codex's spawn and resume lines carry each value as a quoted
// argument the coordinator copies into a JSON tool call.
func TestHostileQuotedToolCallLines(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			v := in.Value
			if v == "" {
				t.Skip("an empty value is left out of the line")
			}
			for _, c := range []struct {
				what, line, name string
				want             map[string]string
			}{
				{"claude spawn", claude{}.SpawnLine(Spawn{Brief: v, Model: v, Effort: v, AgentType: v}), "Agent",
					map[string]string{"subagent_type": v, "model": v, "effort": v, "prompt": v}},
				{"claude resume", claude{}.ResumeLine(v, v), "SendMessage", map[string]string{"to": v, "message": v}},
				{"codex spawn", codex{}.SpawnLine(Spawn{Brief: v, Model: v, Effort: v}), "spawn_agent",
					map[string]string{"message": v, "model": v, "reasoning_effort": v, "fork_turns": "none"}},
			} {
				got, err := hostileCall(c.line, c.name)
				if err != nil {
					if !utf8.ValidString(v) {
						continue // invalid UTF-8 has no JSON form at all
					}
					t.Errorf("%s: %v\nline: %s", c.what, err, c.line)
					continue
				}
				for k, w := range c.want {
					// JSON cannot carry invalid UTF-8: the encoder writes
					// U+FFFD, as every reader would.
					if w = hostileJSON(w); got[k] != w {
						t.Errorf("%s: %s = %q, want %q", c.what, k, got[k], w)
					}
				}
			}
		})
	}
}

func hostileJSON(v string) string {
	if utf8.ValidString(v) {
		return v
	}
	b, _ := json.Marshal(v)
	var out string
	_ = json.Unmarshal(b, &out)
	return out
}

// agy's and Codex's JSON lines decode to the values given.
func TestHostileJSONToolCallLines(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			v, want := in.Value, hostileJSON(in.Value)
			var spawn agySpawnArgs
			line := agy{}.SpawnLine(Spawn{Brief: v, Model: v})
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "invoke_subagent ")), &spawn); err != nil || spawn.Prompt != want || v != agyInherit && spawn.Model != want {
				t.Errorf("agy spawn %s decodes as %+v (%v)", line, spawn, err)
			}
			if v == "" {
				return // no agent to resume
			}
			var send struct{ Recipient, Message string }
			line = agy{}.ResumeLine(v, v)
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "send_message ")), &send); err != nil || send.Recipient != want || send.Message != want {
				t.Errorf("agy resume %s decodes as %+v (%v)", line, send, err)
			}
			var follow struct{ Target, Message string }
			line = codex{}.ResumeLine(v, v)
			body, ok := strings.CutPrefix(line, "collaboration.followup_task(")
			body, ok2 := strings.CutSuffix(body, ")")
			if err := json.Unmarshal([]byte(body), &follow); !ok || !ok2 || err != nil || follow.Target != want || follow.Message != want {
				t.Errorf("codex resume %s decodes as %+v (%v)", line, follow, err)
			}
		})
	}
}

// The generic spawn line ends with the brief, whole.
func TestHostileGenericSpawnLine(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			line := Generic{}.SpawnLine(Spawn{Brief: in.Value})
			_, brief, ok := strings.Cut(line, "its whole prompt being: ")
			if !ok || brief != in.Value {
				t.Errorf("generic spawn line %q carries brief %q", line, brief)
			}
		})
	}
}

// hostilePi runs a pi launch or resume line in bash, as a coordinator's
// shell tool does, with a fake pi on PATH that records its argv, stdin and
// child marker. It reports whether pi ran.
type hostilePiRun struct {
	argv  []string
	stdin string
	child string
}

func hostilePiShell(t *testing.T, line string) (hostilePiRun, bool, string) {
	t.Helper()
	dir := t.TempDir()
	bin, out, cwd := filepath.Join(dir, "bin"), filepath.Join(dir, "out"), filepath.Join(dir, "cwd")
	for _, d := range []string{bin, out, cwd} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake := "#!/bin/sh\nprintf '%s\\0' \"$@\" > \"$HOSTILE_OUT/argv\"\ncat > \"$HOSTILE_OUT/stdin\"\n" +
		"printf '%s' \"$SUPER_PI_CHILD\" > \"$HOSTILE_OUT/child\"\necho pi-stderr >&2\n"
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", line+"\nwait")
	cmd.Dir = cwd
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + dir, "HOSTILE_OUT=" + out, "PI_SESSION_ID=parent-session"}
	shellOut, _ := cmd.CombinedOutput()
	argv, err := os.ReadFile(filepath.Join(out, "argv"))
	if err != nil {
		return hostilePiRun{}, false, string(shellOut)
	}
	stdin, _ := os.ReadFile(filepath.Join(out, "stdin"))
	child, _ := os.ReadFile(filepath.Join(out, "child"))
	return hostilePiRun{strings.Split(strings.TrimSuffix(string(argv), "\x00"), "\x00"), string(stdin), string(child)}, true, string(shellOut)
}

// pi's launch line, run in bash, starts pi with the model, effort and
// session id as single arguments, the brief verbatim on stdin and its
// stderr in ErrLog.
func TestHostilePiSpawnLineInShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			v := in.Value
			if strings.ContainsRune(v, 0) {
				t.Skip("a shell command line cannot carry NUL")
			}
			errLog := filepath.Join(t.TempDir(), "err.log")
			base := Spawn{Brief: "do the work", Claim: "c1", Model: "m", Effort: "high", ErrLog: errLog}
			for _, c := range []struct {
				field string
				set   func(*Spawn)
			}{
				{"Brief", func(s *Spawn) { s.Brief = v }},
				{"Model", func(s *Spawn) { s.Model = v }},
				{"Effort", func(s *Spawn) { s.Effort = v }},
				{"Claim", func(s *Spawn) { s.Claim = v }},
				{"ErrLog", func(s *Spawn) {
					if v != "" && v != "." && v != ".." && !strings.Contains(v, "/") {
						s.ErrLog = filepath.Join(filepath.Dir(errLog), v)
					}
				}},
			} {
				s := base
				c.set(&s)
				if c.field == "Brief" && (pi{}).CheckBrief(s.Brief) != nil {
					continue // refused before any line is printed
				}
				if s.Model == "" || s.Effort == "" || s.Claim == "" {
					continue // an empty field is left out of the line
				}
				got, ran, shellOut := hostilePiShell(t, pi{}.SpawnLine(s))
				if !ran {
					t.Errorf("%s %q: pi never ran; shell said %q", c.field, v, shellOut)
					continue
				}
				want := []string{"-p", "-ne", "--session-id", "wf-" + s.Claim, "--model", s.Model, "--thinking", s.Effort}
				if !slices.Equal(got.argv, want) {
					t.Errorf("%s %q: pi argv %q, want %q", c.field, v, got.argv, want)
				}
				if got.stdin != s.Brief+"\n" {
					t.Errorf("%s %q: pi stdin %q, want the brief %q", c.field, v, got.stdin, s.Brief)
				}
				if got.child != "wf-"+s.Claim {
					t.Errorf("%s %q: SUPER_PI_CHILD %q, want %q", c.field, v, got.child, "wf-"+s.Claim)
				}
				if b, err := os.ReadFile(s.ErrLog); err != nil || !strings.Contains(string(b), "pi-stderr") {
					t.Errorf("%s %q: stderr not in ErrLog %q (%v)", c.field, v, s.ErrLog, err)
				}
			}
		})
	}
}

// pi's resume line, run in bash, passes the session id and the message
// as single arguments.
func TestHostilePiResumeLineInShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			v := in.Value
			if strings.ContainsRune(v, 0) {
				t.Skip("a shell command line cannot carry NUL")
			}
			for _, c := range []struct{ field, agent, message string }{
				{"message", "wf-c1", v},
				{"agent", v, "go on"},
			} {
				line := pi{}.ResumeLine(c.agent, c.message)
				if c.agent == "" {
					// No session to resume: no line, as for claude and agy.
					if line != "" {
						t.Errorf("ResumeLine with no agent = %q, want none", line)
					}
					continue
				}
				got, ran, shellOut := hostilePiShell(t, line)
				if !ran {
					t.Errorf("%s %q: pi never ran; shell said %q", c.field, v, shellOut)
					continue
				}
				want := []string{"-p", "-ne", "--session-id", c.agent, "--", c.message}
				if !slices.Equal(got.argv, want) {
					t.Errorf("%s %q: pi argv %q, want %q", c.field, v, got.argv, want)
				}
				if got.child != c.agent {
					t.Errorf("%s %q: SUPER_PI_CHILD %q, want %q", c.field, v, got.child, c.agent)
				}
			}
		})
	}
}
