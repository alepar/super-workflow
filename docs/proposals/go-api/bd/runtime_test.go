package bd_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// tempTracker returns a fresh git repo with an initialised bd tracker; the
// bd runtime finds it by cwd. It skips when bd or git is missing. The tracker
// is initialised once per package run (templateTracker) and copied per test:
// bd init costs seconds of CPU and the copy is a reflink on most filesystems.
func tempTracker(t *testing.T) string {
	t.Helper()
	for _, tool := range []string{"bd", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH; skipping bd runtime test", tool)
		}
	}
	tpl, err := templateTracker()
	if err != nil {
		t.Fatalf("template tracker: %v", err)
	}
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if out, err := exec.Command("cp", "-a", "--reflink=auto", tpl+"/.", dir).CombinedOutput(); err != nil {
		t.Fatalf("copy template tracker (%v): %s", err, out)
	}
	return dir
}

var (
	templateOnce sync.Once
	templateDir  string
	templateErr  error
)

// templateTracker builds the git repo plus bd tracker every tempTracker copy
// starts from. TestMain removes it when the package run ends.
func templateTracker() (string, error) {
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bd-template-")
		if err != nil {
			templateErr = err
			return
		}
		templateDir = dir
		home, err := os.MkdirTemp("", "bd-template-home-")
		if err != nil {
			templateErr = err
			return
		}
		defer os.RemoveAll(home)
		for _, c := range [][]string{{"git", "init", "-q"}, {"bd", "init", "--quiet", "--prefix", "tc"}} {
			cmd := exec.Command(c[0], c[1:]...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "HOME="+home)
			if out, err := cmd.CombinedOutput(); err != nil {
				templateErr = fmt.Errorf("setup %v failed (%v): %s", c, err, out)
				return
			}
		}
	})
	return templateDir, templateErr
}

func apply(t *testing.T, rt wf.AdapterRuntime, dir string, e wf.ExternalEffect) bd.Issue {
	t.Helper()
	raw, err := rt.Apply(dir, e)
	if err != nil {
		t.Fatalf("apply %s: %v", e.Op, err)
	}
	var is bd.Issue
	if err := json.Unmarshal(raw, &is); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return is
}

func read[T any](t *testing.T, rt wf.AdapterRuntime, dir string, d wf.ObservationDesc) T {
	t.Helper()
	raw, err := rt.Read(dir, d)
	if err != nil {
		t.Fatalf("read %s: %v", d.Op, err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func check(t *testing.T, rt wf.AdapterRuntime, dir string, e wf.ExternalEffect) wf.EffectStatus {
	t.Helper()
	st, err := rt.Check(dir, e)
	if err != nil {
		t.Fatalf("check %s: %v", e.Op, err)
	}
	return st
}

func ids(is []bd.Issue) []string {
	out := []string{}
	for _, i := range is {
		out = append(out, i.ID)
	}
	return out
}

func TestEveryDeclaredOpHasARuntime(t *testing.T) {
	src, err := os.ReadFile("bd.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, re := range []string{`Observe\[[^\]]*\]\[?[^\]]*\]?\("bd", "(\w+)"`, `Adapter: "bd", Op: "(\w+)"`} {
		for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(string(src), -1) {
			declared["bd."+m[1]] = true
		}
	}
	if len(declared) != 10 {
		t.Fatalf("found %d declared ops %v, want 10: the source scan is stale", len(declared), declared)
	}
	have := bd.Runtime(bd.Config{}).Ops()
	for op := range declared {
		if !slices.Contains(have, op) {
			t.Errorf("declared op %s has no runtime", op)
		}
	}
	for _, op := range have {
		if !declared[op] {
			t.Errorf("runtime serves undeclared op %s", op)
		}
	}
	if err := wf.NewAdapters().Register(bd.Runtime(bd.Config{})); err != nil {
		t.Errorf("register: %v", err)
	}
}

func TestMissingCLIUnavailable(t *testing.T) {
	rt := bd.Runtime(bd.Config{Bin: "bd-definitely-not-installed"})
	dir := t.TempDir()
	if _, err := rt.Read(dir, bd.Show("x").Desc()); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("read err = %v, want unavailable", err)
	}
	if _, err := rt.Apply(dir, bd.Close(wf.KeyOf("k"), "x", "r")); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("apply err = %v, want unavailable", err)
	}
	if _, err := rt.Check(dir, bd.Close(wf.KeyOf("k"), "x", "r")); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("check err = %v, want unavailable", err)
	}
}

