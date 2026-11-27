package wf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// defineHeadlessFlags are run's flags for its cross-harness children,
// journaled in run_started as its HeadlessConfig.
func defineHeadlessFlags(fs *flag.FlagSet, f *flags) {
	fs.Func("family", "coord=harness: the harness FamilyOther means for coordinator harness coord (other=harness: for every coordinator); repeatable", func(v string) error {
		k, h, ok := strings.Cut(v, "=")
		k, h = normalizeHarness(k), normalizeHarness(h)
		if !ok || k == "" || h == "" {
			return fmt.Errorf("want coord=harness, got %q", v)
		}
		if harness.HeadlessFor(h) == nil {
			return fmt.Errorf("harness %s has no headless launch (have: %s)", h, strings.Join(headlessNames(), ", "))
		}
		if f.headless.Other == nil {
			f.headless.Other = map[string]string{}
		}
		f.headless.Other[k] = h
		return nil
	})
	fs.Func("family-home", "harness=dir: the login home a child harness runs with (CODEX_HOME, CLAUDE_CONFIG_DIR); unset, it inherits yours; repeatable", func(v string) error {
		h, dir, ok := strings.Cut(v, "=")
		h = normalizeHarness(h)
		if !ok || harness.HeadlessFor(h) == nil || !filepath.IsAbs(dir) {
			return fmt.Errorf("want harness=absolute-dir for a harness with a headless launch, got %q", v)
		}
		if f.headless.Homes == nil {
			f.headless.Homes = map[string]string{}
		}
		f.headless.Homes[h] = dir
		return nil
	})
	fs.Func("child-env-drop", "NAME or PREFIX*: an environment variable scrubbed from every headless child, beyond the harnesses' own session identities; repeatable", func(v string) error {
		if v == "" || strings.ContainsAny(v, "= ") {
			return fmt.Errorf("want a variable name or a prefix ending in *, got %q", v)
		}
		f.headless.EnvDrop = append(f.headless.EnvDrop, v)
		return nil
	})
	fs.Func("headless-cmd", "harness=JSON-argv: the launch of that harness's headless children in place of its default; placeholders "+strings.Join(headlessPlaceholders, " "), func(v string) error {
		h, argv, err := ParseHeadlessCmd(v)
		if err != nil {
			return err
		}
		if f.headless.Cmd == nil {
			f.headless.Cmd = map[string][]string{}
		}
		f.headless.Cmd[h] = argv
		return nil
	})
}

// orNil is cfg, or nil when it sets nothing.
func (cfg HeadlessConfig) orNil() *HeadlessConfig {
	if len(cfg.Other) == 0 && len(cfg.Homes) == 0 && len(cfg.EnvDrop) == 0 && len(cfg.Cmd) == 0 {
		return nil
	}
	return &cfg
}

// headlessCheckTimeout bounds each program the preflight runs.
const headlessCheckTimeout = 20 * time.Second

// runCheck runs argv with env and returns its stdout and exit status (-1
// when it did not run to an exit).
func runCheck(argv, env []string) ([]byte, int) {
	ctx, cancel := context.WithTimeout(context.Background(), headlessCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.Bytes(), 0
	case errors.As(err, &ee):
		return out.Bytes(), ee.ExitCode()
	}
	return out.Bytes(), -1
}

