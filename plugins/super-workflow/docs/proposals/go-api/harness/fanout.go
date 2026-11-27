package harness

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// CoordinatorEvidence counts transcript tool invocations, not tool results or
// prose mentioning tools. When Known is false, counts are observed lower bounds;
// zero must not be interpreted as evidence that no calls were made.
type CoordinatorEvidence struct {
	Known      bool
	Calls      int
	ChildVerbs map[string]int
}

// InspectCoordinator reads Claude/Codex JSONL transcripts. Hermes coordinator
// storage has no supported transcript schema here. Executable names identify the
// workflow CLI; absent an explicit list, wf, super and workflow are recognized.
// Only literal command forms are interpreted; no shell or JavaScript is run.
func InspectCoordinator(transcript, harnessName string, executableNames ...string) CoordinatorEvidence {
	r := CoordinatorEvidence{ChildVerbs: map[string]int{}}
	if harnessName != "claude" && harnessName != "codex" {
		return r
	}
	if len(executableNames) == 0 {
		executableNames = []string{"wf", "super", "workflow"}
	}
	seen := map[string]bool{}
	recognized, valid := false, true
	command := func(argv []string) {
		verb, ok := coordinatorChildVerb(argv, executableNames)
		if !ok {
			valid = false
		}
		if verb != "" {
			r.ChildVerbs[verb]++
		}
	}
	call := func(id, name, input, arguments string) {
		if id != "" && seen[id] {
			return
		}
		if id != "" {
			seen[id] = true
		}
		r.Calls++
		if name == "" {
			valid = false
			return
		}
		if harnessName == "claude" && name == "Bash" {
			var args struct{ Command string }
			if json.Unmarshal([]byte(arguments), &args) != nil || args.Command == "" {
				valid = false
				return
			}
			argv, ok := literalShellArgv(args.Command)
			if !ok {
				valid = false
				return
			}
			command(argv)
		} else if harnessName == "codex" && (name == "exec" || name == "exec_command") {
			commands := codexExecArgv(name, input, arguments)
			if len(commands) == 0 {
				valid = false
			}
			for _, c := range commands {
				command(c.argv)
			}
		}
	}
	ok := eachLine(transcript, func(line []byte) {
		if len(strings.TrimSpace(string(line))) == 0 {
			return
		}
		var row struct {
			Type    string
			Message struct {
				Role    string
				Content json.RawMessage
			}
			Payload struct {
				Type, Name, Input, Arguments string
				CallID                       string `json:"call_id"`
			}
		}
		if json.Unmarshal(line, &row) != nil {
			valid = false
			return
		}
		if harnessName == "codex" {
			switch row.Type {
			case "session_meta", "turn_context", "event_msg":
				recognized = true
			case "response_item":
				switch row.Payload.Type {
				case "function_call", "custom_tool_call":
					recognized = true
					call(row.Payload.CallID, row.Payload.Name, row.Payload.Input, row.Payload.Arguments)
				case "message", "agent_message", "reasoning", "function_call_output", "custom_tool_call_output":
					recognized = true
				default:
					valid = false
				}
			default:
				valid = false
			}
			return
		}
		switch row.Type {
		case "assistant":
			var content []struct {
				Type, ID, Name string
				Input          json.RawMessage
			}
			if json.Unmarshal(row.Message.Content, &content) != nil {
				valid = false
				return
			}
			recognized = true
			for _, c := range content {
				if c.Type == "tool_use" {
					call(c.ID, c.Name, "", string(c.Input))
				}
			}
		case "user", "system", "progress", "summary", "file-history-snapshot", "queue-operation":
			recognized = true
		default:
			valid = false
		}
	})
	r.Known = ok && recognized && valid
	return r
}

func coordinatorChildVerb(argv, executables []string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	if len(argv) == 3 && (filepath.Base(argv[0]) == "bash" || filepath.Base(argv[0]) == "sh") && (argv[1] == "-c" || argv[1] == "-lc") {
		parsed, ok := literalShellArgv(argv[2])
		if !ok {
			return "", false
		}
		return coordinatorChildVerb(parsed, executables)
	}
	if len(argv) >= 4 && argv[0] == "cd" && argv[2] == shellAnd {
		argv = argv[3:]
	}
	for _, arg := range argv {
		if arg == shellAnd {
			return "", false
		}
	}
	if strings.ContainsAny(argv[0], "=*?[") || argv[0] == "env" || argv[0] == "sudo" {
		return "", false
	}
	matched := false
	for _, executable := range executables {
		if executable != "" && filepath.Base(argv[0]) == filepath.Base(executable) {
			matched = true
		}
	}
	if !matched {
		return "", true
	}
	args := argv[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		flag, _, inline := strings.Cut(args[0], "=")
		switch flag {
		case "--state-dir", "--run-id", "--assets", "--workspace":
			args = args[1:]
			if !inline {
				if len(args) == 0 {
					return "", false
				}
				args = args[1:]
			}
		default:
			return "", false
		}
	}
	if len(args) == 0 {
		return "", true
	}
	switch args[0] {
	case "prompt", "done", "failed", "started", "validate":
		return args[0], true
	}
	return "", true
}
