package site

import (
	"regexp"
	"unicode"
	"unicode/utf8"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Bounds of a site's maintenance page (#79, internal/engine SPEC §4.9).
const (
	MaxMaintenanceAllow    = 50
	MaxMaintenanceTitle    = 100
	MaxMaintenanceMessage  = 2000
	MinMaintenanceRetrySec = 60
	MaxMaintenanceRetrySec = 7 * 24 * 3600
)

var maintenanceKeyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidMaintenanceKey reports whether k can be a maintenance key.
func ValidMaintenanceKey(k string) bool { return maintenanceKeyRe.MatchString(k) }

// textOK reports whether s is text a page may show: valid UTF-8 without
// control characters other than line breaks.
func textOK(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r != '\n' && unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// CheckMaintenance refuses a maintenance page that could not be rendered as
// it is: the text and the key are pasted into nginx's configuration (the
// renderer encodes the text; the key is hexadecimal).
func CheckMaintenance(m state.SiteMaintenance) error {
	switch {
	case utf8.RuneCountInString(m.Title) > MaxMaintenanceTitle || !textOK(m.Title):
		return invalid("the maintenance title is at most %d characters of plain text", MaxMaintenanceTitle)
	case utf8.RuneCountInString(m.Message) > MaxMaintenanceMessage || !textOK(m.Message):
		return invalid("the maintenance message is at most %d characters of plain text", MaxMaintenanceMessage)
	case m.RetryAfterSec != 0 && (m.RetryAfterSec < MinMaintenanceRetrySec || m.RetryAfterSec > MaxMaintenanceRetrySec):
		return invalid("Retry-After must be %d–%d seconds", MinMaintenanceRetrySec, MaxMaintenanceRetrySec)
	case len(m.Allow) > MaxMaintenanceAllow:
		return invalid("at most %d addresses may see a site in maintenance", MaxMaintenanceAllow)
	case m.Enabled && !ValidMaintenanceKey(m.Key):
		return invalid("a site in maintenance needs its key")
	case m.Key != "" && !ValidMaintenanceKey(m.Key):
		return invalid("invalid maintenance key")
	}
	for _, a := range m.Allow {
		if n, err := NormalizeXMLRPCAllow(a); err != nil || n != a {
			return invalid("invalid address %q", a)
		}
	}
	return nil
}