// headlessCheck is the CLI's preflight of child harness child for a
// coordinator of harness coord (EngineOptions.HeadlessCheck), through each
// CLI's public surface only: its binary on PATH, its --version, its login
// status, and, for a Codex coordinator, `codex execpolicy check` that the
// spawn verb's command, spawn as next prints it, runs outside Codex's
// sandbox.
func (c *cli) headlessCheck(child, coord string, cfg *HeadlessConfig, spawn string) HarnessCheckedData {
	d := HarnessCheckedData{Harness: child, Coordinator: coord}
	hl := harness.HeadlessFor(child)
	if hl == nil {
		d.Why = "it has no headless launch"
		return d
	}
	argv := hl.HeadlessArgv(harness.HeadlessSpawn{})
	custom := cfg != nil && len(cfg.Cmd[child]) > 0
	if custom {
		argv = cfg.Cmd[child]
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		d.Why, d.Fix = fmt.Sprintf("%s is not on PATH", argv[0]), fmt.Sprintf("install the %s CLI, or pass run --headless-cmd %s=<JSON argv>", child, child)
		return d
	}
	env := childEnv(os.Environ(), cfg, child, hl) // what the child itself gets
	if custom {
		// A launch of the user's own (a wrapper, a container) takes no
		// --version or login check of the harness's: its program exists.
		if why, fix := c.codexRuleIfCoordinator(coord, spawn); why != "" {
			d.Why, d.Fix = why, fix
			return d
		}
		d.Available, d.Version = true, "custom launch "+argv[0]
		return d
	}
	if out, code := runCheck([]string{bin, "--version"}, env); code == 0 {
		d.Version, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
	}
	login, ok := hl.HeadlessLogin()
	login[0] = bin
	if out, code := runCheck(login, env); !ok(out, code) {
		d.Why, d.Fix = fmt.Sprintf("%s is not logged in (%s exited %d)", child, strings.Join(login, " "), code), fmt.Sprintf("log in to %s yourself (its own login command), or pass run --family-home %s=<its login home>", child, child)
		return d
	}
	if why, fix := c.codexRuleIfCoordinator(coord, spawn); why != "" {
		d.Why, d.Fix = why, fix
		return d
	}
	d.Available = true
	return d
}

// codexRuleIfCoordinator is codexRuleCheck for a Codex coordinator, whose
// sandbox would confine the launch; "" for any other.
func (c *cli) codexRuleIfCoordinator(coord, spawn string) (why, fix string) {
	if normalizeHarness(coord) != "codex" {
		return "", ""
	}
	return c.codexRuleCheck(spawn)
}

// spawnPrefix is the spawn verb's command as next prints it, up to and
// including the verb: the argv prefix a harness rule allows.
func (c *cli) spawnPrefix() []string {
	bin, err := os.Executable()
	if err != nil {
		return nil
	}
	return spawnPrefixFor(c.cfg, bin)
}

// spawnPrefixFor is spawnPrefix for the binary at bin wired by cfg.
func spawnPrefixFor(cfg MainConfig, bin string) []string {
	line := multiVerbLine(bin, "S", "R", "spawn")
	if cfg.named() {
		line = wfLine(bin, "S", "R", "spawn")
	}
	words := shellWords(line)
	if i := slices.Index(words, "spawn"); i >= 0 {
		return words[:i+1]
	}
	return nil
}

// setupCodexRulesRow prints, or with --write writes, the Codex rule that
// lets the spawn verb run outside Codex's sandbox.
func setupCodexRulesRow() Subcommand {
	return Subcommand{Name: "setup-codex-rules", Summary: "print, or with --write write, the Codex execpolicy rule that lets the spawn verb start headless children outside Codex's sandbox (a person's one confirmation)",
		Flags: []string{"--write"}, handle: func(c *cli, row Subcommand, args []string) int {
			fs := flag.NewFlagSet(row.Name, flag.ContinueOnError)
			fs.SetOutput(c.stderr)
			write := fs.Bool("write", false, "write the rule (else print it)")
			if err := fs.Parse(args); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					return exitOK
				}
				return exitUsage
			}
			if fs.NArg() != 0 {
				return c.failf(exitUsage, "%s: takes no arguments", row.Name)
			}
			return c.setupCodexRules(*write)
		}}
}

// codexRulesName is the rules file the engine writes, in the Codex home's
// rules directory and in a project's .codex/rules.
const codexRulesName = "super-workflow.rules"

// codexHome is the Codex home: CODEX_HOME, else ~/.codex.
func codexHome(env harness.Env) string {
	if home := env("CODEX_HOME"); home != "" {
		return home
	}
	return filepath.Join(env("HOME"), ".codex")
}

// codexRulesFile is where setup-codex-rules writes the rule: the Codex
// home's rules directory, as herdr-threads keeps its own.
func codexRulesFile(env harness.Env) string {
	return filepath.Join(codexHome(env), "rules", codexRulesName)
}

// codexRule is the execpolicy rule that lets Codex run prefix outside its
// sandbox without asking.
func codexRule(prefix []string) string {
	b, _ := json.Marshal(prefix)
	return "# super-workflow: the spawn verb starts a headless child of another harness outside Codex's sandbox\n" +
		"prefix_rule(pattern = " + string(b) + ", decision = \"allow\", justification = \"workflow engine child dispatch\")\n"
}

