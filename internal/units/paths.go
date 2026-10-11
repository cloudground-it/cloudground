package units

import (
	"slices"
	"strings"
)

// pathKeys are the directives whose paths systemd needs when it sets up a
// unit's mount namespace: a path that is missing fails the unit at the
// NAMESPACE step (status 226) unless it carries the "-" prefix.
var pathKeys = []string{"ReadWritePaths", "ReadOnlyPaths", "BindPaths", "BindReadOnlyPaths",
	"InaccessiblePaths", "ExecPaths", "NoExecPaths", "TemporaryFileSystem"}

// RequiredPaths returns the paths that lines ("Key=value", as in a unit file
// or a systemd-run -p) need to exist for the unit to start: a path with the
// "-" prefix (ignore if missing) is left out, a "+" prefix and a ":options"
// or ":destination" suffix are stripped (SPEC.md).
func RequiredPaths(lines []string) []string {
	var out []string
	for _, l := range lines {
		k, v, found := strings.Cut(strings.TrimSpace(l), "=")
		if !found || !slices.Contains(pathKeys, k) {
			continue
		}
		for _, f := range strings.Fields(v) {
			if strings.HasPrefix(f, "-") {
				continue
			}
			f = strings.TrimPrefix(f, "+")
			f, _, _ = strings.Cut(f, ":")
			out = append(out, f)
		}
	}
	return out
}
