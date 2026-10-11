package wp

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// configLimit bounds a wp-config.php read for its table prefix.
const configLimit = 1 << 20

var (
	// tablePrefixRe is a table prefix WordPress accepts (wp-load refuses
	// any other character), not empty. Nothing else is read or written.
	tablePrefixRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	// prefixLineRe is a statement that starts by assigning $table_prefix;
	// prefixAssignRe is the one form read: a quoted literal, nothing else
	// on the statement.
	prefixLineRe   = regexp.MustCompile(`(?m)^[ \t]*\$table_prefix\b`)
	prefixAssignRe = regexp.MustCompile(`(?m)^([ \t]*\$table_prefix[ \t]*=[ \t]*)(['"])([^'"\r\n]*)(['"])([ \t]*;)`)
)

// ErrNoTablePrefix is a wp-config.php whose $table_prefix cannot be read as
// one literal assignment.
var ErrNoTablePrefix = errors.New("wp-config.php has no single $table_prefix assignment that can be read")

// ValidTablePrefix reports whether p is a table prefix WordPress accepts.
func ValidTablePrefix(p string) bool { return tablePrefixRe.MatchString(p) }

// ParseTablePrefix reads $table_prefix from the text of a wp-config.php
// without running it (SPEC §5, table prefix): exactly one statement may
// assign it, at the start of a line, and it must be a quoted literal that
// is a valid prefix. Anything else (a computed value, two assignments, an
// empty or odd prefix) is ErrNoTablePrefix or invalid.
func ParseTablePrefix(config []byte) (string, error) {
	if len(prefixLineRe.FindAllIndex(config, 2)) != 1 {
		return "", ErrNoTablePrefix
	}
	m := prefixAssignRe.FindSubmatch(config)
	if m == nil || string(m[2]) != string(m[4]) {
		return "", ErrNoTablePrefix
	}
	if p := string(m[3]); ValidTablePrefix(p) {
		return p, nil
	}
	return "", agentrpc.Invalid("the table prefix in wp-config.php is not one WordPress accepts")
}

// TablePrefix reads $table_prefix from the site's shared/wp-config.php
// (SPEC §5, table prefix).
func (o *Ops) TablePrefix(_ context.Context, s state.Site) (string, error) {
	r, err := o.OpenSite(s)
	if err != nil {
		return "", err
	}
	defer r.Close()
	b, err := r.ReadFile(sharedConfig, configLimit)
	if err != nil {
		return "", fmt.Errorf("read wp-config.php: %w", err)
	}
	return ParseTablePrefix(b)
}

// SetTablePrefix rewrites the one $table_prefix assignment of the site's
// shared/wp-config.php to prefix, in one rename (every request reads the
// old file or the new one), keeping its owner and mode.
func (o *Ops) SetTablePrefix(_ context.Context, s state.Site, prefix string) error {
	if !ValidTablePrefix(prefix) {
		return agentrpc.Invalid("invalid table prefix")
	}
	r, err := o.OpenSite(s)
	if err != nil {
		return err
	}
	defer r.Close()
	b, err := r.ReadFile(sharedConfig, configLimit)
	if err != nil {
		return fmt.Errorf("read wp-config.php: %w", err)
	}
	if _, err := ParseTablePrefix(b); err != nil {
		return err
	}
	out := prefixAssignRe.ReplaceAll(b, []byte("${1}'"+prefix+"'${5}"))
	if err := r.WriteFile(sharedConfig, out, 0o600); err != nil {
		if errors.Is(err, sitefs.ErrNotRegular) {
			return agentrpc.Invalid("wp-config.php is not a regular file")
		}
		return fmt.Errorf("write wp-config.php: %w", err)
	}
	return nil
}