func TestCreateReadyCloseRoundTrip(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	epic := apply(t, rt, dir, bd.Create(wf.KeyOf("epic"), bd.Issue{Title: "E", Type: "epic"}))
	if epic.ID == "" || epic.Type != "epic" {
		t.Fatalf("epic = %+v", epic)
	}
	task := apply(t, rt, dir, bd.Create(wf.KeyOf("t1"), bd.Issue{Title: "T", Description: "d", Parent: epic.ID, NoInheritLabels: true, RootLabel: "sp:" + epic.ID, Labels: []string{"x"}}))
	if task.Parent != epic.ID || task.Title != "T" || task.Description != "d" {
		t.Fatalf("task = %+v", task)
	}
	for _, want := range []string{"sp:" + epic.ID, "x", "key:t1"} {
		if !slices.Contains(task.Labels, want) {
			t.Errorf("labels %v lack %s", task.Labels, want)
		}
	}
	if got := ids(read[[]bd.Issue](t, rt, dir, bd.Ready(epic.ID).Desc())); !slices.Equal(got, []string{task.ID}) {
		t.Fatalf("ready = %v, want only %s (epics are excluded)", got, task.ID)
	}
	// ReadyIn takes the label and exclusions from the caller.
	if got := ids(read[[]bd.Issue](t, rt, dir, bd.ReadyIn("sp:"+epic.ID).Desc())); !slices.Equal(got, []string{task.ID}) {
		t.Errorf("ReadyIn(sp:epic) = %v, want only %s", got, task.ID)
	}
	if got := ids(read[[]bd.Issue](t, rt, dir, bd.ReadyIn(epic.ID).Desc())); len(got) != 0 {
		t.Errorf("ReadyIn(bare epic id) = %v, want none: the label is taken as given", got)
	}
	if got := ids(read[[]bd.Issue](t, rt, dir, bd.ReadyIn("sp:"+epic.ID, "y", "x").Desc())); len(got) != 0 {
		t.Errorf("ReadyIn excluding label x = %v, want none", got)
	}
	if got := ids(read[[]bd.Issue](t, rt, dir, bd.Ready("tc-nosuch").Desc())); len(got) != 0 {
		t.Errorf("ready for another epic = %v, want none", got)
	}
	if got := read[bd.Issue](t, rt, dir, bd.Show(task.ID).Desc()); got.ID != task.ID || got.Status != bd.Open {
		t.Errorf("show = %+v", got)
	}
	if got := ids(read[[]bd.Issue](t, rt, dir, bd.ListByLabel("key:t1").Desc())); !slices.Equal(got, []string{task.ID}) {
		t.Errorf("list by key label = %v", got)
	}
	cl := bd.Close(wf.KeyOf("close", task.ID), task.ID, "done")
	if st := check(t, rt, dir, cl); st != wf.EffectNotDone {
		t.Fatalf("check before close = %v", st)
	}
	if got := apply(t, rt, dir, cl); got.Status != bd.Closed {
		t.Fatalf("closed = %+v", got)
	}
	if st := check(t, rt, dir, cl); st != wf.EffectDone {
		t.Errorf("check after close = %v", st)
	}
	apply(t, rt, dir, cl) // closing a closed bead is a no-op, not an error
	if got := ids(read[[]bd.Issue](t, rt, dir, bd.Ready(epic.ID).Desc())); len(got) != 0 {
		t.Errorf("ready after close = %v", got)
	}
}

func TestRepeatedCreateIsDoneByCheck(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	e := bd.Create(bd.IssueKey("sp:x", "T"), bd.Issue{Title: "T"})
	if st := check(t, rt, dir, e); st != wf.EffectNotDone {
		t.Fatalf("check before create = %v", st)
	}
	apply(t, rt, dir, e)
	if st := check(t, rt, dir, e); st != wf.EffectDone {
		t.Errorf("check after create = %v, want done", st)
	}
	if st := check(t, rt, dir, bd.Create(bd.IssueKey("sp:x", "other"), bd.Issue{Title: "T"})); st != wf.EffectNotDone {
		t.Errorf("a different key = %v, want not done", st)
	}
	if st := check(t, rt, dir, bd.Create(wf.NonIdempotent("why"), bd.Issue{Title: "T"})); st != wf.EffectUnknown {
		t.Errorf("non-idempotent key = %v, want unknown", st)
	}
}

