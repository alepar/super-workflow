package goapi_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The parsing allowlist: every call that splits, cuts, prefix-tests or
// pattern-matches text, and every conversion into a named string type that
// should have a constructor, outside the packages whose job is formats
// (form, sink). Each permitted site is a line of parsing-allowlist.txt with
// a reason. A new site, or a higher count, fails until a reviewed line
// permits it; a lower count fails until the line is lowered, so the list
// only shrinks by edits a reviewer sees.

const allowlistFile = "parsing-allowlist.txt"

// unclassifiedReason marks a line seeded from the parsing audit's inventory
// whose site has not been judged yet.
const unclassifiedReason = "seed, unclassified"

// parsingExempt are the module directories the gate does not scan: the
// format packages, the deliberately invalid lint fixtures, the development
// tools and the test-only git isolation.
var parsingExempt = []string{"form/", "sink/", "bad/", "tools/", "internal/testgit/"}

// watchedCalls are the parsing functions, by import path.
var watchedCalls = map[string][]string{
	"strings": {"Split", "SplitN", "SplitAfter", "SplitAfterN", "SplitSeq", "SplitAfterSeq", "Cut", "CutPrefix", "CutSuffix", "Fields", "FieldsFunc", "FieldsSeq", "FieldsFuncSeq", "Join", "TrimPrefix", "TrimSuffix", "HasPrefix", "HasSuffix", "Index", "LastIndex", "IndexByte", "LastIndexByte", "IndexAny", "LastIndexAny", "IndexRune", "IndexFunc", "LastIndexFunc"},
	"bytes":   {"Split", "SplitN", "SplitAfter", "SplitAfterN", "SplitSeq", "Cut", "CutPrefix", "CutSuffix", "Fields", "FieldsFunc", "Join", "TrimPrefix", "TrimSuffix", "HasPrefix", "HasSuffix", "Index", "LastIndex", "IndexByte", "LastIndexByte", "IndexAny", "IndexRune", "IndexFunc"},
	"regexp":  {"Compile", "MustCompile", "CompilePOSIX", "MustCompilePOSIX", "Match", "MatchString", "MatchReader"},
	"fmt":     {"Sscan", "Sscanf", "Sscanln"},
}

// typedText are the named string types without a constructor, by package
// directory, with the file that defines each. A conversion T(x) outside
// that file is a site.
var typedText = map[string]map[string]string{
	"wf": {"RunID": "wf/executor.go", "RunPath": "wf/run_path.go"},
	"bd": {"Status": "bd/bd.go"},
}

const modulePath = "github.com/alepar/super-workflow/docs/proposals/go-api/"

type siteKey struct{ kind, file, fn, callee string }

func (k siteKey) String() string { return k.kind + "\t" + k.file + "\t" + k.fn + "\t" + k.callee }

func TestParsingAllowlist(t *testing.T) {
	found := scanParsingSites(t)
	allowed, reasons := readAllowlist(t)
	calls, unclassified := 0, 0
	for _, n := range found {
		calls += n
	}
	for _, r := range reasons {
		if r == unclassifiedReason {
			unclassified++
		}
	}
	t.Logf("parsing allowlist: %d sites under %d lines, %d lines %q", calls, len(found), unclassified, unclassifiedReason)

	var problems []string
	for k, n := range found {
		want, ok := allowed[k]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("new parsing site: use a form or sink type, or add this line with a reason a reviewer will accept:\n\t%s\t%d\t<reason>", k, n))
		case n > want:
			problems = append(problems, fmt.Sprintf("%s: %d sites, the allowlist permits %d; use a form or sink type, or raise the count with a reviewed reason", k, n, want))
		case n < want:
			problems = append(problems, fmt.Sprintf("%s: %d sites, the allowlist says %d; lower the count to %d (the list only shrinks)", k, n, want, n))
		}
	}
	for k := range allowed {
		if _, ok := found[k]; !ok {
			problems = append(problems, fmt.Sprintf("%s: no such site any more; delete the line", k))
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// scanParsingSites counts each watched call and typed-text conversion by
// file, enclosing top-level declaration and callee.
func scanParsingSites(t *testing.T) map[siteKey]int {
	t.Helper()
	watched := map[string]map[string]bool{}
	for pkg, names := range watchedCalls {
		watched[pkg] = map[string]bool{}
		for _, n := range names {
			watched[pkg][n] = true
		}
	}
	found := map[siteKey]int{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(path)
		if d.IsDir() {
			if name := d.Name(); path != "." && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			for _, dir := range parsingExempt {
				if rel+"/" == dir {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		imports := map[string]string{}
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			name := p[strings.LastIndex(p, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			imports[name] = p
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		for _, decl := range f.Decls {
			fn := declName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := c.Fun.(type) {
				case *ast.SelectorExpr:
					x, ok := fun.X.(*ast.Ident)
					if !ok {
						return true
					}
					pkg := imports[x.Name]
					if watched[pkg][fun.Sel.Name] {
						found[siteKey{"call", rel, fn, pkg + "." + fun.Sel.Name}]++
					}
					if def, ok := typedText[strings.TrimPrefix(pkg, modulePath)][fun.Sel.Name]; ok && strings.HasPrefix(pkg, modulePath) && def != rel && len(c.Args) == 1 {
						found[siteKey{"conv", rel, fn, strings.TrimPrefix(pkg, modulePath) + "." + fun.Sel.Name}]++
					}
				case *ast.Ident:
					if def, ok := typedText[dir][fun.Name]; ok && def != rel && len(c.Args) == 1 {
						found[siteKey{"conv", rel, fn, dir + "." + fun.Name}]++
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// declName names a top-level declaration: Func, Recv.Method, or "-" for the
// file's package-level variables and constants.
func declName(decl ast.Decl) string {
	fd, ok := decl.(*ast.FuncDecl)
	if !ok {
		return "-"
	}
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	for {
		switch r := recv.(type) {
		case *ast.StarExpr:
			recv = r.X
			continue
		case *ast.IndexExpr:
			recv = r.X
			continue
		case *ast.IndexListExpr:
			recv = r.X
			continue
		case *ast.Ident:
			return r.Name + "." + fd.Name.Name
		}
		return fd.Name.Name
	}
}

// readAllowlist reads parsing-allowlist.txt: tab-separated kind, file,
// declaration, callee, count and reason; '#' starts a comment line.
func readAllowlist(t *testing.T) (map[siteKey]int, map[siteKey]string) {
	t.Helper()
	data, err := os.ReadFile(allowlistFile)
	if err != nil {
		t.Fatal(err)
	}
	allowed, reasons := map[siteKey]int{}, map[siteKey]string{}
	for i, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			t.Errorf("%s:%d: want 6 tab-separated fields (kind, file, declaration, callee, count, reason), got %d", allowlistFile, i+1, len(f))
			continue
		}
		k := siteKey{f[0], f[1], f[2], f[3]}
		n, err := strconv.Atoi(f[4])
		if err != nil || n < 1 {
			t.Errorf("%s:%d: count %q is not a positive integer", allowlistFile, i+1, f[4])
			continue
		}
		if reason := strings.TrimSpace(f[5]); reason == "" || strings.HasPrefix(reason, "<") {
			t.Errorf("%s:%d: %s has no reason", allowlistFile, i+1, k)
		}
		if _, dup := allowed[k]; dup {
			t.Errorf("%s:%d: %s is listed twice", allowlistFile, i+1, k)
		}
		allowed[k], reasons[k] = n, f[5]
	}
	return allowed, reasons
}
