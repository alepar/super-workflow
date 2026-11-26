// Package testgit keeps the git a test binary runs away from the
// environment the binary was started in. A suite started from a git hook, or
// with GIT_DIR and GIT_WORK_TREE exported for a worktree, would otherwise send
// every fixture's git command to the repository those variables name.
package testgit

import (
	"os"
	"strings"
)

// Isolate removes every GIT_* variable from the process environment and
// points git's global and system configuration away from the user's, so a
// fixture's git reads only its own repository. A test package that runs git,
// directly or through the engine, calls it first in TestMain. Tests that need
// a GIT_* variable set it themselves with t.Setenv.
func Isolate() {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_") {
			os.Unsetenv(name)
		}
	}
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
}