// codexRuleCheck asks Codex whether spawn, the spawn verb's command as next
// prints it, is allowed outside its sandbox by the rules Codex loads: those
// in its home and the project rules of a session in this process's working
// directory (codexProjectLayers). Codex matches its rules against the argv
// of each command a shell line runs, so the check takes the printed line's
// words as they are, and a line that ran another command first would fail
// it. why and fix are empty when it is allowed.
func (c *cli) codexRuleCheck(spawn string) (why, fix string) {
	prefix := c.spawnPrefix()
	if len(prefix) == 0 {
		return "", ""
	}
	env := c.env
	if env == nil {
		env = os.Getenv
	}
	dirs := []string{filepath.Dir(codexRulesFile(env))}
	var skipped []string // project rules Codex skips: it does not trust root
	cwd, _ := os.Getwd()
	layers, root, trusted := codexProjectLayers(env, cwd)
	if trusted {
		dirs = append(dirs, layers...)
	} else {
		skipped = rulesFiles(layers)
	}
	files := rulesFiles(dirs)
	fix = fmt.Sprintf("run %s and confirm it: it writes %s with the one rule %s",
		strings.Join([]string{prefix[0], "setup-codex-rules"}, " "), codexRulesFile(env), strings.TrimSpace(strings.SplitN(codexRule(prefix), "\n", 2)[1]))
	untrusted := func() (string, string) {
		return fmt.Sprintf("Codex does not trust the project %s, so it skips its rules (%s): no rule it loads lets the spawn verb run outside its sandbox", root, strings.Join(skipped, ", ")),
			fmt.Sprintf("trust %s in Codex (it asks when a session starts there), or %s", root, fix)
	}
	if len(files) == 0 {
		if len(skipped) > 0 {
			return untrusted()
		}
		return "no Codex rule lets the spawn verb run outside Codex's sandbox (no rules files)", fix
	}
	argv := []string{"codex", "execpolicy", "check"}
	for _, f := range files {
		argv = append(argv, "--rules", f)
	}
	argv = append(argv, shellWords(spawn)...)
	out, code := runCheck(argv, os.Environ())
	var res struct {
		Decision string `json:"decision"`
	}
	if code != 0 || json.Unmarshal(out, &res) != nil {
		return fmt.Sprintf("codex execpolicy check did not answer (exit %d)", code), fix
	}
	if res.Decision != "allow" {
		if len(skipped) > 0 {
			return untrusted()
		}
		return "no Codex rule lets the spawn verb run outside Codex's sandbox: a sandboxed child has no network and hangs", fix
	}
	return "", ""
}

// rulesFiles are the *.rules files in dirs, in order.
func rulesFiles(dirs []string) []string {
	var files []string
	for _, d := range dirs {
		m, _ := filepath.Glob(filepath.Join(d, "*.rules"))
		files = append(files, m...)
	}
	return files
}

// setupCodexRules prints, or with write writes, the Codex rule for the
// spawn verb into the Codex home's rules directory.
func (c *cli) setupCodexRules(write bool) int {
	prefix := c.spawnPrefix()
	if len(prefix) == 0 {
		return c.failf(exitInfra, "setup-codex-rules: cannot tell the spawn verb's command")
	}
	env := c.env
	if env == nil {
		env = os.Getenv
	}
	file, rule := codexRulesFile(env), codexRule(prefix)
	if !write {
		fmt.Fprintf(c.stdout, "%s would hold:\n%s\nrun again with --write to write it (a person's confirmation: it lets Codex run this command outside its sandbox).\n", file, rule)
		return exitOK
	}
	if b, err := os.ReadFile(file); err == nil && slices.Contains(strings.Split(string(b), "\n"), strings.TrimSpace(strings.SplitN(rule, "\n", 2)[1])) {
		fmt.Fprintf(c.stdout, "%s already holds the rule.\n", file)
		return exitOK
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return c.failf(exitInfra, "setup-codex-rules: %v", err)
	}
	fh, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return c.failf(exitInfra, "setup-codex-rules: %v", err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(rule); err != nil {
		return c.failf(exitInfra, "setup-codex-rules: %v", err)
	}
	fmt.Fprintf(c.stdout, "wrote the rule to %s\n", file)
	return exitOK
}
