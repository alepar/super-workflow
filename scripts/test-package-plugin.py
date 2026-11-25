#!/usr/bin/env python3
"""Verify that the native catalogs and ZIP agree on the release payload."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import zipfile

repo = Path(__file__).resolve().parents[1]
ref = sys.argv[1] if len(sys.argv) == 2 else "HEAD"
with tempfile.TemporaryDirectory(prefix=".test-package-", dir=repo) as scratch:
    output = Path(scratch) / "release"
    repeat = Path(scratch) / "repeat"
    for dest in (output, repeat):
        subprocess.run([sys.executable, repo / "scripts/package-plugin.py",
                        "--ref", ref, "--output", dest], check=True)
    market = output / "marketplace"
    location = "./plugins/super-workflow"
    plugin = market / location
    claude = json.loads((market / ".claude-plugin/marketplace.json").read_text())
    codex = json.loads((market / ".agents/plugins/marketplace.json").read_text())
    assert claude["name"] == codex["name"] == "super-workflow-framework"
    assert claude["plugins"][0]["source"] == location
    assert codex["plugins"][0]["source"] == {"source": "local", "path": location}
    release = json.loads((plugin / "release.json").read_text())
    # The staged manifests and both transports must omit workshop history.
    commit = subprocess.check_output(["git", "rev-parse", f"{ref}^{{commit}}"],
                                     cwd=repo, text=True).strip()
    assert set(release) == {"name", "version", "payload_sha256", "files"}
    assert release["payload_sha256"] == hashlib.sha256(
        json.dumps(release["files"], sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    assert (plugin / "README.md").is_file()
    for path in market.rglob("*"):
        if path.is_file():
            assert commit.encode() not in path.read_bytes(), path
    files = set(release["files"])
    # Non-Go engine files the allowlist completeness test does not walk.
    assert "docs/proposals/go-api/opencode/super-workflow.js" in files
    assert not any(f.endswith("_test.go") and "/examples/tour/" not in f for f in files)
    for file in files:
        if "/harness/testdata/" in file:
            assert (plugin / file).stat().st_size < 1200, file

    expected = subprocess.check_output(
        ["git", "show", f"{ref}:packaging/payload-files.txt"], cwd=repo, text=True)
    assert files == set(expected.splitlines())
    assert {p.relative_to(plugin).as_posix() for p in plugin.rglob("*") if p.is_file()} == files | {
        ".claude-plugin/plugin.json", ".codex-plugin/plugin.json", "release.json"}
    assert json.loads((plugin / ".claude-plugin/plugin.json").read_text())["version"] == release["version"]
    if not os.environ.get("SW_PACKAGE_TEST_NESTED"):
        # The staged engine, shipped tests included, must compile offline from the payload alone.
        go_env = {k: v for k, v in os.environ.items() if k != "WF_HARNESS_FIXTURES_DIR"}
        go_env.update(GOWORK="off", GOTOOLCHAIN="local", GOFLAGS="-mod=readonly", GOPROXY="off")
        module = plugin / "docs/proposals/go-api"
        subprocess.run(["go", "vet", "./..."], cwd=module, env=go_env, check=True)
        subprocess.run(["go", "run", "./cmd/wfcheck", "check", "feature"], cwd=module, env=go_env,
                       check=True, stdout=subprocess.DEVNULL)
    zip_path = output / f'super-workflow-{release["version"]}-codex.zip'
    assert zip_path.read_bytes() == (repeat / zip_path.name).read_bytes()
    with zipfile.ZipFile(zip_path) as archive:
        assert set(archive.namelist()) == files | {".codex-plugin/plugin.json", "release.json"}
        assert json.loads(archive.read(".codex-plugin/plugin.json"))["version"] == release["version"]
        for file, digest in release["files"].items():
            content = (plugin / file).read_bytes()
            assert content == archive.read(file)
            assert hashlib.sha256(content).hexdigest() == digest
    verify = plugin / "skills/super-workflow/verify-release.py"
    snapshot = Path(scratch) / "saved-release"
    # The root catalogs install the staged tree from its orphan release tag.
    clone = Path(scratch) / "clone"
    subprocess.run(["git", "clone", "-q", "--shared", "--no-tags", "--no-checkout", repo, clone], check=True)
    (clone / "scripts").mkdir()
    (clone / "scripts/package-plugin.py").write_bytes((repo / "scripts/package-plugin.py").read_bytes())
    for mode in ("--catalogs", "--publish-ref", "--publish-ref"):
        subprocess.run([sys.executable, clone / "scripts/package-plugin.py", "--ref", commit, mode],
                       check=True, stdout=subprocess.DEVNULL)
    tag = f'plugin-v{release["version"]}'
    def log(fmt, rev):
        return subprocess.check_output(["git", "-C", clone, "log", "-1", f"--format={fmt}", rev], text=True)
    release_identity = "super-workflow release <release@super-workflow.invalid>"
    assert log("%an <%ae>|%cn <%ce>", tag).strip() == f"{release_identity}|{release_identity}"
    assert log("%ad %cd", tag) == log("%ad %cd", commit)
    tagged = Path(scratch) / "tagged"
    tagged.mkdir()
    subprocess.run(f'git -C "{clone}" archive {tag} | tar -x -C "{tagged}"', shell=True, check=True)
    assert {p.relative_to(tagged).as_posix(): p.read_bytes() for p in tagged.rglob("*") if p.is_file()} == {
        f"{location[2:]}/{p.relative_to(plugin).as_posix()}": p.read_bytes()
        for p in plugin.rglob("*") if p.is_file()}
    subprocess.run(["git", "-C", clone, "tag", "-f", tag, commit], stdout=subprocess.DEVNULL, check=True)
    result = subprocess.run([sys.executable, clone / "scripts/package-plugin.py", "--ref", commit,
                             "--publish-ref"], capture_output=True, text=True)
    assert result.returncode != 0 and "different plugin tree" in result.stderr, result.stderr
    for name in (".codex-plugin/plugin.json", ".claude-plugin/plugin.json"):
        manifest = subprocess.check_output(["git", "show", f"{commit}:{name}"], cwd=repo)
        assert json.loads(manifest)["version"] == release["version"], name
    for name in (".claude-plugin/marketplace.json", ".agents/plugins/marketplace.json"):
        committed = subprocess.run(["git", "show", f"{commit}:{name}"], cwd=repo, capture_output=True)
        if committed.returncode == 0:  # releases before the GitHub route have no root catalogs
            assert committed.stdout == (clone / name).read_bytes(), f"{name} is stale; rerun --catalogs"
            assert tag in committed.stdout.decode(), name
    subprocess.run([sys.executable, verify, "--snapshot", snapshot], check=True)
    subprocess.run([sys.executable, snapshot / verify.relative_to(plugin)], check=True)
    extra = plugin / "docs/proposals/go-api/wf/unlisted.go"
    extra.write_text("package wf\n")
    result = subprocess.run([sys.executable, verify], capture_output=True, text=True)
    assert result.returncode != 0 and "unlisted.go" in result.stderr
    extra.unlink()
    (plugin / "docs/proposals/go-api/go.mod").write_text("module wrong\n")
    result = subprocess.run([sys.executable, verify], capture_output=True, text=True)
    assert result.returncode != 0 and "digest mismatch" in result.stderr
    if not os.environ.get("SW_PACKAGE_TEST_NESTED"):
        # A published tag for this version with a different payload must not fail the gate.
        tagged_repo = Path(scratch) / "tagged-repo"
        subprocess.run(["git", "clone", "-q", "--shared", "--no-checkout", repo, tagged_repo], check=True)
        subprocess.run(["git", "-C", tagged_repo, "tag", "-f", tag, commit],
                       check=True, stdout=subprocess.DEVNULL)
        (tagged_repo / "scripts").mkdir()
        for script in ("package-plugin.py", "test-package-plugin.py"):
            (tagged_repo / "scripts" / script).write_bytes((repo / "scripts" / script).read_bytes())
        subprocess.run([sys.executable, tagged_repo / "scripts/test-package-plugin.py", commit],
                       check=True, stdout=subprocess.DEVNULL,
                       env={**os.environ, "SW_PACKAGE_TEST_NESTED": "1"})
print("Plugin payload, manifests, reproducible ZIP and snapshot verified.")
