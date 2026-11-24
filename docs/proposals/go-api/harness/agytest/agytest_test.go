package agytest_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/harness/agytest"
	"github.com/alepar/super-workflow/docs/proposals/go-api/internal/harnessfixture"
)

// agytest builds agy's records by hand, so its own tests compare what every
// builder writes with the shapes the embedded probe tree (agy 1.5.12) holds,
// and prove the adapter reads the builders' output as agy's. StartSpoofed is
// not tested here: harness/agy_identity_test.go exercises each spoof, each of
// which is a variant of a builder covered below.

const (
	coord  = "11111111-aaaa-4aaa-8aaa-000000000001"
	childA = "22222222-bbbb-4bbb-8bbb-00000000000a"
	childB = "22222222-bbbb-4bbb-8bbb-00000000000b"
	brief  = "wf prompt run-1 --claim tok1 --state-dir /tmp/s\nunit"
)

var t0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// shapes are the record shapes the probe tree holds.
type shapes struct {
	steps    map[string]map[string]bool // "source/type" -> the sorted-key lists its steps have
	statuses map[string]bool
	listing  map[string]bool // the keys of a subagents/<id>.json
	message  map[string]bool // the keys of a messages/<id>.json
	child0   string          // a probe child's step 0 content
	task     string          // a probe task message's content
}

var (
	shapesOnce sync.Once
	probe      shapes
)

func keyList(m map[string]json.RawMessage) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func probeShapes(t *testing.T) shapes {
	t.Helper()
	shapesOnce.Do(func() {
		probe = shapes{steps: map[string]map[string]bool{}, statuses: map[string]bool{}, listing: map[string]bool{}, message: map[string]bool{}}
		src, err := fs.Sub(harnessfixture.Files(), "agy/probe")
		if err != nil {
			t.Fatal(err)
		}
		err = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") && !strings.HasSuffix(p, ".jsonl") {
				return err
			}
			b, err := fs.ReadFile(src, p)
			if err != nil {
				return err
			}
			switch {
			case strings.HasSuffix(p, "transcript_full.jsonl"):
				for i, line := range bytes.Split(b, []byte("\n")) {
					if len(bytes.TrimSpace(line)) == 0 {
						continue
					}
					var m map[string]json.RawMessage
					if err := json.Unmarshal(line, &m); err != nil {
						return err
					}
					var s struct{ Source, Type, Status, Content string }
					for k, dst := range map[string]*string{"source": &s.Source, "type": &s.Type, "status": &s.Status, "content": &s.Content} {
						_ = json.Unmarshal(m[k], dst) // absent keys leave ""
					}
					k := s.Source + "/" + s.Type
					if probe.steps[k] == nil {
						probe.steps[k] = map[string]bool{}
					}
					probe.steps[k][keyList(m)] = true
					probe.statuses[s.Status] = true
					if s.Type == "SYSTEM_MESSAGE" && i == 0 && probe.child0 == "" {
						probe.child0 = s.Content
					}
					if s.Type == "SYSTEM_MESSAGE" && strings.Contains(s.Content, "/task-") {
						probe.task = s.Content
					}
				}
			case strings.Contains(p, "/subagents/"):
				var m map[string]json.RawMessage
				if err := json.Unmarshal(b, &m); err != nil {
					return err
				}
				probe.listing[keyList(m)] = true
			case strings.Contains(p, "/messages/") && filepath.Base(p) != "read.json":
				var m map[string]json.RawMessage
				if err := json.Unmarshal(b, &m); err != nil {
					return err
				}
				probe.message[keyList(m)] = true
			}
			return nil
		})
		if err != nil {
			panic(err)
		}
	})
	if probe.child0 == "" || probe.task == "" || len(probe.listing) != 1 || len(probe.message) != 1 {
		t.Fatalf("the probe tree did not yield every shape: %d listing shapes, %d message shapes, child step 0 %v, task message %v", len(probe.listing), len(probe.message), probe.child0 != "", probe.task != "")
	}
	return probe
}

