package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"text/scanner"
)

// codexExecArgv reads literal exec_command arguments, never arbitrary text
// containing a command. Dynamic JavaScript or shell expansion is not evidence.
type codexCommand struct {
	argv    []string
	workdir string
}

var codexResultBinding = regexp.MustCompile(`^[A-Za-z_][A-Za-z_0-9]*$`)

// jsReserved is the names a const binding cannot take, or must not shadow:
// reserved words fail the script at parse, before any call runs; Promise,
// tools and text are what the script itself reads.
var jsReserved = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`break case catch class const continue debugger default delete do else enum export
		extends false finally for function if import in instanceof new null return super switch this throw true try
		typeof var void while with yield let static implements interface package private protected public await
		arguments eval undefined NaN Infinity globalThis Promise tools text`) {
		jsReserved[w] = true
	}
}

// resultBinding reports whether name may bind a call's result: a plain
// identifier that is no reserved word and shadows nothing the script reads.
func resultBinding(name string) bool {
	return codexResultBinding.MatchString(name) && !jsReserved[name]
}

// commandFields are the exec_command fields the claim reads.
var commandFields = map[string]bool{"cmd": true, "args": true, "workdir": true}

// seenFolded reports whether fields already holds key under another
// spelling (case): the tool reads one of them, so the call is ambiguous.
func seenFolded(fields map[string]json.RawMessage, key string) bool {
	for k := range fields {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// toolCommand is the one command exec_command's fields name, read as the
// tool reads them: cmd (a string) or args (strings), never both, an
// optional workdir string, field names exact. A field that names one of
// those in another case is refused: the tool would not read it, so it is
// no evidence of what ran.
func toolCommand(fields map[string]json.RawMessage) (codexCommand, bool) {
	for k := range fields {
		if commandFields[strings.ToLower(k)] && !commandFields[k] {
			return codexCommand{}, false
		}
	}
	var c codexCommand
	if raw, ok := fields["workdir"]; ok {
		if !jsonString(raw, &c.workdir) {
			return codexCommand{}, false
		}
	}
	rawCmd, hasCmd := fields["cmd"]
	rawArgs, hasArgs := fields["args"]
	switch {
	case hasCmd == hasArgs:
		return codexCommand{}, false
	case hasCmd:
		var cmd string
		if !jsonString(rawCmd, &cmd) {
			return codexCommand{}, false
		}
		argv, ok := literalShellArgv(cmd)
		if !ok {
			return codexCommand{}, false
		}
		c.argv = argv
	default:
		var elems []json.RawMessage
		if json.Unmarshal(rawArgs, &elems) != nil || len(elems) == 0 {
			return codexCommand{}, false
		}
		args := make([]string, len(elems))
		for i, e := range elems {
			if !jsonString(e, &args[i]) || strings.ContainsRune(args[i], 0) {
				return codexCommand{}, false
			}
		}
		c.argv = args
	}
	return c, true
}

// jsonFields is a JSON object's top-level fields; a duplicate key, also in
// another case, refuses it (encoding/json would silently keep the last).
func jsonFields(raw []byte) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, false
		}
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			return nil, false
		}
		if _, dup := fields[key]; dup || seenFolded(fields, key) {
			return nil, false
		}
		fields[key] = v
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF { // nothing after the object
		return nil, false
	}
	return fields, true
}

// jsonString decodes raw as a JSON string, never null (which encoding/json
// would decode into "" without error).
func jsonString(raw json.RawMessage, s *string) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, s) == nil
}

