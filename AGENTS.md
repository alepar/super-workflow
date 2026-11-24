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
`python3 scripts/test-package-plugin.py` for changes to the packaged skill
or release tooling. Both tests read from the committed tree. The
[release guide](packaging/README.md) covers the staged checkers and
native install smoke.

## Contribute

Keep the engine usable across harnesses. Put workflow decisions in the
definition and reusable protocol or state handling in the engine.
Use synthetic fixtures to exercise behavior in tests. Include the
verification commands and results when handing off a change.

## Merge policy

A feature lands on main as one squash commit. Published history on main is
never rewritten. Fix forward with a new commit.
