package wf

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

// Round-trip tests over the shared hostile corpus: every input is rendered
// through the real renderer and parsed back with the format's parser (sh for
// shell lines, encoding/json for JSON, the filesystem rules for paths, a
// precise line or heading check for text). A renderer may refuse an input
// with an error; silent corruption fails.

// oneLine is v with its line breaks made spaces: the baseline a renderer
// that keeps v on its line renders the same number of lines for.
func oneLine(v string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(v)
}

// hostileShells is every POSIX shell on the host the shell tests run under.
func hostileShells(t *testing.T) []string {
	var out []string
	for _, sh := range []string{"sh", "bash", "dash"} {
		if p, err := exec.LookPath(sh); err == nil {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Skip("no POSIX shell")
	}
	return out
}

// hostileArgv runs line under shell with fake (and cd) defined as functions
// that print their arguments NUL-separated, and returns the argv the shell
// built. An error means the rendered line could not be run at all.
func hostileArgv(shell, line string) ([]string, error) {
	prelude := `fake() { for a in "$@"; do printf '%s\0' "$a"; done; }; cd() { printf '%s\0' cd "$@"; }; `
	out, err := exec.Command(shell, "-c", prelude+line).Output()
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return []string{}, nil
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"), nil
}

// skipNUL skips a value holding a NUL byte: no shell word or argument can
// carry one, so the engine refuses it where it enters (run ids, Map keys,
// command arguments) and a shell renderer never sees it.
func skipNUL(t *testing.T, v string) {
	t.Helper()
	if strings.IndexByte(v, 0) >= 0 {
		t.Skip("NUL is refused at the value's source")
	}
}

// checkShell runs line under every shell and requires argv want.
func checkShell(t *testing.T, line string, want []string) {
	t.Helper()
	for _, sh := range hostileShells(t) {
		got, err := hostileArgv(sh, line)
		if err != nil {
			t.Errorf("%s: rendered line does not run (%v): %q", filepath.Base(sh), err, line)
			continue
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: argv = %q, want %q (line %q)", filepath.Base(sh), got, want, line)
		}
	}
}

func TestHostileShellQuote(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			skipNUL(t, in.Value)
			checkShell(t, "fake "+sink.Quote(in.Value), []string{in.Value})
		})
	}
}

func TestHostileLineArg(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			skipNUL(t, in.Value)
			checkShell(t, "fake "+lineArg(in.Value), []string{in.Value})
		})
	}
}

func TestHostileMultiVerbLine(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			skipNUL(t, in.Value)
			v := in.Value
			line := multiVerbLine("fake", v, RunID(v), "done", v, "--output", v)
			checkShell(t, line, []string{"done", v, v, "--output", v, "--state-dir", v})
		})
	}
}

func TestHostileWfLine(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			skipNUL(t, in.Value)
			v := in.Value
			line := wfLine("fake", v, RunID(v), "prompt", "--claim", v, v)
			want := []string{"wf", "prompt", "--claim", v, "--state-dir", v, v}
			if v != "" {
				want = append(want, "--run-id", v)
			}
			checkShell(t, line, want)
		})
	}
}

func TestHostileWorkspaceCommand(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			skipNUL(t, in.Value)
			v := in.Value
			line := workspaceCommand("", v, "fake x")
			want := []string{"cd", v, "x"}
			if v == "" {
				want = []string{"x"}
			}
			checkShell(t, line, want)
		})
	}
}

// gateIntInstructions splices a shell-quoted answer into the submit line and
// also names the answer in the prose line above it. The prose must keep its
// lines; the command line carries the answer as one shell word, a line break
// in it included (checkShell reads it back).
func TestHostileGateIntInstructions(t *testing.T) {
	p := 3
	prose := func(answer string) string {
		out := gateIntInstructions([]GateIntValueView{{Answer: answer, Min: 0, Max: 9, Proposal: &p}}, "fake answer --value <value>")
		i := strings.LastIndex(out, "\nfake ")
		if i < 0 {
			t.Fatalf("no command line in %q", out)
		}
		return out[:i]
	}
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			skipNUL(t, in.Value)
			base := strings.Count(prose(oneLine(in.Value)), "\n")
			if text := prose(in.Value); strings.Count(text, "\n") != base {
				t.Errorf("prose line injection: %d line breaks, want %d:\n%s", strings.Count(text, "\n"), base, text)
			}
			out := gateIntInstructions([]GateIntValueView{{Answer: in.Value, Min: 0, Max: 9, Proposal: &p}}, "fake answer --value <value>")
			i := strings.LastIndex(out, "\nfake ")
			line := strings.TrimSuffix(out[i+1:], " --number <integer>")
			checkShell(t, line, []string{"answer", "--value", in.Value})
		})
	}
}

func TestHostileSandboxShim(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			skipNUL(t, in.Value)
			script := sandboxShimScript(in.Value)
			i := strings.Index(script, "\nexec ")
			if i < 0 {
				t.Fatalf("no exec line in %q", script)
			}
			line := strings.TrimSuffix(script[i+len("\nexec "):], "\n")
			checkShell(t, "fake "+strings.Replace(line, `"$@"`, "", 1), []string{in.Value, "--sandbox"})
		})
	}
}

