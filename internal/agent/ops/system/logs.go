package system

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
)

const (
	defaultLines = 200
	maxLines     = 2000
	tailWindow   = 4 << 20 // bytes read from the end of a log file
	maxLineLen   = 8 << 10
	lineCut      = " …(line truncated)"
	noLogYet     = "(log is empty or not created yet)"
)

// journalSources are the logs that live in the journal: source → unit
// pattern and the Path reported for it.
var journalSources = map[string]struct{ unit, path string }{
	"agent":     {"cloudground-agent.service", "journal:cloudground-agent"},
	"api":       {"cloudground-api.service", "journal:cloudground-api"},
	"php-error": {sitePHPPattern, "journal:php-fpm"},
}

// siteLogFiles maps a site log kind to its file in <LogRoot>/<domain>/.
var siteLogFiles = map[string]string{"access": "access.log", "error": "error.log", "php": "php-error.log", "cleanup": "cleanup.log", "app": "app.log"}

// TailLog returns the last lines of one known log (SPEC.md §3).
func (o *Ops) TailLog(ctx context.Context, p rpc.TailLogParams) (rpc.TailLogResult, error) {
	n := p.Lines
	if n <= 0 || n > maxLines {
		n = defaultLines
	}
	if j, ok := journalSources[p.Source]; ok {
		text, err := o.journalTail(ctx, j.unit, n)
		if err != nil {
			return rpc.TailLogResult{}, err
		}
		return rpc.TailLogResult{Path: j.path, Content: text}, nil
	}
	path, err := o.logFile(p.Source, p.Site)
	if err != nil {
		return rpc.TailLogResult{}, err
	}
	text, err := tailFile(path, n)
	if err != nil {
		return rpc.TailLogResult{}, err
	}
	return rpc.TailLogResult{Path: path, Content: text}, nil
}

// logFile resolves a file-backed source to its path. A site source names its
// domain; the Site parameter, when given, must agree with it.
func (o *Ops) logFile(source, siteParam string) (string, error) {
	switch source {
	case "nginx-access":
		return filepath.Join(o.Paths.NginxLogDir, "access.log"), nil
	case "nginx-error":
		return filepath.Join(o.Paths.NginxLogDir, "error.log"), nil
	case "mariadb":
		return filepath.Join(o.Paths.MariaDBLogDir, "error.log"), nil
	}
	rest, ok := strings.CutPrefix(source, "site:")
	if !ok {
		return "", agentrpc.Invalid("unknown log source %q", source)
	}
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return "", agentrpc.Invalid("unknown log source %q", source)
	}
	domain, kind := rest[:i], rest[i+1:]
	file, ok := siteLogFiles[kind]
	if !ok {
		return "", agentrpc.Invalid("unknown log source %q", source)
	}
	if domain == "" {
		domain = siteParam
	}
	if siteParam != "" && siteParam != domain {
		return "", agentrpc.Invalid("log source %q does not belong to site %q", source, siteParam)
	}
	if err := site.ValidateDomain(domain); err != nil {
		return "", agentrpc.Invalid("invalid site %q", domain)
	}
	return filepath.Join(o.Paths.LogRoot, domain, file), nil
}

// tailFile returns the last n lines of a log file, reading at most its last
// tailWindow bytes. A symlink at the name is refused (O_NOFOLLOW) and a FIFO
// cannot block the open (O_NONBLOCK); only a regular file is read.
func tailFile(path string, n int) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return noLogYet, nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	off := max(fi.Size()-tailWindow, 0)
	buf, err := io.ReadAll(io.NewSectionReader(f, off, tailWindow))
	if err != nil {
		return "", err
	}
	text := string(buf)
	if off > 0 {
		// The window opened inside a line: drop the fragment.
		if _, after, found := strings.Cut(text, "\n"); found {
			text = after
		}
	}
	return lastLines(text, n), nil
}

// lastLines keeps the final n lines of text, each cut at maxLineLen.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	lines = lines[max(len(lines)-n, 0):]
	for i, l := range lines {
		if len(l) > maxLineLen {
			lines[i] = l[:maxLineLen] + lineCut
		}
	}
	return strings.Join(lines, "\n")
}
