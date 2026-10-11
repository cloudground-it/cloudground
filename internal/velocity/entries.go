package velocity

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// CacheKey is the vhost's fastcgi_cache_key for one request (SPEC §2).
func CacheKey(scheme, method, host, requestURI string) string {
	return strings.Join([]string{scheme, method, host, requestURI}, "")
}

// CacheFilePath is where nginx stores the entry for key under dir, with
// levels=1:2 (SPEC §2).
func CacheFilePath(dir, key string) string {
	sum := md5.Sum([]byte(key))
	name := hex.EncodeToString(sum[:])
	n := len(name)
	return filepath.Join(dir, name[n-1:], name[n-3:n-1], name)
}

// SanitizeCacheDirName is the cache directory name of domain (SPEC §2).
func SanitizeCacheDirName(domain string) string {
	var b strings.Builder
	for _, r := range domain {
		switch {
		case 'A' <= r && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		case 'a' <= r && r <= 'z', '0' <= r && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// removeEntry deletes one cache file and reports whether it was there.
func removeEntry(path string) (bool, error) {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// keyHost is a host as nginx's $host holds it: lower case, no port
// (SPEC §2). An IPv6 literal keeps its brackets.
func keyHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
		if strings.Contains(h, ":") {
			host = "[" + h + "]"
		}
	}
	return strings.ToLower(host)
}

// purgeHosts is the URL's host and then extra, as $host values, each once.
func purgeHosts(u *url.URL, extra []string) []string {
	hosts := []string{keyHost(u.Host)}
	for _, h := range extra {
		if h = keyHost(h); !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// PurgeURLs removes the GET and HEAD entries of each URL, on both schemes,
// under the URL's host and every one of extraHosts (SPEC §3).
func PurgeURLs(dir string, urls []string, extraHosts ...string) (int, error) {
	removed := 0
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return removed, fmt.Errorf("invalid purge url %q", raw)
		}
		uri := u.RequestURI()
		for _, host := range purgeHosts(u, extraHosts) {
			for _, scheme := range [...]string{"https", "http"} {
				for _, method := range [...]string{"GET", "HEAD"} {
					gone, err := removeEntry(CacheFilePath(dir, CacheKey(scheme, method, host, uri)))
					if err != nil {
						return removed, fmt.Errorf("purge %s: %w", raw, err)
					}
					if gone {
						removed++
					}
				}
			}
		}
	}
	return removed, nil
}

// PurgeAll empties dir and counts the entries it held (SPEC §4).
func PurgeAll(dir string) (int, error) {
	levels, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, l := range levels {
		top := filepath.Join(dir, l.Name())
		_ = filepath.WalkDir(top, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				removed++
			}
			return nil
		})
		if err := os.RemoveAll(top); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
