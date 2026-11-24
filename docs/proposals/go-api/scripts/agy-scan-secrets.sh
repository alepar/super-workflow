#!/usr/bin/env bash
# agy-scan-secrets.sh DIR: fail when DIR holds anything that looks like an agy
# credential: an ANTIGRAVITY_CSRF_TOKEN value other than <redacted>, a Google
# OAuth access or refresh token, an OAuth token or client-secret JSON field, a
# Google API key, or a file named antigravity-oauth-token. It prints only the
# offending file names and the kind of match, never the value, so its output
# is safe to keep. Used on harness/testdata/agy and on smoke-eval evidence.
# It fails closed: binary files are read as text, and a file it cannot read
# as text (unreadable, compressed by magic bytes, or one grep errors on) is
# named and never passed as clean.
# Exit 0 clean, 1 found (takes precedence), 2 usage, 3 a file could not be scanned.
set -euo pipefail
[ $# -eq 1 ] && [ -d "$1" ] || { echo "usage: agy-scan-secrets.sh DIR" >&2; exit 2; }
dir=$1
found=0
unscannable=0
out=
say() { echo "agy-scan-secrets: $1: $2"; }
report() { # kind, then file names on stdin
  local kind=$1 f
  while IFS= read -r f; do
    if [ -n "$f" ]; then say "$kind" "$f"; found=1; fi
  done
}
# Files no grep can read as text: unreadable, or compressed.
while IFS= read -r -d '' f; do
  say "unreadable, not scanned" "$f"; unscannable=1
done < <(find "$dir" ! -readable -print0)
while IFS= read -r -d '' f; do
  case $(head -c 4 -- "$f" | od -An -tx1 | tr -d ' \n') in
    1f8b*|504b0304|28b52ffd) say "compressed, not scanned" "$f"; unscannable=1 ;;
  esac
done < <(find "$dir" -type f -readable -print0)
# scan GREP-ARGS...: runs grep in this shell, leaves its matches in $out. grep's
# own errors stay on stderr; exit 2 marks the scan incomplete, exit 1 is no match.
scan() {
  local rc=0
  out=$(grep -ra "$@" "$dir") || rc=$?
  [ "$rc" -le 1 ] || { say "grep error (exit $rc), scan incomplete" "$dir"; unscannable=1; }
}
# A CSRF value is anything after the name and its separator that is not <redacted>.
csrf='ANTIGRAVITY_CSRF_TOKEN[\\"]*[[:space:]]*[=:][[:space:]\\"]*'
scan -HoE -- "${csrf}[^[:space:]\\\"]{0,12}"
report "ANTIGRAVITY_CSRF_TOKEN value" < <(
  { printf '%s\n' "$out" | grep -avE -- "${csrf}<redacted>" || true; } | cut -d: -f1 | sort -u)
for p in 'ya29\.[0-9A-Za-z_-]{20,}' '1//0[0-9A-Za-z_-]{20,}' 'AIza[0-9A-Za-z_-]{35}' 'GOCSPX-[0-9A-Za-z_-]{10,}' \
         '\\?"(access_token|refresh_token|id_token|client_secret)\\?"[[:space:]]*:[[:space:]]*\\?"[^"<\\]'; do
  scan -lE -- "$p"
  report "credential pattern $p" < <(printf '%s\n' "$out")
done
report "credential file" < <(find "$dir" -name 'antigravity-oauth-token*')
[ "$found" -eq 0 ] || exit 1
[ "$unscannable" -eq 0 ] || exit 3
exit 0
