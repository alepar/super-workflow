package wf_test

import (
	"os/exec"
	"strings"
	"testing"
	"unicode"

	ha "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

// shellArgs is the argument vector sh, bash and zsh with extendedglob read
// from words, failing the test when they disagree.
func shellArgs(t *testing.T, words string) []string {
	t.Helper()
	shells := [][]string{{"sh"}, {"bash"}}
	if _, err := exec.LookPath("zsh"); err == nil {
		shells = append(shells, []string{"zsh", "-o", "extendedglob"}) // a user's interactive shell
	}
	var first []string
	for i, sh := range shells {
		out, err := exec.Command(sh[0], append(sh[1:], "-c", "printf '%s\\0' x "+words)...).Output()
		if err != nil {
			t.Fatalf("%s -c ... %s: %v", sh, words, err)
		}
		got := strings.Split(string(out), "\x00")
		got = got[1 : len(got)-1] // drop the x sentinel and the final NUL's empty tail
		if i == 0 {
			first = got
		} else if strings.Join(got, "\x00") != strings.Join(first, "\x00") {
			t.Fatalf("sh read %q, %s read %q from %s", first, sh, got, words)
		}
	}
	return first
}

// shellBoundRun binds dir to value and returns the one instruction issued,
// or nil with the refusal when none was (the state blocks, and its retry
// route may end the run).
func shellBoundRun(t *testing.T, value string, in wf.Input) (*wf.Instruction, string) {
	t.Helper()
	return shellBoundIn(t, "Run this:\n\n```sh\ncd [W] && git status\n```\n", value, in)
}

func shellBoundIn(t *testing.T, prompt, value string, in wf.Input) (*wf.Instruction, string) {
	t.Helper()
	d := boundDef("shell-bound", prompt, in)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: t.TempDir()})
	run := h.run(d, map[string]string{"dir": value})
	got := h.next(run)
	if len(got) == 0 {
		st := h.status(run)
		if st.Phase == wf.RunBlocked {
			return nil, st.Instances[0].Reason
		}
		return nil, st.Cause
	}
	if len(got) != 1 {
		t.Fatalf("instructions %+v", got)
	}
	return &got[0], ""
}

// unwordable is true for text no prompt line carries as one copyable shell
// word: a control character (NUL included) or a backtick. A run parameter
// is stored as JSON, so invalid UTF-8 reaches binding as U+FFFD
// (hostile.Readable).
func unwordable(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == '`' }) >= 0
}

// Every hostile input either becomes exactly one word, the same for sh and
// bash, that the prompt shows on the line the template put it, or is
// refused, which blocks the state. It is refused exactly when no prompt line
// can carry it.
func TestShellBoundIsOneWordForEveryHostileInput(t *testing.T) {
	placements := map[string]string{
		"fence":  "Run this:\n\n```sh\ncd [W] && git status\n```\n",
		"prose":  "Then run cd [W] && git status now.",
		"span":   "Run `cd [W] && git status` now.",
		"list":   "- cd [W] && git status",
		"double": "Run ``cd [W] && git status`` now.",
		"start":  "[W] --help is it",
		"item":   "- [W] && git status",
		"indent": "Run:\n\n    [W] --help",
		"token":  "Then run cd {{w}} && git status now.",
	}
	for _, in := range hostile.Corpus() {
		for where, prompt := range placements {
			t.Run(in.Name+"/"+where, func(t *testing.T) {
				ins, reason := shellBoundIn(t, prompt, in.Value, wf.ShellBound("W", "{dir}"))
				if unwordable(in.Value) {
					if ins != nil || reason == "" {
						t.Fatalf("%q must be refused; got instruction %v", in.Value, ins)
					}
					return
				}
				if ins == nil {
					t.Fatalf("blocked: %s", reason)
				}
				word := ins.Inputs["W"]
				if got := shellArgs(t, word); len(got) != 1 || got[0] != hostile.Readable(in.Value) {
					t.Fatalf("ShellBound value %s reads as %q, want the one word %q", word, got, hostile.Readable(in.Value))
				}
				line := strings.Replace(strings.Replace(prompt, "[W]", word, 1), "{{w}}", word, 1)
				if i := strings.LastIndex(line, "\n```\n"); where == "fence" {
					line = line[strings.Index(line, "cd "):i]
				}
				// At the start of a line the word cannot open a block: a list
				// item ("-", "+", "1."), a setext underline ("--"), a thematic
				// break ("___").
				if d := strings.TrimLeft(word, "0123456789"); word[0] == '-' || word[0] == '+' || word[0] == '_' || len(d) < len(word) && (strings.HasPrefix(d, ".") || strings.HasPrefix(d, ")")) {
					t.Errorf("word %s could start a Markdown block", word)
				}
				if !strings.Contains(ins.Prompt, line) {
					t.Errorf("prompt lacks the line %q:\n%s", line, ins.Prompt)
				}
				// A code span reads back as exactly the command.
				if where == "span" || where == "double" {
					open := strings.Index(line, "`")
					span := line[open : strings.LastIndex(line, "`")+1]
					if got, err := hostile.ReadInline(span); err != nil || got != "cd "+word+" && git status" {
						t.Errorf("span %s reads back %q, %v", span, got, err)
					}
				}
			})
		}
	}
}