// The coordinator Stop hook command is stored in the harness's JSON config.
func TestHostileCoordinatorHookJSON(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			data, err := mergeCoordinatorHookConfig(nil, in.Value)
			if err != nil {
				return // refused
			}
			var cfg struct {
				Hooks struct {
					Stop []struct {
						Hooks []struct{ Command string }
					}
				}
			}
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatalf("config is not JSON: %v\n%s", err, data)
			}
			if len(cfg.Hooks.Stop) != 1 || len(cfg.Hooks.Stop[0].Hooks) != 1 {
				t.Fatalf("unexpected config shape: %s", data)
			}
			if got := cfg.Hooks.Stop[0].Hooks[0].Command; got != in.Value {
				t.Errorf("command = %q, want %q", got, in.Value)
			}
		})
	}
}

// jsonQuoted must spell s the way the adapter's encoding/json does (it finds
// briefSlot in the adapter's output), so invalid UTF-8, which JSON cannot
// carry, reads back as encoding/json writes it: U+FFFD per bad byte.
func TestHostileJSONQuoted(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			var got string
			if err := json.Unmarshal([]byte(jsonQuoted(in.Value)), &got); err != nil {
				t.Fatalf("not a JSON string: %v", err)
			}
			want := in.Value
			if !utf8.ValidString(want) {
				b, _ := json.Marshal(want)
				_ = json.Unmarshal(b, &want)
			}
			if got != want {
				t.Errorf("decoded %q, want %q", got, in.Value)
			}
		})
	}
}

// A run id names one directory under <state>/runs.
func TestHostileRunDir(t *testing.T) {
	root := t.TempDir()
	j := &fileJournal{dir: root}
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			run := RunID(in.Value)
			if validRunID(run) != nil {
				return // refused
			}
			p := j.runDir(run)
			if filepath.Dir(p) != filepath.Join(root, "runs") || filepath.Base(p) != in.Value {
				t.Errorf("run dir %q is not the single component %q under runs", p, in.Value)
			}
			if strings.ContainsRune(in.Value, 0) {
				t.Errorf("validRunID accepts a NUL byte; the run dir %q cannot exist (open fails with EINVAL)", p)
			}
		})
	}
}

var safeComponent = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func TestHostileSlugOutput(t *testing.T) {
	seen := map[string]string{}
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			s := slugOutput(in.Value)
			if !safeComponent.MatchString(s) || s == "." || s == ".." || !strings.HasSuffix(s, ".md") {
				t.Errorf("slug %q is not one safe component", s)
			}
			if other, ok := seen[s]; ok {
				t.Errorf("slug %q also names %q", s, other)
			}
			seen[s] = in.Value
		})
	}
}

func hostileCycle(t *testing.T) (*cycle, string) {
	dir := t.TempDir()
	return &cycle{e: &Engine{opts: EngineOptions{RunDir: func(RunID) string { return dir }}}, run: "r"}, dir
}

// legacyOutputPath escapes every component of a unit name: the path stays
// under <run>/out and decodes back to the name.
func TestHostileLegacyOutputPath(t *testing.T) {
	c, dir := hostileCycle(t)
	out := filepath.Join(dir, "out")
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			p, err := c.legacyOutputPath(in.Value)
			if err != nil {
				return // refused
			}
			rel, err := filepath.Rel(out, p)
			if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				t.Fatalf("path %q escapes %q", p, out)
			}
			parts := strings.Split(strings.TrimSuffix(rel, ".md"), "/")
			for i, part := range parts {
				if part == "%EMPTY" {
					parts[i] = ""
					continue
				}
				dec, err := url.PathUnescape(strings.ReplaceAll(part, "%2E", "."))
				if err != nil {
					t.Fatalf("component %q does not decode: %v", part, err)
				}
				parts[i] = dec
			}
			if got := strings.Join(parts, "/"); got != in.Value {
				t.Errorf("decodes to %q, want %q (path %q)", got, in.Value, p)
			}
		})
	}
}

// The allocated-artifact directory slugs the instance id (map keys included).
func TestHostileArtifactDir(t *testing.T) {
	c, dir := hostileCycle(t)
	arts := filepath.Join(dir, "records", "artifacts")
	producer := &node{name: "Review"}
	seen := map[string]string{}
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			p, err := c.artifactDir(&instance{ID: "Fan[" + in.Value + "]", Visits: map[string]int{}}, producer)
			if err != nil {
				return // refused
			}
			if filepath.Dir(p) != arts || !safeComponent.MatchString(filepath.Base(p)) {
				t.Errorf("artifact dir %q is not one safe component under %q", p, arts)
			}
			if other, ok := seen[p]; ok {
				t.Errorf("artifact dir %q also belongs to key %q", p, other)
			}
			seen[p] = in.Value
		})
	}
}

