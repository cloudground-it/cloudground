package site

import "github.com/cloudground-it/cloudground/internal/state"

// Bounds of a site's scheduled database cleanup (#78).
const (
	// DefaultKeepRevisions is how many revisions of a post stay when the
	// site names none.
	DefaultKeepRevisions = 5
	MinKeepRevisions     = 1
	MaxKeepRevisions     = 100
)

// KeepRevisions is the number of revisions per post a cleanup leaves.
func KeepRevisions(c state.WPCleanup) int {
	if c.KeepRevisions == 0 {
		return DefaultKeepRevisions
	}
	return c.KeepRevisions
}

// CheckWPCleanup refuses a cleanup setting out of bounds: the number goes
// into a SQL statement.
func CheckWPCleanup(c state.WPCleanup) error {
	if c.KeepRevisions != 0 && (c.KeepRevisions < MinKeepRevisions || c.KeepRevisions > MaxKeepRevisions) {
		return invalid("keep between %d and %d revisions of each post", MinKeepRevisions, MaxKeepRevisions)
	}
	return nil
}
