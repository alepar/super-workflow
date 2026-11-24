#!/usr/bin/env python3
"""Stage a committed super-workflow release for Claude Code and Codex."""
import argparse
import hashlib
import io
import json
import os
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
VERSION_SOURCE = ".claude-plugin/plugin.json"
CATALOGS = (".claude-plugin/marketplace.json", ".agents/plugins/marketplace.json")
# The repository root catalogs install the staged tree from its release tag.
REPOSITORY = "https://github.com/alepar/super-workflow.git"
RELEASE_NAME, RELEASE_EMAIL = "super-workflow release", "release@super-workflow.invalid"
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


def tag_name(version):
    return f"plugin-v{version}"


def catalogs(common, claude_source, codex_source):
    return {
        CATALOGS[0]: encoded({
            "name": MARKET,
            "description": DESCRIPTION,
            "owner": {"name": NAME},
            "plugins": [{**common, "source": claude_source}],
        }),
        CATALOGS[1]: encoded({
            "name": MARKET,
            "plugins": [{
                "name": NAME,
                "source": codex_source,
                "policy": {"installation": "AVAILABLE"},
                "category": "Developer Tools",
            }],
        }),
    }


def root_catalogs(version):
    common = {"name": NAME, "version": version, "description": DESCRIPTION, "license": LICENSE}
    source = {"source": "git-subdir", "url": REPOSITORY, "path": SOURCE[2:], "ref": tag_name(version)}
    return catalogs(common, source, source)


def publish(plugin, version, source):
    """Commit the staged plugin tree as an orphan release commit and tag it.

    The commit has a fixed generic identity and the source commit's dates, so it
    is reproducible and carries no personal address."""
    paths = sorted(p for p in plugin.rglob("*") if p.is_file())
    blobs = git_input("\n".join(map(str, paths)) + "\n", "hash-object", "-w", "--stdin-paths").split()
    with tempfile.TemporaryDirectory(prefix=".index-", dir=ROOT) as tmp:
        env = {**os.environ, "GIT_INDEX_FILE": str(Path(tmp) / "index")}
        entries = "".join(f"100644 {blob}\t{SOURCE[2:]}/{path.relative_to(plugin).as_posix()}\n"
                          for blob, path in zip(blobs, paths))
        git_input(entries, "update-index", "--add", "--index-info", env=env)
        tree = git_input("", "write-tree", env=env).strip()
    tag = tag_name(version)
    existing = subprocess.run(["git", "-C", str(ROOT), "rev-parse", "--verify", "-q",
                               f"refs/tags/{tag}^{{tree}}"], capture_output=True, text=True)
    if existing.returncode == 0:
        if existing.stdout.strip() != tree:
            raise ValueError(f"tag {tag} exists with a different plugin tree")
        return tag
    authored, committed = git("log", "-1", "--format=%ad%x00%cd", "--date=raw",
                              source).decode().rstrip("\n").split("\0")
    env = {**os.environ, "GIT_AUTHOR_NAME": RELEASE_NAME, "GIT_AUTHOR_EMAIL": RELEASE_EMAIL,
           "GIT_AUTHOR_DATE": authored, "GIT_COMMITTER_NAME": RELEASE_NAME,
           "GIT_COMMITTER_EMAIL": RELEASE_EMAIL, "GIT_COMMITTER_DATE": committed}
    commit = git_input(f"super-workflow plugin v{version}\n", "commit-tree", tree, env=env).strip()
    git("tag", tag, commit)
    return tag


def git_input(text, *args, env=None):
    return subprocess.run(["git", "-C", str(ROOT), *args], input=text, capture_output=True,
                          text=True, check=True, env=env).stdout


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
    for name, data in catalogs(common, SOURCE, {"source": "local", "path": SOURCE}).items():
        write(market, name, data)
    with zipfile.ZipFile(output / f"{NAME}-{version}-codex.zip", "w") as archive:
        for name, data in sorted(files.items()):
            entry = zipfile.ZipInfo(name)
            entry.create_system = 3
            entry.external_attr = 0o100644 << 16
            archive.writestr(entry, data, compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ref", default="HEAD", help="committed source tree")
    parser.add_argument("--version", help=f"release version, without v (default: {VERSION_SOURCE})")
    target = parser.add_mutually_exclusive_group(required=True)
    target.add_argument("--output", type=Path, help="new release directory")
    target.add_argument("--catalogs", action="store_true",
                        help="write the repository-root catalogs for the committed version")
    target.add_argument("--publish-ref", action="store_true",
                        help="commit the staged plugin tree and tag it plugin-v<version>")
    args = parser.parse_args()
    try:
        commit = git("rev-parse", "--verify", "--end-of-options", f"{args.ref}^{{commit}}").decode().strip()
        committed = json.loads(git("show", f"{commit}:{VERSION_SOURCE}"))["version"]
        version = args.version or committed
        if not re.fullmatch(r"(?:0|[1-9][0-9]*)(?:\.(?:0|[1-9][0-9]*)){2}", version):
            parser.error("version must have the form MAJOR.MINOR.PATCH")
        if version != committed and not args.output:
            parser.error(f"version must match {VERSION_SOURCE}: {committed}")
        if args.catalogs:
            for name, data in root_catalogs(version).items():
                write(ROOT, name, data)
            print("\n".join(str(ROOT / name) for name in CATALOGS))
            return
        output = args.output.absolute() if args.output else None
        with tempfile.TemporaryDirectory(prefix=".stage-", dir=output.parent if output else ROOT) as tmp:
            staging = Path(tmp) / "release"
            stage(staging, version, commit)
            if args.publish_ref:
                print(publish(staging / "marketplace" / SOURCE, version, commit))
                return
            output.mkdir()
            staging.rename(output)
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"{error}\n")
    print(output)


if __name__ == "__main__":
    main()
