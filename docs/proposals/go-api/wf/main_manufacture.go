package wf

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// scriptFlags collects a repeated --script.
type scriptFlags []string

func (s *scriptFlags) String() string     { return strings.Join(*s, ",") }
func (s *scriptFlags) Set(v string) error { *s = append(*s, v); return nil }

var devManufacture = Subcommand{
	Name:    "manufacture",
	Args:    []string{"registration"},
	Flags:   []string{"--script file (repeatable)", "--workspace fixture", "--out dir", "--state-dir dir", "--assets dir", "--apply-trigger", "--do-sandbox JSON-argv", "--do-on-host"},
	Summary: "drive a fresh run of a registration with no model, from a script, to a target offer in a copy of a fixture world (MANUFACTURE.md)",
	handle:  (*cli).manufacture,
}

var devRestore = Subcommand{
	Name: "restore", subject: "run id", handle: (*cli).drive,
	Flags:   []string{"--state-dir dir", "--workspace dir", "--seq n", "--out dir"},
	Summary: "restore the world of <run> at a world_snapshot record into a new directory (workspace and state); the source is only read",
	define: func(fs *flag.FlagSet, f *flags) {
		fs.Int64Var(&f.restoreSeq, "seq", 0, "the world_snapshot record's seq")
		fs.StringVar(&f.forkOut, "out", "", "new directory for workspace and state")
	},
	act: func(c *cli, row Subcommand, stateDir string, run RunID, f *flags) int {
		if f.restoreSeq <= 0 || f.forkOut == "" {
			return c.failf(exitUsage, "dev restore: --seq and --out are required")
		}
		return c.withEngine(row, stateDir, func(x Executor) int {
			e := x.(*Engine)
			if why := e.restorePoint(run, f.restoreSeq); why != "" {
				return c.failf(exitUsage, "dev restore: %s", why)
			}
			fork, err := e.ForkSnapshot(run, f.restoreSeq, f.forkOut)
			if err != nil {
				return c.fromError("dev restore", err)
			}
			fmt.Fprintf(c.stdout, "%s\n%s\n", fork.Workspace, fork.StateDir)
			c.reportInstalled(fork)
			return exitOK
		})
	},
}

func init() { DevSubcommands = append(DevSubcommands, devManufacture, devRestore) }

// restorePoint is why seq is no world snapshot of run to restore ("" when
// it is), naming the nearest available checkpoint after it.
func (e *Engine) restorePoint(run RunID, seq int64) string {
	why := ""
	_ = e.withRecords(run, func(recs []JournalRecord) {
		if seq <= int64(len(recs)) && recs[seq-1].Kind == JournalWorldSnapshot {
			return
		}
		why = fmt.Sprintf("record %d of run %s is not a world_snapshot", seq, run)
		for _, r := range recs[min(int(seq), len(recs)):] {
			var w WorldSnapshotData
			if r.Kind == JournalWorldSnapshot && json.Unmarshal(r.Data, &w) == nil && w.Unavailable == "" {
				why += fmt.Sprintf("; the nearest checkpoint after it is %d", r.Seq)
				return
			}
		}
		why += "; there is no available checkpoint after it"
	})
	return why
}

