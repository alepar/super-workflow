# Stage and install the framework plugin

Use a source checkout, Python 3.11 or newer, and Git.
The v0.1 harness floors are Claude Code 2.1.331 and Codex CLI 0.171.0 on
Linux and macOS ARM. To compile workflows, install Go 1.26 or newer.

From the checkout, stage a committed release into a new directory. Retain
the marketplace directory at its absolute path while it is registered.
Use the same source commit and version for both harnesses.

```sh
python3 scripts/package-plugin.py --ref HEAD --version 0.1.0 --output "$PWD/plugin-release"
marketplace="$PWD/plugin-release/marketplace"
```

### Claude Code

```sh
claude plugin marketplace add "$marketplace"
claude plugin install super-workflow@super-workflow-framework
```

In a fresh session, invoke `/super-workflow:super-workflow` or ask to author a
super-workflow definition.

### Codex

```sh
codex plugin marketplace add "$marketplace"
codex plugin add super-workflow@super-workflow-framework
```

In a fresh session, invoke `$super-workflow` or ask to author a super-workflow
definition. The rootless Codex ZIP is a transport artifact for the plugin
payload. The install commands above use the unpacked marketplace.

## Validate the release

`python3 scripts/test-package-plugin.py <source-commit>` checks both catalogs,
the exact staged allowlist, the reproducible Codex ZIP, and release pinning.
To exercise the packaged fixture data, run these from
`marketplace/plugins/super-workflow/docs/proposals/go-api`, with
`WF_HARNESS_FIXTURES_DIR` unset and a writable Go cache.

```sh
GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOPROXY=off go run ./examples/tour/cmd/workflow check tour
GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOPROXY=off go run ./cmd/wfcheck check feature
```

The committed [allowlist](payload-files.txt) contains the runtime engine source,
the author's reference files, and the synthetic child protocol fixtures used by both development checkers.
`release.json` records the version, the digest of the canonical payload file
map, and individual file digests. The skill explains how to verify and retain
the exact payload.

For the install and recovery smoke, give the script an empty disposable
profile directory. It installs the staged plugin, builds a one-step workflow
from the installed payload, removes the plugin and marketplace, verifies the
profile's plugin settings, then resumes and rebuilds from the relocated pin.

```sh
python3 scripts/smoke-plugin.py claude --profile "$profile" --marketplace "$marketplace" --workdir "$scratch/claude-run"
python3 scripts/smoke-plugin.py codex --profile "$profile" --marketplace "$marketplace" --workdir "$scratch/codex-run"
```

Use `--live` with a profile authorized for the test and already signed in.
The script starts a fresh coordinator session and checks the artifact
delivered by its native worker. It removes the plugin and marketplace even
when the run fails, and reports the before and after plugin settings hashes
and structural changes. It leaves the run and log in the requested workdir
for inspection.

To verify an upgrade in a disposable profile, stage a higher version and
replace the registered local marketplace tree with the new one at the same
path. Run `claude plugin marketplace update super-workflow-framework` and
`claude plugin update super-workflow@super-workflow-framework`, or run
`codex plugin add super-workflow@super-workflow-framework` again. Check the
new version in `plugin list` before opening a fresh session.

The live smoke completes its native run before it removes the plugin. The
disposable recovery case stops the run, removes the plugin, then resumes the
journal and rebuilds from the relocated release pin.
