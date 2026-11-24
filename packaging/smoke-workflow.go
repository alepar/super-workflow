package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type write struct{}
type answer struct{ OK bool }
type finished struct{}
type stopped struct{}

func main() {
	d := wf.New("plugin-smoke")
	d.Describe("A native worker writes a single text receipt to its allocated artifact.")
	worker := wf.NewAgent[write, answer](d)
	done := wf.NewTerminal[finished](d, wf.Success)
	stop := wf.NewTerminal[stopped](d, wf.Stopped)
	wf.NewArtifact[string](worker, "receipt.txt")
	worker.Spec(
		wf.PromptText("Write the line native plugin smoke passed to your allocated receipt.txt artifact. Complete the step with JSON output {\u0022OK\u0022:true} using the engine's printed workflow commands."),
		wf.Attempts(1, worker.To(stop, "worker could not deliver the receipt")),
	)
	worker.ThenTo(done, "receipt accepted")
	d.OnFailures(stop, wf.Blocked, wf.Unavailable, wf.Uncertain)
	wf.Main(d)
}
