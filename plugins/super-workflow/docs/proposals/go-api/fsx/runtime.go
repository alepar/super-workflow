package fsx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Config configures the fsx runtime. It has no options yet.
type Config struct{}

// Runtime returns the runtime serving the fsx observations. Relative paths
// resolve against the workspace of each call. The file system is always there,
// so nothing here is ever classified unavailable.
func Runtime(Config) wf.AdapterRuntime { return runtime{} }

type runtime struct{}

func (runtime) Ops() []string {
	return []string{"fsx.exists", "fsx.hash", "fsx.read_json", "fsx.schema_check", "fsx.glob"}
}

// Apply fails: fsx declares no write effect.
func (runtime) Apply(_ string, e wf.ExternalEffect) (json.RawMessage, error) {
	return nil, fmt.Errorf("fsx.%s: fsx has no effects", e.Op)
}

// Check answers unknown: fsx has no effects to have completed.
func (runtime) Check(string, wf.ExternalEffect) (wf.EffectStatus, error) {
	return wf.EffectUnknown, nil
}

func (runtime) Read(workspace string, d wf.ObservationDesc) (json.RawMessage, error) {
	var a struct {
		Path, Pointer string
		Schema        Schema
		Dir, Pattern  string
	}
	if err := decodeArgs(d.Args, &a); err != nil {
		return nil, fmt.Errorf("fsx.%s: %w", d.Op, err)
	}
	if d.Op == "glob" {
		return glob(workspace, a.Dir, a.Pattern)
	}
	path := resolve(workspace, a.Path)
	var (
		v   any
		err error
	)
	switch d.Op {
	case "exists":
		_, serr := os.Stat(path)
		if serr != nil && !errors.Is(serr, fs.ErrNotExist) {
			return nil, fmt.Errorf("fsx.exists %s: %w", a.Path, serr)
		}
		v = serr == nil
	case "hash":
		var b []byte
		if b, err = os.ReadFile(path); err == nil {
			sum := sha256.Sum256(b)
			v = hex.EncodeToString(sum[:])
		}
	case "read_json":
		var b []byte
		if b, err = os.ReadFile(path); err == nil {
			return pointer(b, a.Pointer)
		}
	case "schema_check":
		v, err = schemaCheck(workspace, path, a.Schema)
	default:
		return nil, fmt.Errorf("fsx.%s: unknown op", d.Op)
	}
	if err != nil {
		return nil, fmt.Errorf("fsx.%s %s: %w", d.Op, a.Path, err)
	}
	return json.Marshal(v)
}

// glob lists the regular files directly in dir whose base name matches
// pattern. An absent dir is an empty list, never null. Path keeps dir as
// declared so the journaled value does not embed the workspace.
func glob(workspace, dir, pattern string) (json.RawMessage, error) {
	if _, err := filepath.Match(pattern, ""); err != nil {
		return nil, fmt.Errorf("fsx.glob %s: %w", pattern, err)
	}
	entries, err := os.ReadDir(resolve(workspace, dir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("fsx.glob %s: %w", dir, err)
	}
	files := []File{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ok, _ := filepath.Match(pattern, e.Name()); ok {
			files = append(files, File{Name: e.Name(), Path: filepath.Join(dir, e.Name())})
		}
	}
	return json.Marshal(files)
}

// decodeArgs decodes the generic JSON args the Engine binds into a local struct.
func decodeArgs(args map[string]any, into any) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

func resolve(workspace, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workspace, p)
}

