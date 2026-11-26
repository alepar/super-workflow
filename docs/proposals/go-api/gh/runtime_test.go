package gh_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/gh"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// declaredOps lists the ops gh.go's constructors declare, read from source so
// a new constructor without a runtime fails here.
func declaredOps(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("gh.go")
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, m := range regexp.MustCompile(`(?:wf\.Observe\[\w+\]\(|Adapter: )"gh",? ?(?:Op: )?"(\w+)"`).FindAllStringSubmatch(string(src), -1) {
		ops = append(ops, "gh."+m[1])
	}
	return ops
}

func TestEveryDeclaredOpHasARuntime(t *testing.T) {
	declared := declaredOps(t)
	if len(declared) != 3 {
		t.Fatalf("declared ops = %v, want 3", declared)
	}
	have := map[string]bool{}
	for _, op := range gh.Runtime(gh.Config{}).Ops() {
		have[op] = true
	}
	for _, op := range declared {
		if !have[op] {
			t.Errorf("no runtime for %s", op)
		}
	}
}

func registry(t *testing.T) *wf.Adapters {
	t.Helper()
	a := wf.NewAdapters()
	if err := a.Register(gh.Runtime(gh.Config{})); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAbsentGhClassifiesUnavailable(t *testing.T) {
	t.Setenv("PATH", "")
	a := registry(t)
	ws := t.TempDir()
	for _, d := range []wf.ObservationDesc{gh.PluginJSON("o/r", "main").Desc(), gh.IssueView("o/r", 1).Desc()} {
		if _, err := a.Read(ws, d); !errors.Is(err, wf.ErrUnavailable) {
			t.Errorf("Read %s = %v, want unavailable", d.Op, err)
		}
	}
	e := gh.IssueCreate(wf.KeyOf("k"), "o/r", "t", "b")
	if _, err := a.Apply(ws, e); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("Apply = %v, want unavailable", err)
	}
	st, err := a.Check(ws, e)
	if !errors.Is(err, wf.ErrUnavailable) || st != wf.EffectUnknown {
		t.Errorf("Check = %v, %v, want unknown + unavailable", st, err)
	}
}

