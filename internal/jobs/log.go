package jobs

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// The bounds of a job's log (SPEC.md §5).
const (
	LogHeadBytes = 16 << 10
	LogTailBytes = 48 << 10
	LogLineBytes = 2 << 10
)

// boundedLog keeps the first lines of a log whole and its last lines up to a
// size, counting what falls out between them.
type boundedLog struct {
	head, tail string
	dropped    int64
}

// capLine cuts a line longer than LogLineBytes, at a character boundary.
func capLine(s string) string {
	if len(s) <= LogLineBytes {
		return s
	}
	cut := LogLineBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// add appends one line (capped by the caller).
func (l *boundedLog) add(line string) {
	line += "\n"
	if l.tail == "" && l.dropped == 0 && len(l.head)+len(line) <= LogHeadBytes {
		l.head += line
		return
	}
	l.tail += line
	for len(l.tail) > LogTailBytes {
		i := strings.IndexByte(l.tail, '\n')
		if i < 0 || i+1 >= len(l.tail) {
			// One piece longer than the tail (a line with no newline cannot
			// come from add): drop all of it but the newest bytes.
			cut := len(l.tail) - LogTailBytes
			l.dropped += int64(cut)
			l.tail = l.tail[cut:]
			break
		}
		l.dropped += int64(i + 1)
		l.tail = l.tail[i+1:]
	}
}

// String is the log as read: head, a marker if anything was dropped, tail.
func (l boundedLog) String() string {
	return composeLog(l.head, l.tail, l.dropped)
}

func composeLog(head, tail string, dropped int64) string {
	if dropped == 0 {
		return head + tail
	}
	return head + "… " + strconv.FormatInt(dropped, 10) + " bytes of log omitted …\n" + tail
}