// codexExecArgv is the commands a Codex tool call requested, read from the
// call's input alone. Limit: it cannot tell a request the tool refused
// before launch from a process that started (an allSettled batch proves no
// launch either), for the direct forms as for a batch.
func codexExecArgv(name, input, arguments string) []codexCommand {
	var commands []codexCommand
	add := func(fields map[string]json.RawMessage) {
		if c, ok := toolCommand(fields); ok {
			commands = append(commands, c)
		}
	}
	if name == "exec_command" {
		if fields, ok := jsonFields([]byte(arguments)); ok {
			add(fields)
		}
		return commands
	}
	if name != "exec" {
		return nil
	}
	var lex scanner.Scanner
	lex.Init(strings.NewReader(input))
	lex.Mode = scanner.ScanIdents | scanner.ScanStrings | scanner.ScanRawStrings | scanner.ScanChars | scanner.ScanComments | scanner.SkipComments | scanner.ScanInts
	lex.Error = func(*scanner.Scanner, string) {}
	var tokens []string
	for tok := lex.Scan(); tok != scanner.EOF; tok = lex.Scan() {
		tokens = append(tokens, lex.TokenText())
	}
	if calls, ok := execBatch(tokens); ok {
		// A batch counts only when every element is a call the tool reads
		// as one command: one it cannot interpret voids the whole batch.
		var batch []codexCommand
		for _, fields := range calls {
			c, ok := toolCommand(fields)
			if !ok {
				return nil
			}
			batch = append(batch, c)
		}
		return batch
	}
	for i := 0; i+5 < len(tokens); i++ {
		if strings.Join(tokens[i:i+5], "") != "tools.exec_command({" {
			continue
		}
		// Accept only complete known direct-call forms, including a bound
		// result printed by the next statement. Never scan arbitrary JS
		// for an embedded call: it may be unreachable or merely quoted.
		prefix := strings.Join(tokens[:i], " ")
		binding := ""
		if i == 4 && tokens[0] == "const" && tokens[2] == "=" && tokens[3] == "await" && resultBinding(tokens[1]) {
			binding = tokens[1]
		}
		if prefix != "" && prefix != "await" && prefix != "text ( await" && binding == "" {
			continue
		}

		fields, j, valid := execCall(tokens, i)
		if valid {
			end := j
			if prefix == "text ( await" {
				if end >= len(tokens) || tokens[end] != ")" {
					continue
				}
				end++
			}
			if binding != "" {
				// The bound result is printed whole or by its output
				// (baseline-1117: two real children printed text(r)).
				suffix := strings.TrimSuffix(strings.Join(tokens[end:], " "), " ;")
				if suffix != "; text ( "+binding+" . output )" && suffix != "; text ( "+binding+" )" {
					continue
				}
				end = len(tokens)
			}
			if end < len(tokens) && tokens[end] == ";" {
				end++
			}
			if end != len(tokens) {
				continue
			}
			add(fields)
		}
	}
	return commands
}

// execCall parses the direct call tools.exec_command({...}) whose first
// token is tokens[i]: its literal fields and the index after its closing
// parenthesis. ok is false for anything but a complete call of literal fields.
func execCall(tokens []string, i int) (fields map[string]json.RawMessage, end int, ok bool) {
	if i+5 > len(tokens) || strings.Join(tokens[i:i+5], "") != "tools.exec_command({" {
		return nil, 0, false
	}
	fields = map[string]json.RawMessage{}
	j := i + 5
	valid := true
	for j < len(tokens) && tokens[j] != "}" {
		if j+2 >= len(tokens) || tokens[j+1] != ":" {
			valid = false
			break
		}
		key := tokens[j]
		// A property name with an escape is refused: JS and this reader
		// could decode it to different names.
		if strings.ContainsRune(key, '\\') {
			valid = false
			break
		}
		switch {
		case strings.HasPrefix(key, `"`):
			k, err := strconv.Unquote(key)
			if err != nil {
				valid = false
				break
			}
			key = k
		case strings.HasPrefix(key, "'") && strings.HasSuffix(key, "'") && len(key) >= 2:
			key = key[1 : len(key)-1]
		}
		if !valid {
			break
		}
		if _, dup := fields[key]; dup || seenFolded(fields, key) {
			valid = false
			break
		}
		j += 2
		start := j
		if tokens[j] == "[" {
			for j < len(tokens) && tokens[j] != "]" {
				j++
			}
			if j < len(tokens) {
				j++
			}
		} else {
			j++
		}
		raw := strings.Join(tokens[start:j], "")
		// JS single-quoted strings have no interpolation; normalize them to JSON.
		if strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'") {
			value := strings.ReplaceAll(raw[1:len(raw)-1], `\'`, "'")
			value = strings.ReplaceAll(value, `"`, `\"`)
			raw = `"` + value + `"`
		}
		if !json.Valid([]byte(raw)) {
			valid = false
			break
		}
		fields[key] = json.RawMessage(raw)
		if j < len(tokens) && tokens[j] == "," {
			j++
		} else if j >= len(tokens) || tokens[j] != "}" {
			valid = false
			break
		}
	}
	if !valid || j+1 >= len(tokens) || tokens[j] != "}" || tokens[j+1] != ")" {
		return nil, 0, false
	}
	return fields, j + 2, true
}

