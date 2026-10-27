package site

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cloudground-it/cloudground/internal/cronjob"
)

// The rules a site's git deploy settings obey (internal/api SPEC §15.1),
// held by the API before it stores them and by the agent before it runs
// anything from them.

// Limits of the settings.
const (
	MaxGitRepo         = 512
	MaxGitBranch       = 100
	MaxGitBuildCommand = 1024
	// DefaultGitBranch is the branch a site deploys when none is named.
	DefaultGitBranch = "main"
)

var (
	gitBranchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	gitHostRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	gitUserRe   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// An scp-style address: user@host:path, the path not starting with "-"
	// or "/-" and made of plain characters.
	gitScpRe = regexp.MustCompile(`^([A-Za-z0-9._-]+)@([A-Za-z0-9][A-Za-z0-9.-]*):([A-Za-z0-9._~/+-]+)$`)
)

// CheckGitBranch refuses a branch name that is not a plain git branch.
func CheckGitBranch(b string) error {
	switch {
	case b == "" || len(b) > MaxGitBranch:
		return fmt.Errorf("the branch is 1 to %d characters", MaxGitBranch)
	case !gitBranchRe.MatchString(b):
		return errors.New("the branch may only use letters, digits and . _ / -")
	case strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasPrefix(b, "."):
		return errors.New("the branch cannot start with - / or .")
	case strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock"):
		return errors.New("the branch cannot end with / . or .lock")
	case strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "/."):
		return errors.New("the branch cannot contain .. // or /.")
	}
	return nil
}

// CheckGitBuildCommand refuses a build command that is not one line.
func CheckGitBuildCommand(c string) error {
	if c == "" {
		return nil
	}
	if len(c) > MaxGitBuildCommand {
		return fmt.Errorf("the build command is at most %d characters", MaxGitBuildCommand)
	}
	return cronjob.ValidateCommand(c)
}

// CheckGitRepo refuses a repository address that is not one of the four
// forms: https://host/path, ssh://user@host[:port]/path, user@host:path
// and file:///path inside root (the site's own directory). No address may
// carry a password or start with a dash.
func CheckGitRepo(repo, root string) error {
	if repo == "" || len(repo) > MaxGitRepo {
		return fmt.Errorf("the repository is 1 to %d characters", MaxGitRepo)
	}
	if !utf8.ValidString(repo) {
		return errors.New("the repository is not valid UTF-8")
	}
	for _, r := range repo {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("the repository cannot contain spaces or control characters")
		}
	}
	if strings.HasPrefix(repo, "-") {
		return errors.New("the repository cannot start with -")
	}
	if m := gitScpRe.FindStringSubmatch(repo); m != nil && !strings.Contains(repo, "://") {
		if strings.HasPrefix(m[3], "-") || strings.Contains(m[3], "..") {
			return errors.New("the repository's path is not valid")
		}
		return nil
	}
	u, err := url.Parse(repo)
	if err != nil || u.Scheme == "" {
		return errors.New("the repository is https://host/path, ssh://user@host/path, user@host:path or a file:// path inside the site")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the repository's address cannot have a query or a fragment")
	}
	switch u.Scheme {
	case "https":
		if _, has := u.User.Password(); has || u.User.Username() != "" {
			return errors.New("the repository's address cannot carry a user or a password: use the deploy key for a private repository")
		}
		if !gitHostRe.MatchString(u.Hostname()) || u.Path == "" || u.Path == "/" || strings.Contains(u.Path, "..") {
			return errors.New("the repository is https://host/path")
		}
	case "ssh":
		if _, has := u.User.Password(); has || !gitUserRe.MatchString(u.User.Username()) {
			return errors.New("an ssh repository is ssh://user@host/path, with no password")
		}
		if !gitHostRe.MatchString(u.Hostname()) || u.Path == "" || u.Path == "/" || strings.Contains(u.Path, "..") {
			return errors.New("an ssh repository is ssh://user@host/path")
		}
	case "file":
		if u.Host != "" || !filepath.IsAbs(u.Path) || filepath.Clean(u.Path) != u.Path || root == "" {
			return errors.New("a file repository is file:///path")
		}
		if rel, err := filepath.Rel(root, u.Path); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			return errors.New("a file repository must be inside the site's directory " + root)
		}
	default:
		return errors.New("the repository's scheme is https, ssh or file (or user@host:path)")
	}
	return nil
}
