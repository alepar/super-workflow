// Package shell is the command adapter: constructors return descriptors and
// Runtime serves them. It describes a command by argv, with the exit code,
// stdout and stderr as the result.
package shell

import (
	"fmt"
	"strconv"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Result is what a command observed: exit code, stdout and stderr.
type Result = wf.RunResult

// Invocation is a command line: Argv, run in Dir (the executor's working
// directory when empty), reading Stdin on its standard input when set.
type Invocation struct {
	Argv  []string
	Dir   string
	Stdin string // template bound like an argv element; empty: no input

	argvTemplates []argvTemplate
	dirTemplate   *wf.Text
	stdinTemplate *wf.Text
}

type argvTemplate struct {
	index int
	text  wf.Text
}

// Run describes the command argv.
func Run(argv ...string) Invocation { return Invocation{Argv: argv} }

// RunWith describes command argv containing strings and wf.Text templates.
// It panics for any other argument type. Effect binds the marked templates.
func RunWith(argv ...any) Invocation {
	i := Invocation{Argv: make([]string, len(argv))}
	for index, arg := range argv {
		switch value := arg.(type) {
		case string:
			i.Argv[index] = value
		case wf.Text:
			i.Argv[index] = value.Raw()
			i.argvTemplates = append(i.argvTemplates, argvTemplate{index: index, text: value})
		default:
			panic(fmt.Sprintf("shell.RunWith: argv[%d] has type %T, want string or wf.Text", index, arg))
		}
	}
	return i
}

// In returns a copy of i that runs in dir.
func (i Invocation) In(dir string) Invocation {
	i.Dir = dir
	i.dirTemplate = nil
	return i
}

// InTemplate returns a copy of i with a working directory template for Effect.
func (i Invocation) InTemplate(dir wf.Text) Invocation {
	i.Dir, i.dirTemplate = dir.Raw(), &dir
	return i
}

// WithStdin returns a copy of i that feeds tmpl to the command on stdin. For
// Command, tmpl may name {placeholders}; the bound text is journaled with the
// effect, so a payload too big for argv or the environment still survives a
// crash.
func (i Invocation) WithStdin(tmpl string) Invocation {
	i.Stdin = tmpl
	i.stdinTemplate = nil
	return i
}

// WithStdinTemplate returns a copy of i with a standard input template for Effect.
func (i Invocation) WithStdinTemplate(stdin wf.Text) Invocation {
	i.Stdin, i.stdinTemplate = stdin.Raw(), &stdin
	return i
}

func (i Invocation) args() map[string]any {
	a := map[string]any{"argv": i.Argv, "dir": i.Dir}
	if i.Stdin != "" {
		a["stdin"] = i.Stdin
	}
	return a
}

// Observe reads through a command. The command must not mutate anything;
// use Effect for one that does.
func Observe(i Invocation) wf.Observation[Result] {
	return wf.Observe[Result]("shell", "run", i.args())
}

// Effect is an effectful command, keyed by key.
//
// Idempotence is the caller's claim, carried by key: pass wf.KeyOf(...) when
// the command is safe to re-run or checkable, wf.NonIdempotent(reason) when it
// is not. already_done: the adapter has no generic check for a shell command,
// so a keyed command is re-run after a crash and a NonIdempotent one routes to
// a manual verification instruction.
func Effect(key wf.Key, i Invocation) wf.ExternalEffect {
	e := wf.ExternalEffect{Adapter: "shell", Op: "run", Args: i.args(), Key: key}
	for _, arg := range i.argvTemplates {
		if arg.index < len(i.Argv) && i.Argv[arg.index] == arg.text.Raw() {
			e = e.WithTemplateArg(arg.text, "argv", strconv.Itoa(arg.index))
		}
	}
	if i.dirTemplate != nil && i.Dir == i.dirTemplate.Raw() {
		e = e.WithTemplateArg(*i.dirTemplate, "dir")
	}
	if i.stdinTemplate != nil && i.Stdin != "" && i.Stdin == i.stdinTemplate.Raw() {
		e = e.WithTemplateArg(*i.stdinTemplate, "stdin")
	}
	return e
}

// Command declares a deterministic command state running i.Argv. Script
// contracts build on Run and Command.
func Command[S any](d *wf.Def, i Invocation) *wf.Command[S] {
	c := wf.NewCommand[S](d, i.Argv...)
	if i.Dir != "" {
		c.In(i.Dir)
	}
	if i.Stdin != "" {
		c.Stdin(i.Stdin)
	}
	return c
}
