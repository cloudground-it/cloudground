package system

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
)

const noEntries = "-- No entries --"

// journalTail returns the last n entries of the units matching unit, one per
// line in the shape of `journalctl -o short-iso`, decoded from -o json.
func (o *Ops) journalTail(ctx context.Context, unit string, n int) (string, error) {
	res, err := o.Exec.Run(ctx, host.Cmd{
		Name:    "journalctl",
		Args:    []string{"--no-pager", "-q", "-o", "json", "-n", strconv.Itoa(n), "-u", unit},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return "", fmt.Errorf("read the journal of %s: %w", unit, err)
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		e, err := decodeEntry(sc.Bytes())
		if err != nil {
			return "", fmt.Errorf("decode the journal of %s: %w", unit, err)
		}
		l := e.String()
		if len(l) > maxLineLen {
			l = l[:maxLineLen] + lineCut
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read the journal of %s: %w", unit, err)
	}
	if len(lines) == 0 {
		return noEntries, nil
	}
	return strings.Join(lines, "\n"), nil
}

// entry is the part of a journal record a log line shows.
type entry struct {
	At         time.Time
	Host       string
	Identifier string
	PID        string
	Message    string
}

// String renders e like `journalctl -o short-iso`.
func (e entry) String() string {
	ident := e.Identifier
	if e.PID != "" {
		ident += "[" + e.PID + "]"
	}
	return fmt.Sprintf("%s %s %s: %s", e.At.Local().Format(time.RFC3339), e.Host, ident, e.Message)
}

// decodeEntry decodes one line of `journalctl -o json`. A field is a string,
// an array of bytes (a value that is not valid UTF-8), an array of either (a
// field set more than once) or null.
func decodeEntry(line []byte) (entry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return entry{}, err
	}
	field := func(name string) string { return journalValue(raw[name]) }
	var e entry
	if us, err := strconv.ParseInt(field("__REALTIME_TIMESTAMP"), 10, 64); err == nil {
		e.At = time.UnixMicro(us)
	}
	e.Host = field("_HOSTNAME")
	e.Identifier = field("SYSLOG_IDENTIFIER")
	if e.Identifier == "" {
		e.Identifier = field("_COMM")
	}
	e.PID = field("SYSLOG_PID")
	if e.PID == "" {
		e.PID = field("_PID")
	}
	e.Message = field("MESSAGE")
	return e, nil
}

// journalValue decodes one field value; anything unexpected reads as "".
func journalValue(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var ints []int
	if json.Unmarshal(v, &ints) == nil {
		b := make([]byte, 0, len(ints))
		for _, c := range ints {
			b = append(b, byte(c))
		}
		return strings.ToValidUTF8(string(b), "\uFFFD")
	}
	var many []json.RawMessage
	if json.Unmarshal(v, &many) == nil && len(many) > 0 {
		return journalValue(many[0])
	}
	return ""
}
