package wf

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

type (
	scVerdict string
	scExit    int
	scInner   struct {
		ID int `json:"id"`
	}
	scEmbedded struct {
		Flat string `json:"flat"`
	}
	scBig struct {
		Name     string  `json:"name"`
		Inner    scInner `json:"inner"`
		Opt      *int    `json:"opt,omitempty"`
		Hidden   string  `json:"-"`
		private  int
		Items    []scInner          `json:"items"`
		Scores   map[string]float64 `json:"scores,omitempty"`
		Untagged bool
	}
	scWithEmbedded struct {
		scEmbedded
		Own string `json:"own"`
	}
	scEnums struct {
		Verdict scVerdict `json:"verdict"`
		Exit    scExit    `json:"exit"`
		Plain   string    `json:"plain"`
	}
	scRecursive struct {
		Next *scRecursive `json:"next,omitempty"`
	}
	scEncoded struct {
		At  time.Time       `json:"at"`
		Raw json.RawMessage `json:"raw"`
		Any any             `json:"any"`
	}
	scPattern struct {
		Req   string  `json:"req" pattern:"^R[0-9]+$"`
		Row   *string `json:"row,omitempty" pattern:"^B[0-9]+$"`
		Label string  `json:"label"`
	}
	scSubmission struct {
		Verdict scVerdict `json:"verdict"`
		Notes   []string  `json:"notes,omitempty"`
	}
)

// scEnumData is what NewEnum registers: the plain-string answer set must not
// constrain every string.
func scEnumData() []*enumData {
	d := New("sc")
	NewEnum(d, "verdict", scVerdict("pass"), scVerdict("fail"))
	NewEnum(d, "exit", scExit(0), scExit(2))
	NewEnum(d, "answer", "go")
	return d.enums
}

func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		t.Fatalf("%v in %s", err, s)
	}
	return b.String()
}

func TestOutputSchema(t *testing.T) {
	const inner = `{"properties":{"id":{"type":"integer"}},"required":["id"],"type":"object"}`
	for _, tc := range []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{"string", reflect.TypeFor[string](), `{"type":"string"}`},
		{"int", reflect.TypeFor[int](), `{"type":"integer"}`},
		{"uint8", reflect.TypeFor[uint8](), `{"type":"integer"}`},
		{"float", reflect.TypeFor[float64](), `{"type":"number"}`},
		{"bool", reflect.TypeFor[bool](), `{"type":"boolean"}`},
		{"duration", reflect.TypeFor[time.Duration](), `{"type":"integer"}`},
		{"pointer", reflect.TypeFor[*string](), `{"type":"string"}`},
		{"slice", reflect.TypeFor[[]string](), `{"items":{"type":"string"},"type":"array"}`},
		{"array", reflect.TypeFor[[2]int](), `{"items":{"type":"integer"},"type":"array"}`},
		{"bytes", reflect.TypeFor[[]byte](), `{"type":"string"}`},
		{"map", reflect.TypeFor[map[string]int](), `{"additionalProperties":{"type":"integer"},"type":"object"}`},
		{"struct", reflect.TypeFor[scBig](), `{"properties":{"Untagged":{"type":"boolean"},"inner":` + inner + `,"items":{"items":` + inner + `,"type":"array"},"name":{"type":"string"},"opt":{"type":"integer"},"scores":{"additionalProperties":{"type":"number"},"type":"object"}},"required":["name","inner","items","Untagged"],"type":"object"}`},
		{"embedded", reflect.TypeFor[scWithEmbedded](), `{"properties":{"flat":{"type":"string"},"own":{"type":"string"}},"required":["flat","own"],"type":"object"}`},
		{"enums", reflect.TypeFor[scEnums](), `{"properties":{"exit":{"enum":[0,2],"type":"integer"},"plain":{"type":"string"},"verdict":{"enum":["pass","fail"],"type":"string"}},"required":["verdict","exit","plain"],"type":"object"}`},
		{"recursive", reflect.TypeFor[scRecursive](), `{"properties":{"next":{"type":"object"}},"type":"object"}`},
		{"own encoding", reflect.TypeFor[scEncoded](), `{"properties":{"any":{},"at":{},"raw":{}},"required":["at","raw","any"],"type":"object"}`},
		{"pattern", reflect.TypeFor[scPattern](), `{"properties":{"label":{"type":"string"},"req":{"pattern":"^R[0-9]+$","type":"string"},"row":{"pattern":"^B[0-9]+$","type":"string"}},"required":["req","label"],"type":"object"}`},
		{"empty struct", reflect.TypeFor[struct{}](), `{"properties":{},"type":"object"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactJSON(t, outputSchema(tc.typ, scEnumData())); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestSubmissionBlockGolden(t *testing.T) {
	const want = `## Submission

Work in /work/repo. Use absolute paths under it for every file you read, edit or test.

When the work is done, write your result as JSON to a file outside /work/repo and outside any other git worktree, for example in a directory made by mktemp -d: a result file left in a worktree stops its removal. It must match this JSON schema:

{
  "properties": {
    "notes": {
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "verdict": {
      "enum": [
        "pass",
        "fail"
      ],
      "type": "string"
    }
  },
  "required": [
    "verdict"
  ],
  "type": "object"
}

Then run this exact command, with <file> replaced by that file's absolute path:

/bin/workflow submit r1 --token tk --output <file>
`
	got := submissionBlock(reflect.TypeFor[scSubmission](), scEnumData(), "/work/repo", "/bin/workflow submit r1 --token tk --output <file>")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if again := submissionBlock(reflect.TypeFor[scSubmission](), scEnumData(), "/work/repo", "/bin/workflow submit r1 --token tk --output <file>"); again != got {
		t.Error("the block is not deterministic")
	}
}
