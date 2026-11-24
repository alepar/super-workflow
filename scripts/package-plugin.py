#!/usr/bin/env python3
"""Stage a committed super-workflow release for Claude Code and Codex."""
import argparse
import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
NAME = "super-workflow"
MARKET = "super-workflow-framework"
SOURCE = "./plugins/super-workflow"
DESCRIPTION = "Author and drive Go super-workflow definitions."
LICENSE = "MIT OR Apache-2.0"
ALLOWLIST = "packaging/payload-files.txt"
USER_HOME = re.compile(rb"/(?:var/home|home|Users)/[A-Za-z][A-Za-z0-9_.-]+")
KEY_PREFIX = b"sk-or-" + b"v1-"


def git(*args):
    return subprocess.check_output(["git", "-C", str(ROOT), *args])


def payload(commit):
    paths = git("show", f"{commit}:{ALLOWLIST}").decode().splitlines()
    if not paths or paths != sorted(set(paths)):
        raise ValueError("payload allowlist must be nonempty, sorted and unique")
    for path in paths:
        name = PurePosixPath(path)
        if name.is_absolute() or ".." in name.parts or name.as_posix() != path:
            raise ValueError(f"invalid payload path: {path}")
    files = {}
    with tarfile.open(fileobj=io.BytesIO(git("archive", commit, "--", *paths))) as archive:
        for member in archive:
            if member.isdir():
                continue
            if not member.isfile() or member.name not in paths:
                raise ValueError(f"unexpected archive entry: {member.name}")
            data = archive.extractfile(member).read()
            if USER_HOME.search(data):
                raise ValueError(f"absolute user home in payload: {member.name}")
            if KEY_PREFIX in data:
                raise ValueError(f"credential marker in payload: {member.name}")
            files[member.name] = data
    if files.keys() != set(paths):
        raise ValueError(f"missing payload files: {sorted(set(paths) - files.keys())}")
    return files


def encoded(data):
    return (json.dumps(data, indent=2, sort_keys=True) + "\n").encode()


def write(root, name, data):
    path = root / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)


def stage(output, version, commit):
    files = payload(commit)
    common = {"name": NAME, "version": version, "description": DESCRIPTION, "license": LICENSE}
    market = output / "marketplace"
    plugin = market / SOURCE
    write(market, "README.md", files["README.md"])
    digests = {path: hashlib.sha256(data).hexdigest() for path, data in files.items()}
    canonical = json.dumps(digests, sort_keys=True, separators=(",", ":")).encode()
    files["release.json"] = encoded({
        "name": NAME,
        "version": version,
        "payload_sha256": hashlib.sha256(canonical).hexdigest(),
        "files": digests,
    })
    files[".codex-plugin/plugin.json"] = encoded({**common, "skills": "./skills/"})
    for name, data in files.items():
        write(plugin, name, data)
    write(plugin, ".claude-plugin/plugin.json", encoded({**common, "author": {"name": NAME}}))
    write(market, ".claude-plugin/marketplace.json", encoded({
        "name": MARKET,
        "description": DESCRIPTION,
        "owner": {"name": NAME},
        "plugins": [{**common, "source": SOURCE}],
    }))
    write(market, ".agents/plugins/marketplace.json", encoded({
        "name": MARKET,
        "plugins": [{
            "name": NAME,
            "source": {"source": "local", "path": SOURCE},
            "policy": {"installation": "AVAILABLE", "authentication": "ON_INSTALL"},
            "category": "Developer Tools",
        }],
    }))
    with zipfile.ZipFile(output / f"{NAME}-{version}-codex.zip", "w") as archive:
        for name, data in sorted(files.items()):
            entry = zipfile.ZipInfo(name)
            entry.create_system = 3
            entry.external_attr = 0o100644 << 16
            archive.writestr(entry, data, compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ref", default="HEAD", help="committed source tree")
    parser.add_argument("--version", default="0.1.0", help="release version, without v")
    parser.add_argument("--output", required=True, type=Path, help="new release directory")
    args = parser.parse_args()
    if not re.fullmatch(r"(?:0|[1-9][0-9]*)(?:\.(?:0|[1-9][0-9]*)){2}", args.version):
        parser.error("version must have the form MAJOR.MINOR.PATCH")
    output = args.output.absolute()
    try:
        commit = git("rev-parse", "--verify", "--end-of-options", f"{args.ref}^{{commit}}").decode().strip()
        with tempfile.TemporaryDirectory(prefix=".stage-", dir=output.parent) as tmp:
            staging = Path(tmp) / "release"
            stage(staging, args.version, commit)
            output.mkdir()
            staging.rename(output)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"{error}\n")
    print(output)


if __name__ == "__main__":
    main()
