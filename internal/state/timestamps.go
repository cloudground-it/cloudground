package state

import (
	"database/sql"
	"time"
)

// Times are stored as text: RFC 3339 in UTC with as many fractional digits
// as the clock gives, which sorts as text in time order for the times the
// panel writes.

// now is the current time as the store writes it.
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// parseTime reads a stored time; text that is not one reads as the zero time.
func parseTime(v string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseTimePtr reads a nullable stored time: NULL or empty is nil.
func parseTimePtr(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseTime(v.String)
	return &t
}
