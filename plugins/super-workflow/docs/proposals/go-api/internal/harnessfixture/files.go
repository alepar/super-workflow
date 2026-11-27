// Package harnessfixture locates recorded harness fixtures for tests and
// development checkers. Workflow execution does not depend on these files.
package harnessfixture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// Files opens the harness testdata tree from the engine source. A checker
// built with -trimpath or moved off the build host can be given a copy of that
// tree through WF_HARNESS_FIXTURES_DIR.
func Files() fs.FS {
	return os.DirFS(fixtureRoot())
}

const rootEnv = "WF_HARNESS_FIXTURES_DIR"

func fixtureRoot() string {
	root := os.Getenv(rootEnv)
	if root == "" {
		_, source, _, _ := runtime.Caller(0)
		root = filepath.Join(filepath.Dir(source), "..", "..", "harness", "testdata")
	}
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return root
}

// CheckRoot verifies the fixture directory before the checker starts its cases.
func CheckRoot() error {
	root := fixtureRoot()
	info, err := os.Stat(root)
	if err == nil && !info.IsDir() {
		err = errors.New("not a directory")
	}
	if err != nil {
		return fmt.Errorf("cannot use harness fixture root %q: %w. Set %s to the engine harness/testdata directory", root, err, rootEnv)
	}
	return nil
}

// ReadFile reads a fixture from the testdata tree.
func ReadFile(name string) ([]byte, error) {
	return fs.ReadFile(Files(), name)
}