// only is the one key list of a set the probe recorded exactly one of.
func only(m map[string]bool) []string {
	for k := range m {
		return strings.Split(k, ",")
	}
	return nil
}

// logs are every conversation's step log under tr.
func logs(t *testing.T, tr *agytest.Tree) []string {
	t.Helper()
	ps, err := filepath.Glob(filepath.Join(tr.Root, "brain", "*", ".system_generated", "logs", "transcript_full.jsonl"))
	if err != nil || len(ps) == 0 {
		t.Fatalf("no step logs under %s (%v)", tr.Root, err)
	}
	return ps
}

// checkLogs asserts every line of every log tr holds is a step shaped as the
// probe's: valid JSON, step_index its line number, a recorded status, and the
// key set a probe step of the same source and type has. It returns the
// "source/type" of every line, so a test can assert what was written.
func checkLogs(t *testing.T, tr *agytest.Tree) []string {
	t.Helper()
	sh := probeShapes(t)
	var kinds []string
	for _, p := range logs(t, tr) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, line := range bytes.Split(b, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatalf("%s line %d is not JSON: %v", filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p)))), n, err)
			}
			var s struct{ Source, Type, Status string }
			for k, dst := range map[string]*string{"source": &s.Source, "type": &s.Type, "status": &s.Status} {
				_ = json.Unmarshal(m[k], dst)
			}
			var index int
			if err := json.Unmarshal(m["step_index"], &index); err != nil || index != n {
				t.Errorf("line %d has step_index %s (%v)", n, m["step_index"], err)
			}
			k := s.Source + "/" + s.Type
			if !sh.steps[k][keyList(m)] {
				t.Errorf("%s step has keys %s; the probe recorded %v", k, keyList(m), keys(sh.steps[k]))
			}
			if !sh.statuses[s.Status] {
				t.Errorf("%s step has status %q, which the probe never recorded", k, s.Status)
			}
			kinds = append(kinds, k)
			n++
		}
	}
	return kinds
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

func launchOf(child string, at time.Time) agytest.Launch {
	return agytest.Launch{Coordinator: coord, Child: child, Brief: brief, At: at}
}

var runCmd = agytest.Call{Name: "run_command", Args: map[string]any{"CommandLine": "true"}}

