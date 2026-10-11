package files

import (
	"bytes"
	"unicode/utf8"
)

// sniffLen is how much of a file is searched for a NUL byte.
const sniffLen = 8 << 10

// The encodings ReadFile reports for an editable file.
const (
	encUTF8 = "utf-8"
	encBOM  = "utf-8-bom"
)

var bom = []byte{0xEF, 0xBB, 0xBF}

type kind int

const (
	text kind = iota
	textBOM
	binary
)

// classify decides whether b, the start of a file (all of it when whole), is
// text the editor can hold (SPEC §4): no NUL in the first sniffLen bytes and
// valid UTF-8 after an optional byte order mark. When the file goes on past
// b, a character cut in two at the end of b does not count against it.
func classify(b []byte, whole bool) kind {
	if bytes.IndexByte(b[:min(len(b), sniffLen)], 0) >= 0 {
		return binary
	}
	k := text
	if bytes.HasPrefix(b, bom) {
		b, k = b[len(bom):], textBOM
	}
	if !whole {
		b = dropCutRune(b)
	}
	if !utf8.Valid(b) {
		return binary
	}
	return k
}

// dropCutRune trims an incomplete UTF-8 sequence from the end of b.
func dropCutRune(b []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		c := b[len(b)-i]
		if !utf8.RuneStart(c) {
			continue
		}
		if !utf8.FullRune(b[len(b)-i:]) {
			return b[:len(b)-i]
		}
		break
	}
	return b
}
