#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
git -C "$repo" archive "${1:-HEAD}" skills/super-workflow | python3 -c '
from pathlib import PurePosixPath
import re
import sys
import tarfile

root = PurePosixPath("skills/super-workflow")
entry = root / "SKILL.md"
reference = root / "workflow-checks.md"
metadata = root / "agents/openai.yaml"
expected = {entry, reference, metadata, root / "verify-release.py"}

with tarfile.open(fileobj=sys.stdin.buffer, mode="r|") as archive:
    files = {
        PurePosixPath(member.name): archive.extractfile(member).read().decode()
        for member in archive if member.isfile()
    }

assert files.keys() == expected, f"unexpected authoring skill payload: {sorted(map(str, files))}"
assert re.search(r"^name: super-workflow$", files[entry], re.M), "incorrect skill trigger"
assert re.search(r"^description: Use when .+$", files[entry], re.M), "missing usage description"
assert re.search(r"\$super-workflow(?![\w-])", files[metadata]), "incorrect Codex invocation"
for path in (entry, reference):
    prose = re.sub(r"`[^`\n]+`", "", files[path])
    for target in re.findall(r"\]\(([^)]+)\)", prose):
        assert path.parent / target in files, f"{path.name}: archive is missing {target}"
'
