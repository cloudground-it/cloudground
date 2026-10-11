#!/usr/bin/env bash
# e2e-affected.sh — the e2e sections a change affects, as a -test.run regexp.
#
#   scripts/e2e-affected.sh [base-ref]        changes of `git diff base-ref...HEAD` (default: main)
#   scripts/e2e-affected.sh --paths < list    the paths on stdin, each as changed throughout
#   scripts/e2e-affected.sh --sections        the sections, in the suite's order
#
# It prints, on stdout, the regexp for cg-e2e's -test.run:
#
#   ^TestE2E$/^(wordpress_backups|security)$  the sections affected
#   ^TestE2E$                                 every section (a change no line maps, or one
#                                             mapped to ALL)
#
# and on stderr, which path selected which sections. When no section is
# affected it prints nothing and exits 1, so that
#
#   R=$(scripts/e2e-affected.sh) && ./cg-e2e -test.v -test.run "$R"
#
# never runs the whole suite by accident (an empty -test.run selects
# everything).
#
# A path outside e2e/ takes the sections of the first line of
# e2e/sections.map that matches it. A changed line of e2e/*.go takes the
# sections that reach the function it is in, from e2e/sections.index; a whole
# e2e file given with --paths takes the sections of all its functions.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
# shellcheck source=scripts/e2e-lib.sh
. scripts/e2e-lib.sh

# map_sections < paths: "<path>\t<section or ALL>" per selected section, and
# "<path>\t" for a path mapped to NONE.
map_sections() {
	awk -v mapfile="$E2E_MAP" '
		function glob2re(g,    re, i, n, c) {
			re = "^"
			n = length(g)
			for (i = 1; i <= n; i++) {
				c = substr(g, i, 1)
				if (c == "*") {
					if (substr(g, i + 1, 1) == "*") {
						if (substr(g, i + 2, 1) == "/") { re = re "(.*/)?"; i += 2 }
						else { re = re ".*"; i++ }
					} else re = re "[^/]*"
				} else if (c == "?") re = re "[^/]"
				else if (index(".+()|[]{}^$\\", c)) re = re "\\" c
				else re = re c
			}
			return re "$"
		}
		function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
		function expand(rhs,    out, n, items, i, it) {
			out = ""
			n = split(rhs, items, ",")
			for (i = 1; i <= n; i++) {
				it = trim(items[i])
				if (it == "") continue
				if (substr(it, 1, 1) == "@") it = group[it]
				out = out (out == "" ? "" : ",") it
			}
			return out
		}
		BEGIN {
			while ((getline l < mapfile) > 0) {
				ln++
				l = trim(l)
				if (l == "" || substr(l, 1, 1) == "#") continue
				if (substr(l, 1, 1) == "@") {
					eq = index(l, "=")
					group[trim(substr(l, 1, eq - 1))] = expand(substr(l, eq + 1))
					continue
				}
				split(l, f, /[ \t]+/)
				n++
				re[n] = glob2re(f[1])
				secs[n] = expand(trim(substr(l, length(f[1]) + 1)))
				lineno[n] = ln
			}
		}
		{
			p = $0
			if (p == "") next
			hit = 0
			for (i = 1; i <= n; i++) {
				if (p ~ re[i]) { hit = i; break }
			}
			if (!hit) {
				print p "\tALL\t(no line of sections.map matches it)"
				next
			}
			if (secs[hit] == "NONE") { print p "\t\t(sections.map line " lineno[hit] ": NONE)"; next }
			m = split(secs[hit], s, ",")
			for (j = 1; j <= m; j++) print p "\t" s[j] "\t(sections.map line " lineno[hit] ")"
		}
	'
}

# e2e_file_sections FILE [lines...]: "<path>\t<section>" for the functions
# of FILE (HEAD's) the lines are in, or for all of its functions.
e2e_file_sections() {
	local file=$1
	shift
	local keys
	if [ $# -eq 0 ]; then
		keys="-"
	elif git cat-file -e "HEAD:$file" 2>/dev/null; then
		keys=$(git show "HEAD:$file" | e2e_funcmap | awk -F '\t' -v want=" $* " '
			index(want, " " $1 " ") { print $2 }' | sort -u)
	else
		keys="-"
	fi
	local k
	for k in $keys; do
		e2e_index_sections "$file" "$k" | while IFS= read -r s; do
			printf '%s\t%s\t(%s in sections.index)\n' "$file" "$s" "$k"
		done
	done
}

paths_mode=0
case "${1:-}" in
--sections)
	e2e_sections
	exit 0
	;;
--func-at)
	e2e_funcmap < "$2" | awk -F '\t' 'prev != $2 && $2 != "-" { print }  { prev = $2 }'
	exit 0
	;;
--paths)
	paths_mode=1
	;;
-h | --help)
	sed -n '2,28p' "$0"
	exit 0
	;;
esac

if [ "$paths_mode" -eq 0 ]; then
	base=${1:-main}
	git rev-parse --verify --quiet "$base^{commit}" >/dev/null || {
		echo "e2e-affected: unknown ref $base" >&2
		exit 2
	}
fi

# collect prints "<path>\t<section, ALL or empty>\t<why>" per selection.
collect() {
	if [ "$paths_mode" -eq 1 ]; then
		while IFS= read -r p; do
			[ -n "$p" ] || continue
			case "$p" in
			e2e/*.go) e2e_file_sections "$p" ;;
			*) printf '%s\n' "$p" | map_sections ;;
			esac
		done
	else
		git diff --name-only --no-renames "$base...HEAD" | while IFS= read -r p; do
			case "$p" in
			e2e/*.go)
				if git cat-file -e "HEAD:$p" 2>/dev/null; then
					# shellcheck disable=SC2046
					e2e_file_sections "$p" $(e2e_changed_lines "$base" "$p")
				else
					printf '%s\tALL\t(deleted)\n' "$p"
				fi
				;;
			*) printf '%s\n' "$p" | map_sections ;;
			esac
		done
	fi
}
selected=$(collect)

printf '%s\n' "$selected" | awk -F '\t' 'NF >= 2 && !seen[$0]++ { printf "  %-48s %s %s\n", $1, ($2 == "" ? "-" : $2), $3 }' >&2

if printf '%s\n' "$selected" | awk -F '\t' '$2 == "ALL" { found = 1 } END { exit !found }'; then
	echo "e2e-affected: every section" >&2
	printf '^TestE2E$\n'
	exit 0
fi
names=$(printf '%s\n' "$selected" | awk -F '\t' '$2 != "" { print $2 }' | e2e_order)
if [ -z "$names" ]; then
	echo "e2e-affected: no e2e section is affected" >&2
	exit 1
fi
printf '^TestE2E$/%s\n' "$(printf '%s\n' "$names" | e2e_alternation)"
