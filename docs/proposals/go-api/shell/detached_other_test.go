//go:build !linux

package shell

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestStartUnavailableOffLinux(t *testing.T) {
	dir := t.TempDir()
	_, err := DetachedRunner("/bin/true").Start(wf.DetachedRequest{
		Argv: []string{"true"}, Dir: dir,
		ResultPath: filepath.Join(dir, "1.json"), ProcPath: filepath.Join(dir, "1.proc"), LogPath: filepath.Join(dir, "1.log"),
	})
	if !errors.Is(err, wf.ErrUnavailable) || !strings.Contains(err.Error(), "Linux") {
		t.Fatalf("Start = %v, want ErrUnavailable naming Linux", err)
	}
}