func TestCreateChecksTitleAndParent(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	p1 := apply(t, rt, dir, bd.Create(wf.KeyOf("p1"), bd.Issue{Title: "P1", Type: "epic"}))
	p2 := apply(t, rt, dir, bd.Create(wf.KeyOf("p2"), bd.Issue{Title: "P2", Type: "epic"}))
	k := wf.KeyOf("child")
	apply(t, rt, dir, bd.Create(k, bd.Issue{Title: "C", Parent: p1.ID}))
	cases := []struct {
		name  string
		issue bd.Issue
		want  wf.EffectStatus
	}{
		{"same title and parent", bd.Issue{Title: "C", Parent: p1.ID}, wf.EffectDone},
		{"no parent given", bd.Issue{Title: "C"}, wf.EffectDone},
		{"other parent", bd.Issue{Title: "C", Parent: p2.ID}, wf.EffectNotDone},
		{"other title", bd.Issue{Title: "D", Parent: p1.ID}, wf.EffectNotDone},
	}
	for _, c := range cases {
		if got := check(t, rt, dir, bd.Create(k, c.issue)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEpicCloseEligibleDoesNotClose(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	epic := apply(t, rt, dir, bd.Create(wf.KeyOf("e"), bd.Issue{Title: "E", Type: "epic"}))
	child := apply(t, rt, dir, bd.Create(wf.KeyOf("c"), bd.Issue{Title: "C", Parent: epic.ID}))
	if got := read[[]string](t, rt, dir, bd.CloseEligible(epic.ID).Desc()); len(got) != 0 {
		t.Fatalf("eligible with an open child = %v", got)
	}
	apply(t, rt, dir, bd.Close(wf.KeyOf("cc"), child.ID, "done"))
	got := read[[]string](t, rt, dir, bd.CloseEligible(epic.ID).Desc())
	if !slices.Equal(got, []string{epic.ID}) {
		t.Fatalf("eligible = %v, want [%s]", got, epic.ID)
	}
	if again := read[[]string](t, rt, dir, bd.CloseEligible("").Desc()); !slices.Equal(again, []string{epic.ID}) {
		t.Errorf("second observation = %v: the first one changed the tracker", again)
	}
	if st := read[bd.Issue](t, rt, dir, bd.Show(epic.ID).Desc()).Status; st != bd.Open {
		t.Errorf("epic status after observing = %s, want open (an observation must not close)", st)
	}
}

func TestDepAddRecordsReasonAndIsIdempotent(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	a := apply(t, rt, dir, bd.Create(wf.KeyOf("a"), bd.Issue{Title: "A", Description: "about A"}))
	b := apply(t, rt, dir, bd.Create(wf.KeyOf("b"), bd.Issue{Title: "B"}))
	reason := "blocked-by " + b.ID + ": consumes X"
	e := bd.DepAdd(wf.KeyOf("dep"), a.ID, b.ID, reason)
	if st := check(t, rt, dir, e); st != wf.EffectNotDone {
		t.Fatalf("check before = %v", st)
	}
	apply(t, rt, dir, e)
	got := apply(t, rt, dir, e) // second apply must not duplicate anything
	if n := strings.Count(got.Description, reason); n != 1 || !strings.Contains(got.Description, "about A") {
		t.Errorf("description = %q, want original text and the reason once", got.Description)
	}
	if st := check(t, rt, dir, e); st != wf.EffectDone {
		t.Errorf("check after = %v", st)
	}
	if st := check(t, rt, dir, bd.DepAdd(wf.KeyOf("dep"), b.ID, a.ID, "r")); st != wf.EffectNotDone {
		t.Errorf("reverse edge = %v, want not done", st)
	}
	// B blocks A, so only B is ready.
	if r := ids(read[[]bd.Issue](t, rt, dir, bd.Ready("").Desc())); !slices.Equal(r, []string{b.ID}) {
		t.Errorf("ready = %v, want [%s]", r, b.ID)
	}
}

func TestUpdateStatusCheck(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	a := apply(t, rt, dir, bd.Create(wf.KeyOf("a"), bd.Issue{Title: "A"}))
	e := bd.UpdateStatus(wf.KeyOf("s"), a.ID, bd.InProgress)
	if st := check(t, rt, dir, e); st != wf.EffectNotDone {
		t.Fatalf("check before = %v", st)
	}
	if got := apply(t, rt, dir, e); got.Status != bd.InProgress {
		t.Fatalf("applied = %+v", got)
	}
	if st := check(t, rt, dir, e); st != wf.EffectDone {
		t.Errorf("check after = %v", st)
	}
	apply(t, rt, dir, e) // idempotent
	if st, err := rt.Check(dir, bd.UpdateStatus(wf.KeyOf("s"), "tc-nosuch", bd.Open)); st != wf.EffectUnknown || err == nil {
		t.Errorf("check of a missing bead = %v, %v; want unknown with the error", st, err)
	}
}

// bdDirect runs the bd CLI itself, bypassing the runtime, to stage the state
// a crash between two steps of an effect would leave behind.
func bdDirect(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("bd", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd %v: %v: %s", args, err, out)
	}
}

func TestDepAddCheckRequiresReasonLine(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	a := apply(t, rt, dir, bd.Create(wf.KeyOf("a"), bd.Issue{Title: "A"}))
	b := apply(t, rt, dir, bd.Create(wf.KeyOf("b"), bd.Issue{Title: "B"}))
	reason := "blocked-by " + b.ID + ": consumes X"
	e := bd.DepAdd(wf.KeyOf("dep"), a.ID, b.ID, reason)
	bdDirect(t, dir, "dep", "add", a.ID, b.ID) // crash after the edge, before the reason line
	if st := check(t, rt, dir, e); st != wf.EffectNotDone {
		t.Fatalf("edge without its reason line = %v, want not done", st)
	}
	apply(t, rt, dir, e)
	if st := check(t, rt, dir, e); st != wf.EffectDone {
		t.Errorf("check after apply = %v, want done", st)
	}
	got := apply(t, rt, dir, e)
	if n := strings.Count(got.Description, reason); n != 1 {
		t.Errorf("description = %q, want the reason line once", got.Description)
	}
}

func TestDepAddNoReasonDoneOnEdge(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	a := apply(t, rt, dir, bd.Create(wf.KeyOf("a"), bd.Issue{Title: "A"}))
	b := apply(t, rt, dir, bd.Create(wf.KeyOf("b"), bd.Issue{Title: "B"}))
	e := bd.DepAdd(wf.KeyOf("dep"), a.ID, b.ID, "")
	if st := check(t, rt, dir, e); st != wf.EffectNotDone {
		t.Fatalf("check before = %v", st)
	}
	apply(t, rt, dir, e)
	if st := check(t, rt, dir, e); st != wf.EffectDone {
		t.Errorf("check after = %v, want done", st)
	}
}

func TestCommaTitleMakesNoStrayLabels(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	e := bd.Create(bd.IssueKey("sp:x", "Add, then sub"), bd.Issue{Title: "Add, then sub", Labels: []string{"sp:x"}})
	is := apply(t, rt, dir, e)
	shown := read[bd.Issue](t, rt, dir, bd.Show(is.ID).Desc())
	var stray []string
	for _, l := range shown.Labels {
		if l != "sp:x" && !strings.HasPrefix(l, "key:") {
			stray = append(stray, l)
		}
	}
	keys := 0
	for _, l := range shown.Labels {
		if strings.HasPrefix(l, "key:") {
			keys++
		}
	}
	if len(stray) != 0 || keys != 1 || len(shown.Labels) != 2 {
		t.Errorf("labels = %v, want exactly sp:x and one key label", shown.Labels)
	}
	if st := check(t, rt, dir, e); st != wf.EffectDone {
		t.Errorf("repeated check = %v, want done", st)
	}
}

func TestLongTitleKeyLabelIsBounded(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	title := strings.Repeat("long finding title ", 20)
	e := bd.Create(bd.IssueKey("sp:x", title), bd.Issue{Title: title, Labels: []string{"sp:x"}})
	if st := check(t, rt, dir, e); st != wf.EffectNotDone {
		t.Fatalf("check before = %v, want not done", st)
	}
	is := apply(t, rt, dir, e)
	shown := read[bd.Issue](t, rt, dir, bd.Show(is.ID).Desc())
	keys := 0
	for _, l := range shown.Labels {
		if strings.HasPrefix(l, "key:") {
			keys++
			if len(l) > 255 {
				t.Errorf("key label is %d characters, want at most 255", len(l))
			}
		}
	}
	if keys != 1 {
		t.Errorf("labels = %v, want exactly one key label", shown.Labels)
	}
	if st := check(t, rt, dir, e); st != wf.EffectDone {
		t.Errorf("repeated check = %v, want done", st)
	}
}

func TestHungToolTimesOutAsUnavailable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "hung")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 & wait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := bd.Runtime(bd.Config{Bin: bin, Timeout: 200 * time.Millisecond}).Read(t.TempDir(), bd.Show("x").Desc())
	if !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v, want a prompt timeout", d)
	}
}