func (c *cli) manufacture(row Subcommand, args []string) int {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("manufacture", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	var scripts scriptFlags
	fs.Var(&scripts, "script", "script file; repeat for predecessor scripts first")
	workspace := fs.String("workspace", "", "fixture workspace, copied")
	out := fs.String("out", "", "new directory for the world and the report")
	stateDir := fs.String("state-dir", "", "state dir inside the copied workspace (default: the registration's)")
	assets := fs.String("assets", "", "asset root")
	apply := fs.Bool("apply-trigger", false, "apply the last script's trigger and check its edge")
	sandbox := fs.String("do-sandbox", "", "JSON argv every world action runs under (placeholders {workspace}, {dir}); required for a script with world actions")
	onHost := fs.Bool("do-on-host", false, "run world actions on the host, unconfined (recorded in the report)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if name == "" || len(scripts) == 0 || *workspace == "" || *out == "" {
		return c.failf(exitUsage, "manufacture: a registration, --script, --workspace and --out are required")
	}
	reg := c.registration(name)
	if reg == nil {
		return c.failf(exitUsage, "manufacture: unknown definition %s", name)
	}
	var parsed []*ManufactureScript
	for i, p := range scripts {
		raw, err := os.ReadFile(p)
		if err != nil {
			return c.failf(exitUsage, "manufacture: %v", err)
		}
		s, err := ParseManufactureScript(raw)
		if err != nil {
			return c.failf(exitUsage, "manufacture: %s: %v", p, err)
		}
		last := i == len(scripts)-1
		switch {
		case s.Registration != name:
			return c.failf(exitUsage, "manufacture: %s is a script for %s, not %s", p, s.Registration, name)
		case s.Hash != reg.Def.Hash():
			return c.failf(exitUsage, "manufacture: %s was written for definition hash %s; this binary's %s is %s", p, s.Hash, name, reg.Def.Hash())
		case !last && s.Until != "ended":
			return c.failf(exitUsage, "manufacture: %s: every script but the last is a predecessor script (until \"ended\")", p)
		}
		if s.Options != nil {
			if _, err := decodeOptions(reg.Def, nil, s.Options); err != nil {
				return c.failf(exitUsage, "manufacture: %s: options: %v", p, err)
			}
		}
		parsed = append(parsed, s)
	}
	var wrapper []string
	switch {
	case *sandbox != "" && *onHost:
		return c.failf(exitUsage, "manufacture: --do-sandbox and --do-on-host exclude each other")
	case *sandbox != "":
		var err error
		if wrapper, err = fanoutArgv(*sandbox); err != nil {
			return c.failf(exitUsage, "manufacture: --do-sandbox: %v", err)
		}
	case !*onHost:
		for _, s := range parsed {
			if scriptHasDo(s) {
				return c.failf(exitUsage, "manufacture: the script for %s has world actions (do): pass --do-sandbox with the boundary to run them in, or --do-on-host to run them unconfined", s.Edge+s.Until)
			}
		}
	}
	src, err := realPath(*workspace)
	if err != nil {
		return c.failf(exitUsage, "manufacture: %v", err)
	}
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return c.failf(exitUsage, "manufacture: --workspace %s: no such directory", *workspace)
	}
	dst, err := realPath(*out)
	if err != nil {
		return c.failf(exitUsage, "manufacture: %v", err)
	}
	if _, err := checkForkDestination(dst, src); err != nil {
		return c.failf(exitUsage, "manufacture: --out: %v", err)
	}
	ws := filepath.Join(dst, "workspace")
	state := *stateDir
	if state == "" {
		state = c.cfg.stateDir()
	}
	if filepath.IsAbs(state) {
		return c.failf(exitUsage, "manufacture: --state-dir %s: must be relative to the copied workspace", state)
	}
	state = filepath.Join(ws, state)
	if _, inside := snapshotRelative(ws, state); !inside {
		return c.failf(exitUsage, "manufacture: --state-dir %s: must resolve inside the copied workspace", *stateDir)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return c.failf(exitInfra, "manufacture: %v", err)
	}
	if res, err := exec.Command("cp", "-a", "--no-dereference", src, ws).CombinedOutput(); err != nil {
		return c.failf(exitInfra, "manufacture: copying the fixture: %v: %s", err, res)
	}
	// The copy must hold no way back out: a link leaving it, a state root
	// resolving outside it, or git metadata naming the fixture or another
	// repository. Links into the fixture are moved onto the copy.
	dropped, err := confineCopy(src, ws, state)
	if err != nil {
		_ = os.RemoveAll(dst)
		return c.failf(exitUsage, "manufacture: the fixture cannot be confined to its copy: %v", err)
	}
	// Adapters' git in this drive reads no system or global config: only
	// the copy's, cut to data and with hooks off.
	for k, v := range map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull} {
		old, had := os.LookupEnv(k)
		os.Setenv(k, v)
		defer func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		}()
	}
	doRun := func(argv []string, dir string) (string, error) {
		return runWorldAction(wrapper, ws, argv, dir)
	}
	if wrapper == nil && !*onHost {
		doRun = nil
	}
	// The scripted coordinator's clock: real time, moved on past waits.
	var mu sync.Mutex
	var offset time.Duration
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return time.Now().Add(offset) }
	advance := func(d time.Duration) { mu.Lock(); offset += d; mu.Unlock() }

	var reports []ManufactureReport
	for i, s := range parsed {
		last := i == len(parsed)-1
		run, code := c.manufactureStart(name, ws, state, *assets, s)
		if code != exitOK {
			return code
		}
		cc := *c
		cc.workspace, cc.stderr, cc.assets = ws, io.Discard, *assets
		x, err := cc.engine(state, EngineOptions{Workspace: ws, Snapshots: true, Clock: clock})
		if err != nil {
			return c.failf(exitInfra, "manufacture: %v", err)
		}
		rep, err := x.(*Engine).Manufacture(run, s, ManufactureOptions{ApplyTrigger: last && *apply, Advance: advance, Do: doRun})
		if err != nil {
			return c.fromError("manufacture", err)
		}
		rep.DoOnHost = *onHost && scriptHasDo(s)
		rep.GitConfigDropped = dropped
		reports = append(reports, *rep)
		if !rep.Reached {
			break
		}
	}
	final := reports[len(reports)-1]
	if len(parsed) > 1 {
		final.Predecessors = reports[:len(reports)-1]
	}
	data, err := json.MarshalIndent(final, "", "  ")
	if err != nil {
		return c.failf(exitInfra, "manufacture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "report.json"), append(data, '\n'), 0o600); err != nil {
		return c.failf(exitInfra, "manufacture: %v", err)
	}
	c.manufactureSummary(final, len(reports) < len(parsed), state)
	if len(reports) < len(parsed) || !final.Reached {
		return exitCheckFailed
	}
	return exitOK
}

