package shell_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type Marker struct{}

func TestShell(t *testing.T) {
	i := shell.Run("make", "test").In("/w")
	if i.Dir != "/w" || len(i.Argv) != 2 || i.Argv[0] != "make" {
		t.Fatalf("invocation = %+v", i)
	}
	if d := shell.Run("x").Dir; d != "" {
		t.Errorf("default Dir = %q", d)
	}
	o := shell.Observe(i).Desc()
	if o.Adapter != "shell" || o.Op != "run" || o.Result != "wf.RunResult" {
		t.Errorf("observe = %+v", o)
	}
	e := shell.Effect(wf.KeyOf("k"), i)
	if e.Adapter != "shell" || e.Op != "run" || e.Key == nil {
		t.Errorf("effect = %+v", e)
	}
}

func TestCommandBuilds(t *testing.T) {
	d := wf.New("t")
	shell.Command[Marker](d, shell.Run("true"))
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleBuild {
			t.Errorf("build problem: %s", p)
		}
	}
}

// TestShellCommandHonoursDir: shell.Command carries Invocation.Dir into the
// declared state, which the view shows; without a Dir it stays unset.
func TestShellCommandHonoursDir(t *testing.T) {
	d := wf.New("dir")
	shell.Command[Marker](d, shell.Run("make").In("sub"))
	text := d.Render()
	if !strings.Contains(text, "command make (in sub)") {
		t.Errorf("a Command built from an Invocation with Dir sub must run in sub:\n%s", text)
	}
	d2 := wf.New("nodir")
	shell.Command[Marker](d2, shell.Run("make"))
	if strings.Contains(d2.Render(), "(in ") {
		t.Errorf("a Command without Dir must not show a directory:\n%s", d2.Render())
	}
}
