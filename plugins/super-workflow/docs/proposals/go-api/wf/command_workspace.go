package wf

import (
	"fmt"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
)

// workspaceCommand anchors a shell command to the stable run root. The tool
// workdir still needs to be explicit: some harnesses cd before running cmd.
func workspaceCommand(harnessName, workspace, command string) string {
	if workspace == "" || command == "" {
		return command
	}
	// Some harnesses only apply their command permissions to a single invocation.
	if adapter, ok := harness.For(harnessName).(interface{ UnchainedCommands() bool }); ok && adapter.UnchainedCommands() {
		return command
	}
	return "cd " + sink.Quote(workspace) + " && " + command
}

const toolWorkdirAdviceHead = "For workflow commands, set the shell tool working-directory (workdir/cwd) to "

func toolWorkdirAdvice(workspace string) string {
	if workspace == "" {
		return ""
	}
	return fmt.Sprintf(toolWorkdirAdviceHead+"%s; never inherit a removable task directory.\n", workspace)
}

func (c *cycle) verbLine(verb string, args ...string) string {
	return c.commandLine(c.e.opts.VerbLine(c.run, verb, args...))
}

func (c *cycle) commandLine(command string) string {
	return c.e.workspaceCommand(c.workspace(), command)
}

func (e *Engine) workspaceCommand(workspace, command string) string {
	return workspaceCommand(e.opts.caller.Harness, workspace, command)
}

// resultLines are the command lines of a next result, from the workspace its
// own cycle read: rendering takes no lock.
func (e *Engine) resultLines(run RunID, r NextResult) func(string, ...string) string {
	return func(verb string, args ...string) string {
		return e.workspaceCommand(r.workspace, e.opts.VerbLine(run, verb, args...))
	}
}

// commandLines is for CLI rendering outside a locked cycle. Resolve the root
// from the run, not the CLI process's current directory or an optional flag.
func (e *Engine) commandLines(run RunID) (func(string, ...string) string, error) {
	status, err := e.Status(run)
	if err != nil {
		return nil, err
	}
	return func(verb string, args ...string) string {
		return e.workspaceCommand(status.Workspace, e.opts.VerbLine(run, verb, args...))
	}, nil
}
