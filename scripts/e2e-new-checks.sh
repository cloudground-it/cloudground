#!/usr/bin/env bash
# e2e-new-checks.sh — the e2e checks added or changed since a ref, as a
# -test.run regexp, to prove on the old build that they fail there.
#
#   scripts/e2e-new-checks.sh [base-ref]       checks of `git diff base-ref...HEAD` (default: main)
#   scripts/e2e-new-checks.sh --patterns < go  the name pattern of each check call on stdin
#
# A check is a call h.check(t, <name>, …), h.assert(t, <name>, …) or
# h.fail(t, <name>, …) with an added or changed line anywhere in it, in
# e2e/*.go; a call of a helper that takes t first and a name-like string
# last (f.objectCacheSet(t, true, "…"), h.warmSite(t, …, "R-3.8: …")) counts
# too, as a pattern that contains that string. A name built at run time
# becomes a pattern: fmt.Sprintf verbs and non-literal operands match
# anything (scripts/e2e-checks.pl).
#
# It prints, on stdout:
#
#   ^TestE2E$/^(<sections>)$/^(<check patterns>)(#[0-9]+)?$
#
# where the sections are those that reach the function each check is in
# (e2e/sections.index). Run it on the server against the OLD build:
#
#   R=$(scripts/e2e-new-checks.sh main) && ./cg-e2e -test.v -test.run "$R"
#
# The sections still run whole (their fixtures and steps), but only the
# selected checks are reported and counted: on the old build, RESULT should
# show them failed. A fixture that cannot be made fails its section outside
# any check, so the filter never hides it. A check whose call did not
# change but whose inputs did (a changed helper, a new fixture) is not
# found: run the affected sections (scripts/e2e-affected.sh) for those.
# With no check found it prints nothing and exits 1.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
# shellcheck source=scripts/e2e-lib.sh
. scripts/e2e-lib.sh

case "${1:-}" in
--patterns)
	perl scripts/e2e-checks.pl all | cut -f 2
	exit 0
	;;
-h | --help)
	sed -n '2,31p' "$0"
	exit 0
	;;
esac

base=${1:-main}
git rev-parse --verify --quiet "$base^{commit}" >/dev/null || {
	echo "e2e-new-checks: unknown ref $base" >&2
	exit 2
}

tab=$(printf '\t')
found=$(
	git diff --name-only --no-renames --diff-filter=AM "$base...HEAD" -- 'e2e/*.go' | while IFS= read -r f; do
		lines=$(e2e_changed_lines "$base" "$f" | tr '\n' ' ')
		[ -n "$lines" ] || continue
		src=$(git show "HEAD:$f")
		funcs=$(printf '%s\n' "$src" | e2e_funcmap)
		printf '%s\n' "$src" | perl scripts/e2e-checks.pl "$lines" | while IFS="$tab" read -r l pat; do
			fn=$(printf '%s\n' "$funcs" | awk -F '\t' -v l="$l" '$1 == l { print $2 }')
			e2e_index_sections "$f" "$fn" | while IFS= read -r s; do
				printf '%s\t%s\t%s\t%s\n' "$f:$l" "$fn" "$s" "$pat"
			done
		done
	done
)

if [ -z "$found" ]; then
	echo "e2e-new-checks: no check added or changed in e2e/*.go since $base" >&2
	exit 1
fi
printf '%s\n' "$found" | awk -F '\t' '{ printf "  %-32s %-28s %s\n", $1, $3, $4 }' >&2

pats=$(printf '%s\n' "$found" | cut -f 4 | awk '!seen[$0]++' | paste -sd '|' -)
# awk reads to the end: grep -q would quit at its first match and, under
# pipefail, the SIGPIPE of the commands before it would fail the test.
if printf '%s\n' "$found" | awk -F '\t' '$3 == "ALL" { all = 1 } END { exit !all }'; then
	secs='[^/]*'
else
	secs=$(printf '%s\n' "$found" | cut -f 3 | e2e_order | e2e_alternation)
fi
printf '^TestE2E$/%s/^(%s)(#[0-9]+)?$\n' "$secs" "$pats"