func TestBuilderStepsMatchProbeShapes(t *testing.T) {
	at := t0.Add(2 * time.Second)
	for _, tc := range []struct {
		name  string
		build func(tr *agytest.Tree)
		want  []string // source/type values the builder must have written
	}{
		{"Coordinator", func(tr *agytest.Tree) {}, []string{"USER_EXPLICIT/USER_INPUT"}},
		{"Respond with calls", func(tr *agytest.Tree) {
			tr.Respond(coord, at, "m", 10, 5, "", runCmd)
		}, []string{"MODEL/PLANNER_RESPONSE"}},
		{"Respond with content only", func(tr *agytest.Tree) {
			tr.Respond(coord, at, "m", 10, 5, "done")
		}, []string{"MODEL/PLANNER_RESPONSE"}},
		{"Start", func(tr *agytest.Tree) {
			tr.Start(launchOf(childA, at))
		}, []string{"MODEL/PLANNER_RESPONSE", "MODEL/GENERIC", "SYSTEM/SYSTEM_MESSAGE"}},
		{"StartAll", func(tr *agytest.Tree) {
			tr.StartAll(coord, at, launchOf(childA, at), launchOf(childB, at))
		}, []string{"MODEL/PLANNER_RESPONSE", "MODEL/GENERIC", "SYSTEM/SYSTEM_MESSAGE"}},
		{"Response", func(tr *agytest.Tree) {
			l := launchOf(childA, at)
			tr.Response(coord, at, agytest.Item{Launch: &l}, agytest.Item{Call: &runCmd})
		}, []string{"MODEL/PLANNER_RESPONSE", "SYSTEM/SYSTEM_MESSAGE"}},
		{"StartPartial, 1 of 2 written", func(tr *agytest.Tree) {
			tr.StartPartial(coord, at, 1, launchOf(childA, at), launchOf(childB, at))
		}, []string{"MODEL/PLANNER_RESPONSE", "MODEL/GENERIC", "SYSTEM/SYSTEM_MESSAGE"}},
		{"LaunchResult", func(tr *agytest.Tree) {
			tr.LaunchResult(coord, at, childA)
		}, []string{"MODEL/GENERIC"}},
		{"CommandResult", func(tr *agytest.Tree) {
			tr.CommandResult(coord, at, "ok")
		}, []string{"MODEL/GENERIC"}},
		{"ErrorMessage", func(tr *agytest.Tree) {
			tr.ErrorMessage(coord, at)
		}, []string{"SYSTEM/ERROR_MESSAGE"}},
		{"TaskMessage", func(tr *agytest.Tree) {
			tr.TaskMessage(coord, at, 3)
		}, []string{"SYSTEM/SYSTEM_MESSAGE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := agytest.New(t)
			tr.Coordinator(coord, t0)
			tc.build(tr)
			kinds := checkLogs(t, tr)
			for _, w := range tc.want {
				if !contains(kinds, w) {
					t.Errorf("%s wrote no %s step (wrote %v)", tc.name, w, kinds)
				}
			}
		})
	}

	// OtherResult writes the one synthetic step type: CODE_ACTION, which no
	// probe recorded. It is a MODEL-source step with GENERIC's keys and status,
	// a result that is never a created-child one.
	t.Run("OtherResult", func(t *testing.T) {
		sh := probeShapes(t)
		tr := agytest.New(t)
		tr.Coordinator(coord, t0)
		tr.OtherResult(coord, at, "CODE_ACTION")
		b, err := os.ReadFile(tr.Transcript(coord))
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
		var m map[string]json.RawMessage
		if err := json.Unmarshal(lines[len(lines)-1], &m); err != nil {
			t.Fatal(err)
		}
		var src, typ, status string
		_ = json.Unmarshal(m["source"], &src)
		_ = json.Unmarshal(m["type"], &typ)
		_ = json.Unmarshal(m["status"], &status)
		if src != "MODEL" || typ != "CODE_ACTION" || !sh.statuses[status] {
			t.Errorf("OtherResult wrote source %q type %q status %q", src, typ, status)
		}
		if sh.steps["MODEL/CODE_ACTION"] != nil {
			t.Error("the probe now records CODE_ACTION steps: compare OtherResult with them instead")
		}
		if !sh.steps["MODEL/GENERIC"][keyList(m)] {
			t.Errorf("OtherResult has keys %s, want a key set of GENERIC's: %v", keyList(m), keys(sh.steps["MODEL/GENERIC"]))
		}
	})
}

var wrapHeader = regexp.MustCompile(`^timestamp=\S+ sender=(\S+) priority=\S+ content=`)