// manufactureStart starts the script's run in the copied workspace through
// the same run chain as wf run, marked manufactured, with exactly the
// script's params and options and no harness taken from the caller's
// environment.
func (c *cli) manufactureStart(name, ws, state, assets string, s *ManufactureScript) (RunID, int) {
	cc := *c
	var stdout, stderr bytes.Buffer
	cc.stdout, cc.stderr = &stdout, &stderr
	cc.env = func(string) string { return "" }
	cc.workspace, cc.assets = ws, assets
	cc.manufactured = &ManufacturedData{Edge: s.Edge, Until: s.Until, Hash: s.Hash, Script: ScriptDigest(s)}
	f := flags{workspace: ws, runID: s.RunID, snapshots: true, optionsJSON: s.Options}
	for k, v := range s.Params {
		f.params = append(f.params, k+"="+v)
	}
	if err := applyStateEnv(state); err != nil {
		return "", c.failf(exitInfra, "manufacture: %v", err)
	}
	before := map[string]bool{}
	if entries, err := os.ReadDir(filepath.Join(state, "runs")); err == nil {
		for _, e := range entries {
			before[e.Name()] = true
		}
	}
	if code := cc.start(name, state, &f); code != exitOK {
		fmt.Fprint(c.stderr, stderr.String())
		return "", code
	}
	id, _, _ := strings.Cut(stdout.String(), "\n")
	run := RunID(strings.TrimSpace(id))
	if validRunID(run) != nil {
		return "", c.failf(exitInfra, "manufacture: run start printed no run id: %q", stdout.String())
	}
	if before[string(run)] {
		// The run chain resumed or returned a run that was there: only a
		// fresh run or a successor is driven from its start.
		return "", c.failf(exitUsage, "manufacture: the start landed on the existing run %s; a successor comes from the definition's run key (params), not a run_id, and a live predecessor must end first", run)
	}
	return run, exitOK
}

