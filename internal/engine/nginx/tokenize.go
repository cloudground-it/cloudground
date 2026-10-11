package nginx

import (
	"fmt"
	"strings"
)

// A reader of nginx's configuration language with nginx's own rules
// (ngx_conf_read_token, SPEC §7.2). The routing rules check the directives
// this produces, so it must split text exactly where nginx does: a
// directive it saw differently from nginx would be checked as one thing and
// loaded as another.

// directive is one directive with its arguments and, for a block, the
// directives inside it. Words are unquoted and unescaped, as nginx passes
// them to the module.
type directive struct {
	words []string
	// spans are the raw offsets [start, end) of each word in the text,
	// inside its quotes.
	spans [][2]int
	line  int
	at    int // offset of its first word
	// block is true for "name args { … }"; closeAt is the offset of its "}".
	block    bool
	children []*directive
	closeAt  int
}

func (d *directive) name() string { return strings.ToLower(d.words[0]) }

// parsed is a block of text read as nginx reads it.
type parsed struct {
	top []*directive
	// continued holds the offsets of the lines that begin inside a word
	// (a quoted string, or an escaped line break): their leading
	// whitespace belongs to the word and must not be touched.
	continued map[int]bool
}

// syntaxError is a refusal of the text itself, with its line.
type syntaxError struct {
	line int
	msg  string
}

func (e *syntaxError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.msg) }

// maxDepth bounds nested blocks (SPEC §7.1).
const maxDepth = 8

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// parseConf reads text into directives.
func parseConf(text string) (*parsed, error) {
	p := &parsed{continued: map[int]bool{}}
	line := 1
	var (
		words              []string
		spans              [][2]int
		wordStart          int
		wordStartRaw       int // wordStart before an opening quote is skipped
		wordLine           int
		inWord             bool // reading a word (last_space == 0)
		needSpace          bool // just after a closing quote
		dq, sq             bool
		escaped, variable  bool
		comment            bool
		stack              []*directive // open blocks
		directiveFirstLine int
		directiveAt        int
	)
	add := func(d *directive) {
		if len(stack) == 0 {
			p.top = append(p.top, d)
		} else {
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, d)
		}
	}
	endWord := func(end int) {
		raw := text[wordStart:end]
		if len(words) == 0 {
			directiveFirstLine, directiveAt = wordLine, wordStartRaw
		}
		words = append(words, unescapeWord(raw))
		spans = append(spans, [2]int{wordStart, end})
		inWord = false
	}
	// finish ends the directive at a ';' or a '{' at offset i.
	finish := func(c byte, i int) error {
		if len(words) == 0 {
			return &syntaxError{line, fmt.Sprintf("unexpected %q", c)}
		}
		d := &directive{words: words, spans: spans, line: directiveFirstLine, at: directiveAt}
		words, spans = nil, nil
		add(d)
		if c == '{' {
			if len(stack) == maxDepth {
				return &syntaxError{line, fmt.Sprintf("blocks nested deeper than %d", maxDepth)}
			}
			d.block = true
			stack = append(stack, d)
		}
		return nil
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\n' {
			line++
		}
		if comment {
			if c == '\n' {
				comment = false
			}
			continue
		}
		if escaped {
			escaped = false
			if c == '\n' {
				p.continued[i+1] = true
			}
			continue
		}
		if needSpace {
			switch {
			case isSpace(c):
				needSpace = false
				continue
			case c == ';' || c == '{':
				needSpace = false
				if err := finish(c, i); err != nil {
					return nil, err
				}
				continue
			case c == ')':
				// nginx takes ")" after a quote as the start of a new word.
				needSpace = false
			default:
				return nil, &syntaxError{line, fmt.Sprintf("unexpected %q after a quoted string", c)}
			}
		}
		if !inWord {
			if isSpace(c) {
				continue
			}
			wordStart, wordStartRaw, wordLine = i, i, line
			switch c {
			case ';', '{':
				if err := finish(c, i); err != nil {
					return nil, err
				}
				continue
			case '}':
				if len(words) != 0 {
					return nil, &syntaxError{line, `unexpected "}": the directive before it has no ";"`}
				}
				if len(stack) == 0 {
					return nil, &syntaxError{line, `unexpected "}": no block is open`}
				}
				stack[len(stack)-1].closeAt = i
				stack = stack[:len(stack)-1]
				continue
			case '#':
				comment = true
				continue
			case '\\':
				escaped, inWord = true, true
				continue
			case '"':
				wordStart++
				dq, inWord = true, true
				continue
			case '\'':
				wordStart++
				sq, inWord = true, true
				continue
			case '$':
				variable, inWord = true, true
				continue
			default:
				inWord = true
				continue
			}
		}
		// Inside a word.
		if c == '{' && variable {
			continue // "${name}"
		}
		variable = false
		switch {
		case c == '\\':
			escaped = true
			continue
		case c == '$':
			variable = true
			continue
		case dq:
			if c == '"' {
				dq = false
				endWord(i)
				needSpace = true
			} else if c == '\n' {
				p.continued[i+1] = true
			}
		case sq:
			if c == '\'' {
				sq = false
				endWord(i)
				needSpace = true
			} else if c == '\n' {
				p.continued[i+1] = true
			}
		case isSpace(c) || c == ';' || c == '{':
			endWord(i)
			if c == ';' || c == '{' {
				if err := finish(c, i); err != nil {
					return nil, err
				}
			}
		}
	}
	switch {
	case dq || sq:
		return nil, &syntaxError{line, "a quoted string is not closed"}
	case escaped:
		return nil, &syntaxError{line, `the text ends with "\"`}
	case inWord || len(words) > 0:
		return nil, &syntaxError{line, `the last directive has no ";"`}
	case len(stack) > 0:
		return nil, &syntaxError{stack[len(stack)-1].line, fmt.Sprintf("the %q block is not closed", stack[len(stack)-1].words[0])}
	}
	return p, nil
}

// unescapeWord is nginx's copy of a word: \" \' \\ stand for the
// character, \t \r \n for tab, CR and LF, any other \ stays.
func unescapeWord(raw string) string {
	if !strings.Contains(raw, `\`) {
		return raw
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) {
			switch raw[i+1] {
			case '"', '\'', '\\':
				i++
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			}
		}
		b.WriteByte(raw[i])
	}
	return b.String()
}
