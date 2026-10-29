package site

import (
	"strings"
	"testing"
)

func TestCheckGitRepo(t *testing.T) {
	const root = "/srv/sites/example.com"
	for _, ok := range []string{
		"https://github.com/acme/shop.git",
		"https://git.example.com:8443/team/app",
		"ssh://git@gitlab.com/acme/shop.git",
		"ssh://git@example.com:2222/srv/repo.git",
		"git@github.com:acme/shop.git",
		"deploy@git.example.com:team/app",
		"file://" + root + "/tmp/repo.git",
	} {
		if err := CheckGitRepo(ok, root); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "-oProxyCommand=x", "--upload-pack=x", "http://example.com/a.git", "git://example.com/a.git",
		"ext::sh -c id", "https://user:pw@example.com/a.git", "https://user@example.com/a.git",
		"https://example.com", "https://example.com/", "https://example.com/../x", "https://example.com/a?b=c",
		"https://exa mple.com/a", "https://example.com/a\nb", "ssh://example.com/a.git", "ssh://git:pw@example.com/a.git",
		"git@example.com:-oProxyCommand=x", "git@example.com:../x", "ftp://example.com/a",
		"file:///etc", "file:///srv/sites/other.com/tmp/r", "file://" + root, "file://" + root + "/../other/r",
		"file://host/srv/sites/example.com/r", "/srv/sites/example.com/r", "file://" + root + "/a/../b",
		// N-8-1: one slash or two is not a file URL to git.
		"file:" + root + "/tmp/repo.git", "file:" + root, "FILE://" + root + "/tmp/repo.git",
		"https://" + strings.Repeat("a", 600),
	} {
		if err := CheckGitRepo(bad, root); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckGitBranch(t *testing.T) {
	for _, ok := range []string{"main", "release/1.2", "feature_x-1", "v1.0"} {
		if err := CheckGitBranch(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "/x", ".x", "x/", "x.", "x.lock", "a..b", "a//b", "a/.b", "a b", "a;b", "a~1", strings.Repeat("a", 101)} {
		if err := CheckGitBranch(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckGitBuildCommand(t *testing.T) {
	for _, ok := range []string{"", "npm ci && npm run build", "composer install --no-dev"} {
		if err := CheckGitBuildCommand(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"a\nb", "a\x00b", strings.Repeat("a", 1025)} {
		if err := CheckGitBuildCommand(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
