// Package fsx is the file-system adapter: constructors return descriptors and
// Runtime serves them. It is read-only: files are produced by states as
// artifacts, so there is no write effect.
package fsx

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

// Schema names a schema declaration: the executor runs the check.
type Schema struct{ Name, Path string }

// SchemaResult is the outcome of a schema check.
type SchemaResult struct {
	OK     bool
	Errors []string
}

// Exists observes whether path exists.
func Exists(path string) wf.Observation[bool] {
	return wf.Observe[bool]("fsx", "exists", map[string]any{"path": path})
}

// Hash observes the sha256 of the file at path, hex-encoded.
func Hash(path string) wf.Observation[string] {
	return wf.Observe[string]("fsx", "hash", map[string]any{"path": path})
}

// ReadJSON observes the JSON value at pointer (RFC 6901; "" is the whole
// document) in the file at path, decoded into T.
func ReadJSON[T any](path, pointer string) wf.Observation[T] {
	return wf.Observe[T]("fsx", "read_json", map[string]any{"path": path, "pointer": pointer})
}

// SchemaCheck declares a check of the file at path against s. It is a
// declaration only; the executor runs the check and the definition never
// validates in a closure.
func SchemaCheck(path string, s Schema) wf.Observation[SchemaResult] {
	return wf.Observe[SchemaResult]("fsx", "schema_check", map[string]any{"path": path, "schema": s})
}

// File is one file a Glob lists: Name is its base name (the item key a Map
// over the glob uses) and Path is dir joined with Name, as declared.
type File struct{ Name, Path string }

// Glob observes the regular files directly in dir whose base name matches
// pattern (filepath.Match syntax), one item per file, sorted by name. An
// absent dir is an empty set, so a Map over it with Requery gains a child for
// each file that appears.
func Glob(dir, pattern string) wf.Observation[[]File] {
	return wf.Observe[[]File]("fsx", "glob", map[string]any{"dir": dir, "pattern": pattern})
}