// The CLI boundary is controlled because these tests must not write to the
// caller's tracker. Fixtures use bd show's old -> new supersedes dependency.
func TestSupersedeApplyAndReplay(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	script := `#!/bin/sh
printf '%s\n' "$*" >> calls
case "$*" in
 'show old --json') cat old.json ;;
 'show new --json') cat new.json ;;
 'supersede old --with new')
   printf '%s' '[{"id":"old","status":"closed","dependencies":[{"id":"new","dependency_type":"supersedes"}]}]' > old.json ;;
 'dep list old --direction=up -t blocks --json') printf '[]' ;;
 *) exit 19 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("old.json", `[{"id":"old","status":"open"}]`)
	write("new.json", `[{"id":"new","status":"open"}]`)
	rt := bd.Runtime(bd.Config{Bin: bin})
	e := bd.Supersede(wf.KeyOf("replace"), "old", "new")
	if got := check(t, rt, dir, e); got != wf.EffectNotDone {
		t.Fatalf("before = %v", got)
	}
	got := apply(t, rt, dir, e)
	if got.ID != "old" || got.Status != bd.Closed {
		t.Fatalf("result = %+v", got)
	}
	if got := check(t, rt, dir, e); got != wf.EffectDone {
		t.Fatalf("after = %v", got)
	}
	apply(t, rt, dir, e)
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "supersede old --with new\n") != 1 {
		t.Fatalf("write must occur exactly once: %s", calls)
	}
}

func TestSupersedeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, old, target string
		want              wf.EffectStatus
		wantErr           bool
	}{
		{"matching", `[{"id":"old","status":"closed","dependencies":[{"id":"new","dependency_type":"supersedes"}]}]`, `[{"id":"new"}]`, wf.EffectDone, false},
		{"different", `[{"id":"old","status":"closed","dependencies":[{"id":"other","dependency_type":"supersedes"}]}]`, `[{"id":"new"}]`, wf.EffectUnknown, true},
		{"closed only", `[{"id":"old","status":"closed"}]`, `[{"id":"new"}]`, wf.EffectNotDone, false},
		{"wrong edge", `[{"id":"old","status":"closed","dependencies":[{"id":"new","dependency_type":"blocks"}]}]`, `[{"id":"new"}]`, wf.EffectNotDone, false},
		{"partial", `[{"id":"old","status":"open","dependencies":[{"id":"new","dependency_type":"supersedes"}]}]`, `[{"id":"new"}]`, wf.EffectNotDone, false},
		{"missing target", `[{"id":"old","status":"closed","dependencies":[{"id":"new","dependency_type":"supersedes"}]}]`, `[]`, wf.EffectUnknown, true},
		{"wrong target", `[{"id":"old","status":"closed","dependencies":[{"id":"new","dependency_type":"supersedes"}]}]`, `[{"id":"other"}]`, wf.EffectUnknown, true},
		{"wrong source", `[{"id":"other","status":"closed","dependencies":[{"id":"new","dependency_type":"supersedes"}]}]`, `[{"id":"new"}]`, wf.EffectUnknown, true},
		{"invalid json", `garbage`, `[{"id":"new"}]`, wf.EffectUnknown, true},
		{"empty edge", `[{"id":"old","status":"closed","dependencies":[{"id":"","dependency_type":"supersedes"}]}]`, `[{"id":"new"}]`, wf.EffectUnknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bd")
			script := "#!/bin/sh\ncase \"$*\" in\n 'show old --json') cat old.json ;;\n 'show new --json') cat new.json ;;\n 'dep list old --direction=up -t blocks --json') printf '[]' ;;\n *) exit 19 ;;\nesac\n"
			for name, data := range map[string]string{"bd": script, "old.json": tc.old, "new.json": tc.target} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			rt := bd.Runtime(bd.Config{Bin: bin})
			e := bd.Supersede(wf.KeyOf("replace"), "old", "new")
			got, err := rt.Check(dir, e)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("Check = %v, %v; want %v, error=%v", got, err, tc.want, tc.wantErr)
			}
			if tc.wantErr {
				if _, err := rt.Apply(dir, e); err == nil {
					t.Fatal("Apply accepted invalid evidence")
				}
			}
		})
	}
}

func TestSupersedeUnavailableAndInvalidIDs(t *testing.T) {
	rt := bd.Runtime(bd.Config{Bin: "bd-definitely-not-installed"})
	e := bd.Supersede(wf.KeyOf("replace"), "old", "new")
	if _, err := rt.Apply(t.TempDir(), e); !errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("Apply = %v", err)
	}
	if got, err := rt.Check(t.TempDir(), e); got != wf.EffectUnknown || !errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("Check = %v, %v", got, err)
	}
	for _, pair := range [][2]string{{"", "new"}, {"old", ""}, {"old", "old"}, {"-old", "new"}, {"old", " new "}} {
		e := bd.Supersede(wf.KeyOf("replace"), pair[0], pair[1])
		if _, err := rt.Apply(t.TempDir(), e); err == nil || errors.Is(err, wf.ErrUnavailable) {
			t.Fatalf("invalid IDs reached CLI: %v", err)
		}
		if got, err := rt.Check(t.TempDir(), e); got != wf.EffectUnknown || err == nil || errors.Is(err, wf.ErrUnavailable) {
			t.Fatalf("invalid IDs: %v, %v", got, err)
		}
	}
}

func TestSupersedeUnconfirmedWrite(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	// A successful exit without the persisted edge must not claim completion.
	script := `#!/bin/sh
case "$*" in
 'show old --json') printf '%s' '[{"id":"old","status":"open"}]' ;;
 'show new --json') printf '%s' '[{"id":"new"}]' ;;
 'supersede old --with new') exit 0 ;;
 *) exit 19 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := bd.Runtime(bd.Config{Bin: bin}).Apply(dir, bd.Supersede(wf.KeyOf("replace"), "old", "new")); err == nil {
		t.Fatal("unconfirmed write accepted")
	}
}

