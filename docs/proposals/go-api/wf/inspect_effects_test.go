package wf_test

import (
	"reflect"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type effectInspectCommand struct{}
type effectInspectEnd struct{}

func TestStateEffectKindsListsCommandsAndDeclaredExternalUses(t *testing.T) {
	d := wf.New("effects")
	command := wf.NewCommand[effectInspectCommand](d, "check")
	end := wf.NewTerminal[effectInspectEnd](d, wf.Success)
	count := d.Counter("attempts")
	edge := command.To(end, "done").With(wf.ExternalEffect{Adapter: "git", Op: "merge", Key: wf.KeyOf("merge")}, count.Inc(), wf.ExternalEffect{Adapter: "git", Op: "merge", Key: wf.KeyOf("another")})
	command.Then(edge)
	options := wf.Options[struct{ Skip bool }](d)
	command.To(end, "skip").With(wf.ExternalEffect{Adapter: "tracker", Op: "update", Key: wf.KeyOf("skip")}).When(wf.Field(options, func(o *struct{ Skip bool }) *bool { return &o.Skip }).Is(true))
	got := wf.Inspect(d).States[0].EffectKinds()
	want := []wf.EffectKind{{Adapter: "shell", Op: "run"}, {Adapter: "git", Op: "merge"}, {Adapter: "tracker", Op: "update"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("effect kinds %v, want %v", got, want)
	}
}
