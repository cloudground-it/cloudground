# Shared by scripts/e2e-affected.sh and scripts/e2e-new-checks.sh: sourced,
# not run. Needs bash, git and a POSIX awk (BSD awk, mawk and gawk all do).

E2E_TABLE=e2e/e2e_test.go
E2E_MAP=e2e/sections.map
E2E_INDEX=e2e/sections.index

# e2e_sections prints the section names in the table's order (the
# `var sections = []section{…}` literal of e2e/e2e_test.go).
e2e_sections() {
	awk '
		/^var sections = \[\]section\{/ { on = 1; next }
		on && /^}/ { exit }
		on && match($0, /name: "[^"]*"/) { print substr($0, RSTART + 7, RLENGTH - 8) }
	' "$E2E_TABLE"
}

# e2e_funcmap prints "<line>\t<function>" for every line of the Go source on
# stdin: the function the line is in (Recv.Name for a method, as in
# e2e/sections.index; a doc comment belongs to the function below it), or
# "-" for a line outside every function.
e2e_funcmap() {
	awk '
		function funckey(l,    s, r, n, parts, t, recv) {
			s = substr(l, 6)
			recv = ""
			if (substr(s, 1, 1) == "(") {
				r = substr(s, 2, index(s, ")") - 2)
				n = split(r, parts, " ")
				t = parts[n]
				sub(/^\*/, "", t)
				sub(/\[.*/, "", t)
				recv = t "."
				s = substr(s, index(s, ")") + 1)
				sub(/^ +/, "", s)
			}
			match(s, /^[A-Za-z_][A-Za-z0-9_]*/)
			return recv substr(s, RSTART, RLENGTH)
		}
		{ line[NR] = $0 }
		END {
			cur = "-"
			for (i = 1; i <= NR; i++) {
				l = line[i]
				if (l ~ /^func /) {
					cur = funckey(l)
					key[i] = cur
					for (j = i - 1; j >= 1 && line[j] ~ /^\/\//; j--)
						key[j] = cur
					if (l ~ /\{/ && l ~ /\}[ \t]*$/)
						cur = "-" # a one-line function
					continue
				}
				key[i] = cur
				if (l ~ /^}/)
					cur = "-"
			}
			for (i = 1; i <= NR; i++)
				print i "\t" key[i]
		}
	'
}

# e2e_index_sections FILE FUNC prints the sections of FUNC in FILE, one per
# line, from the index: ALL, nothing (NONE), or names. FUNC "-" (a line
# outside every function) is every function of the file together; a file or
# function the index does not know is ALL.
e2e_index_sections() {
	awk -F '\t' -v f="$1" -v k="$2" '
		/^#/ { next }
		$1 == f && (k == "-" || $2 == k) {
			found = 1
			if ($3 == "NONE") next
			n = split($3, s, "|")
			for (i = 1; i <= n; i++) print s[i]
		}
		END { if (!found) print "ALL" }
	' "$E2E_INDEX"
}

# e2e_changed_lines BASE FILE prints the line numbers (in HEAD's FILE) that
# `git diff -U0 BASE...HEAD` adds or changes; for a hunk that only deletes,
# the line after which it deleted.
e2e_changed_lines() {
	git diff -U0 "$1...HEAD" -- "$2" | awk '
		/^@@/ {
			split($3, a, ",")
			start = substr(a[1], 2) + 0
			n = (a[2] == "" ? 1 : a[2] + 0)
			if (n == 0) { print (start > 0 ? start : 1); next }
			for (i = 0; i < n; i++) print start + i
		}
	'
}

# e2e_regex_name NAME prints a section or check name as `go test` matches it:
# spaces as underscores, regexp metacharacters escaped.
e2e_regex_name() {
	printf '%s\n' "$1" | sed -e 's/[][\\.+*?(){}|^$]/\\&/g' -e 's/[[:space:]]/_/g'
}

# e2e_order prints the section names read on stdin in the table's order,
# once each.
e2e_order() {
	local want
	want=$(cat)
	e2e_sections | while IFS= read -r s; do
		if printf '%s\n' "$want" | grep -qxF -- "$s"; then
			printf '%s\n' "$s"
		fi
	done
}

# e2e_alternation prints the sections read on stdin as ^(a|b|…)$.
e2e_alternation() {
	local out="" s
	while IFS= read -r s; do
		[ -n "$s" ] || continue
		out="${out:+$out|}$(e2e_regex_name "$s")"
	done
	printf '^(%s)$' "$out"
}