func TestSupersedeTrackerFailure(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'tracker offline' >&2\nexit 12\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rt := bd.Runtime(bd.Config{Bin: bin})
	e := bd.Supersede(wf.KeyOf("replace"), "old", "new")
	if got, err := rt.Check(dir, e); got != wf.EffectUnknown || err == nil {
		t.Fatalf("Check = %v, %v", got, err)
	}
	if _, err := rt.Apply(dir, e); err == nil {
		t.Fatal("offline tracker accepted")
	}
}

// Item 85: superseding a bead moves every bead blocked on it onto the
// replacement, so a dependent never starts early on a closed blocker. The
// effect is done only once no dependent is left on the old bead.
func TestSupersedeRepointsDependents(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	script := `#!/bin/sh
printf '%s\n' "$*" >> calls
case "$*" in
 'show old --json') cat old.json ;;
 'show new --json') cat new.json ;;
 'supersede old --with new')
   printf '%s' '[{"id":"old","status":"closed","dependencies":[{"id":"new","dependency_type":"supersedes"}]}]' > old.json ;;
 'dep list old --direction=up -t blocks --json') cat dependents.json ;;
 'dep add d1 new'|'dep add d2 new') ;;
 'dep remove d1 old') printf '%s' '[{"id":"d2","dependency_type":"blocks"}]' > dependents.json ;;
 'dep remove d2 old') printf '%s' '[]' > dependents.json ;;
 *) exit 19 ;;
esac
`
	for name, data := range map[string]string{
		"bd":              script,
		"old.json":        `[{"id":"old","status":"open"}]`,
		"new.json":        `[{"id":"new","status":"open"}]`,
		"dependents.json": `[{"id":"d1","dependency_type":"blocks"},{"id":"d2","dependency_type":"blocks"}]`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rt := bd.Runtime(bd.Config{Bin: bin})
	e := bd.Supersede(wf.KeyOf("replace"), "old", "new")
	apply(t, rt, dir, e)
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	for _, want := range []string{"dep add d1 new\ndep remove d1 old\n", "dep add d2 new\ndep remove d2 old\n"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("calls lack %q (add before remove):\n%s", want, calls)
		}
	}
	if got := check(t, rt, dir, e); got != wf.EffectDone {
		t.Fatalf("after repoint = %v, want done", got)
	}
	// A dependent still on the old bead (a crash between supersede and the
	// repoint) is not done: the replay repoints it.
	if err := os.WriteFile(filepath.Join(dir, "dependents.json"), []byte(`[{"id":"d2","dependency_type":"blocks"}]`), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := check(t, rt, dir, e); got != wf.EffectNotDone {
		t.Fatalf("dependent left on old = %v, want not done", got)
	}
}

// A dependent that cannot take the edge to the replacement (bd refuses it as a
// cycle, or because the dependent is the replacement's parent) still leaves
// the closed old bead: the supersede does not fail on it forever.
func TestSupersedeRepointSkipsARefusedEdge(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	script := `#!/bin/sh
printf '%s\n' "$*" >> calls
case "$*" in
 'show old --json') printf '%s' '[{"id":"old","status":"closed","dependencies":[{"id":"new","dependency_type":"supersedes"}]}]' ;;
 'show new --json') printf '%s' '[{"id":"new","status":"open"}]' ;;
 'dep list old --direction=up -t blocks --json') cat dependents.json ;;
 'dep add d1 new') echo 'Error: adding dependency would create a cycle' >&2; exit 1 ;;
 'dep add d2 new') echo 'Error: cannot add dependency: d2 is already a child of new' >&2; exit 1 ;;
 'dep add d3 new') echo 'Error: database is locked' >&2; exit 1 ;;
 'dep remove d1 old'|'dep remove d2 old') printf '%s' '[]' > dependents.json ;;
 *) exit 19 ;;
esac
`
	for name, data := range map[string]string{"bd": script, "dependents.json": `[{"id":"d1","dependency_type":"blocks"},{"id":"d2","dependency_type":"blocks"}]`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rt := bd.Runtime(bd.Config{Bin: bin})
	e := bd.Supersede(wf.KeyOf("replace"), "old", "new")
	if _, err := rt.Apply(dir, e); err != nil {
		t.Fatalf("Apply = %v, want the refused edges skipped", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	for _, want := range []string{"dep remove d1 old\n", "dep remove d2 old\n"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("calls lack %q:\n%s", want, calls)
		}
	}
	// Any other failure still fails the effect.
	if err := os.WriteFile(filepath.Join(dir, "dependents.json"), []byte(`[{"id":"d3","dependency_type":"blocks"}]`), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Apply(dir, e); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("Apply with a locked database = %v, want the failure", err)
	}
}

// the regression case (the workflow run, BeadsWire seq 2456): d1 waits on old, and the same
// plan superseded d1 with new first. Superseding old with new must skip the
// closed d1: bd refused "dep add d1 new" because d1 already had a supersedes
// edge to new. A closed dependent carries no work; an open one still moves.
func TestSupersedeRepointSkipsAClosedDependent(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	script := `#!/bin/sh
printf '%s\n' "$*" >> calls
case "$*" in
 'show old --json') printf '%s' '[{"id":"old","status":"closed","dependencies":[{"id":"new","dependency_type":"supersedes"}]}]' ;;
 'show new --json') printf '%s' '[{"id":"new","status":"open"}]' ;;
 'dep list old --direction=up -t blocks --json') cat dependents.json ;;
 'dep add d1 new') echo 'Error: dependency d1 -> new already exists with type "supersedes" (requested "blocks")' >&2; exit 1 ;;
 'dep add d2 new') ;;
 'dep remove d2 old') printf '%s' '[{"id":"d1","status":"closed","dependency_type":"blocks"}]' > dependents.json ;;
 *) exit 19 ;;
esac
`
	for name, data := range map[string]string{"bd": script,
		"dependents.json": `[{"id":"d1","status":"closed","dependency_type":"blocks"},{"id":"d2","status":"open","dependency_type":"blocks"}]`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rt := bd.Runtime(bd.Config{Bin: bin})
	e := bd.Supersede(wf.KeyOf("replace"), "old", "new")
	if _, err := rt.Apply(dir, e); err != nil {
		t.Fatalf("Apply = %v, want the closed dependent skipped", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(calls), "dep add d1 new") {
		t.Errorf("the closed dependent d1 was repointed:\n%s", calls)
	}
	if !strings.Contains(string(calls), "dep add d2 new\ndep remove d2 old\n") {
		t.Errorf("the open dependent d2 did not move:\n%s", calls)
	}
	if got := check(t, rt, dir, e); got != wf.EffectDone {
		t.Fatalf("with only a closed dependent left on old = %v, want done", got)
	}
}

// SetMetadata writes each field as key=value through bd update: a value that
// looks like an option or a command stays the part after its key's '='.
func TestSetMetadata(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	a := apply(t, rt, dir, bd.Create(wf.KeyOf("a"), bd.Issue{Title: "A"}))
	e := bd.SetMetadata(wf.KeyOf("m"), a.ID, map[string]string{"sp_owns": `["--help","$(rm -rf /)"]`, "sp_consumes": "[]"})
	apply(t, rt, dir, e)
	apply(t, rt, dir, e) // idempotent
	cmd := exec.Command("bd", "show", a.ID, "--json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd show: %v: %s", err, out)
	}
	if !strings.Contains(string(out), `--help`) || !strings.Contains(string(out), `$(rm -rf /)`) || !strings.Contains(string(out), "sp_consumes") {
		t.Fatalf("metadata not written verbatim: %s", out)
	}
	if _, err := rt.Apply(dir, bd.SetMetadata(wf.KeyOf("bad"), a.ID, map[string]string{"--x": "y"})); err == nil {
		t.Fatal("an option-shaped key was accepted")
	}
}

// A dependency already linking the pair (old blocked by new, as example workflow's
// Amend wiring adds before superseding an integration sweep) makes bd refuse
// the supersedes edge. Superseding makes that edge obsolete: the supersede
// replaces it and succeeds against a real tracker.
func TestSupersedeReplacesALinkBetweenThePair(t *testing.T) {
	dir := tempTracker(t)
	rt := bd.Runtime(bd.Config{})
	old := apply(t, rt, dir, bd.Create(wf.KeyOf("old"), bd.Issue{Title: "Integration sweep"}))
	repl := apply(t, rt, dir, bd.Create(wf.KeyOf("new"), bd.Issue{Title: "Replacement"}))
	apply(t, rt, dir, bd.DepAdd(wf.KeyOf("dep"), old.ID, repl.ID, "blocked-by "+repl.ID+": consumes all leaves"))
	e := bd.Supersede(wf.KeyOf("replace"), old.ID, repl.ID)
	if st := check(t, rt, dir, e); st != wf.EffectNotDone {
		t.Fatalf("before = %v", st)
	}
	if got := apply(t, rt, dir, e); got.ID != old.ID || got.Status != bd.Closed {
		t.Fatalf("result = %+v", got)
	}
	if st := check(t, rt, dir, e); st != wf.EffectDone {
		t.Fatalf("after = %v", st)
	}
	apply(t, rt, dir, e) // replay is a no-op
}

// Only a blocks edge is obsolete under a supersede. Any other type linking
// old to new (provenance, a relation) is kept: bd refuses the supersede over
// it, so the effect fails naming the type and the link survives.
func TestSupersedeKeepsANonBlocksLinkBetweenThePair(t *testing.T) {
	for _, typ := range []string{"discovered-from", "related"} {
		t.Run(typ, func(t *testing.T) {
			dir := tempTracker(t)
			rt := bd.Runtime(bd.Config{})
			old := apply(t, rt, dir, bd.Create(wf.KeyOf("old"), bd.Issue{Title: "Old"}))
			repl := apply(t, rt, dir, bd.Create(wf.KeyOf("new"), bd.Issue{Title: "Replacement"}))
			cmd := exec.Command("bd", "dep", "add", old.ID, repl.ID, "--type", typ)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("dep add --type %s: %v: %s", typ, err, out)
			}
			_, err := rt.Apply(dir, bd.Supersede(wf.KeyOf("replace"), old.ID, repl.ID))
			if err == nil || !strings.Contains(err.Error(), `"`+typ+`"`) {
				t.Fatalf("apply err = %v, want a refusal naming %q", err, typ)
			}
			cmd = exec.Command("bd", "show", old.ID, "--json")
			cmd.Dir = dir
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			var v []struct {
				Status string `json:"status"`
				Deps   []struct {
					ID   string `json:"id"`
					Type string `json:"dependency_type"`
				} `json:"dependencies"`
			}
			if err := json.Unmarshal(out, &v); err != nil || len(v) != 1 {
				t.Fatalf("decode %s: %v", out, err)
			}
			if v[0].Status == "closed" || len(v[0].Deps) != 1 || v[0].Deps[0].ID != repl.ID || v[0].Deps[0].Type != typ {
				t.Fatalf("old after refused supersede = %+v, want open with only its %s link", v[0], typ)
			}
		})
	}
}

func TestTransientFailureIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"Error: database is locked", true},
		{"lock acquisition timeout after 5s", true},
		{"open .beads/dolt/lock: permission denied (lock held)", false},
		{"no beads database found", false},
		// An echoed input or a hint is not the diagnostic.
		{"Issue x not found\nHint: if the database is locked, retry", false},
	} {
		bin := filepath.Join(t.TempDir(), "bd")
		script := "#!/bin/sh\necho '" + tc.out + "' >&2\nexit 1\n"
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := bd.Runtime(bd.Config{Bin: bin}).Read(t.TempDir(), bd.Show("x").Desc())
		if err == nil || errors.Is(err, wf.ErrUnavailable) != tc.want {
			t.Errorf("%q: err = %v, want unavailable %v", tc.out, err, tc.want)
		}
	}
}