// agy's delivery wrapper: the bytes before the header and the suffix, and the
// header's form for a child's step 0 and for a task message.
func TestBuilderWrapperBytes(t *testing.T) {
	sh := probeShapes(t)
	probePrefix, _, ok := strings.Cut(sh.child0, "timestamp=")
	if !ok {
		t.Fatalf("a probe child's step 0 has no header: %q", sh.child0)
	}
	const suffix = "\n</SYSTEM_MESSAGE>"
	at := t0.Add(2 * time.Second)

	tr := agytest.New(t)
	tr.Coordinator(coord, t0)
	tr.Start(launchOf(childA, at))
	tr.TaskMessage(coord, at, 3)
	content := func(id string, n int) string {
		b, err := os.ReadFile(tr.Transcript(id))
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
		var s struct{ Content string }
		if err := json.Unmarshal(lines[n], &s); err != nil {
			t.Fatal(err)
		}
		return s.Content
	}
	check := func(what, c, sender string) {
		prefix, rest, ok := strings.Cut(c, "timestamp=")
		if !ok || prefix != probePrefix {
			t.Errorf("%s: prefix %q, want the probe's %q", what, prefix, probePrefix)
		}
		if !strings.HasSuffix(c, suffix) {
			t.Errorf("%s does not end with %q", what, suffix)
		}
		m := wrapHeader.FindStringSubmatch("timestamp=" + rest)
		if m == nil || m[1] != sender {
			t.Errorf("%s: header %q, want sender %q", what, rest, sender)
		}
	}
	check("a child's step 0", content(childA, 0), coord)
	// The coordinator's log: 0 user input, 1 response, 2 result, 3 task message.
	check("a task message", content(coord, 3), coord+"/task-3")
	// The probe's own task message has the same header form, sender <id>/task-<n>.
	if _, rest, _ := strings.Cut(sh.task, "timestamp="); !regexp.MustCompile(`^\S+ sender=\S+/task-\d+ priority=\S+ content=`).MatchString(rest) {
		t.Errorf("the probe's task message header changed shape: %q", rest)
	}
}

