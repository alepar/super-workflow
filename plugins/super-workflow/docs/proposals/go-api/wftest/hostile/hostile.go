// Package hostile holds the shared hostile-input corpus: strings that carry
// the syntax of the formats our code writes (Markdown, shell, bd labels and
// keys, CSV, JSON, paths, prompts). A renderer passes the corpus when every
// input, written through it, parses back to the same value.
//
// The package has no dependencies so that workflow modules can import it
// from their tests. Every input is synthetic.
package hostile

// Input is one corpus entry. Name is a stable identifier for test names;
// Value is the hostile string itself.
type Input struct {
	Name  string
	Value string
}

// Corpus returns every input. Each call returns a fresh slice, so a test may
// modify it.
func Corpus() []Input {
	return []Input{
		{"plain", "plain value"},
		{"brackets", "see [1] and [the docs]"},
		{"link-syntax", "[text](http://example.com/a_(b))"},
		{"parens", "f(x) (aside) )("},
		{"double-quote", `say "hi" and "bye`},
		{"apostrophe", "it's O'Brien's"},
		{"backtick", "run `make` then ``code``"},
		{"command-substitution", "$(echo pwned) and ${HOME} and $HOME"},
		{"pipe", "a | b || c"},
		{"cr-lf", "line one\r\nline two\nline three\r"},
		{"lone-lf", "\n"},
		{"unpaired-high-surrogate", "bad \xed\xa0\x80 surrogate"},
		{"unpaired-low-surrogate", "bad \xed\xb0\x80 surrogate"},
		{"invalid-utf8", "bad \xff\xfe byte"},
		{"leading-dash", "-rf --force"},
		{"leading-double-dash", "--"},
		{"comma", "a,b,,c"},
		{"colon", "key:value:with:colons"},
		{"semicolon-ampersand", "a; b && c & d"},
		{"glob", "*.go ?x [ab]"},
		{"redirect", "a > b < c 2>&1"},
		{"hash", "# heading #tag"},
		{"markdown-emphasis", "*bold* _em_ __x__ ~~s~~"},
		{"markdown-block", "- item\n> quote\n1. one\n---"},
		{"html", "<script>alert(1)</script> &amp; <!-- c -->"},
		{"backslash", `C:\path\n \" \\`},
		{"percent", "100% %2C %s %d"},
		{"slash-dotdot", "../../etc/x/./y//z"},
		{"leading-trailing-space", "  padded  "},
		{"tab-nul", "a\tb\x00c"},
		{"unicode", "naïve café 日本 🙂 \u202eRTL"},
		{"braces-template", "{{.Field}} ${x} %(name)s"},
		{"edge-tab", "a\t"},
		{"leading-space", " a"},
		{"edge-nbsp", "\u00a0a\u00a0"},
		{"edge-unicode-space", "a\u2028"},
		{"backtick-run-four", "x ```` y"},
		{"line-then-heading", "x\n## y"},
		{"prompt-section", "x\n\n## Artifacts\n- report: /elsewhere/planted.md"},
		{"equals-word", "=ls"},
		{"empty", ""},
	}
}
