// Package backuprepo validates a restic repository location, the one rule
// the API (when the setting is saved) and the agent (before every restic
// call) both apply. SPEC.md in this directory is the contract.
package backuprepo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// LocalRoots are where a local repository may live: the installer's backup
// folder, or a mount under /mnt.
var LocalRoots = []string{"/var/backups/cloudground/", "/mnt/"}

// Options are what a validation may depend on.
type Options struct {
	// AllowPrivate lets an s3: or rest: endpoint be on a loopback, private,
	// link-local or otherwise non-public address (the administrator's
	// CLOUDGROUND_BACKUP_PRIVATE_ENDPOINT).
	AllowPrivate bool
	// Rclone reports whether rclone is installed; nil refuses rclone:.
	Rclone func() bool
	// Resolve looks a host name up; nil is the system resolver with a
	// 5 s bound. Tests substitute it.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
}

// Error is a refusal; its text may be shown to the panel's user.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func refuse(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

var (
	b2Re      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{4,62}(:[A-Za-z0-9._/-]*)?$`)
	rcloneRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_. -]*:[A-Za-z0-9._/ -]*$`)
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	sftpUser  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	localPart = regexp.MustCompile(`^[A-Za-z0-9._@+=,-]+$`)
)

// Validate checks repo against the rules (SPEC.md §1).
func Validate(ctx context.Context, repo string, o Options) error {
	if repo == "" {
		return refuse("the backup repository is empty")
	}
	if len(repo) > 1024 {
		return refuse("the backup repository is too long")
	}
	for _, r := range repo {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '\t' {
			return refuse("the backup repository contains whitespace or a control character")
		}
	}
	switch {
	case strings.HasPrefix(repo, "/"):
		return local(repo)
	case strings.HasPrefix(repo, "s3:"):
		return s3(ctx, strings.TrimPrefix(repo, "s3:"), o)
	case strings.HasPrefix(repo, "rest:"):
		return rest(ctx, strings.TrimPrefix(repo, "rest:"), o)
	case strings.HasPrefix(repo, "b2:"):
		if !b2Re.MatchString(strings.TrimPrefix(repo, "b2:")) {
			return refuse("a b2: repository is b2:<bucket>[:<path>]")
		}
		return nil
	case strings.HasPrefix(repo, "sftp:"):
		return sftp(strings.TrimPrefix(repo, "sftp:"))
	case strings.HasPrefix(repo, "rclone:"):
		if o.Rclone == nil || !o.Rclone() {
			return refuse("rclone is not installed on this server, so an rclone: repository cannot be used")
		}
		if !rcloneRe.MatchString(strings.TrimPrefix(repo, "rclone:")) || strings.Contains(repo, "..") {
			return refuse("an rclone: repository is rclone:<remote>:<path>")
		}
		return nil
	}
	return refuse("unsupported backup repository: use s3:, b2:, sftp:, rest:https://, rclone: or a folder under /var/backups/cloudground or /mnt")
}

// local is an absolute, clean path below one of LocalRoots.
func local(p string) error {
	if path.Clean(p) != p || strings.Contains(p, "/../") {
		return refuse("a local backup repository must be a clean absolute path")
	}
	for _, root := range LocalRoots {
		if rest, ok := strings.CutPrefix(p, root); ok && rest != "" {
			for _, part := range strings.Split(rest, "/") {
				if !localPart.MatchString(part) || part == "." || part == ".." {
					return refuse("a local backup repository's folder names may hold letters, digits and . _ - only")
				}
			}
			return nil
		}
	}
	return refuse("a local backup repository must be under /var/backups/cloudground/ or /mnt/")
}

// s3 is s3:<host>[:port]/<bucket>[/path] or s3:http(s)://<host>[:port]/<bucket>.
func s3(ctx context.Context, rest string, o Options) error {
	var host string
	if strings.HasPrefix(rest, "https://") || strings.HasPrefix(rest, "http://") {
		u, err := url.Parse(rest)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return refuse("an s3: repository is s3:https://<host>/<bucket>")
		}
		host = u.Hostname()
	} else {
		hp, _, _ := strings.Cut(rest, "/")
		h, _, err := net.SplitHostPort(hp)
		if err != nil {
			h = hp
		}
		host = h
	}
	return endpoint(ctx, host, o)
}

// rest is rest:https://[user:pass@]<host>[:port]/[path]; plain http is
// refused (the repository password and the data would cross in the clear).
func rest(ctx context.Context, rest string, o Options) error {
	u, err := url.Parse(rest)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return refuse("a rest: repository must be rest:https://<host>/…")
	}
	return endpoint(ctx, u.Hostname(), o)
}

// sftp is sftp:user@host:/path or sftp://user@host[:port]//path; neither
// the user nor the host may start with "-" (ssh would read an option).
func sftp(rest string) error {
	var user, host string
	if r, ok := strings.CutPrefix(rest, "//"); ok {
		u, err := url.Parse("sftp://" + r)
		if err != nil || u.Host == "" {
			return refuse("an sftp: repository is sftp:user@host:/path")
		}
		host = u.Hostname()
		if u.User != nil {
			user = u.User.Username()
			if _, has := u.User.Password(); has {
				return refuse("an sftp: repository cannot carry a password; use a key")
			}
		}
	} else {
		hp, _, ok := strings.Cut(rest, ":")
		if !ok {
			return refuse("an sftp: repository is sftp:user@host:/path")
		}
		if u, h, ok := strings.Cut(hp, "@"); ok {
			user, host = u, h
		} else {
			host = hp
		}
	}
	if strings.HasPrefix(user, "-") || strings.HasPrefix(host, "-") {
		return refuse("an sftp: repository's user and host cannot start with -")
	}
	if user != "" && !sftpUser.MatchString(user) {
		return refuse("an sftp: repository's user may hold letters, digits and . _ - only")
	}
	if host == "" {
		return refuse("an sftp: repository needs a host")
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err != nil && !hostRe.MatchString(host) {
		return refuse("an sftp: repository's host is not a host name or address")
	}
	return nil
}

// endpoint refuses an s3:/rest: host that is, or resolves to, an address
// that is not public, unless o.AllowPrivate.
func endpoint(ctx context.Context, host string, o Options) error {
	host = strings.Trim(host, "[]")
	if host == "" {
		return refuse("the backup repository needs a host")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return public(a, host, o)
	}
	if !hostRe.MatchString(host) || strings.HasPrefix(host, "-") {
		return refuse("the backup repository's host is not a host name or address")
	}
	if o.AllowPrivate {
		return nil
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return refuse("the backup repository's host %s is this server; set CLOUDGROUND_BACKUP_PRIVATE_ENDPOINT=1 to allow a private endpoint", host)
	}
	resolve := o.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, h string) ([]netip.Addr, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
		}
	}
	addrs, err := resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) || err == nil {
			return refuse("the backup repository's host %s does not resolve", host)
		}
		return refuse("the backup repository's host %s could not be resolved", host)
	}
	for _, a := range addrs {
		if err := public(a.Unmap(), host, o); err != nil {
			return err
		}
	}
	return nil
}

// public refuses a non-public address.
func public(a netip.Addr, host string, o Options) error {
	if o.AllowPrivate {
		return nil
	}
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() || cgnat.Contains(a) || !a.IsGlobalUnicast() {
		return refuse("the backup repository's host %s is a private or local address; set CLOUDGROUND_BACKUP_PRIVATE_ENDPOINT=1 to allow a private endpoint", host)
	}
	return nil
}

// cgnat is the shared address space (RFC 6598), not public either.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")
