"""Verify an installed super-workflow release and optionally pin its payload."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import shutil

MODULE = "docs/proposals/go-api"
DECLARATION = "module github.com/alepar/super-workflow/docs/proposals/go-api"


def verify(root):
    release_file = root / "release.json"
    release = json.loads(release_file.read_text())
    if (release["name"] != "super-workflow"
            or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", release["version"])
            or not re.fullmatch(r"[0-9a-f]{64}", release["payload_sha256"])):
        raise ValueError("invalid super-workflow release identity")

    for manifest in (".claude-plugin/plugin.json", ".codex-plugin/plugin.json"):
        path = root / manifest
        if path.is_file():
            data = json.loads(path.read_text())
            if any(data[key] != release[key] for key in ("name", "version")):
                raise ValueError(f"release identity mismatch: {manifest}")

    files = release["files"]
    canonical = json.dumps(files, sort_keys=True, separators=(",", ":")).encode()
    if hashlib.sha256(canonical).hexdigest() != release["payload_sha256"]:
        raise ValueError("release digest does not match the payload manifest")

    module = root / MODULE
    listed = {name for name in files if name.startswith(f"{MODULE}/")}
    actual = {path.relative_to(root).as_posix() for path in module.rglob("*")
              if path.is_symlink() or not path.is_dir()}
    if actual != listed:
        mismatch = ", ".join(sorted(actual ^ listed))
        raise ValueError(f"engine module file roster mismatch: {mismatch}")

    for required in (f"{MODULE}/go.mod", "skills/super-workflow/verify-release.py"):
        if required not in files:
            raise ValueError(f"release manifest is missing {required}")
    for name, digest in files.items():
        rel = PurePosixPath(name)
        path = root / rel
        if (rel.is_absolute() or ".." in rel.parts or rel.as_posix() != name
                or path.resolve(strict=True) != path or not path.is_file()):
            raise ValueError(f"invalid release path: {name}")
        if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise ValueError(f"digest mismatch: {name}")
    if DECLARATION not in (root / MODULE / "go.mod").read_text().splitlines():
        raise ValueError("unexpected engine module declaration")
    return release


def snapshot(root, dest, release):
    dest = dest.resolve()
    if dest.is_relative_to(root):
        raise ValueError("snapshot must be outside the installed release")
    dest.mkdir()
    try:
        for name in (*release["files"], "release.json"):
            path = dest / name
            path.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(root / name, path)
        verify(dest)
    except (OSError, ValueError, KeyError, TypeError):
        shutil.rmtree(dest)
        raise
    return dest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", type=Path, help="new path outside the installer cache")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    try:
        release = verify(root)
        if args.snapshot is not None:
            root = snapshot(root, args.snapshot, release)
    except (OSError, ValueError, KeyError, TypeError) as error:
        parser.exit(1, f"{error}\n")
    print(json.dumps({
        "module": str(root / MODULE),
        "version": f'v{release["version"]}',
        "payload_sha256": release["payload_sha256"],
    }))


if __name__ == "__main__":
    main()