// Map child ids are "<parent>/<State>[<key>]": the key (model or compute
// output) stays inside its brackets, escaped (idKey) so that no delimiter of
// an id stands in it, and it reads back as the key. Distinct keys keep
// distinct ids (sw-ivuk).
func TestHostileMapChildID(t *testing.T) {
	n := &node{name: "Fan"}
	visit1 := &instance{ID: "", Visits: map[string]int{"Fan": 1}}
	seen := map[string]string{}
	for _, in := range append(hostile.Corpus(), hostile.Input{Name: "spelled-id", Value: "c1]/Panel[s1"}, hostile.Input{Name: "escaped-spelling", Value: "c1%5D%2FPanel%5Bs1"}) {
		t.Run(in.Name, func(t *testing.T) {
			id := mapChildID(visit1, n, in.Value, 1)
			esc, ok := strings.CutPrefix(id, "Fan[")
			esc, ok2 := strings.CutSuffix(esc, "]")
			if !ok || !ok2 || strings.ContainsAny(esc, "/[]") {
				t.Fatalf("id %q does not hold its key inside one bracket pair", id)
			}
			if key, err := url.PathUnescape(esc); err != nil || key != in.Value {
				t.Errorf("id %q reads back as %q (%v), want %q", id, key, err, in.Value)
			}
			if other, ok := seen[id]; ok {
				t.Errorf("id %q also belongs to key %q", id, other)
			}
			seen[id] = in.Value
		})
	}
}

// RenderStatus is one line per unit and per halted instance; unit notes,
// coordinator heartbeat notes and blocked reasons are model text.
func TestHostileRenderStatus(t *testing.T) {
	render := func(v string) string {
		return RenderStatus(RunStatus{
			Run:       "r",
			Phase:     RunBlocked,
			Units:     []UnitStatus{{Name: "Review#1", State: "running", Note: v, Observed: v}},
			Instances: []InstanceStatus{{ID: "Fan[k]", Phase: "blocked", State: "Review", Reason: v}},
		})
	}
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			base := strings.Count(render(oneLine(in.Value)), "\n")
			out := render(in.Value)
			if n := strings.Count(out, "\n"); n != base {
				t.Errorf("line injection: %d lines, want %d:\n%s", n, base, out)
			}
			if strings.ContainsAny(out, "\r\x00") {
				t.Errorf("raw control character in status view: %q", out)
			}
		})
	}
}

type hsStart struct{}
type hsEnd struct{}
type hsOut struct{ V string }

func hostileEdgeDef(label string) *Def {
	d := New("hostile")
	a := NewAgent[hsStart, hsOut](d)
	end := NewTerminal[hsEnd](d, Success)
	e := a.To(end, label)
	a.Spec(PromptText("p"), Retry[hsStart](Const(1), a.To(end, "exhausted")))
	a.Route(func(hsOut, Ctx) Edge[hsStart] { return e }, e)
	return d
}

// mermaidEntity is one Mermaid entity code, #<decimal>;.
var mermaidEntity = regexp.MustCompile(`#([0-9]+);`)

// mermaidDecode reads a Mermaid label back: each entity code is its
// character. ok is false when a # or ; stands outside an entity code, which
// Mermaid would read as syntax.
func mermaidDecode(s string) (string, bool) {
	rest := mermaidEntity.ReplaceAllString(s, "")
	if strings.ContainsAny(rest, "#;") {
		return "", false
	}
	return mermaidEntity.ReplaceAllStringFunc(s, func(m string) string {
		var r rune
		for _, d := range m[1 : len(m)-1] {
			r = r*10 + d - '0'
		}
		return string(r)
	}), true
}

// Edge labels are definition text rendered into Mermaid and the text view.
// The label is written as mermaidText encodes it, on its transition's line,
// and reads back as the label with each control character a space and each
// invalid byte U+FFFD: nothing in it becomes Mermaid syntax.
func TestHostileMermaidEdgeLabel(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			base := strings.Count(hostileEdgeDef(oneLine(in.Value)).Mermaid(), "\n")
			out := hostileEdgeDef(in.Value).Mermaid()
			if n := strings.Count(out, "\n"); n != base || strings.Contains(out, "\r") {
				t.Errorf("statement injection: %d lines, want %d:\n%s", n, base, out)
			}
			label := in.Value
			if label == "" {
				label = "ok"
			}
			want := strings.Map(func(r rune) rune {
				if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
					return ' '
				}
				return r
			}, strings.ToValidUTF8(label, "\uFFFD"))
			var found bool
			for _, l := range strings.Split(out, "\n") {
				_, text, ok := strings.Cut(l, " --> hsEnd : ")
				if !ok || text == "exhausted" {
					continue
				}
				found = true
				if text != mermaidText(label) {
					t.Errorf("label written as %q, want the mermaidText encoding %q", text, mermaidText(label))
				}
				if got, ok := mermaidDecode(text); !ok || got != want {
					t.Errorf("label %q decodes to %q (entity codes only: %v), want %q", text, got, ok, want)
				}
			}
			if !found {
				t.Errorf("no labelled transition in:\n%s", out)
			}
		})
	}
}

func TestHostileRenderEdgeLabel(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			base := strings.Count(hostileEdgeDef(oneLine(in.Value)).Render(), "\n")
			out := hostileEdgeDef(in.Value).Render()
			if n := strings.Count(out, "\n"); n != base || strings.Contains(out, "\r") {
				t.Errorf("line injection: %d lines, want %d:\n%s", n, base, out)
			}
		})
	}
}

// mdHeading is a CommonMark ATX heading line, or a setext underline; mdFence
// is a code fence line: its fence (three or more backticks or tildes) and
// what follows it.
var (
	mdATX    = regexp.MustCompile(`^ {0,3}#{1,6}([ \t]|$)`)
	mdSetext = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	mdFence  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
)

