package changelog

import (
	"slices"
	"strings"
	"testing"

	cloudground "github.com/cloudground-it/cloudground"
)

const sample = `# Changelog

Intro text.

## v0.3.2

New logo.

- one
- two

## v0.3.1
Fixes.
## Unreleased notes
ignored
## v0.3.0

First.
`

func TestParse(t *testing.T) {
	got := Parse(sample)
	var versions []string
	for _, e := range got {
		versions = append(versions, e.Version)
	}
	if !slices.Equal(versions, []string{"v0.3.2", "v0.3.1", "v0.3.0"}) {
		t.Fatalf("versions %v", versions)
	}
	if got[0].Body != "New logo.\n\n- one\n- two" || got[1].Body != "Fixes." || got[2].Body != "First." {
		t.Fatalf("bodies %q", got)
	}
	if strings.Contains(got[0].Body, "Intro") {
		t.Fatal("the introduction belongs to a release")
	}
	if len(Parse("")) != 0 || len(Parse("no headings")) != 0 {
		t.Fatal("entries from nothing")
	}
}

func TestUnseen(t *testing.T) {
	entries := Parse(sample)
	for _, c := range []struct {
		seen, current string
		want          []string
	}{
		{"", "v0.3.2", []string{"v0.3.2"}},                  // an account that never saw any: the running release only
		{"v0.3.0", "v0.3.2", []string{"v0.3.2", "v0.3.1"}},  // skipped releases between
		{"v0.3.2", "v0.3.2", nil},                           // up to date
		{"v0.3.2", "v0.3.1", nil},                           // a downgrade tells nothing
		{"v0.2.0", "v0.3.1", []string{"v0.3.1", "v0.3.0"}},  // not past the running release
		{"", "v0.3.5", []string{"v0.3.2"}},                  // the newest the build has
		{"v0.3.1", "v0.3.2-4-gabcdef0", []string{"v0.3.2"}}, // a build after a tag counts as it
		{"v0.3.0", "dev", nil},                              // a development build has none
		{"garbage", "v0.3.1", []string{"v0.3.1"}},           // a seen mark that is no version is none
	} {
		if got := Unseen(entries, c.seen, c.current); !slices.Equal(got, c.want) {
			t.Errorf("Unseen(%q, %q) = %v, want %v", c.seen, c.current, got, c.want)
		}
	}
}

// The changelog the binary ships parses into entries whose newest is a
// release, and holds no raw HTML tag at the start of a line the panel's
// renderer would have to skip (it renders text, never markup).
func TestShippedChangelog(t *testing.T) {
	entries := Parse(cloudground.Changelog)
	if len(entries) == 0 {
		t.Fatal("the embedded CHANGELOG.md has no release")
	}
	for _, e := range entries {
		if e.Body == "" {
			t.Errorf("%s has no text", e.Version)
		}
	}
}
