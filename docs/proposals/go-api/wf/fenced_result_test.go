package wf_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// submittedRecords returns the decoded payloads of run's submitted records.
func submittedRecords(t *testing.T, e *cliEnv, run wf.RunID) []wf.SubmittedData {
	t.Helper()
	recs, err := wf.ReadJournal(e.state, run)
	if err != nil {
		t.Fatal(err)
	}
	var out []wf.SubmittedData
	for _, r := range recs {
		if r.Kind != wf.JournalSubmitted {
			continue
		}
		var d wf.SubmittedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// TestFencedResultUnwrapped: a result file that is exactly one fenced code
// block (a ``` or ```json opening line, a closing ``` line, only whitespace
// outside) is parsed from inside the fence, and the submitted record notes it.
func TestFencedResultUnwrapped(t *testing.T) {
	for name, body := range map[string]string{
		"json tag":            "```json\n{\"OK\":true}\n```\n",
		"bare fence":          "```\n{\"OK\":true}\n```",
		"whitespace outside":  "\n  \n```json\n{\n  \"OK\": true\n}\n```\n\n",
		"crlf line endings":   "```json\r\n{\"OK\":true}\r\n```\r\n",
		"multi-line contents": "```json\n{\n\"OK\":\ntrue\n}\n```",
		"indented fences":     "  ```json\n{\"OK\":true}\n  ```",
	} {
		t.Run(name, func(t *testing.T) {
			e := newCLIEnv(t)
			run := e.start("agent-cli")
			in := e.next(run)[0]
			args := e.doneArgs(in, body)
			validate := append([]string{"validate"}, args[1:]...)
			if code, _, er := e.cli(validate...); code != 0 {
				t.Fatalf("validate of a fenced result = %d %q, want 0", code, er)
			}
			if code, _, er := e.cli(args...); code != 0 {
				t.Fatalf("done of a fenced result = %d %q, want 0", code, er)
			}
			if st := e.status(run); st.Phase != wf.RunEnded {
				t.Errorf("status = %+v, want ended", st)
			}
			subs := submittedRecords(t, e, run)
			if len(subs) != 1 {
				t.Fatalf("submitted records = %d, want 1", len(subs))
			}
			var got map[string]any
			if err := json.Unmarshal(subs[0].Output, &got); err != nil || got["OK"] != true {
				t.Errorf("journaled output = %s, want the fence's contents", subs[0].Output)
			}
			if !strings.Contains(subs[0].Normalized, "fence") {
				t.Errorf("submitted Normalized = %q, want a note that the fence was unwrapped", subs[0].Normalized)
			}
		})
	}
}

// TestFenceToleranceIsNarrow: anything other than exactly one fenced block
// around valid JSON is refused as before, and valid JSON carries no note.
func TestFenceToleranceIsNarrow(t *testing.T) {
	for name, body := range map[string]string{
		"trailing prose":     "```json\n{\"OK\":true}\n```\nAll done.",
		"leading prose":      "Here is the result:\n```json\n{\"OK\":true}\n```",
		"two blocks":         "```json\n{\"OK\":true}\n```\n```json\n{\"OK\":true}\n```",
		"other language tag": "```js\n{\"OK\":true}\n```",
		"tag with prose":     "```json result\n{\"OK\":true}\n```",
		"invalid inside":     "```json\n{\"OK\":true,}\n```",
		"unclosed":           "```json\n{\"OK\":true}\n",
		"inline fence":       "```{\"OK\":true}```",
		"prose with object":  "The result is {\"OK\":true}",
	} {
		t.Run(name, func(t *testing.T) {
			e := newCLIEnv(t)
			run := e.start("agent-cli")
			in := e.next(run)[0]
			args := e.doneArgs(in, body)
			if code, _, er := e.cli(args...); code != 5 || !strings.Contains(er, "is a JSON string") {
				t.Fatalf("done = %d %q, want 5 and the not-JSON refusal", code, er)
			}
			subs := submittedRecords(t, e, run)
			if len(subs) != 1 || subs[0].Normalized != "" {
				t.Fatalf("submitted = %+v, want one record with no note", subs)
			}
			var raw string
			if err := json.Unmarshal(subs[0].Output, &raw); err != nil || raw != strings.TrimSpace(body) {
				t.Errorf("journaled output = %s, want the file's text as a JSON string", subs[0].Output)
			}
		})
	}
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	in := e.next(run)[0]
	e.submit(in, `{"OK":true}`)
	if subs := submittedRecords(t, e, run); len(subs) != 1 || subs[0].Normalized != "" {
		t.Errorf("plain JSON submitted = %+v, want no note", subs)
	}
}

// TestFencedResultWrongTypeKeepsNote: a fenced block of valid JSON that fails
// the state's output type is refused, and its submitted record keeps the note.
func TestFencedResultWrongTypeKeepsNote(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	in := e.next(run)[0]
	if code, _, er := e.cli(e.doneArgs(in, "```json\n{\"OK\":\"yes\"}\n```")...); code != 5 || !strings.Contains(er, "OK") {
		t.Fatalf("done = %d %q, want 5 naming the field", code, er)
	}
	subs := submittedRecords(t, e, run)
	if len(subs) != 1 || subs[0].Normalized == "" || string(subs[0].Output) != `{"OK":"yes"}` {
		t.Fatalf("submitted = %+v, want the unwrapped output with its note", subs)
	}
}