func (c *cli) manufactureSummary(r ManufactureReport, predecessorFailed bool, state string) {
	for _, p := range r.Predecessors {
		fmt.Fprintf(c.stdout, "predecessor %s: ended %s (%s)\n", p.Run, p.Ended.Terminal, p.Ended.Outcome)
	}
	if predecessorFailed {
		fmt.Fprintf(c.stdout, "predecessor %s did not end; the target script did not run\n", r.Run)
	}
	switch {
	case r.Until == "ended" && r.Reached:
		fmt.Fprintf(c.stdout, "run %s ended at %s\n", r.Run, r.Ended.Terminal)
	case r.Reached && r.Target != nil:
		fmt.Fprintf(c.stdout, "run %s reached %s: offer %d (checkpoint %d), unit %s, state dir %s\n", r.Run, r.Edge, r.Target.Seq, r.Target.SnapshotSeq, r.Target.Unit, state)
	default:
		fmt.Fprintf(c.stdout, "run %s did not reach %s%s\n", r.Run, r.Edge, r.Until)
	}
	if r.TriggerApplied {
		fmt.Fprintf(c.stdout, "  trigger took %s (script: %s)\n", r.TriggerEdgeTaken, r.Edge)
	}
	if d := r.Divergence; d != nil {
		fmt.Fprintf(c.stdout, "  divergence at %s (%s): expected %s, took %s", d.State, d.Instance, d.Expected, d.Taken)
		if d.NeedsStub {
			fmt.Fprint(c.stdout, " [needs-stub]")
		}
		if d.NoDeclaredReads {
			fmt.Fprint(c.stdout, " [no declared reads: earlier outputs, options, params or effect outcomes route this state; check the script's earlier steps]")
		}
		fmt.Fprintln(c.stdout)
	}
	if s := r.RefusedStep; s != nil {
		fmt.Fprintf(c.stdout, "  refused at %s (%s): %s\n", s.State, s.Unit, strings.Join(s.Reasons, "; "))
	}
	if s := r.Stuck; s != nil {
		fmt.Fprintf(c.stdout, "  stuck at %s (%s): %s\n", s.State, s.Kind, s.Why)
	}
	if r.Ended != nil && r.Until == "" {
		fmt.Fprintf(c.stdout, "  the run ended at %s (%s) before the target\n", r.Ended.Terminal, r.Ended.Cause)
	}
}

func scriptHasDo(s *ManufactureScript) bool {
	for _, st := range s.Steps {
		if st.Step != nil && len(st.Step.Do) > 0 {
			return true
		}
	}
	return s.Trigger != nil && len(s.Trigger.Do) > 0
}

// runWorldAction runs one world action in dir, which must be inside ws,
// under wrapper (placeholders {workspace} and {dir}) or, with none, on the
// host; it returns the action's stdout.
func runWorldAction(wrapper []string, ws string, argv []string, dir string) (string, error) {
	dir = filepath.Clean(dir)
	if _, inside := snapshotRelative(ws, dir); !inside && dir != ws {
		return "", fmt.Errorf("its directory %s is outside the copied workspace", dir)
	}
	full := argv
	if len(wrapper) > 0 {
		replace := strings.NewReplacer("{workspace}", ws, "{dir}", dir)
		full = nil
		for _, a := range wrapper {
			full = append(full, replace.Replace(a))
		}
		full = append(full, argv...)
	}
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// confineCopy checks the copied fixture ws holds no way out of it, and moves
// links that name the fixture src onto the copy: every symbolic link must
// resolve inside ws; the state root must resolve inside ws; the repository
// must be a primary one with no alternates; each linked worktree's gitdir
// and each worktree record must be inside the fixture (rewritten to the
// copy); and each remote that is a local path must be inside the fixture
// (rewritten to the copy). Remotes over the network are not confined.
func confineCopy(src, ws, state string) (dropped []string, err error) {
	inside := func(p string) bool {
		_, ok := snapshotRelative(ws, filepath.Clean(p))
		return ok || filepath.Clean(p) == ws
	}
	onto := func(p string) (string, bool) {
		if rel, ok := snapshotRelative(src, filepath.Clean(p)); ok {
			return filepath.Join(ws, rel), true
		}
		if filepath.Clean(p) == src {
			return ws, true
		}
		return "", false
	}
	err = filepath.WalkDir(ws, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(p), target)
			}
			if to, ok := onto(target); ok {
				if err := os.Remove(p); err != nil {
					return err
				}
				return os.Symlink(to, p)
			}
			if !inside(target) {
				return fmt.Errorf("link %s leads to %s, outside the copy", p, target)
			}
			return nil
		}
		if d.Name() == ".git" && d.Type().IsRegular() || filepath.Base(filepath.Dir(filepath.Dir(p))) == "worktrees" && d.Name() == "gitdir" {
			return relinkGitFile(p, onto, inside)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if dropped, err = confineRepository(src, ws, onto); err != nil {
		return nil, err
	}
	// The state root, as it resolves once its existing part is followed.
	probe := state
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		probe = filepath.Dir(probe)
	}
	resolved, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return nil, err
	}
	if root, err := filepath.EvalSymlinks(ws); err != nil || !(resolved == root || strings.HasPrefix(resolved, root+string(filepath.Separator))) {
		return nil, fmt.Errorf("the state root %s resolves to %s, outside the copy", state, resolved)
	}
	return dropped, nil
}