// mdHeadings counts the headings of md, outside fenced code, treating a
// setext underline after a non-blank paragraph line as a heading. A fence
// closes only on a line of the same character, at least as long, with
// nothing after it, as CommonMark reads it; every line ending (CR LF, LF,
// CR) ends a line.
func mdHeadings(md string) int {
	n, open, prev := 0, "", ""
	for _, l := range strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(md), "\n") {
		m := mdFence.FindStringSubmatch(l)
		switch {
		case open != "":
			if m != nil && m[1][0] == open[0] && len(m[1]) >= len(open) && strings.TrimSpace(m[2]) == "" {
				open = ""
			}
		case m != nil && !(m[1][0] == '`' && strings.Contains(m[2], "`")):
			open = m[1]
		case mdATX.MatchString(l):
			n++
		case mdSetext.MatchString(l) && strings.TrimSpace(prev) != "" && !mdATX.MatchString(prev) && !strings.HasPrefix(strings.TrimSpace(prev), "- ") && !regexp.MustCompile(`^\s*(\d+\.|>)`).MatchString(prev):
			n++
		}
		prev = l
	}
	return n
}

// backtickRun is the length of the longest run of backticks in s.
func backtickRun(s string) int {
	run, longest := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// fencedBody is the content of md when md is exactly one fenced code block
// whose backtick fence is longer than any backtick run inside it, without
// the final line break the closing fence's line adds; ok is false otherwise.
func fencedBody(md string) (string, bool) {
	n := len(md) - len(strings.TrimLeft(md, "`"))
	fence := md[:n]
	if n < 3 || !strings.HasPrefix(md, fence+"\n") || !strings.HasSuffix(md, "\n"+fence) || len(md) < 2*n+2 {
		return "", false
	}
	content := md[n+1 : len(md)-n-1]
	return content, n > backtickRun(content)
}

// fenceCorpus is the corpus with values that must be fenced and hold
// backtick runs as long as the shortest fence and longer, so a fence that
// is not longer than every run in its value is caught.
func fenceCorpus() []hostile.Input {
	return append(hostile.Corpus(),
		hostile.Input{Name: "fence-three", Value: "x\n```\n## y"},
		hostile.Input{Name: "fence-four", Value: "```` x\n## y\n```"},
		hostile.Input{Name: "fence-tilde", Value: "~~~\n## y"},
	)
}

// The Inputs block puts each value (often an earlier state's model output)
// under its own ### heading; a value must not open sections of its own or
// leave a code fence open over the rest of the prompt. A single plain line
// is carried as it is; any other value is fenced, with a fence longer than
// every backtick run in it, and the fenced content is the value exactly.
func TestHostileInputsBlock(t *testing.T) {
	for _, in := range fenceCorpus() {
		t.Run(in.Name, func(t *testing.T) {
			blk := inputsBlock([]promptInput{{name: "findings", value: in.Value}}, nil)
			if n := mdHeadings(blk); n != 2 {
				t.Errorf("value adds Markdown headings: %d headings, want 2:\n%s", n, blk)
			}
			_, body, ok := strings.Cut(blk, "\n### findings\n\n")
			body, ok2 := strings.CutSuffix(body, "\n")
			if !ok || !ok2 {
				t.Fatalf("no findings section: %q", blk)
			}
			if in.Value == "" || plainLine(in.Value) {
				if body != in.Value {
					t.Errorf("plain value %q written as %q", in.Value, body)
				}
				return
			}
			content, ok := fencedBody(body)
			if !ok {
				t.Errorf("value not fenced by a fence longer than its backtick runs (longest %d): %q", backtickRun(in.Value), body)
			}
			if content != in.Value {
				t.Errorf("fenced content %q, want the value %q exactly", content, in.Value)
			}
		})
	}
}

// splicedValue reports how out carries v between prefix and suffix: inline
// (out is prefix, v, suffix), or fenced on lines of its own, with a fence
// longer than every backtick run in v and v as its exact content. ok is
// false for any other shape.
func splicedValue(out, prefix, suffix, v string) (fenced, ok bool) {
	if out == prefix+v+suffix {
		return false, true
	}
	mid, ok1 := strings.CutPrefix(out, prefix)
	mid, ok2 := strings.CutSuffix(mid, suffix)
	if !ok1 || !ok2 {
		return false, false
	}
	if !strings.HasSuffix(prefix, "\n") {
		if mid, ok1 = strings.CutPrefix(mid, "\n"); !ok1 {
			return false, false
		}
	}
	if mid, ok2 = strings.CutSuffix(mid, "\n"); !ok2 {
		return false, false
	}
	content, ok := fencedBody(mid)
	return true, ok && content == v
}

// promptSpots are places a prompt template puts a token: mid-sentence, on a
// line of its own, and as a list item. inline reports whether a value stays
// as it is there: mid-sentence any single line does, since inline text
// cannot start a block; where a block can start only a plain line does.
var promptSpots = []struct {
	name, prefix, suffix string
	inline               func(v string) bool
}{
	{"mid-sentence", "Read ", " and say.", func(v string) bool { return !strings.ContainsAny(v, "\r\n") }},
	{"own-line", "Intro.\n\n", "\n\nOutro.", func(v string) bool { return v == "" || plainLine(v) }},
	{"list-item", "Steps:\n\n- ", "\n", func(v string) bool { return v == "" || plainLine(v) }},
}

// bindPrompt substitutes {{name}} and Bound [NAME] once: a value holding
// token syntax is never expanded again, and a value (model text) cannot add
// prompt structure. A single plain line stays as it is; a value that could
// start a block where it lands goes into a fence on lines of its own. A
// Bound input is the definition's prose ([B], or {{B}} by name): it stays
// as it is unless it could add a heading or a code fence, and then it is
// fenced too.
func TestHostileBindPrompt(t *testing.T) {
	for _, in := range fenceCorpus() {
		for _, spot := range promptSpots {
			for _, token := range []string{"{{v}}", "[B]", "{{B}}"} {
				t.Run(in.Name+"/"+spot.name+"/"+token, func(t *testing.T) {
					v := in.Value
					ins := []promptInput{{name: "v", value: v}, {name: "B", value: v, bound: true}}
					out, used, err := scope{}.bindPrompt(spot.prefix+token+spot.suffix, ins)
					if err != nil {
						t.Fatalf("bindPrompt: %v", err)
					}
					if !slices.Contains(used, true) {
						t.Errorf("no input consumed")
					}
					if n := mdHeadings(out); n != 0 {
						t.Errorf("value adds %d Markdown headings:\n%s", n, out)
					}
					fenced, ok := splicedValue(out, spot.prefix, spot.suffix, v)
					if !ok {
						t.Fatalf("value not carried verbatim, inline or fenced: %q", out)
					}
					inline := spot.inline(v)
					if token == "{{v}}" && fenced == inline {
						t.Errorf("fenced = %v, want %v: %q", fenced, !inline, out)
					}
					raw := spot.prefix + v + spot.suffix
					if token != "{{v}}" && !fenced && (mdHeadings(raw) > 0 || mdFenceLine.MatchString(strings.ReplaceAll(raw, "\r", "\n"))) {
						t.Errorf("bound value that adds a heading or a fence is not fenced: %q", out)
					}
					if token != "{{v}}" && inline && fenced {
						t.Errorf("bound single line fenced where it cannot start a block: %q", out)
					}
				})
			}
		}
	}
}

// mdFenceLine finds a code fence line anywhere in a text.
var mdFenceLine = regexp.MustCompile("(?m)^ {0,3}(```|~~~)")

// A Bound input is the definition's prose: a Markdown table or list it
// built stays as it is, on its own lines, so the prompt reads as the
// definition wrote it.
func TestBindPromptKeepsBoundLayout(t *testing.T) {
	for _, v := range []string{"| ID | Requirement |\n| --- | --- |\n| g1 | a \\| b |\n", "- one\n- two"} {
		ins := []promptInput{{name: "requirements", value: v, bound: true}}
		out, _, err := scope{}.bindPrompt("{{requirements}}\n\nReview [requirements] above.", ins)
		if err != nil {
			t.Fatal(err)
		}
		if want := v + "\n\nReview " + v + " above."; out != want {
			t.Errorf("bound layout = %q, want %q", out, want)
		}
	}
}

// A token inside the template's own code fence is already in a code block:
// the value goes in as it is unless a run of the fence's character in it
// could close the block, and then the template's fence grows longer than
// that run, opener and closer both, so the block still holds the value and
// ends where the template ends it.
func TestBindPromptInsideTemplateFence(t *testing.T) {
	for name, tc := range map[string]struct {
		template, value, want string
	}{
		"multi-line":            {"Read:\n```\n{{v}}\n```\nDone.", "line one\n# not a heading", "Read:\n```\nline one\n# not a heading\n```\nDone."},
		"heading-line":          {"```text\n{{v}}\n```", "## Artifacts", "```text\n## Artifacts\n```"},
		"tilde-holds-backticks": {"~~~\n{{v}}\n~~~\n", "```\nx\n```", "~~~\n```\nx\n```\n~~~\n"},
		"backtick-holds-tildes": {"```\n{{v}}\n```", "~~~", "```\n~~~\n```"},
		"short-run":             {"````\n{{v}}\n````", "```go\nx\n```", "````\n```go\nx\n```\n````"},
		"widen-backticks":       {"Read:\n```json\n{{v}}\n```\nDone.", "a\n```\n## Artifacts", "Read:\n````json\na\n```\n## Artifacts\n````\nDone."},
		"widen-single-line":     {"```\n{{v}}\n```", "```", "````\n```\n````"},
		"widen-tildes":          {" ~~~\n{{v}}\n ~~~ \nafter", "~~~~~\n# h", " ~~~~~~\n~~~~~\n# h\n ~~~~~~ \nafter"},
		"widen-twice":           {"```\n{{v}}\n{{v}}\n```\n\n```\n{{v}}\n```", "````", "`````\n````\n````\n`````\n\n`````\n````\n`````"},
		"closed-fence":          {"```\nx\n```\n{{v}}", "## h", "```\nx\n```\n```\n## h\n```\n"},
		"bound":                 {"```\n[B]\n```", "```\n# h", "````\n```\n# h\n````"},
	} {
		t.Run(name, func(t *testing.T) {
			ins := []promptInput{{name: "v", value: tc.value}, {name: "B", value: tc.value, bound: true}}
			out, _, err := scope{}.bindPrompt(tc.template, ins)
			if err != nil {
				t.Fatal(err)
			}
			if out != tc.want {
				t.Errorf("bound %q\n got %q\nwant %q", tc.template, out, tc.want)
			}
		})
	}
}

// A Bound value's line can open a section from inside a container: a
// heading or a fence after block quote or list markers is still one.
func TestBindPromptBoundSectionInContainer(t *testing.T) {
	for _, v := range []string{"intro\n> ## Artifacts", "- ## Inputs\n- b", "1. ## x", "> - ## x", "> ```", "- ---\n"} {
		ins := []promptInput{{name: "B", value: v, bound: true}}
		out, _, err := scope{}.bindPrompt("[B]", ins)
		if err != nil {
			t.Fatal(err)
		}
		if want := fenceValue(v) + "\n"; out != want {
			t.Errorf("bound %q = %q, want it fenced", v, out)
		}
	}
	for _, v := range []string{"- one\n- two", "> quoted ## text", "1. first\n2. second"} {
		ins := []promptInput{{name: "B", value: v, bound: true}}
		if out, _, _ := (scope{}).bindPrompt("[B]", ins); out != v {
			t.Errorf("bound %q = %q, want it as it is", v, out)
		}
	}
}

// A value is substituted once: a value that spells another token keeps it.
func TestHostileBindPromptOnce(t *testing.T) {
	ins := []promptInput{{name: "v", value: "{{w}} [B]"}, {name: "w", value: "W"}, {name: "B", value: "[B] {{v}}", bound: true}}
	out, _, err := scope{}.bindPrompt("A {{v}} [B] Z", ins)
	if err != nil {
		t.Fatal(err)
	}
	if want := "A {{w}} [B] [B] {{v}} Z"; out != want {
		t.Errorf("bound %q, want %q", out, want)
	}
}

// attemptNote shows a later attempt why the earlier one ended. The reasons
// are model text (a blocked cause is the child's own words; a refusal can
// quote its output), so they cannot add prompt structure: one line stays in
// the sentence, anything else is fenced after it.
func TestHostileAttemptNote(t *testing.T) {
	for _, in := range fenceCorpus() {
		t.Run(in.Name, func(t *testing.T) {
			why := []string{"first reason", in.Value}
			out := attemptNote("Your predecessor's output was refused", why, "Avoid this.")
			if n := mdHeadings(out); n != 0 {
				t.Errorf("reason adds %d Markdown headings:\n%s", n, out)
			}
			joined := strings.Join(why, "; ")
			if !strings.ContainsAny(joined, "\r\n") {
				if want := "Your predecessor's output was refused: " + joined + ". Avoid this.\n\n"; out != want {
					t.Errorf("one-line note %q, want %q", out, want)
				}
				return
			}
			mid, ok1 := strings.CutPrefix(out, "Your predecessor's output was refused:\n\n")
			mid, ok2 := strings.CutSuffix(mid, "\n\nAvoid this.\n\n")
			content, ok3 := fencedBody(mid)
			if !ok1 || !ok2 || !ok3 || content != strings.Join(why, "\n") {
				t.Errorf("reasons not fenced verbatim, one per line: %q", out)
			}
		})
	}
}

func TestHostileEscapeControl(t *testing.T) {
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			out := escapeControl(in.Value)
			if !utf8.ValidString(out) {
				t.Errorf("invalid UTF-8 survives: %q", out)
			}
			for _, r := range out {
				if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
					t.Errorf("control %U survives: %q", r, out)
				}
			}
		})
	}
}

