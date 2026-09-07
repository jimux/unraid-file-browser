#!/usr/bin/env bash
# leakcheck.sh — refuse to ship private network details.
#
# Scans the tree (or the files passed as arguments) for things that must never
# land in a public repo: private IPv4 ranges, MAC addresses, LAN-style host
# names, and any extra patterns listed one-per-line in .leakcheck.local
# (git-ignored, so the patterns themselves — e.g. your server's hostname —
# are never committed either).
#
# Usage: scripts/leakcheck.sh            # whole tree
#        scripts/leakcheck.sh FILE...    # e.g. from a pre-commit hook
# Exit 1 if anything matches.
set -euo pipefail
cd "$(dirname "$0")/.."

EXCLUDES=(--exclude-dir=node_modules --exclude-dir=build --exclude-dir=dist
          --exclude-dir=third_party --exclude-dir=.git --exclude=package-lock.json
          --exclude=leakcheck.sh --exclude=.leakcheck.local)

# Generic patterns (extended regex, case-insensitive).
PATTERNS=(
  '\b192\.168\.[0-9]{1,3}\.[0-9]{1,3}\b'
  '\b10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\b'
  '\b172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}\b'
  '\b([0-9a-f]{2}:){5}[0-9a-f]{2}\b'
  '\b[a-z0-9-]+\.(lan|home\.arpa|internal)\b'
)
if [ -f .leakcheck.local ]; then
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    case "$line" in \#*) continue ;; esac
    PATTERNS+=("$line")
  done < .leakcheck.local
fi

status=0
for pat in "${PATTERNS[@]}"; do
  if [ $# -gt 0 ]; then
    hits=$(grep -nEIi "${EXCLUDES[@]}" -- "$pat" "$@" 2>/dev/null || true)
  else
    hits=$(grep -rnEIi "${EXCLUDES[@]}" -- "$pat" . 2>/dev/null || true)
  fi
  if [ -n "$hits" ]; then
    echo "leakcheck: pattern /$pat/ matched:" >&2
    echo "$hits" >&2
    status=1
  fi
done
[ $status -eq 0 ] && echo "leakcheck: clean"
exit $status
