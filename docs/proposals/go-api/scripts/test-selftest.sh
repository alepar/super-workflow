#!/usr/bin/env bash
# Probes scripts/test's package selection in a throwaway git repo holding a copy
# of this module. go test runs with -run=NONE, so only the selection is exercised.
set -uo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mod=$tmp/docs/proposals/go-api
mkdir -p "$mod"
cp -a "$here/." "$mod/"
git -C "$tmp" init -q -b main
git -C "$tmp" -c user.name=t -c user.email=t@t add -A
git -C "$tmp" -c user.name=t -c user.email=t@t commit -qm base
fail=0
run() { (cd "$mod" && BASE=${BASE:-HEAD} GOFLAGS=-run=NONE bash scripts/test 2>&1); }
expect() { # name, output, must-contain pkg suffix...
  local name=$1 out=$2; shift 2
  for pkg in "$@"; do
    case "$pkg" in
      !*) if grep -q "go-api/${pkg#!}[[:space:]]" <<<"$out"; then echo "FAIL $name: ran ${pkg#!}"; fail=1; fi ;;
      *) if ! grep -q "go-api/$pkg[[:space:]]" <<<"$out" && ! grep -q "every package" <<<"$out"; then echo "FAIL $name: did not run $pkg"; fail=1; fi ;;
    esac
  done
}
reset() { git -C "$tmp" reset -q --hard HEAD; git -C "$tmp" clean -qfd; }

echo '// x' >> "$mod/wf/types.go"
expect "go change reaches doc readers" "$(run)" wf tools/excerptcheck
reset

git -C "$tmp" mv docs/proposals/go-api/fsx docs/proposals/go-api/fsx2
expect "directory rename seeds the old side" "$(run)" fsx2 wf
reset

git -C "$tmp" mv docs/proposals/go-api/gh/runtime_test.go docs/proposals/go-api/git/moved_gh_test.go 2>/dev/null \
  || git -C "$tmp" mv "$(cd "$tmp" && ls docs/proposals/go-api/gh/*_test.go | head -1)" docs/proposals/go-api/git/moved_gh_test.go
expect "file move seeds source and destination" "$(run)" gh git
reset

echo '// x' >> "$mod/bd/bd.go"
git -C "$tmp" config diff.renames bogus
out=$(run; echo "rc=$?")
if grep -q 'rc=0' <<<"$out"; then echo "FAIL a failing git diff exited 0"; fail=1; fi
git -C "$tmp" config --unset diff.renames; reset

[ "$fail" = 0 ] && echo "selftest: ok"
exit "$fail"
