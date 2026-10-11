package site

import "strings"

// Prefixes of the two login names a site has (ops/sftp SPEC §5.1).
const (
	userPrefix  = "cg-site-"
	shellPrefix = "cg-shell-"
)

// ShellUser is the shell login of a site user: cg-site-<id> -> cg-shell-<id>,
// the same uid under a name sshd does not chroot. "" for a name that is not
// a site user.
func ShellUser(systemUser string) string {
	if !ValidSystemUser(systemUser) {
		return ""
	}
	return shellPrefix + strings.TrimPrefix(systemUser, userPrefix)
}

// SiteUserOfShell is the inverse of ShellUser ("" when shellUser is not a
// shell login).
func SiteUserOfShell(shellUser string) string {
	id, ok := strings.CutPrefix(shellUser, shellPrefix)
	if !ok {
		return ""
	}
	if u := userPrefix + id; ValidSystemUser(u) {
		return u
	}
	return ""
}
