// Package cronjob is a site's scheduled command as both sides of the agent
// socket see it. The API checks a job with Validate before it stores one, so
// the operator hears about a bad schedule at once; the agent checks it again
// and renders the crontab line from it with Line. The API never sends shell
// text: it sends Jobs, and only the agent writes a crontab.
//
// The format is the one Debian's and Ubuntu's cron (3.0pl1) read, measured on
// Ubuntu 24.04 and 26.04: five numeric fields, then the command field, which
// crontab(1) refuses past MaxCommandField bytes.
package cronjob

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Job is one scheduled command of a site.
type Job struct {
	ID       int64  `json:"id"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
	Enabled  bool   `json:"enabled"`
}

// MaxCommandField is the longest command field crontab(1) installs: 998
// bytes, as written (escapes included). One byte more and it refuses the
// whole crontab with "command too long", which stopped every job of the
// site.
const MaxCommandField = 998

// Error is a job that cannot be scheduled. Its text is safe to show.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// field is the inclusive range of one schedule field.
type field struct {
	name     string
	min, max int
}

var fields = [5]field{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day of month", 1, 31},
	{"month", 1, 12},
	{"day of week", 0, 7}, // 0 and 7 are both Sunday
}

// ValidateSchedule accepts five numeric fields separated by spaces or tabs.
// A field is a comma list of "*", "N" or "N-M", where "*" and a range may
// carry a step "/S". Names ("mon"), "@daily" and the like are refused, as is
// "N/S", which Debian's cron rejects although other crons accept it.
func ValidateSchedule(s string) error {
	for _, r := range s {
		if !strings.ContainsRune("0123456789*,-/ \t", r) {
			return invalid("the schedule may only contain digits, spaces and * , - /")
		}
	}
	parts := strings.Fields(s)
	if len(parts) != 5 {
		return invalid("the schedule needs 5 fields (minute hour day month weekday), e.g. */15 * * * *")
	}
	for i, p := range parts {
		if err := checkField(p, fields[i]); err != nil {
			return err
		}
	}
	return nil
}

func checkField(s string, f field) error {
	for _, item := range strings.Split(s, ",") {
		base, step, stepped := strings.Cut(item, "/")
		if stepped {
			n, ok := number(step)
			if !ok || n < 1 {
				return invalid("%s: step %q must be a number from 1", f.name, step)
			}
		}
		if base == "*" {
			continue
		}
		lo, hi, isRange := strings.Cut(base, "-")
		if stepped && !isRange {
			return invalid("%s: a step needs * or a range before it, e.g. */5 or 0-30/5", f.name)
		}
		a, ok := number(lo)
		if !ok || a < f.min || a > f.max {
			return invalid("%s: %q is not between %d and %d", f.name, lo, f.min, f.max)
		}
		if !isRange {
			continue
		}
		b, ok := number(hi)
		if !ok || b < f.min || b > f.max {
			return invalid("%s: %q is not between %d and %d", f.name, hi, f.min, f.max)
		}
		if a > b {
			return invalid("%s: the range %s runs backwards", f.name, base)
		}
	}
	return nil
}

// number parses a plain decimal, refusing an empty string and anything long
// enough to overflow.
func number(s string) (int, bool) {
	if s == "" || len(s) > 3 {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// ValidateCommand accepts one line of valid UTF-8 without control
// characters (a tab is allowed). A newline would start a second crontab
// entry.
func ValidateCommand(c string) error {
	if strings.TrimSpace(c) == "" {
		return invalid("the command is required")
	}
	if !utf8.ValidString(c) {
		return invalid("the command is not valid UTF-8")
	}
	for _, r := range c {
		if r != '\t' && unicode.IsControl(r) {
			return invalid("the command must be a single line without control characters")
		}
	}
	return nil
}

// Validate checks the whole job, including that its crontab line fits.
func (j Job) Validate() error {
	_, err := j.Line()
	return err
}

// LogPath is where a job's output goes, relative to the site root, which is
// the site user's home and so cron's working directory for its jobs.
func LogPath(id int64) string { return "logs/cron-" + strconv.FormatInt(id, 10) + ".log" }

// wrapper runs the job ($1) in a shell of its own, so its "exit", a "#" or
// an unbalanced quote stays inside it; appends both streams to the log ($2)
// between start and end markers; keeps the last 500 lines; and exits with
// the job's status. It is constant (the job and the log path reach it as
// arguments, never spliced into it) and holds no "%" (see Line).
const wrapper = `l=$2;{ echo;echo "[cloudground start $(date -Is)]";/bin/sh -c "$1";s=$?;` +
	`echo "[cloudground end $(date -Is) status=$s]";}>>"$l" 2>&1;` +
	`tail -n 500 "$l">"$l.t"&&mv -f "$l.t" "$l";exit $s`

// Wrapper is the wrapper as the scheduled runner passes it, as an argv
// word (no crontab line, no "%" escaping).
const Wrapper = wrapper

// cron turns an unescaped "%" into the end of the command (the rest becomes
// its standard input), and its escape "\%" cannot follow a backslash: cron
// reads `\\%` as a backslash and then a bare "%", so a job containing `\%`
// could not be written at all. The line therefore holds no "%": the shell
// cron starts makes one in $p, and the job's quoted word splices it in.
const (
	percentVar  = `p=$(printf '\045');`
	percentWord = `'"$p"'`
)

// Line is the job's crontab entry, without the newline. The schedule is
// normalised to single spaces.
func (j Job) Line() (string, error) {
	if j.ID <= 0 {
		return "", invalid("invalid job id %d", j.ID)
	}
	if err := ValidateSchedule(j.Schedule); err != nil {
		return "", err
	}
	if err := ValidateCommand(j.Command); err != nil {
		return "", err
	}
	job, prefix := Quote(j.Command), ""
	if strings.Contains(job, "%") {
		job, prefix = strings.ReplaceAll(job, "%", percentWord), percentVar
	}
	cmd := prefix + "/bin/sh -c " + Quote(wrapper) + " cg-cron " + job + " " + LogPath(j.ID)
	if len(cmd) > MaxCommandField {
		return "", invalid("the command is too long for cron: shorten it by %d bytes, or move it into a script", len(cmd)-MaxCommandField)
	}
	return strings.Join(strings.Fields(j.Schedule), " ") + " " + cmd, nil
}

// Quote makes s one single-quoted shell word.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
