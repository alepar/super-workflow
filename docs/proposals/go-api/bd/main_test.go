package bd_test

import (
	"os"
	"testing"
)

// TestMain turns bd's metrics off for every bd the tests start themselves,
// as the adapter does for its own (runtime.go): the metrics flush writes
// under HOME, a test TempDir, and can still be writing when the test returns
// .
func TestMain(m *testing.M) {
	os.Setenv("BD_DISABLE_METRICS", "1")
	code := m.Run()
	if templateDir != "" {
		os.RemoveAll(templateDir)
	}
	os.Exit(code)
}
