#!/usr/bin/env bash
# Lines of the tree that `git blame -w` still gives to the two commits the
# repository started from: 014726a (the import) and e7e8957 (its mechanical
# rename). The licence change waits for 0 lines everywhere.
#
#   scripts/provenance.sh             a row per area: lines, imported lines, share
#   scripts/provenance.sh -f [path]   a row per file with imported lines, most first
#                                     (the whole tree without a path)
#
# The built UI (ui/dist) is left out: it is generated from ui/app.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
IMPORT='^(014726a|e7e8957)'

# tally <file>: prints "<lines> <imported lines>".
tally() {
  git blame -w --line-porcelain HEAD -- "$1" 2>/dev/null | grep -E '^[0-9a-f]{40} ' | cut -c1-7 |
    awk -v re="$IMPORT" '{n++; if ($1 ~ re) i++} END {print n + 0, i + 0}'
}

# files <area>: the tracked files of an area; "(root)" is the top-level files.
files() {
  if [ "$1" = "(root)" ]; then
    git ls-files | grep -v /
  else
    git ls-files -- "$1" | grep -v '^ui/dist/'
  fi
}

if [ "${1:-}" = "-f" ]; then
  git ls-files -- "${2:-.}" | grep -v '^ui/dist/' | while read -r f; do
    read -r n i <<<"$(tally "$f")"
    if [ "$i" -gt 0 ]; then printf '%6d of %6d  %s\n' "$i" "$n" "$f"; fi
  done | sort -rn
  exit 0
fi

printf '%-10s %8s %9s %6s\n' area lines imported share
for area in "(root)" .github cmd internal installer connector e2e scripts bench docs ui; do
  total=0; imported=0
  while read -r f; do
    [ -n "$f" ] || continue
    read -r n i <<<"$(tally "$f")"
    total=$((total + n)); imported=$((imported + i))
  done < <(files "$area")
  share=0
  [ "$total" -gt 0 ] && share=$((imported * 100 / total))
  printf '%-10s %8d %9d %5d%%\n' "$area" "$total" "$imported" "$share"
done
