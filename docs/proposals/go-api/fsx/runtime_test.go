package fsx_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read[T any](t *testing.T, ws string, o wf.Observation[T]) T {
	t.Helper()
	raw, err := fsx.Runtime(fsx.Config{}).Read(ws, o.Desc())
	if err != nil {
		t.Fatalf("%s: %v", o.Desc().Op, err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func TestEveryDeclaredOpHasARuntime(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	pats := []*regexp.Regexp{
		regexp.MustCompile(`wf\.Observe\[.*\]\("([a-z_]+)", "([a-z_]+)"`),
		regexp.MustCompile(`Adapter: "([a-z_]+)", Op: "([a-z_]+)"`),
	}
	declared := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pats {
			for _, m := range p.FindAllStringSubmatch(string(src), -1) {
				declared[m[1]+"."+m[2]] = true
			}
		}
	}
	var want []string
	for op := range declared {
		want = append(want, op)
	}
	got := fsx.Runtime(fsx.Config{}).Ops()
	sort.Strings(want)
	sort.Strings(got)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime ops %v, declared ops %v", got, want)
	}
}

func TestExists(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "a.txt", "x")
	if !read(t, ws, fsx.Exists("a.txt")) {
		t.Error("existing file reported missing")
	}
	if read(t, ws, fsx.Exists("nope.txt")) {
		t.Error("missing file reported present")
	}
}

func TestHash(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "a.txt", "hello")
	sum := sha256.Sum256([]byte("hello"))
	if got := read(t, ws, fsx.Hash("a.txt")); got != hex.EncodeToString(sum[:]) {
		t.Errorf("hash = %s", got)
	}
	if _, err := fsx.Runtime(fsx.Config{}).Read(ws, fsx.Hash("nope").Desc()); err == nil {
		t.Error("hash of missing file succeeded")
	}
}

func TestReadJSONPointer(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "d.json", `{"a":{"b":[10,{"c":"x"}]},"k/s":1,"t~":2,"":3}`)
	type kv = map[string]any
	if got := read(t, ws, fsx.ReadJSON[float64]("d.json", "/a/b/0")); got != 10 {
		t.Errorf("array index = %v", got)
	}
	if got := read(t, ws, fsx.ReadJSON[string]("d.json", "/a/b/1/c")); got != "x" {
		t.Errorf("nested = %v", got)
	}
	if got := read(t, ws, fsx.ReadJSON[float64]("d.json", "/k~1s")); got != 1 {
		t.Errorf("~1 escape = %v", got)
	}
	if got := read(t, ws, fsx.ReadJSON[float64]("d.json", "/t~0")); got != 2 {
		t.Errorf("~0 escape = %v", got)
	}
	if got := read(t, ws, fsx.ReadJSON[float64]("d.json", "/")); got != 3 {
		t.Errorf("empty key = %v", got)
	}
	if got := read(t, ws, fsx.ReadJSON[kv]("d.json", "")); len(got) != 4 {
		t.Errorf("whole document = %v", got)
	}
	for _, ptr := range []string{"/zzz", "/a/b/9", "/a/b/x", "/a/b/-", "no-slash", "/a/b/0/deeper"} {
		if _, err := fsx.Runtime(fsx.Config{}).Read(ws, fsx.ReadJSON[any]("d.json", ptr).Desc()); err == nil {
			t.Errorf("pointer %q resolved", ptr)
		}
	}
	if _, err := fsx.Runtime(fsx.Config{}).Read(ws, fsx.ReadJSON[any]("missing.json", "").Desc()); err == nil {
		t.Error("missing file read succeeded")
	}
	write(t, ws, "bad.json", `{`)
	if _, err := fsx.Runtime(fsx.Config{}).Read(ws, fsx.ReadJSON[any]("bad.json", "").Desc()); err == nil {
		t.Error("malformed JSON read succeeded")
	}
}

func TestSchemaCheckSubset(t *testing.T) {
	ws := t.TempDir()
	cases := []struct {
		name, schema, data string
		errs               []string
	}{
		{"type ok", `{"type":"string"}`, `"a"`, nil},
		{"type bad", `{"type":"string"}`, `1`, []string{"(root): want type string, got number"}},
		{"type integer ok", `{"type":"integer"}`, `3`, nil},
		{"type integer bad", `{"type":"integer"}`, `3.5`, []string{"(root): want type integer, got number"}},
		{"type number accepts integer", `{"type":"number"}`, `3`, nil},
		{"type list", `{"type":["string","null"]}`, `null`, nil},
		{"type list bad", `{"type":["string","null"]}`, `true`, []string{"(root): want type string or null, got boolean"}},
		{"type object", `{"type":"object"}`, `[]`, []string{"(root): want type object, got array"}},
		{"required ok", `{"required":["a"]}`, `{"a":1}`, nil},
		{"required bad", `{"required":["b","a"]}`, `{}`, []string{`(root): missing required property "a"`, `(root): missing required property "b"`}},
		{"properties ok", `{"properties":{"a":{"type":"string"}}}`, `{"a":"x","b":1}`, nil},
		{"properties absent is fine", `{"properties":{"a":{"type":"string"}}}`, `{}`, nil},
		{"properties bad", `{"properties":{"a":{"type":"string"}}}`, `{"a":1}`, []string{"/a: want type string, got number"}},
		{"items ok", `{"items":{"type":"integer"}}`, `[1,2]`, nil},
		{"items bad", `{"items":{"type":"integer"}}`, `[1,"x",2,"y"]`, []string{"/1: want type integer, got string", "/3: want type integer, got string"}},
		{"enum ok", `{"enum":["a",2,null]}`, `2`, nil},
		{"enum bad", `{"enum":["a",2]}`, `"b"`, []string{"(root): value not in enum"}},
		{"nested path", `{"properties":{"x":{"items":{"required":["id"]}}}}`, `{"x":[{"id":1},{}]}`, []string{`/x/1: missing required property "id"`}},
		{"escaped path", `{"properties":{"a/b":{"type":"string"}}}`, `{"a/b":1}`, []string{"/a~1b: want type string, got number"}},
		{"unsupported keyword ignored", `{"pattern":"^a$","minimum":5}`, `"zzz"`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			write(t, ws, "s.json", c.schema)
			write(t, ws, "d.json", c.data)
			got := read(t, ws, fsx.SchemaCheck("d.json", fsx.Schema{Name: "S", Path: "s.json"}))
			if got.OK != (len(c.errs) == 0) || !reflect.DeepEqual(got.Errors, c.errs) {
				t.Errorf("got %+v, want errors %v", got, c.errs)
			}
		})
	}
}

