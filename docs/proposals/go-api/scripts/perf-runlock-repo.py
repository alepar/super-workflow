#!/usr/bin/env python3
"""Synthetic large workspace for wf/perf_runlock_test.go: a git repo with long history and a big ignored build tree.
usage: perf-runlock-repo.py <dir> [commits] [files] [target_mb]"""
import os, random, subprocess, sys
d = sys.argv[1]; commits = int(sys.argv[2]) if len(sys.argv) > 2 else 2000
nfiles = int(sys.argv[3]) if len(sys.argv) > 3 else 8000
target_mb = int(sys.argv[4]) if len(sys.argv) > 4 else 1000
rng = random.Random(1)
os.makedirs(d)
subprocess.run(["git", "init", "-q", "-b", "main", d], check=True)
words = [("%x" % rng.getrandbits(40)) for _ in range(5000)]
def body():
    return (" ".join(rng.choice(words) for _ in range(1200)) + "\n").encode()
p = subprocess.Popen(["git", "fast-import", "--quiet"], cwd=d, stdin=subprocess.PIPE)
w = p.stdin.write
t = 1700000000
for c in range(commits):
    w(b"commit refs/heads/main\nauthor S <s@x> %d +0000\ncommitter S <s@x> %d +0000\ndata 4\nc%03d\n" % (t + c, t + c, c % 1000))
    names = range(nfiles) if c == 0 else rng.sample(range(nfiles), 20)
    for i in names:
        b = body()
        w(b"M 100644 inline src/m%03d/f%05d.rs\ndata %d\n" % (i % 200, i, len(b)) + b + b"\n")
    if c == 0:
        g = b"target/\n"
        w(b"M 100644 inline .gitignore\ndata %d\n" % len(g) + g + b"\n")
p.stdin.close(); p.wait()
subprocess.run(["git", "checkout", "-q", "-f", "main"], cwd=d, check=True)
subprocess.run(["git", "repack", "-adq"], cwd=d, check=True)
os.makedirs(os.path.join(d, "target"), exist_ok=True)
chunk = os.urandom(64 * 1024)
for i in range(target_mb * 16):
    sub = os.path.join(d, "target", "d%03d" % (i % 300)); os.makedirs(sub, exist_ok=True)
    with open(os.path.join(sub, "o%06d" % i), "wb") as f: f.write(chunk)
