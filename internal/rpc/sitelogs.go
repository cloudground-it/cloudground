package rpc

import (
	"fmt"
	"regexp"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Downloads of a site's access and error logs (#84, internal/agent/ops/system
// SPEC §3b). The files are the ones nginx writes in the site's log directory
// and logrotate keeps: access.log, access.log.1, access.log.2.gz, … and the
// same for error.log.

// MaxSiteLogChunk bounds one read of a log: the API never holds more than
// this of a file, whatever its size.
const MaxSiteLogChunk = 4 << 20

var siteLogNameRe = regexp.MustCompile(`^(access|error)\.log(\.[0-9]{1,3})?(\.gz)?$`)

// ValidSiteLogName reports whether name is a log a site's owner may
// download: the current or a rotated access or error log, compressed or
// not. It holds no slash and no dot segment, so it cannot leave the site's
// log directory.
func ValidSiteLogName(name string) bool { return siteLogNameRe.MatchString(name) }

// SiteLogsParams names the site whose logs are listed.
type SiteLogsParams struct {
	Site state.Site `json:"site"`
}

// SiteLogFile is one log of the site: its name, size and last write.
type SiteLogFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// SiteLogsResult lists the site's logs, the current ones first, then each
// kind's rotated files from the newest.
type SiteLogsResult struct {
	Files []SiteLogFile `json:"files"`
}

// SiteLogChunkParams reads up to Length bytes of a log at Offset.
type SiteLogChunkParams struct {
	Site   state.Site `json:"site"`
	Name   string     `json:"name"`
	Offset int64      `json:"offset"`
	Length int        `json:"length"`
}

// SiteLogChunkResult is the bytes read and the file's size when they were.
// An offset at or past the end answers no bytes (the live log may have been
// rotated meanwhile).
type SiteLogChunkResult struct {
	Data []byte `json:"data"`
	Size int64  `json:"size"`
}

func (p *SiteLogsParams) Sites() []*state.Site     { return []*state.Site{&p.Site} }
func (p *SiteLogChunkParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a log name of ValidSiteLogName and a read of 1 byte to
// MaxSiteLogChunk from a non-negative offset.
func (p *SiteLogChunkParams) Validate() error {
	switch {
	case !ValidSiteLogName(p.Name):
		return fmt.Errorf("invalid log file %q", p.Name)
	case p.Length < 1 || p.Length > MaxSiteLogChunk || p.Offset < 0:
		return fmt.Errorf("read 1 byte to %d MiB from a non-negative offset", MaxSiteLogChunk>>20)
	}
	return nil
}
