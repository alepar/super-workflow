package feature_test

import (
	"os"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/internal/testgit"
)

// TestMain runs every test with git isolated from the environment the suite
// was started in (sw-j721).
func TestMain(m *testing.M) {
	testgit.Isolate()
	os.Exit(m.Run())
}
