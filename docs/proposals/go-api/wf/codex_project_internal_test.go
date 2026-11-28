package wf

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeCodexOnPath puts a `codex execpolicy check --rules F... CMD...` on PATH
// that allows when a rule's two-word pattern is CMD's first two words.
func fakeCodexOnPath(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake codex is a shell script")
	}
	bin := t.TempDir()
	fake := `#!/bin/sh
shift 2
rules=""
while [ "$1" = --rules ]; do rules="$rules $2"; shift 2; done
if cat $rules | grep -qF "pattern = [\"$1\",\"$2\"]"; then echo '{"decision":"allow"}'; else echo '{"matchedRules":[]}'; fi
`
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func trustConfig(paths ...string) string {
	var b strings.Builder
	b.WriteString("model = \"m\"\n\n[features]\nx = true\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "\n[projects.%q]\ntrust_level = \"trusted\"\n", p)
	}
	return b.String()
}

func TestTrustedProjects(t *testing.T) {
	cfg := `[projects."/a"]
trust_level = "trusted"

[projects.'/b c'] # literal key
trust_level = 'trusted'

[projects."/u"]
trust_level = "untrusted"

[other]
trust_level = "trusted"

[projects."/d"]
model = "x"
trust_level = "trusted" # comment
`
	if got, want := trustedProjects(cfg), []string{"/a", "/b c", "/d"}; !slices.Equal(got, want) {
		t.Fatalf("trustedProjects = %q, want %q", got, want)
	}
}

// codexProjectLayers walks from the project root (the nearest .git) down to
// the working directory, and trusts by the root only: a trusted parent of
// the root does not count, a trusted main checkout does for its linked
// worktree. Codex 0.172 behaves so for project config layers
// (`codex doctor` reports the layered model).
func TestCodexProjectLayers(t *testing.T) {
	top := canonicalPath(t.TempDir())
	repo := filepath.Join(top, "repo")
	cwd := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(top, "codex")
	env := map[string]string{"CODEX_HOME": home}
	get := func(k string) string { return env[k] }

	dirs, root, _ := codexProjectLayers(get, cwd)
	want := []string{filepath.Join(repo, ".codex", "rules"), filepath.Join(repo, "a", ".codex", "rules"), filepath.Join(cwd, ".codex", "rules")}
	if root != repo || !slices.Equal(dirs, want) {
		t.Fatalf("layers = %q root %q, want %q root %q", dirs, root, want, repo)
	}
	for _, c := range []struct {
		trust []string
		want  bool
	}{
		{nil, false},
		{[]string{top}, false},
		{[]string{filepath.Join(repo, "a")}, false},
		{[]string{repo}, true},
	} {
		writeTestFile(t, filepath.Join(home, "config.toml"), trustConfig(c.trust...))
		if _, _, got := codexProjectLayers(get, cwd); got != c.want {
			t.Errorf("trusting %q: trusted = %v, want %v", c.trust, got, c.want)
		}
	}

	// A linked worktree: its own root, trusted through its main checkout.
	wt := filepath.Join(top, "wt")
	gitdir := filepath.Join(repo, ".git", "worktrees", "wt")
	writeTestFile(t, filepath.Join(wt, ".git"), "gitdir: "+gitdir+"\n")
	writeTestFile(t, filepath.Join(gitdir, "commondir"), "../..\n")
	writeTestFile(t, filepath.Join(home, "config.toml"), trustConfig(repo))
	dirs, root, trusted := codexProjectLayers(get, wt)
	if root != wt || !trusted || !slices.Equal(dirs, []string{filepath.Join(wt, ".codex", "rules")}) {
		t.Fatalf("worktree: layers %q root %q trusted %v", dirs, root, trusted)
	}
}

// The preflight finds the rule in the project's .codex/rules from a
// directory below the project root, and only while Codex trusts the
// project; untrusted, it says so.
func TestCodexRuleCheckFindsTrustedProjectRules(t *testing.T) {
	fakeCodexOnPath(t)
	top := canonicalPath(t.TempDir())
	repo, home := filepath.Join(top, "repo"), filepath.Join(top, "codex")
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	env := map[string]string{"CODEX_HOME": home}
	c := &cli{env: func(k string) string { return env[k] }, stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	writeTestFile(t, filepath.Join(repo, ".codex", "rules", codexRulesName), codexRule(c.spawnPrefix()))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	printed := multiVerbLine(exe, filepath.Join(top, "state"), "r1", "spawn", "--claim", strings.Repeat("0", 16))

	why, fix := c.codexRuleCheck(printed)
	if !strings.Contains(why, "does not trust the project "+repo) || !strings.Contains(fix, "trust "+repo) {
		t.Fatalf("untrusted project: why %q fix %q", why, fix)
	}
	writeTestFile(t, filepath.Join(home, "config.toml"), trustConfig(repo))
	if why, fix := c.codexRuleCheck(printed); why != "" {
		t.Fatalf("trusted project rule failed the check: %s (fix: %s)", why, fix)
	}
}

// CodexProjectRule names the installed binary's absolute path in the rule
// and the project's .codex/rules as its file; it is the rule the preflight
// would accept for that binary.
func TestCodexProjectRule(t *testing.T) {
	if _, _, ok, _ := (MainConfig{}).CodexProjectRule("/r"); ok {
		t.Fatal("a config without InstalledBinary has a project rule")
	}
	repo := canonicalPath(t.TempDir())
	cfg := MainConfig{Binary: "super", InstalledBinary: ".bin/super"}
	file, rule, ok, err := cfg.CodexProjectRule(repo)
	if err != nil || !ok {
		t.Fatalf("CodexProjectRule: ok %v err %v", ok, err)
	}
	if want := filepath.Join(repo, ".codex", "rules", "super-workflow.rules"); file != want {
		t.Errorf("file %q, want %q", file, want)
	}
	bin := filepath.Join(repo, ".bin", "super")
	if want := fmt.Sprintf(`prefix_rule(pattern = [%q,"wf","spawn"], decision = "allow"`, bin); !strings.Contains(rule, want) {
		t.Errorf("rule %q lacks %q", rule, want)
	}
}
