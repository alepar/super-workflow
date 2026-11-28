# Contributing to super-workflow

The Go engine is in `docs/proposals/go-api`. It requires Go 1.26 or later.
The module path is `github.com/alepar/super-workflow/docs/proposals/go-api`.
The [engine guide](docs/proposals/go-api/README.md) describes its API and
the [public vision](SUPER-WORKFLOW-VISION.md) explains the design.

## Build and test

From the engine module, run the formatting, build and test checks.

```sh
gofmt -l .
go vet ./...
go test -count=1 -p 2 ./...
GOOS=darwin GOARCH=arm64 go build ./...
```

Regenerate `OUTPUT.txt` with `bash scripts/regen-output.sh` in that module
when a change affects the generated examples. The excerpt test compares
the checked-in file with fresh output.

From the repository root, run `bash scripts/test-authoring-skill.sh` and
`python3 scripts/test-package-plugin.py` for any change to a file listed
in `packaging/payload-files.txt`, the packaged skill, the plugin manifests
or the release tooling. Both tests read from the committed tree. The
[release guide](packaging/README.md) covers the staged checkers and
native install smoke.

## Contribute

Keep the engine usable across harnesses. Put workflow decisions in the
definition and reusable protocol or state handling in the engine.
Use synthetic fixtures to exercise behavior in tests. Include the
verification commands and results when handing off a change.

### Text that is parsed

- Decide whether a string is prose or data. Prose is shown to a reader and never parsed.
  Data has a type from `form` or `sink`; do not pass it as `string`.
- Never build data by concatenation, `Sprintf` or `Join`, and never recover it with `Split`,
  `Cut`, a prefix test or a regular expression. Use the type's constructor and parser. If no
  type fits, add a grammar to `form` with its parser beside its formatter; the corpus test
  covers it automatically.
- Identifiers are minted (`F1`, `W3`), never derived from a title, a location or a message.
  A reference is checked by membership in the recorded set.
- A list is a slice or a JSON array. Do not write a list into one line that code reads back.
- Paths, revisions and digests have one type per form. Convert with the named function;
  never compare an absolute path with a relative one.
- A parser returns a value, absent, or an error. Do not turn a malformed value into "none"
  or a default. Do not match on error text; return and test the typed class.
- Do not read a screen, a log line written for people, or another tool's private files to
  decide anything.
- What an agent must return is a typed JSON output checked at submission. Put the shape in
  the prompt with the generated rendering, not in your own words.
- Scripts read JSON. A script does not `grep`, `awk` or `sed` a record this project writes.
- The gate refuses a new parsing site that is not in `parsing-allowlist.txt`. Adding a line
  there needs a reason a reviewer will accept: an external tool's output parsed in its
  adapter is one; convenience is not.
- The allowlist is `docs/proposals/go-api/parsing-allowlist.txt`, checked by
  `TestParsingAllowlist` in that module (`go test -run TestParsingAllowlist -v .` prints
  its totals). A count may go down, never up without a reviewed reason; when you remove a
  site, lower or delete its line.

## Merge policy

A feature lands on main as one squash commit. Published history on main is
never rewritten. Fix forward with a new commit.