// A lookup of an id that reads like a transient failure is still a
// permanent failure: the diagnostic, not the echoed id, is classified.
func TestTransientIgnoresEchoedInput(t *testing.T) {
	dir := tempTracker(t)
	_, err := bd.Runtime(bd.Config{}).Read(dir, bd.Show("timed out").Desc())
	if err == nil || errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("show of a missing id %q: err = %v, want a permanent failure", "timed out", err)
	}
	// The same with bd's JSON error object on stdout and a lock word on stderr.
	bin := filepath.Join(t.TempDir(), "bd")
	script := "#!/bin/sh\necho 'database is locked' >&2\necho '{\"error\": \"no issues found matching the provided IDs\"}'\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := bd.Runtime(bd.Config{Bin: bin}).Read(t.TempDir(), bd.Show("x").Desc()); err == nil || errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("JSON error object: err = %v, want a permanent failure", err)
	}
}

// Cutting the call's arguments out of the diagnostic removes whole tokens
// only: a one-letter label must not erase letters of "database is locked".
func TestTransientSurvivesShortArgument(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'Error: database is locked' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := bd.Runtime(bd.Config{Bin: bin}).Read(t.TempDir(), bd.ReadyIn("a").Desc())
	if !errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("ReadyIn(\"a\") with a held lock: err = %v, want ErrUnavailable", err)
	}
}