// pointer returns the value at the RFC 6901 pointer in the JSON document doc.
func pointer(doc []byte, ptr string) (json.RawMessage, error) {
	cur := json.RawMessage(doc)
	if err := json.Unmarshal(doc, new(any)); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if ptr == "" {
		return json.RawMessage(strings.TrimSpace(string(cur))), nil
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, fmt.Errorf("pointer %q does not start with /", ptr)
	}
	for _, tok := range strings.Split(ptr[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		var obj map[string]json.RawMessage
		var arr []json.RawMessage
		switch {
		case json.Unmarshal(cur, &obj) == nil && obj != nil:
			next, ok := obj[tok]
			if !ok {
				return nil, fmt.Errorf("pointer %q: no member %q", ptr, tok)
			}
			cur = next
		case json.Unmarshal(cur, &arr) == nil && arr != nil:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || strconv.Itoa(i) != tok || i >= len(arr) {
				return nil, fmt.Errorf("pointer %q: no element %q", ptr, tok)
			}
			cur = arr[i]
		default:
			return nil, fmt.Errorf("pointer %q: cannot descend into a scalar at %q", ptr, tok)
		}
	}
	return cur, nil
}

// schemaCheck validates the data file at path against the schema file. A data
// file that is missing or not JSON is a failed check (data), a schema file
// that cannot be read is a failed read.
func schemaCheck(workspace, path string, s Schema) (SchemaResult, error) {
	sb, err := os.ReadFile(resolve(workspace, s.Path))
	if err != nil {
		return SchemaResult{}, fmt.Errorf("schema %s: %w", s.Path, err)
	}
	var schema any
	if err := json.Unmarshal(sb, &schema); err != nil {
		return SchemaResult{}, fmt.Errorf("schema %s: %w", s.Path, err)
	}
	db, err := os.ReadFile(path)
	if err != nil {
		return failed(fmt.Sprintf("(root): cannot read data: %v", err)), nil
	}
	var data any
	if err := json.Unmarshal(db, &data); err != nil {
		return failed(fmt.Sprintf("(root): data is not JSON: %v", err)), nil
	}
	var errs []string
	validate(schema, data, "", &errs)
	sort.Strings(errs)
	return SchemaResult{OK: len(errs) == 0, Errors: errs}, nil
}

func failed(msg string) SchemaResult { return SchemaResult{Errors: []string{msg}} }

// validate checks v against the schema subset type, properties, required,
// items and enum; any other keyword is ignored.
func validate(schema, v any, path string, errs *[]string) {
	sc, ok := schema.(map[string]any)
	if !ok {
		return
	}
	at := path
	if at == "" {
		at = "(root)"
	}
	add := func(format string, args ...any) { *errs = append(*errs, at+": "+fmt.Sprintf(format, args...)) }

	if t, ok := sc["type"]; ok {
		var want []string
		switch t := t.(type) {
		case string:
			want = []string{t}
		case []any:
			for _, x := range t {
				if s, ok := x.(string); ok {
					want = append(want, s)
				}
			}
		}
		if len(want) > 0 && !anyType(want, v) {
			add("want type %s, got %s", strings.Join(want, " or "), jsonType(v))
		}
	}
	if e, ok := sc["enum"].([]any); ok {
		found := false
		for _, x := range e {
			if reflect.DeepEqual(x, v) {
				found = true
			}
		}
		if !found {
			add("value not in enum")
		}
	}
	if obj, ok := v.(map[string]any); ok {
		if req, ok := sc["required"].([]any); ok {
			for _, r := range req {
				if name, ok := r.(string); ok {
					if _, present := obj[name]; !present {
						add("missing required property %q", name)
					}
				}
			}
		}
		if props, ok := sc["properties"].(map[string]any); ok {
			for name, sub := range props {
				if child, present := obj[name]; present {
					validate(sub, child, path+"/"+escape(name), errs)
				}
			}
		}
	}
	if arr, ok := v.([]any); ok {
		if items, ok := sc["items"]; ok {
			for i, el := range arr {
				validate(items, el, path+"/"+strconv.Itoa(i), errs)
			}
		}
	}
}

func escape(tok string) string {
	return strings.ReplaceAll(strings.ReplaceAll(tok, "~", "~0"), "/", "~1")
}

func anyType(want []string, v any) bool {
	for _, w := range want {
		if w == jsonType(v) || w == "integer" && isInteger(v) {
			return true
		}
	}
	return false
}

func isInteger(v any) bool {
	f, ok := v.(float64)
	return ok && f == float64(int64(f))
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return "object"
	}
}