// relinkGitFile moves a gitdir pointer (a linked worktree's .git file, or a
// worktree record's gitdir) that names the fixture onto the copy; one that
// names anything else is refused.
func relinkGitFile(p string, onto func(string) (string, bool), inside func(string) bool) error {
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(string(raw))
	prefix := ""
	if rest, ok := strings.CutPrefix(text, "gitdir: "); ok {
		prefix, text = "gitdir: ", rest
	}
	target := text
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(p), target)
	}
	if to, ok := onto(target); ok {
		return os.WriteFile(p, []byte(prefix+to+"\n"), 0o644)
	}
	if !inside(target) {
		return fmt.Errorf("%s names %s, outside the fixture", p, target)
	}
	return nil
}

// confineRepository checks and isolates the copy's repository, when it has
// one, and every local repository its remotes reach, recursively: each must
// be a repository inside the copy (a remote naming the fixture is moved onto
// the copy first) that sanitizeRepository can isolate. dropped lists the
// settings left out, a secondary repository's prefixed with its path in the
// copy.
func confineRepository(src, ws string, onto func(string) (string, bool)) (dropped []string, err error) {
	info, err := os.Lstat(filepath.Join(ws, ".git"))
	switch {
	case os.IsNotExist(err):
		return nil, nil
	case err != nil:
		return nil, err
	case !info.IsDir():
		return nil, fmt.Errorf("%s is a linked worktree, not a primary git repository", src)
	}
	seen := map[string]bool{}
	var visit func(gitDir, base, label string) error
	visit = func(gitDir, base, label string) error {
		if seen[gitDir] {
			return nil
		}
		seen[gitDir] = true
		remotes, out, err := sanitizeRepository(gitDir)
		if err != nil {
			return fmt.Errorf("%s: %v", label, err)
		}
		for _, k := range out {
			if label != "" {
				k = label + ":" + k
			}
			dropped = append(dropped, k)
		}
		for _, r := range remotes {
			local, ok := localRemote(r.value)
			if !ok {
				continue // over the network: not confined
			}
			if !filepath.IsAbs(local) {
				local = filepath.Join(base, local)
			}
			to, ok := onto(local)
			if !ok {
				to = filepath.Clean(local)
			}
			if _, inside := snapshotRelative(ws, to); !inside {
				return fmt.Errorf("remote %s is %s, a local repository outside the fixture", r.key, r.value)
			}
			rewrite := exec.Command("git", "config", "--file", filepath.Join(gitDir, "config"), r.key, to)
			rewrite.Env = isolatedGitEnv()
			if res, err := rewrite.CombinedOutput(); err != nil {
				return fmt.Errorf("rewriting %s: %v: %s", r.key, err, res)
			}
			next, nextBase, ok := repositoryAt(to)
			if !ok {
				return fmt.Errorf("remote %s is %s, which is no git repository the copy can isolate", r.key, r.value)
			}
			rel, _ := filepath.Rel(ws, next)
			if err := visit(next, nextBase, rel); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(filepath.Join(ws, ".git"), ws, ""); err != nil {
		return nil, err
	}
	return dropped, nil
}

// repositoryAt is the git directory of the repository at p, a work tree
// with a .git directory or a bare repository, and the directory its
// relative remotes resolve from.
func repositoryAt(p string) (gitDir, base string, ok bool) {
	if info, err := os.Lstat(filepath.Join(p, ".git")); err == nil && info.IsDir() {
		return filepath.Join(p, ".git"), p, true
	}
	for _, need := range []string{"HEAD", "config", "objects"} {
		if _, err := os.Lstat(filepath.Join(p, need)); err != nil {
			return "", "", false
		}
	}
	return p, p, true
}

type gitSetting struct{ key, value string }

// sanitizeRepository isolates the repository at gitDir: no common directory
// elsewhere (commondir), no alternates and no per-worktree config; its config is cut to settings that name data
// (gitConfigSafe), hooks are off (core.hooksPath=/dev/null), and an include
// refuses it. It returns its remote URL settings, keys as written (a
// remote's name keeps its case), and the settings it dropped.
func sanitizeRepository(gitDir string) (remotes []gitSetting, dropped []string, err error) {
	// A commondir file moves the repository's shared config and hooks to
	// another directory, which its own config would not govern.
	if _, err := os.Lstat(filepath.Join(gitDir, "commondir")); !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("the repository's common directory is elsewhere (commondir): its effective config and hooks cannot be isolated")
	}
	if _, err := os.Stat(filepath.Join(gitDir, "objects", "info", "alternates")); err == nil {
		return nil, nil, fmt.Errorf("the repository borrows objects from another (objects/info/alternates)")
	}
	if found, _ := filepath.Glob(filepath.Join(gitDir, "worktrees", "*", "config.worktree")); len(found) > 0 {
		return nil, nil, fmt.Errorf("a worktree has its own config (%s)", found[0])
	}
	if _, err := os.Stat(filepath.Join(gitDir, "config.worktree")); err == nil {
		return nil, nil, fmt.Errorf("the repository has per-worktree config (config.worktree)")
	}
	file := filepath.Join(gitDir, "config")
	config := func(args ...string) *exec.Cmd {
		cmd := exec.Command("git", append([]string{"config", "--file", file}, args...)...)
		cmd.Env = isolatedGitEnv()
		return cmd
	}
	out, err := config("--list", "-z").Output()
	if err != nil {
		return nil, nil, fmt.Errorf("reading its git config: %v", err)
	}
	var all []gitSetting
	for _, kv := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if kv != "" {
			k, v, _ := strings.Cut(kv, "\n")
			all = append(all, gitSetting{k, v})
		}
	}
	done := map[string]bool{}
	for _, e := range all {
		lower := strings.ToLower(e.key)
		switch {
		case strings.HasPrefix(lower, "include.") || strings.HasPrefix(lower, "includeif."):
			return nil, nil, fmt.Errorf("the git config includes other files (%s)", e.key)
		case lower == "extensions.worktreeconfig":
			return nil, nil, fmt.Errorf("the repository enables per-worktree config (extensions.worktreeConfig)")
		case gitConfigSafe(lower, e.value):
			if strings.HasPrefix(lower, "remote.") && (strings.HasSuffix(lower, ".url") || strings.HasSuffix(lower, ".pushurl")) {
				remotes = append(remotes, e)
			}
			continue
		case done[e.key]:
			continue
		}
		done[e.key] = true
		if res, err := config("--unset-all", e.key).CombinedOutput(); err != nil {
			return nil, nil, fmt.Errorf("dropping %s: %v: %s", e.key, err, res)
		}
		dropped = append(dropped, lower)
	}
	if res, err := config("core.hooksPath", "/dev/null").CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("turning hooks off: %v: %s", err, res)
	}
	return remotes, dropped, nil
}