func TestShellWordsQuotesEachElement(t *testing.T) {
	if ins, _ := shellBoundRun(t, "- -- 1. +x a#b ___", wf.ShellWords("W", "{dir}")); ins == nil || ins.Inputs["W"] != "'-' '--' '1.' '+x' 'a#b' '___'" {
		t.Fatalf("block-starting words must be quoted, got %+v", ins)
	}
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			ins, reason := shellBoundRun(t, in.Value, wf.ShellWords("W", "{dir}"))
			split := func(v string) []string {
				return strings.FieldsFunc(v, func(r rune) bool { return strings.ContainsRune(" \t\r\n", r) })
			}
			want := split(hostile.Readable(in.Value))
			if unwordable(strings.Join(split(in.Value), " ")) {
				if ins != nil {
					t.Fatalf("%q must be refused; got %s", in.Value, ins.Inputs["W"])
				}
				return
			}
			if ins == nil {
				t.Fatalf("blocked: %s", reason)
			}
			got := shellArgs(t, ins.Inputs["W"])
			if strings.Join(got, "\x00") != strings.Join(want, "\x00") || len(got) != len(want) {
				t.Fatalf("ShellWords value %s reads as %q, want %q", ins.Inputs["W"], got, want)
			}
		})
	}
	ins, _ := shellBoundRun(t, " \n\t", wf.ShellWords("W", "{dir}"))
	if ins == nil || ins.Inputs["W"] != "" {
		t.Fatalf("blank text must bind empty, got %+v", ins)
	}
}

func TestShellBoundShowsItsModeInTheView(t *testing.T) {
	d := boundDef("shell-view", "cd [A] [B] [C]", wf.Bound("A", "{dir}"), wf.ShellBound("B", "{dir}"), wf.ShellWords("C", "{dir}"))
	var view []string
	for _, s := range wf.Inspect(d).States {
		if s.Name == "bdAgent" {
			view = s.Inputs
		}
	}
	if len(view) != 3 || strings.Contains(view[0], "shell") || !strings.Contains(view[1], "B (shell word) = ") || !strings.Contains(view[2], "C (shell words) = ") {
		t.Fatalf("view inputs %q, want the shell mode on B and C only", view)
	}
}

// Paged delivery presents a shell word as bound: no note after it, no
// reference in its place, so the command line it completes stays whole.
func TestPagedDeliveryPresentsAShellWordAsBound(t *testing.T) {
	d, _ := simple("paged-shell", wf.PromptText("Then run cd [W] && git status now.", wf.ShellBound("W", "/ws/a b")))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{PagedPrompts: true})
	run, ins := handOneUnit(t, h, d)
	if err := h.e.Started(run, ins.Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	page, err := h.e.(*wf.Engine).PromptPage(run, ins.Name, "", ha.Caller{}, wf.PromptPageRequest{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Delivered, "Then run cd '/ws/a b' && git status now.") {
		t.Fatalf("the shell word did not reach the page whole:\n%s", page.Delivered)
	}
}