// listing and message: the keys the builders write, against the probe's.
func TestBuilderListingAndMessage(t *testing.T) {
	sh := probeShapes(t)
	tr := agytest.New(t)
	tr.Coordinator(coord, t0)
	tr.Start(launchOf(childA, t0.Add(2*time.Second)))

	read := func(pattern string) map[string]json.RawMessage {
		ps, err := filepath.Glob(filepath.Join(tr.Root, "brain", "*", ".system_generated", pattern))
		if err != nil || len(ps) != 1 {
			t.Fatalf("%s matches %v (%v), want one file", pattern, ps, err)
		}
		b, err := os.ReadFile(ps[0])
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// missing is the probe keys the built record lacks; extra the built keys the probe lacks.
	diff := func(built map[string]json.RawMessage, probe []string) (missing, extra []string) {
		have := map[string]bool{}
		for k := range built {
			have[k] = true
		}
		for _, k := range probe {
			if !have[k] {
				missing = append(missing, k)
			}
			delete(have, k)
		}
		for k := range have {
			extra = append(extra, k)
		}
		sort.Strings(missing)
		sort.Strings(extra)
		return
	}
	for _, tc := range []struct {
		name, pattern string
		probe         map[string]bool
		wantMissing   string
	}{
		{"listing", "subagents/*.json", sh.listing, "spawnStepIndex,workspaceUris"},
		{"message", "messages/*.json", sh.message, "renderDetails,sourceMetadata"},
	} {
		missing, extra := diff(read(tc.pattern), only(tc.probe))
		if len(extra) != 0 {
			t.Errorf("the built %s has keys the probe never recorded: %v", tc.name, extra)
		}
		if got := strings.Join(missing, ","); got != tc.wantMissing {
			t.Errorf("the built %s lacks %q of the probe's keys, want exactly %q", tc.name, got, tc.wantMissing)
		}
	}
}

// the builders' output is read by the adapter as agy's own records.
func TestBuildersReadByTheAdapter(t *testing.T) {
	a := harness.For("agy")
	at := t0.Add(2 * time.Second)
	after := at.Add(400 * time.Millisecond)

	tr := agytest.New(t)
	tr.Coordinator(coord, t0)
	tr.Start(launchOf(childA, at))
	if who := a.Caller(tr.Env(childA)); who.Is != harness.IsChild || who.Session != coord || who.Agent != childA || who.Root != tr.Root {
		t.Errorf("Caller(child) = %+v, want a child of the coordinator", who)
	}
	if who := a.Caller(tr.Env(coord)); who.Is != harness.IsSelf || who.Session != coord {
		t.Errorf("Caller(coordinator) = %+v, want self", who)
	}
	if got := a.(harness.Telemetry).CoordinatorTranscript(tr.Root, coord); got != tr.Transcript(coord) {
		t.Errorf("CoordinatorTranscript = %q, want %q", got, tr.Transcript(coord))
	}

	tr.Respond(childA, at.Add(5*time.Second), "model-x", 100, 7, "", runCmd)
	tr.Respond(childA, at.Add(6*time.Second), "model-y", 30, 3, "all done")
	if got, ok := a.(harness.Telemetry).Usage(tr.Transcript(childA)); !ok || got != 140 {
		t.Errorf("Usage = %d, %v, want 140 (input plus output of both responses)", got, ok)
	}
	if !a.Ended(tr.Transcript(childA)) {
		t.Error("a log ending with a content-only response must be Ended")
	}
	tr.CommandResult(childA, at.Add(7*time.Second), "ok")
	if a.Ended(tr.Transcript(childA)) {
		t.Error("a log ending with a command result must not be Ended")
	}

	// StartPartial writes a written prefix of results: the first child verifies
	// alone, and both once the second result is written.
	p := agytest.New(t)
	p.Coordinator(coord, t0)
	la := agytest.Launch{Coordinator: coord, Child: childA, Brief: "wf prompt r --claim ta --state-dir /s", At: at}
	lb := agytest.Launch{Coordinator: coord, Child: childB, Brief: "wf prompt r --claim tb --state-dir /s", At: at}
	p.StartPartial(coord, at, 1, la, lb)
	found := func(tok string) bool {
		w := harness.Want{Token: tok, After: after}
		_, f := a.Resolve(p.Root, coord, []harness.Want{w})[w]
		return f
	}
	if !found("ta") || found("tb") {
		t.Errorf("one result written: first child verifies %v, second %v, want true, false", found("ta"), found("tb"))
	}
	p.LaunchResult(coord, at, childB)
	if !found("ta") || !found("tb") {
		t.Errorf("both results written: first child verifies %v, second %v, want true, true", found("ta"), found("tb"))
	}
}

// FromProbe copies the embedded tree and changes only the probe briefs' claim
// phrase; Env names the conversation, the root and a temp home.
func TestFromProbeOnlyRewritesClaims(t *testing.T) {
	tr := agytest.FromProbe(t)
	src, err := fs.Sub(harnessfixture.Files(), "agy/probe")
	if err != nil {
		t.Fatal(err)
	}
	rewritten := 0
	err = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(tr.Root, filepath.FromSlash(p)))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		if !bytes.Equal(got, want) {
			rewritten++
		}
		// Reversing the rewrite gives the fixture back byte for byte.
		for _, tok := range []string{"PROBE-A-7f3c", "PROBE-B-9d21", "PROBE2-A-51e0", "PROBE2-B-c4d2"} {
			got = bytes.ReplaceAll(got, []byte("--claim "+tok+" ."), []byte("claim "+tok+"."))
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from its fixture beyond the claim rewrite", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rewritten == 0 {
		t.Error("FromProbe rewrote no file: the briefs would not carry the engine's --claim form")
	}

	env := tr.Env(agytest.Probe1ChildA)
	if env("ANTIGRAVITY_CONVERSATION_ID") != agytest.Probe1ChildA || env("ANTIGRAVITY_APP_DATA_DIR") != tr.Root {
		t.Errorf("Env names conversation %q and root %q", env("ANTIGRAVITY_CONVERSATION_ID"), env("ANTIGRAVITY_APP_DATA_DIR"))
	}
	home := env("HOME")
	if st, err := os.Stat(home); err != nil || !st.IsDir() {
		t.Errorf("Env HOME %q is not a directory (%v)", home, err)
	}
	if real, err := os.UserHomeDir(); err == nil && home == real {
		t.Errorf("Env HOME is the real home %q", real)
	}
	if !strings.HasPrefix(home, os.TempDir()) {
		t.Errorf("Env HOME %q is not under the temp dir %q", home, os.TempDir())
	}
}
