#!/usr/bin/env python3
"""Verify that the native catalogs and ZIP agree on the release payload."""
import hashlib
import json
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
print("Plugin payload, manifests, reproducible ZIP and snapshot verified.")
