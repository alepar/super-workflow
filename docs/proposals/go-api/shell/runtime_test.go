package shell_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func run(t *testing.T, rt wf.AdapterRuntime, ws string, i shell.Invocation) wf.RunResult {
	t.Helper()
	raw, err := rt.Read(ws, shell.Observe(i).Desc())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var r wf.RunResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return r
}

func TestEveryDeclaredOpHasARuntime(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	pats := []*regexp.Regexp{
		regexp.MustCompile(`wf\.Observe\[.*\]\("([a-z_]+)", "([a-z_]+)"`),
		regexp.MustCompile(`Adapter: "([a-z_]+)", Op: "([a-z_]+)"`),
	}
	declared := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pats {
			for _, m := range p.FindAllStringSubmatch(string(src), -1) {
				declared[m[1]+"."+m[2]] = true
			}
		}
	}
	var want []string
	for op := range declared {
		want = append(want, op)
	}
	got := shell.Runtime(shell.Config{}).Ops()
	sort.Strings(want)
	sort.Strings(got)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime ops %v, declared ops %v", got, want)
	}
}

func TestExitCaptured(t *testing.T) {
	rt := shell.Runtime(shell.Config{})
	r := run(t, rt, t.TempDir(), shell.Run("sh", "-c", "echo out; echo err >&2; exit 3"))
	if r.Exit != 3 || r.Stdout != "out\n" || r.Stderr != "err\n" {
		t.Errorf("result = %+v", r)
	}
	if r := run(t, rt, t.TempDir(), shell.Run("true")); r.Exit != 0 {
		t.Errorf("true exit = %d", r.Exit)
	}
	// The same call as an effect.
	raw, err := rt.Apply(t.TempDir(), shell.Effect(wf.KeyOf("k"), shell.Run("sh", "-c", "exit 2")))
	var ar wf.RunResult
	if err != nil || json.Unmarshal(raw, &ar) != nil || ar.Exit != 2 {
		t.Errorf("Apply = %s, %v", raw, err)
	}
}

func TestArgvIsNotShellParsed(t *testing.T) {
	r := run(t, shell.Runtime(shell.Config{}), t.TempDir(), shell.Run("echo", "a b", "$HOME", ";", "x"))
	if r.Stdout != "a b $HOME ; x\n" {
		t.Errorf("stdout = %q", r.Stdout)
	}
}

func TestEmptyArgvFails(t *testing.T) {
	_, err := shell.Runtime(shell.Config{}).Read(t.TempDir(), shell.Observe(shell.Run()).Desc())
	if err == nil || errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}

func TestTimeoutFails(t *testing.T) {
	rt := shell.Runtime(shell.Config{Timeout: 200 * time.Millisecond})
	start := time.Now()
	// The child shell's own child holds the pipes; the process group dies with it.
	_, err := rt.Read(t.TempDir(), shell.Observe(shell.Run("sh", "-c", "sleep 30; echo late")).Desc())
	if err == nil || errors.Is(err, wf.ErrUnavailable) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("timeout took %v", d)
	}
}

func TestCwdIsWorkspace(t *testing.T) {
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	rt := shell.Runtime(shell.Config{})
	for _, c := range []struct{ dir, want string }{
		{"", real}, {".", real}, {"sub", filepath.Join(real, "sub")}, {real, real},
	} {
		if got := strings.TrimSpace(run(t, rt, ws, shell.Run("pwd", "-P").In(c.dir)).Stdout); got != c.want {
			t.Errorf("dir %q: cwd = %q, want %q", c.dir, got, c.want)
		}
	}
}

func TestMissingCLIUnavailable(t *testing.T) {
	_, err := shell.Runtime(shell.Config{}).Read(t.TempDir(), shell.Observe(shell.Run("no-such-cli-xyzzy")).Desc())
	if !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}