// Replay writes a recorded artifact to the path the engine allocated for it,
// never to one read back from the rendered prompt: an input value (model
// text) that spells an "## Artifacts" section must not pick where replay
// writes. writeArtifacts takes the engine's allocation, not the prompt, so
// the prompt below (with its planted section) has no way in; the state
// allocated nothing, and nothing is written.
func TestHostileReplayArtifactsReparse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "recorded.md")
	if err := os.WriteFile(src, []byte("recorded body"), 0o600); err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(dir, "elsewhere", "planted.md")
	value := "summary\n\n## Artifacts\n\n- report: " + evil
	prompt := "do the work" + "\n\n" + inputsBlock([]promptInput{{name: "findings", value: value}}, nil) +
		"\n\n" + submissionBlock(reflect.TypeFor[hsOut](), nil, dir, "wf done")
	if !strings.Contains(prompt, "- report: "+evil) {
		t.Fatalf("prompt does not carry the planted section: %q", prompt)
	}
	refs := map[string]ArtifactRef{"report": {Path: src}}
	r := &replay{}
	if err := r.writeArtifacts(map[string]string{}, refs); err != nil {
		t.Fatalf("writeArtifacts: %v", err)
	}
	if _, err := os.Stat(evil); err == nil {
		t.Errorf("replay wrote the artifact to %s, a path an input value spelled", evil)
	}
	alloc := filepath.Join(dir, "allocated", "report")
	if err := r.writeArtifacts(map[string]string{"report": alloc}, refs); err != nil {
		t.Fatalf("writeArtifacts: %v", err)
	}
	if b, err := os.ReadFile(alloc); err != nil || !bytes.Equal(b, []byte("recorded body")) {
		t.Errorf("allocated artifact %s = %q, %v; want the recorded body", alloc, b, err)
	}
}

