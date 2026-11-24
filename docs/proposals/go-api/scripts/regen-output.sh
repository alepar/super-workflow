#!/usr/bin/env bash
# Regenerates OUTPUT.txt (or prints it with --stdout) from cmd/workflow and go test.
# --no-go-test omits the "#### go test" section (the golden check uses it to avoid
# a nested go test); flags may come in any order.
# Re-run after any code change, then fix document excerpt line ranges.
set -euo pipefail
export GOFLAGS=
cd "$(dirname "$0")/.."

stdout=0 gotest=1
for a in "$@"; do
	case "$a" in
	--stdout) stdout=1 ;;
	--no-go-test) gotest=0 ;;
	*) echo "usage: $0 [--stdout] [--no-go-test]" >&2; exit 2 ;;
	esac
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
go build -p 2 -o "$tmp/workflow" ./cmd/workflow

defs=$("$tmp/workflow" lint | sed -n 's/^lint \([^:]*\): ok$/\1/p' | LC_ALL=C sort)

emit() { # id, command output file
	printf '#### %s\n' "$1"
	sed -e :a -e '/^\n*$/{$d;N;ba' -e '}' "$2"
	printf '\n'
}

{
	"$tmp/workflow" lint >"$tmp/o"
	emit lint "$tmp/o"
	for kind in graph mermaid; do
		for d in $defs; do
			"$tmp/workflow" "$kind" "$d" >"$tmp/o"
			emit "$kind $d" "$tmp/o"
		done
	done
	"$tmp/workflow" hash >"$tmp/o"
	emit hash "$tmp/o"
	if [ "$gotest" = 1 ]; then
		pkgs=$(go list ./... | grep -v '/tools/excerptcheck$')
		# shellcheck disable=SC2086
		if ! go test -p 2 -count=1 $pkgs >"$tmp/tests" 2>&1; then
			cat "$tmp/tests" >&2
			exit 1
		fi
		sed -E 's/\t[0-9.]+s//; s/\t\(cached\)//' "$tmp/tests" >"$tmp/o"
		emit "go test" "$tmp/o"
	fi
} >"$tmp/out"

if [ "$stdout" = 1 ]; then
	cat "$tmp/out"
else
	cp "$tmp/out" OUTPUT.txt
fi