// execBatch is the calls of a script that is one awaited batch of direct
// calls and nothing else: [text(]await Promise.all|allSettled([call, ...])[)],
// or that batch bound and printed whole (const r = await ...; text(r)). Every
// element runs, so each is a run of its command. ok is false for any other
// script, or a batch holding anything but direct calls.
func execBatch(tokens []string) (calls []map[string]json.RawMessage, ok bool) {
	t := tokens
	if len(t) > 0 && t[len(t)-1] == ";" {
		t = t[:len(t)-1]
	}
	var body []string
	switch {
	case len(t) > 4 && t[0] == "text" && t[1] == "(" && t[2] == "await" && t[len(t)-1] == ")":
		body = t[3 : len(t)-1]
	case len(t) > 1 && t[0] == "await":
		body = t[1:]
	case len(t) > 9 && t[0] == "const" && t[2] == "=" && t[3] == "await" && resultBinding(t[1]) &&
		strings.Join(t[len(t)-5:], " ") == "; text ( "+t[1]+" )":
		body = t[4 : len(t)-5]
	default:
		return nil, false
	}
	if len(body) < 8 || body[0] != "Promise" || body[1] != "." || (body[2] != "all" && body[2] != "allSettled") ||
		body[3] != "(" || body[4] != "[" || body[len(body)-2] != "]" || body[len(body)-1] != ")" {
		return nil, false
	}
	elems := body[5 : len(body)-2]
	for i := 0; i < len(elems); {
		fields, end, ok := execCall(elems, i)
		if !ok {
			return nil, false
		}
		calls = append(calls, fields)
		i = end
		if i < len(elems) {
			if elems[i] != "," {
				return nil, false
			}
			i++
		}
	}
	return calls, len(calls) > 0
}

// shellAnd cannot be a literal Unix argv value (NUL is forbidden). Keeping
// the operator distinct preserves quoting and leaves direct argv arrays literal.
const shellAnd = "\x00&&"

// literalShellArgv handles the quoting used by printed workflow commands.
// Operators and expansions need shell evaluation and are not accepted here.
func literalShellArgv(command string) ([]string, bool) {
	if strings.ContainsRune(command, 0) {
		return nil, false
	}
	command = strings.TrimSpace(command)
	var argv []string
	var word strings.Builder
	var quote rune
	started := false
	chars := []rune(command)
	for i := 0; i < len(chars); i++ {
		c := chars[i]
		if c == '\\' && quote != '\'' {
			i++
			if i == len(chars) {
				return nil, false
			}
			if quote == '"' && !strings.ContainsRune("$`\"\\\n", chars[i]) {
				word.WriteRune('\\')
			}
			word.WriteRune(chars[i])
			started = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				if quote == '"' && (c == '$' || c == '`') {
					return nil, false
				}
				word.WriteRune(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			started = true
		case '\n', '\r':
			return nil, false
		case ' ', '\t':
			if started {
				argv = append(argv, word.String())
				word.Reset()
				started = false
			}
		case '#':
			if !started {
				i = len(chars)
				continue
			}
			word.WriteRune(c)
		case '&':
			if i+1 >= len(chars) || chars[i+1] != '&' {
				return nil, false
			}
			if started {
				argv = append(argv, word.String())
				word.Reset()
				started = false
			}
			argv = append(argv, shellAnd)
			i++
		case ';', '|', '<', '>', '(', ')', '$', '`':
			return nil, false
		default:
			word.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if started {
		argv = append(argv, word.String())
	}
	return argv, true
}