func TestSchemaCheckMissingOrBadData(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "s.json", `{"type":"object"}`)
	got := read(t, ws, fsx.SchemaCheck("nope.json", fsx.Schema{Path: "s.json"}))
	if got.OK || len(got.Errors) != 1 || !strings.Contains(got.Errors[0], "nope.json") {
		t.Errorf("missing data = %+v", got)
	}
	write(t, ws, "bad.json", `{`)
	if got := read(t, ws, fsx.SchemaCheck("bad.json", fsx.Schema{Path: "s.json"})); got.OK || len(got.Errors) != 1 {
		t.Errorf("malformed data = %+v", got)
	}
	if _, err := fsx.Runtime(fsx.Config{}).Read(ws, fsx.SchemaCheck("bad.json", fsx.Schema{Path: "no-schema.json"}).Desc()); err == nil {
		t.Error("missing schema file succeeded")
	}
}

func TestRelativePathsUseWorkspace(t *testing.T) {
	ws, other := t.TempDir(), t.TempDir()
	write(t, ws, "sub/a.txt", "x")
	write(t, other, "sub/b.txt", "x")
	if !read(t, ws, fsx.Exists("sub/a.txt")) {
		t.Error("relative path not resolved against workspace")
	}
	if read(t, ws, fsx.Exists("sub/b.txt")) {
		t.Error("relative path resolved elsewhere")
	}
	if !read(t, ws, fsx.Exists(filepath.Join(other, "sub/b.txt"))) {
		t.Error("absolute path not honored")
	}
}

func TestGlobSortedByBaseName(t *testing.T) {
	ws := t.TempDir()
	for _, n := range []string{"f10.json", "f2.json", "f1.json", "notes.md"} {
		write(t, ws, "findings/"+n, "{}")
	}
	if err := os.MkdirAll(filepath.Join(ws, "findings", "f3.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := read(t, ws, fsx.Glob("findings", "f*.json"))
	want := []fsx.File{
		{Name: "f1.json", Path: "findings/f1.json"},
		{Name: "f10.json", Path: "findings/f10.json"},
		{Name: "f2.json", Path: "findings/f2.json"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("glob = %v, want %v", got, want)
	}
}

func TestGlobAbsentDirIsEmpty(t *testing.T) {
	ws := t.TempDir()
	raw, err := fsx.Runtime(fsx.Config{}).Read(ws, fsx.Glob("nope", "*").Desc())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[]" {
		t.Errorf("raw = %s, want []", raw)
	}
	if got := read(t, ws, fsx.Glob("nope", "*")); len(got) != 0 {
		t.Errorf("glob = %v", got)
	}
}

func TestGlobBadPattern(t *testing.T) {
	ws := t.TempDir()
	write(t, ws, "d/a.json", "{}")
	if _, err := fsx.Runtime(fsx.Config{}).Read(ws, fsx.Glob("d", "[").Desc()); err == nil {
		t.Error("bad pattern succeeded")
	}
	if _, err := fsx.Runtime(fsx.Config{}).Read(ws, fsx.Glob("absent", "[").Desc()); err == nil {
		t.Error("bad pattern on absent dir succeeded")
	}
}

func TestGlobRelativeToWorkspace(t *testing.T) {
	ws, other := t.TempDir(), t.TempDir()
	write(t, ws, "d/a.json", "{}")
	write(t, other, "d/b.json", "{}")
	got := read(t, ws, fsx.Glob("d", "*.json"))
	if want := []fsx.File{{Name: "a.json", Path: "d/a.json"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("relative glob = %v, want %v", got, want)
	}
	abs := filepath.Join(other, "d")
	got = read(t, ws, fsx.Glob(abs, "*.json"))
	if want := []fsx.File{{Name: "b.json", Path: filepath.Join(abs, "b.json")}}; !reflect.DeepEqual(got, want) {
		t.Errorf("absolute glob = %v, want %v", got, want)
	}
}

func TestNoEffects(t *testing.T) {
	rt := fsx.Runtime(fsx.Config{})
	e := wf.ExternalEffect{Adapter: "fsx", Op: "write", Key: wf.KeyOf("k")}
	if _, err := rt.Apply(t.TempDir(), e); err == nil || !strings.Contains(err.Error(), "fsx.write") {
		t.Errorf("Apply err = %v", err)
	}
	if st, err := rt.Check(t.TempDir(), e); err != nil || st != wf.EffectUnknown {
		t.Errorf("Check = %v, %v", st, err)
	}
}
