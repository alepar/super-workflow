#!/usr/bin/env python3
"""Exercise plugin install, pinning, and recovery in one explicit profile."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tomllib

NAME = "super-workflow"
MARKET = "super-workflow-framework"
PLUGIN = f"{NAME}@{MARKET}"
ENGINE = "github.com/alepar/super-workflow/docs/proposals/go-api"
ROOT = Path(__file__).resolve().parents[1]


def load(path):
    if path.is_file():
        data = path.read_text()
        return tomllib.loads(data) if path.suffix == ".toml" else json.loads(data)
    return {}


def profile_state(harness, profile):
    if harness == "codex":
        settings = load(profile / "config.toml")
        return {key: settings.get(key, {}) for key in ("plugins", "marketplaces")}
    settings = load(profile / "settings.json")
    return {
        "enabledPlugins": settings.get("enabledPlugins", {}),
        "extraKnownMarketplaces": settings.get("extraKnownMarketplaces", {}),
        "installedPlugins": load(profile / "plugins/installed_plugins.json").get("plugins", {}),
        "knownMarketplaces": load(profile / "plugins/known_marketplaces.json"),
    }


def audit(before, after):
    def digest(data):
        return hashlib.sha256(json.dumps(data, sort_keys=True, default=str).encode()).hexdigest()

    for key in sorted(before):
        old, new = before[key], after[key]
        added = len(new.keys() - old.keys())
        removed = len(old.keys() - new.keys())
        updated = sum(old[item] != new[item] for item in old.keys() & new.keys())
        print(f"{key}: {added} added, {removed} removed, {updated} changed")
    changed = sorted(key for key in before if before[key] != after[key])
    print(f"Plugin/marketplace SHA-256 before {digest(before)}", flush=True)
    print(f"Plugin/marketplace SHA-256 after  {digest(after)}", flush=True)
    print(f"Changed settings sections: {', '.join(changed) if changed else 'none'}", flush=True)
    if changed:
        raise RuntimeError("plugin and marketplace settings differ after removal")


def process(args, env, cwd, *, timeout=60, check=True):
    with subprocess.Popen(args, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                          start_new_session=True) as child:
        try:
            stdout, stderr = child.communicate(timeout=timeout)
        except BaseException:
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            child.communicate()
            raise
        if check and child.returncode:
            raise RuntimeError(f"{Path(args[0]).name} {args[1]} exited {child.returncode}")
    return stdout, stderr, child.returncode


def checked_version(root, env, work, snapshot=None):
    verify = root / "skills/super-workflow/verify-release.py"
    command = [sys.executable, verify]
    if snapshot is not None:
        command += ["--snapshot", str(snapshot)]
    result, _, _ = process(command, env, work)
    return json.loads(result)


def build(work, env, identity):
    go_env = dict(env, GOWORK="off", GOTOOLCHAIN="local", GOPROXY="off",
                  GOFLAGS="-mod=readonly -p=2", GOMAXPROCS="4", GOCACHE=str(work.parent / "go-cache"),
                  GOTMPDIR=str(work.parent / "go-tmp"))
    (work.parent / "go-tmp").mkdir(exist_ok=True)
    binary = work / "workflow"
    source = work / "src"
    if not source.exists():
        source.mkdir()
        shutil.copyfile(ROOT / "packaging/smoke-workflow.go", source / "main.go")
        process(["go", "mod", "init", "example.com/plugin-smoke"], go_env, source)
    engine_go = next(line.split()[1] for line in (Path(identity["module"]) / "go.mod").read_text().splitlines()
                     if line.startswith("go "))
    process(["go", "mod", "edit", f"-go={engine_go}", f"-require={ENGINE}@{identity['version']}",
             f"-replace={ENGINE}={identity['module']}"], go_env, source)
    process(["nice", "-n", "19", "go", "build", "-o", str(binary), "."],
            go_env, source, timeout=240)
    return binary


def drive(harness, env, work, bootstrap, identity):
    prompt = (
        f"Use the installed super-workflow skill to drive an authorized plugin smoke run. "
        f"Read the loaded skill and verify release {identity['version']} "
        f"by payload SHA-256 {identity['payload_sha256']} using its installed path. The workflow is already started "
        f"in {work}. Launch its one worker using the native subagent tool and the brief "
        "from the engine, and wait on the same next invocation if it blocks. "
        "Only the native worker may write the receipt or report done. "
        "Use the workflow commands and paths within this workspace. Report the final "
        "result once next ends.\n\n" + bootstrap
    )
    if harness == "claude":
        cmd = ["claude", "--print", "--strict-mcp-config", "--tools", "Agent,Bash,Read,Write",
               "--allowedTools", "Agent,Bash,Read,Write", "--", prompt]
    else:
        cmd = ["codex", "exec", "--skip-git-repo-check", "-s", "workspace-write", "--", prompt]
    stdout, stderr, code = process(cmd, env, work, timeout=420, check=False)
    (work / "coordinator.log").write_text(stdout + stderr)
    if code:
        raise RuntimeError(f"fresh {harness} coordinator exited {code}")


def smoke(args):
    work = args.workdir.resolve()
    profile = args.profile.resolve(strict=True)
    market = args.marketplace.resolve(strict=True)
    if work.is_relative_to(profile) or work.is_relative_to(market):
        raise ValueError("workdir must be outside the profile and marketplace")
    work.mkdir()
    env = {key: value for key, value in os.environ.items() if not key.startswith("HERDR_")}
    for marker in ("CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID",
                   "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID", "PI_SESSION_ID",
                   "HC_TERMINAL_ID"):
        env.pop(marker, None)
    env[{"claude": "CLAUDE_CONFIG_DIR", "codex": "CODEX_HOME"}[args.harness]] = str(profile)

    def plugin(*command, check=True):
        return process([args.harness, "plugin", *command], env, work, check=check)

    before = profile_state(args.harness, profile)
    if any(MARKET in key for section in before.values() for key in section):
        raise RuntimeError("smoke marketplace or plugin already exists in this profile")
    plugin_root = market / "plugins" / NAME
    expected = checked_version(plugin_root, env, work)
    install = "install" if args.harness == "claude" else "add"
    uninstall = "uninstall" if args.harness == "claude" else "remove"
    market_registered = False
    try:
        plugin("marketplace", "add", str(market))
        market_registered = True
        plugin(install, PLUGIN)
        if args.harness == "codex":
            plugin_root = profile / "plugins/cache" / MARKET / NAME / expected["version"][1:]
        saved = checked_version(plugin_root, env, work, work / "pinned-release")
        if saved["payload_sha256"] != expected["payload_sha256"] or saved["version"] != expected["version"]:
            raise RuntimeError("installed release identity differs from staged release")
        binary = build(work, env, saved)

        def wf(verb, *command):
            return process([str(binary), verb, *command, "--state-dir", str(work / "state")],
                           env, work)

        run_id = "plugin-smoke"
        stdout, bootstrap, _ = wf("run", "plugin-smoke", "--run-id", run_id,
                                  "--workspace", str(work), "--harness", args.harness)
        if stdout.strip() != run_id:
            raise RuntimeError("unexpected smoke run ID")
        if args.live:
            drive(args.harness, env, work, bootstrap, saved)
            status, _, _ = wf("status", run_id, "--json")
            result = json.loads(status)
            receipts = list((work / "state/runs" / run_id / "records/artifacts").glob("*/receipt.txt"))
            units = result.get("Units", [])
            if (result["Outcome"] != "success"
                    or len(units) != 1 or units[0]["State"] != "done"
                    or len(receipts) != 1 or receipts[0].read_text().strip() != "native plugin smoke passed"):
                raise RuntimeError("native child did not deliver the expected artifact")
        else:
            wf("stop", run_id, "--reason", "test recovery after removing the installed release")
    finally:
        try:
            if market_registered:
                plugin(uninstall, PLUGIN, check=False)
        finally:
            try:
                plugin("marketplace", "remove", MARKET, check=False)
            finally:
                audit(before, profile_state(args.harness, profile))

    pinned = work / "pinned-release"
    restored = work / "relocated release"
    pinned.rename(restored)
    saved = checked_version(restored, env, work)
    if not args.live:
        wf("resume", run_id, "--operator", "smoke", "--reason", "profile restored")
    prior, _, _ = wf("status", run_id, "--json")
    build(work, env, saved)
    after, _, _ = wf("status", run_id, "--json")
    if any(json.loads(prior)[key] != json.loads(after)[key]
           for key in ("Phase", "Outcome", "Terminal")):
        raise RuntimeError("run status changed after rebuilding from the same release")
    print(f"{args.harness}: release pin, removal, relocation and rebuild passed"
          + (", including native child success" if args.live else ", including operator resume"))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("harness", choices=("claude", "codex"))
    parser.add_argument("--profile", type=Path, required=True, help="authorized profile directory")
    parser.add_argument("--marketplace", type=Path, required=True)
    parser.add_argument("--workdir", type=Path, required=True, help="new writable run directory")
    parser.add_argument("--live", action="store_true", help="run a fresh authenticated coordinator session")
    args = parser.parse_args()

    def interrupted(number, _frame):
        raise KeyboardInterrupt(f"received signal {number}")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGHUP, interrupted)
    smoke(args)


if __name__ == "__main__":
    main()