// localRemote is the path of a remote URL that names a local repository:
// a file:// URL or a plain path, absolute or relative; not one over the
// network (a scheme, or scp-like host:path).
func localRemote(url string) (string, bool) {
	if rest, ok := strings.CutPrefix(url, "file://"); ok {
		return rest, true
	}
	if strings.Contains(url, "://") {
		return "", false
	}
	if i := strings.Index(url, ":"); i > 0 && !strings.Contains(url[:i], "/") {
		return "", false // host:path
	}
	return url, true
}

// gitConfigSafe reports whether a local git setting of a fixture can stay in
// the copy: it names data, never a program, a hook, a filter or another
// file. Everything else is dropped.
func gitConfigSafe(key, value string) bool {
	switch key {
	case "core.repositoryformatversion", "core.filemode", "core.bare", "core.logallrefupdates", "core.ignorecase",
		"core.precomposeunicode", "core.symlinks", "core.autocrlf", "core.eol", "core.safecrlf", "user.name", "user.email",
		"init.defaultbranch", "gc.auto", "pull.rebase", "push.default":
		return true
	case "commit.gpgsign", "tag.gpgsign":
		return value == "false"
	}
	for _, prefix := range []string{"extensions.", "beads.", "advice."} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	section, rest, _ := strings.Cut(key, ".")
	if i := strings.LastIndex(rest, "."); i >= 0 {
		name := rest[i+1:]
		switch section {
		case "remote":
			return name == "url" || name == "pushurl" || name == "fetch" || name == "push" || name == "tagopt" || name == "prune" || name == "mirror"
		case "branch":
			return name == "remote" || name == "merge" || name == "rebase" || name == "pushremote"
		}
	}
	return false
}
