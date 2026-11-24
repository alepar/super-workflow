# super-workflow

Super-workflow is a Go framework for agent tasks that benefit from explicit
steps, independent workers, or recovery after interruption. You define the
workflow as a graph. The compiled workflow exposes a CLI that tells an
orchestrating agent what to do next and records accepted results in a journal
so a later session can resume the run.

## Install the plugin

Install with Claude Code 2.1.331 or later or Codex CLI 0.171.0 or later on
Linux or macOS ARM. The repository is a marketplace for both harnesses, so
you can add it straight from GitHub. It installs the release that the catalogs
on `main` pin.

Claude Code:

```sh
claude plugin marketplace add alepar/super-workflow
claude plugin install super-workflow@super-workflow-framework
```

Codex:

```sh
codex plugin marketplace add alepar/super-workflow
codex plugin add super-workflow@super-workflow-framework
```

Open a fresh session and invoke `/super-workflow:super-workflow` in Claude
Code or `$super-workflow` in Codex.

To install from a release instead, unpack the marketplace release and set
`marketplace` to the absolute path of its `marketplace` directory, which
contains both catalogs. Keep it at that path while registered.

```sh
claude plugin marketplace add "$marketplace"
claude plugin install super-workflow@super-workflow-framework
codex plugin marketplace add "$marketplace"
codex plugin add super-workflow@super-workflow-framework
```

The rootless Codex ZIP contains the plugin payload. Use the unpacked
marketplace for the CLI installation.

## Run a one-off workflow

1. Use the authoring skill to write a `main.go` with the workflow graph defined in code.
2. Use Go 1.26 or later to build the binary against the bundled engine module.
3. Hand the binary to an orchestrating agent session to start it with `run`.
4. The orchestrator calls `next` to get assignments. Workers report their results with `done`.
5. The orchestrator follows `next` until the workflow ends and reports the result.

The plugin's `docs/proposals/go-api/README.md` contains a five-minute tour
and runnable examples. In a source checkout, see the same path for the API
guide and `packaging/README.md` for release tooling.

## License

Licensed under either of Apache License, Version 2.0 (`LICENSE-APACHE`) or
MIT license (`LICENSE-MIT`), at your option (SPDX: `MIT OR Apache-2.0`).

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in this work by you, as defined in the Apache-2.0 license, shall be dual licensed as above, without any additional terms or conditions.