func TestMissingBashScriptIsExitData(t *testing.T) {
	r := run(t, shell.Runtime(shell.Config{}), t.TempDir(), shell.Run("bash", "no-such-script.sh"))
	if r.Exit != 127 {
		t.Errorf("exit = %d, stderr %q", r.Exit, r.Stderr)
	}
}

func TestKeyedEffectCheckIsNotDone(t *testing.T) {
	rt := shell.Runtime(shell.Config{})
	ws := t.TempDir()
	if st, err := rt.Check(ws, shell.Effect(wf.KeyOf("k"), shell.Run("true"))); err != nil || st != wf.EffectNotDone {
		t.Errorf("keyed = %v, %v", st, err)
	}
	if st, err := rt.Check(ws, shell.Effect(wf.NonIdempotent("why"), shell.Run("true"))); err != nil || st != wf.EffectUnknown {
		t.Errorf("non-idempotent = %v, %v", st, err)
	}
}

func TestBackgroundDescendantAfterExitZero(t *testing.T) {
	start := time.Now()
	r := run(t, shell.Runtime(shell.Config{}), t.TempDir(), shell.Run("sh", "-c", "echo hi; sleep 3 & exit 0"))
	if r.Exit != 0 || r.Stdout != "hi\n" {
		t.Errorf("result = %+v", r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestNonZeroWithBackgroundDescendant(t *testing.T) {
	r := run(t, shell.Runtime(shell.Config{}), t.TempDir(), shell.Run("sh", "-c", "sleep 3 & exit 4"))
	if r.Exit != 4 {
		t.Errorf("exit = %d, result = %+v", r.Exit, r)
	}
}

func TestTimeoutKeepsOutput(t *testing.T) {
	rt := shell.Runtime(shell.Config{Timeout: 200 * time.Millisecond})
	_, err := rt.Read(t.TempDir(), shell.Observe(shell.Run("sh", "-c", "echo partial; echo oops >&2; sleep 30")).Desc())
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"timed out", "partial", "oops"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q lacks %q", err, want)
		}
	}
}

func TestRunFeedsStdin(t *testing.T) {
	payload := strings.Repeat("a line of input\n", 20000) // 320 KB: past MAX_ARG_STRLEN
	r := run(t, shell.Runtime(shell.Config{}), t.TempDir(), shell.Run("cat").WithStdin(payload))
	if r.Exit != 0 || r.Stdout != payload {
		t.Errorf("cat on stdin: exit %d, stdout %d bytes, want exit 0 and the %d-byte input back", r.Exit, len(r.Stdout), len(payload))
	}
}

func TestRunWithoutStdinUnchanged(t *testing.T) {
	i := shell.Run("echo", "x")
	eff := shell.Effect(wf.KeyOf("k"), i)
	if _, has := eff.Args["stdin"]; has {
		t.Errorf("effect args %v carry stdin without WithStdin", eff.Args)
	}
	if got := shell.Effect(wf.KeyOf("k"), i.WithStdin("in")).Args["stdin"]; got != "in" {
		t.Errorf("effect args stdin = %v, want in", got)
	}
	if r := run(t, shell.Runtime(shell.Config{}), t.TempDir(), shell.Run("sh", "-c", "cat; echo done </dev/null")); r.Stdout != "done\n" {
		t.Errorf("no stdin: stdout %q, want done", r.Stdout)
	}
}

func TestCommandDirectoryCheckUsesRuntimeFilesystem(t *testing.T) {
	checker := shell.Runtime(shell.Config{}).(wf.CommandDirectoryChecker)
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"", ".", "sub", filepath.Join(workspace, "sub")} {
		if err := checker.CheckCommandDirectory(workspace, dir); err != nil {
			t.Fatalf("%q: %v", dir, err)
		}
	}
	file := filepath.Join(workspace, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"missing", file} {
		if err := checker.CheckCommandDirectory(workspace, dir); !errors.Is(err, wf.ErrUnavailable) {
			t.Fatalf("%q: %v, want unavailable", dir, err)
		}
	}
}
