package cronjob

import (
	"strings"
	"testing"
	"unicode"
)

// A job Line accepts is one crontab line: no line break, no control
// character, no "%" (cron turns it into a newline and feeds the rest to
// stdin), and the schedule is five fields of digits and * , - /. Its
// command, unquoted by sh, is the job's command exactly.
func FuzzJobLine(f *testing.F) {
	for _, s := range [][2]string{
		{"*/5 * * * *", "php artisan schedule:run"},
		{"0 3 * * 1-5", "echo 100% done"},
		{"@daily", "true"},
		{"* * * * *", "echo 'a'\nrm -rf /"},
		{"1 2 3 4 5 6", "x"},
		{"* * * * *", "printf '%s' \"$HOME\""},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, schedule, command string) {
		line, err := Job{ID: 7, Schedule: schedule, Command: command, Enabled: true}.Line()
		if err != nil {
			return
		}
		if strings.ContainsAny(line, "\r\n%") {
			t.Fatalf("line %q", line)
		}
		for _, r := range line {
			if r != '\t' && unicode.IsControl(r) {
				t.Fatalf("control character in %q", line)
			}
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			t.Fatalf("line %q", line)
		}
		for _, f := range fields[:5] {
			if strings.Trim(f, "0123456789*,-/") != "" {
				t.Fatalf("schedule field %q in %q", f, line)
			}
		}
		if ValidateCommand(command) != nil {
			t.Fatalf("Line accepted a command ValidateCommand refuses: %q", command)
		}
	})
}

// Quote's output is one sh word whose value is s.
func FuzzQuote(f *testing.F) {
	for _, s := range []string{"", "a", "'", "a'b", `\'`, "$(x)", "`x`"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q := Quote(s)
		if got, ok := unquoteSh(q); !ok || got != s {
			t.Fatalf("Quote(%q) = %q, reads back as %q (%v)", s, q, got, ok)
		}
	})
}

// unquoteSh reads a word made of '…' runs and \' escapes, as sh does, and
// fails on anything else outside quotes.
func unquoteSh(w string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(w); {
		switch {
		case w[i] == '\'':
			j := strings.IndexByte(w[i+1:], '\'')
			if j < 0 {
				return "", false
			}
			b.WriteString(w[i+1 : i+1+j])
			i += j + 2
		case w[i] == '\\' && i+1 < len(w) && w[i+1] == '\'':
			b.WriteByte('\'')
			i += 2
		default:
			return "", false
		}
	}
	return b.String(), true
}
