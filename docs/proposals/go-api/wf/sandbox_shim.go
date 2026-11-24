package wf

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// A sandboxed run (a fork: EffectContext.Sandboxed) puts a run-owned bd
// first on PATH for every subprocess it launches: adapter calls, shell
// effects and detached commands. The shim execs the real bd, resolved once
// by absolute path when the shim is written, with --sandbox prepended, so a
// fork's tracker writes never reach a configured remote. The environment
// the shim and the command see has bd's storage overrides (BEADS_*, BD_*,
// DOLT_*, the actor names aside) removed and BD_DISABLE_METRICS=1 set, and
// the shim refuses the global flags that move the database (--db,
// --database, --global).

// sandboxBinDir is the directory, under a run's own directory, that holds
// its bd shim.
const sandboxBinDir = "sandbox-bin"

// WriteSandboxShim writes runDir's bd shim and returns the directory that
// holds it, for PATH. It resolves the real bd now, skipping any shim
// directory; with none, the shim says so and exits 127, as a missing bd
// would, and the run's other commands run as before.
func WriteSandboxShim(runDir string) (string, error) {
	if runDir == "" {
		return "", errors.New("sandboxed run: no run directory for its bd shim")
	}
	dir := filepath.Join(runDir, sandboxBinDir)
	script := "#!/bin/sh\necho 'bd (wf sandbox): no bd on PATH' >&2\nexit 127\n"
	if real, err := realBD(); err == nil {
		script = sandboxShimScript(real)
	}
	path := filepath.Join(dir, "bd")
	if b, err := os.ReadFile(path); err == nil && string(b) == script {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(script), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return dir, nil
}

// realBD is the bd on this process's PATH, by absolute path, skipping shim
// directories (a fork launched from a fork's command).
func realBD() (string, error) {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == "" || filepath.Base(d) == sandboxBinDir {
			continue
		}
		p := filepath.Join(d, "bd")
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			if abs, err := filepath.Abs(p); err == nil {
				return abs, nil
			}
		}
	}
	return "", fmt.Errorf("sandboxed run: %w: bd", exec.ErrNotFound)
}

func sandboxShimScript(real string) string {
	return `#!/bin/sh
# wf sandbox shim: this run is a fork, so every bd call runs with --sandbox.
for a in "$@"; do
	case "$a" in
	--db|--db=*|--database|--database=*|--global|--global=*)
		echo "bd (wf sandbox): $a would move the database; a sandboxed run refuses it" >&2
		exit 2 ;;
	esac
done
for v in $(env | grep -E '^(BEADS|BD|DOLT)_[A-Za-z0-9_]*=' | cut -d= -f1); do
	case "$v" in
	BEADS_ACTOR|BD_ACTOR) ;;
	*) unset "$v" ;;
	esac
done
# Metrics off: bd's first-run notice would prefix the errors callers quote.
export BD_DISABLE_METRICS=1
exec ` + shellQuote(real) + ` --sandbox "$@"
`
}

// SandboxStorageOverride reports whether an environment name is one of bd's
// storage overrides, which a sandboxed run's subprocesses never see. The
// actor names and BD_DISABLE_METRICS (telemetry, which the adapter and the
// shim set themselves) say nothing of where the database lives.
func SandboxStorageOverride(name string) bool {
	if name == "BEADS_ACTOR" || name == "BD_ACTOR" || name == "BD_DISABLE_METRICS" {
		return false
	}
	return strings.HasPrefix(name, "BEADS_") || strings.HasPrefix(name, "BD_") || strings.HasPrefix(name, "DOLT_")
}

// SandboxEnv is this process's environment for a sandboxed run's
// subprocess: bin first on PATH, bd's storage overrides removed.
func SandboxEnv(bin string) []string {
	var env []string
	path := ""
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "PATH":
			path = v
		case SandboxStorageOverride(k):
		default:
			env = append(env, kv)
		}
	}
	if path != "" {
		path = bin + string(filepath.ListSeparator) + path
	} else {
		path = bin
	}
	return append(env, "PATH="+path)
}

// SandboxArgv0 is argv0 as a sandboxed run launches it: any bd, bare (exec
// resolves a bare name on this process's PATH, not the child's) or by path,
// is the shim in bin.
func SandboxArgv0(argv0, bin string) string {
	if filepath.Base(argv0) == "bd" {
		return filepath.Join(bin, "bd")
	}
	return argv0
}

// sandboxBin is the directory of this run's bd shim when the run is
// sandboxed, written on first use, or "" when it is not. A shim that cannot
// be written leaves it "", and a runtime then refuses the sandboxed call.
func (c *cycle) sandboxBin() string {
	if !c.st.forked || c.e.opts.RunDir == nil {
		return ""
	}
	dir, err := WriteSandboxShim(c.e.opts.RunDir(c.run))
	if err != nil {
		return ""
	}
	return dir
}

// ErrNoSandboxShim is a sandboxed call with no bd shim to put on PATH: it
// is refused rather than run where a bare bd would reach a real remote.
var ErrNoSandboxShim = errors.New("sandboxed run: no bd shim for its subprocesses")