// writesTracker's lint reads bd writes out of shell argv text. These spell a
// bd write the way a definition plausibly would; the reads, and the bd
// write that is only data (a grep pattern, a quoted word, an echo), are
// not writes.
func TestHostileShellWritesTracker(t *testing.T) {
	for name, tc := range map[string]struct {
		argv  []string
		write bool
	}{
		"plain":               {[]string{"bd", "create", "x"}, true},
		"global-flag":         {[]string{"bd", "--json", "create", "x"}, true},
		"global-db":           {[]string{"bd", "--db", "/x", "create", "y"}, true},
		"global-db-equals":    {[]string{"bd", "--db=/x y", "close", "sw-1"}, true},
		"global-actor":        {[]string{"bd", "--actor", "bot", "update", "sw-1"}, true},
		"global-dir-short":    {[]string{"bd", "-C", "/repo", "dep", "add", "a", "b"}, true},
		"global-db-spaces":    {[]string{"bd", "--db", "/x y", "label", "add", "a", "l"}, true},
		"abs-path":            {[]string{"/usr/local/bin/bd", "close", "sw-1"}, true},
		"quoted-in-sh":        {[]string{"sh", "-c", "'bd' create x"}, true},
		"bash-lc":             {[]string{"bash", "-lc", "bd update sw-1 --claim"}, true},
		"bash-ec-script":      {[]string{"bash", "-ec", "set -e\nbd update \"$1\" -t epic", "promote", "a"}, true},
		"script-global-db":    {[]string{"sh", "-c", "bd --db '/x y' create z"}, true},
		"script-after-and":    {[]string{"sh", "-c", "test -f x && bd close sw-1"}, true},
		"script-subshell":     {[]string{"sh", "-c", "id=$(bd create x --silent)"}, true},
		"script-quoted-subst": {[]string{"sh", "-c", "id=\"$(bd --json create x)\""}, true},
		"script-quoted-tick":  {[]string{"sh", "-c", "echo \"`bd close sw-1`\""}, true},
		"script-comment":      {[]string{"sh", "-c", "# bd close sw-1\nbd list"}, false},
		"script-if":           {[]string{"sh", "-c", "if bd close sw-1; then echo ok; fi"}, true},
		"script-assign":       {[]string{"sh", "-c", "BD_ACTOR=x bd close sw-1"}, true},
		"nested-shell":        {[]string{"sh", "-c", "bash -c 'bd close sw-1'"}, true},
		"env-prefix":          {[]string{"env", "BD_ACTOR=x", "bd", "close", "sw-1"}, true},
		"env-shell":           {[]string{"env", "-i", "sh", "-c", "bd close sw-1"}, true},
		"read":                {[]string{"bd", "list", "--json"}, false},
		"read-global-db":      {[]string{"bd", "--db", "create", "list"}, false},
		"script-read":         {[]string{"bash", "-c", "bd list --json"}, false},
		"grep-count":          {[]string{"grep", "-c", "bd create", "log.txt"}, false},
		"script-grep":         {[]string{"sh", "-c", "grep -c 'bd create' log.txt"}, false},
		"script-echo":         {[]string{"sh", "-c", "echo \"bd close sw-1\""}, false},
		"script-positional":   {[]string{"sh", "-c", "echo \"$1\"", "x", "bd create y"}, false},
		"not-a-shell-c-flag":  {[]string{"python3", "-c", "bd create x"}, false},
		"bd-like-name":        {[]string{"sh", "-c", "xbd create x; bd-tool close y"}, false},
		"shell-option-value":  {[]string{"bash", "-euo", "pipefail", "-c", "bd create x"}, true},
		"shell-options-after": {[]string{"sh", "-c", "-e", "bd close x"}, true},
		"shell-dashdash":      {[]string{"sh", "-c", "--", "bd close x"}, true},
		"shell-long-option":   {[]string{"bash", "--norc", "--rcfile", "/x", "-c", "bd close x"}, true},
		"shell-shopt":         {[]string{"bash", "-O", "extglob", "-c", "bd close x"}, true},
		"timeout":             {[]string{"timeout", "60", "sh", "-c", "bd close x"}, true},
		"timeout-options":     {[]string{"timeout", "-k", "5", "--signal=TERM", "60", "bd", "close", "x"}, true},
		"env-unset":           {[]string{"env", "-u", "FOO", "sh", "-c", "bd close x"}, true},
		"env-chdir":           {[]string{"env", "-C", "/repo", "-i", "bd", "close", "x"}, true},
		"env-split-string":    {[]string{"env", "-S", "sh -c 'bd close x'"}, true},
		"nice":                {[]string{"nice", "-n", "5", "bash", "-c", "bd update x"}, true},
		"nohup":               {[]string{"nohup", "sh", "-c", "bd create y"}, true},
		"command":             {[]string{"command", "-p", "bd", "close", "x"}, true},
		"command-v":           {[]string{"command", "-v", "bd", "close"}, false},
		"xargs":               {[]string{"xargs", "-n", "1", "-P", "4", "bd", "close"}, true},
		"wrapper-chain":       {[]string{"env", "A=1", "nice", "timeout", "5", "nohup", "sh", "-c", "bd close x"}, true},
		"script-wrapper":      {[]string{"sh", "-c", "timeout 5 bd close x"}, true},
		"shell-script-file":   {[]string{"sh", "-e", "bd create x"}, false},
		"option-value-only":   {[]string{"bash", "-o", "bd create x", "-c", "echo"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := shellWritesTracker(tc.argv); got != tc.write {
				t.Errorf("shellWritesTracker(%q) = %v, want %v", tc.argv, got, tc.write)
			}
		})
	}
}