// stubGh puts a gh script first on PATH. It writes its cwd and then each arg,
// all NUL-terminated, to log, then prints out and exits with code.
func stubGh(t *testing.T, out string, code int) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "argv.log")
	outFile := filepath.Join(dir, "out")
	if err := os.WriteFile(outFile, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n{ printf '%s\\0' \"$PWD\"; for a in \"$@\"; do printf '%s\\0' \"$a\"; done; } >> '" + log + "'\ncat '" + outFile + "'\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// stubGhRoutes puts a gh script first on PATH that prints list for
// "gh issue list" and create for anything else. Each call is logged as its
// NUL-terminated args followed by a record separator (0x1e).
func stubGhRoutes(t *testing.T, list, create string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	for name, out := range map[string]string{"list": list, "create": create} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\n{ for a in \"$@\"; do printf '%s\\0' \"$a\"; done; printf '\\036'; } >> '" + log + "'\n" +
		"if [ \"$1 $2\" = 'issue list' ]; then cat '" + filepath.Join(dir, "list") + "'; else cat '" + filepath.Join(dir, "create") + "'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// calls reads a stubGhRoutes log.
func calls(t *testing.T, log string) (out [][]string) {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\x1e"), "\x1e") {
		out = append(out, strings.Split(strings.TrimSuffix(line, "\x00"), "\x00"))
	}
	return out
}

func argv(t *testing.T, log string) (cwd string, args []string) {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	return parts[0], parts[1:]
}

func TestArgvAgainstStubGh(t *testing.T) {
	marker := "<!-- effect-key: fix/F1 -->"
	key := wf.KeyOf("fix", "F1")
	for _, c := range []struct {
		name string
		out  string
		run  func(a *wf.Adapters, ws string) (json.RawMessage, error)
		args []string
		want string // substring of the decoded result
	}{
		{"plugin_json", `{"name":"sp","version":"1.2"}`,
			func(a *wf.Adapters, ws string) (json.RawMessage, error) {
				return a.Read(ws, gh.PluginJSON("o/r", "main").Desc())
			},
			[]string{"api", "repos/o/r/contents/.claude-plugin/plugin.json?ref=main", "-H", "Accept: application/vnd.github.raw"},
			`"Name":"sp"`},
		{"issue_view", `{"number":7,"title":"T","body":"B","state":"OPEN"}`,
			func(a *wf.Adapters, ws string) (json.RawMessage, error) {
				return a.Read(ws, gh.IssueView("o/r", 7).Desc())
			},
			[]string{"issue", "view", "7", "--repo", "o/r", "--json", "number,title,body,state"},
			`"Number":7`},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := stubGh(t, c.out, 0)
			ws := t.TempDir()
			got, err := c.run(registry(t), ws)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), c.want) {
				t.Errorf("result = %s, want %s", got, c.want)
			}
			cwd, args := argv(t, log)
			if r, _ := filepath.EvalSymlinks(ws); cwd != r && cwd != ws {
				t.Errorf("cwd = %s, want %s", cwd, ws)
			}
			if strings.Join(args, "\x00") != strings.Join(c.args, "\x00") {
				t.Errorf("argv = %q, want %q", args, c.args)
			}
		})
	}

	t.Run("check", func(t *testing.T) {
		for _, k := range []struct {
			out  string
			code int
			want wf.EffectStatus
			err  bool
		}{
			{`[{"number":3,"body":"b\n\n<!-- effect-key: fix/F1 -->"}]`, 0, wf.EffectDone, false},
			{`[]`, 0, wf.EffectNotDone, false},
			// A search hit whose body holds another key's marker is not this effect's issue.
			{`[{"number":4,"body":"b\n\n<!-- effect-key: fix/F1-other -->"}]`, 0, wf.EffectNotDone, false},
			{`[{"number":4,"body":"<!-- effect-key: fix/F1-other -->"},{"number":5,"body":"<!-- effect-key: fix/F1 -->"}]`, 0, wf.EffectDone, false},
			{``, 1, wf.EffectUnknown, true},
			{`not json`, 0, wf.EffectUnknown, true},
		} {
			log := stubGh(t, k.out, k.code)
			st, err := registry(t).Check(t.TempDir(), gh.IssueCreate(key, "o/r", "t", "b"))
			if st != k.want || (err != nil) != k.err {
				t.Errorf("out %q code %d: Check = %v, %v; want %v, err=%v", k.out, k.code, st, err, k.want, k.err)
			}
			_, args := argv(t, log)
			want := []string{"issue", "list", "--repo", "o/r", "--search", `"` + marker + `" in:body`, "--json", "number,title,body,state", "--state", "all"}
			if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
				t.Errorf("argv = %q, want %q", args, want)
			}
		}
	})

	search := []string{"issue", "list", "--repo", "o/r", "--search", `"` + marker + `" in:body`, "--json", "number,title,body,state", "--state", "all"}
	t.Run("issue_create searches for its marker, then creates", func(t *testing.T) {
		log := stubGhRoutes(t, `[]`, "https://github.com/o/r/issues/42\n")
		got, err := registry(t).Apply(t.TempDir(), gh.IssueCreate(key, "o/r", "title", "body"))
		if err != nil || !strings.Contains(string(got), `"Number":42`) {
			t.Fatalf("Apply = %s, %v; want issue 42", got, err)
		}
		want := [][]string{search, {"issue", "create", "--repo", "o/r", "--title", "title", "--body", "body\n\n" + marker}}
		if calls := calls(t, log); fmt.Sprint(calls) != fmt.Sprint(want) {
			t.Errorf("calls = %q, want %q", calls, want)
		}
	})
	t.Run("issue_create returns the issue it finds and creates none", func(t *testing.T) {
		log := stubGhRoutes(t, `[{"number":9,"title":"title","body":"body\n\n`+marker+`","state":"CLOSED"}]`, "https://github.com/o/r/issues/43\n")
		got, err := registry(t).Apply(t.TempDir(), gh.IssueCreate(key, "o/r", "title", "body"))
		if err != nil || !strings.Contains(string(got), `"Number":9`) || !strings.Contains(string(got), `"State":"CLOSED"`) {
			t.Fatalf("Apply = %s, %v; want the found issue 9", got, err)
		}
		if calls := calls(t, log); fmt.Sprint(calls) != fmt.Sprint([][]string{search}) {
			t.Errorf("calls = %q, want only the search", calls)
		}
	})
	t.Run("an unkeyed issue_create does not search", func(t *testing.T) {
		log := stubGhRoutes(t, `[]`, "https://github.com/o/r/issues/44\n")
		if _, err := registry(t).Apply(t.TempDir(), gh.IssueCreate(wf.NonIdempotent("synthetic"), "o/r", "t", "b")); err != nil {
			t.Fatal(err)
		}
		if calls := calls(t, log); len(calls) != 1 || calls[0][1] != "create" {
			t.Errorf("calls = %q, want one create", calls)
		}
	})
	t.Run("a done check returns the issue it found", func(t *testing.T) {
		stubGhRoutes(t, `[{"number":3,"title":"t","body":"b\n\n`+marker+`","state":"OPEN"}]`, "")
		st, res, err := registry(t).CheckResultAt(t.TempDir(), wf.EffectContext{}, gh.IssueCreate(key, "o/r", "t", "b"))
		if err != nil || st != wf.EffectDone || !strings.Contains(string(res), `"Number":3`) {
			t.Errorf("CheckResultAt = %v, %s, %v; want done with issue 3", st, res, err)
		}
	})
	t.Run("a sandboxed run's effect and check are refused, its reads are not", func(t *testing.T) {
		log := stubGhRoutes(t, `[]`, "https://github.com/o/r/issues/45\n")
		a, ws, at := registry(t), t.TempDir(), wf.EffectContext{Sandboxed: true}
		e := gh.IssueCreate(key, "o/r", "t", "b")
		if _, err := a.ApplyAt(ws, at, e); !errors.Is(err, wf.ErrSandboxRefused) || !strings.Contains(err.Error(), "gh.issue_create") {
			t.Errorf("ApplyAt = %v, want a refusal naming gh.issue_create", err)
		}
		if st, err := a.CheckAt(ws, at, e); !errors.Is(err, wf.ErrSandboxRefused) || st != wf.EffectUnknown {
			t.Errorf("CheckAt = %v, %v, want unknown + refused", st, err)
		}
		if _, err := os.Stat(log); !os.IsNotExist(err) {
			t.Errorf("gh ran in a sandboxed run: %q", calls(t, log))
		}
		if _, err := a.ReadAt(ws, at, gh.IssueView("o/r", 1).Desc()); err != nil && errors.Is(err, wf.ErrSandboxRefused) {
			t.Errorf("ReadAt refused: %v", err)
		}
	})

	t.Run("failed gh is a failure, not unavailable", func(t *testing.T) {
		stubGh(t, "", 1)
		_, err := registry(t).Read(t.TempDir(), gh.IssueView("o/r", 1).Desc())
		if err == nil || errors.Is(err, wf.ErrUnavailable) {
			t.Errorf("err = %v, want a plain failure", err)
		}
	})
}

func TestHungToolTimesOutAsUnavailable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "hung")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 & wait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := gh.Runtime(gh.Config{Bin: bin, Timeout: 200 * time.Millisecond}).Read(t.TempDir(), gh.IssueView("o/r", 1).Desc())
	if !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v, want a prompt timeout", d)
	}
}
