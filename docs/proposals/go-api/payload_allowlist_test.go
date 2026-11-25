package goapi_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// payloadExcluded lists the module directories whose Go files stay out of
// the plugin payload on purpose: the deliberately invalid lint fixtures and
// the development tools.
var payloadExcluded = []string{"bad/", "tools/"}

// TestPayloadAllowlistCoversEngineSource fails when a non-test Go file of
// this module is neither in packaging/payload-files.txt nor under an
// excluded directory, so a new engine file cannot be left out of the plugin.
func TestPayloadAllowlistCoversEngineSource(t *testing.T) {
	const prefix = "docs/proposals/go-api/"
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "packaging", "payload-files.txt"))
	if os.IsNotExist(err) {
		t.Skip("not in the source repository: no packaging/payload-files.txt")
	}
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		listed[line] = true
	}
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(path)
		if d.IsDir() {
			// The go tool ignores these directories, as the package build does.
			if name := d.Name(); path != "." && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			for _, dir := range payloadExcluded {
				if rel+"/" == dir {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if !listed[prefix+rel] {
			t.Errorf("%s%s is not in packaging/payload-files.txt; add it, or exclude its directory in payloadExcluded", prefix, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