// manufacture's template check and the tracker lint read a shell call's
// program the same way: through env and wrappers, [shell, options with a -c
// cluster and their values, program], where the program is the first word
// after the options.
func TestShellProgramShape(t *testing.T) {
	for name, tc := range map[string]struct {
		argv []string
		prog string
	}{
		"sh-c":            {[]string{"sh", "-c", "p", "x"}, "p"},
		"bash-ec":         {[]string{"/bin/bash", "-ec", "p"}, "p"},
		"bash-lc":         {[]string{"bash", "-lc", "p"}, "p"},
		"env-prefix":      {[]string{"env", "A=1", "zsh", "-c", "p"}, "p"},
		"no-c":            {[]string{"sh", "-e", "p"}, ""},
		"value-flag":      {[]string{"bash", "-oc", "p"}, ""},
		"not-a-shell":     {[]string{"grep", "-c", "p"}, ""},
		"no-program":      {[]string{"sh", "-c"}, ""},
		"option-value":    {[]string{"bash", "-euo", "pipefail", "-c", "p"}, "p"},
		"options-after-c": {[]string{"sh", "-c", "-e", "p"}, "p"},
		"dashdash":        {[]string{"sh", "-c", "--", "p"}, "p"},
		"plus-o":          {[]string{"bash", "+o", "posix", "-c", "p"}, "p"},
		"c-takes-o-value": {[]string{"bash", "-co", "errexit", "p"}, "p"},
		"wrapped":         {[]string{"env", "-u", "A", "timeout", "60", "nice", "-n", "5", "nohup", "sh", "-c", "p"}, "p"},
		"dashdash-no-c":   {[]string{"sh", "--", "p"}, ""},
		"o-mid-cluster":   {[]string{"bash", "-oec", "errexit", "p"}, ""},
		"lone-dash":       {[]string{"bash", "-c", "-", "p"}, "p"},
		"command-v":       {[]string{"command", "-v", "sh", "-c", "p"}, ""},
		"env-lone-dash":   {[]string{"env", "-", "sh", "-c", "p"}, "p"},
	} {
		t.Run(name, func(t *testing.T) {
			if prog, ok := shellProgram(tc.argv); prog != tc.prog || ok != (tc.prog != "") {
				t.Errorf("shellProgram(%q) = %q, %v; want %q", tc.argv, prog, ok, tc.prog)
			}
		})
	}
	// The same shape gates a templated manufacture command.
	for _, argv := range [][]string{{"bash", "-lc", "echo \"$1\"", "{{item.id}}"}, {"env", "sh", "-c", "echo \"$1\"", "{{item.id}}"}} {
		if err := staticCommand(argv); err != nil {
			t.Errorf("staticCommand(%q) = %v, want the positional shape accepted", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"bash", "-euo", "pipefail", "-c", "echo \"$1\"", "x"},
		{"timeout", "60", "sh", "-c", "--", "echo \"$1\"", "x"},
		{"env", "-u", "FOO", "nice", "-n", "5", "nohup", "sh", "-c", "-e", "echo \"$1\"", "x"},
	} {
		if err := staticCommand(append(argv, "{{item.id}}")); err != nil {
			t.Errorf("staticCommand(%q) = %v, want the positional shape accepted", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"bash", "-c", "echo {{item.id}}"}, {"env", "sh", "-c", "echo {{item.id}}"}, {"bash", "-oc", "x", "echo"},
		{"bash", "-euo", "pipefail", "-c", "echo {{item.id}}"},
		{"timeout", "60", "sh", "-c", "echo {{item.id}}"},
		{"sh", "-c", "--", "echo {{item.id}}"},
		{"sh", "-c", "-e", "{{item.id}}"},
		{"env", "-u", "FOO", "sh", "-c", "echo {{item.id}}"},
		{"nice", "-n", "5", "bash", "-c", "echo {{item.id}}"},
		{"nohup", "sh", "-c", "echo {{item.id}}"},
		{"xargs", "sh", "-c", "echo {{item.id}}"},
		{"env", "-S", "sh -c {{item.id}}"},
		{"bash", "-o", "{{item.id}}", "-c", "echo"},
		{"env", "PATH={{item.id}}", "sh", "-c", "echo \"$1\"", "x"},
		{"timeout", "{{item.id}}", "sh", "-c", "echo \"$1\"", "x"},
		{"timeout", "60", "{{item.id}}"},
	} {
		if err := staticCommand(append(argv, "{{item.id}}")); err == nil {
			t.Errorf("staticCommand(%q) accepted a template outside the positional shape", argv)
		}
	}
}
