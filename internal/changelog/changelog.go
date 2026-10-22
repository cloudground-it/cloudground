// Package changelog reads CHANGELOG.md for the panel's "What's new"
// (SPEC.md).
package changelog

import (
	"regexp"
	"strings"

	"github.com/cloudground-it/cloudground/internal/version"
)

// Entry is one release's section of the changelog.
type Entry struct {
	// Version is the heading's tag, "v0.3.2".
	Version string `json:"version"`
	// Body is the Markdown under the heading, without it.
	Body string `json:"body"`
}

var headingRe = regexp.MustCompile(`^## (v[0-9][^\s]*)\s*$`)

// Parse splits md at its "## vX.Y.Z" headings, in the order written (newest
// first). The text before the first heading belongs to no release; a
// heading that is not a version ends the entry before it and starts none.
func Parse(md string) []Entry {
	var out []Entry
	var cur *Entry
	var body []string
	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(strings.Join(body, "\n"))
			out = append(out, *cur)
		}
		cur, body = nil, nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &Entry{Version: m[1]}
			continue
		}
		if strings.HasPrefix(line, "## ") {
			flush()
			continue
		}
		if cur != nil {
			body = append(body, line)
		}
	}
	flush()
	return out
}

// Unseen lists the versions an account has not seen: the releases after seen
// up to and including current, newest first. An account that has seen none
// (seen is "") is told of the running release only, not of every release
// there ever was. A running build that is no release (a development build)
// has none.
func Unseen(entries []Entry, seen, current string) []string {
	var out []string
	if _, ok := version.Parse(seen); !ok {
		seen = "" // a mark that is no release (an account made by a development build) is none
	}
	for _, e := range entries {
		if c, ok := version.Compare(e.Version, current); !ok || c > 0 {
			continue
		}
		if seen == "" {
			// The newest release the build contains, and no older one.
			return []string{e.Version}
		}
		if s, ok := version.Compare(e.Version, seen); ok && s <= 0 {
			continue
		}
		out = append(out, e.Version)
	}
	return out
}
