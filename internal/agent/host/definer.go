package host

import (
	"bufio"
	"io"
	"regexp"
)

// definerRe is a DEFINER clause as mariadb-dump, mysqldump and people
// write it: DEFINER = user[@host] with each part backquoted, quoted or
// bare, or CURRENT_USER / CURRENT_ROLE with or without (). Matched at the
// start of the lookahead, leftmost-longest, so CURRENT_USERX is a bare
// name and CURRENT_USER() takes its parentheses.
var definerRe = func() *regexp.Regexp {
	part := "(?:`(?:[^`]|``)*`|'(?:[^'\\\\]|\\\\.|'')*'|\"(?:[^\"\\\\]|\\\\.|\"\")*\"|[A-Za-z0-9_$.%-]+)"
	re := regexp.MustCompile(`(?i)^DEFINER\s*=\s*(?:CURRENT_USER(?:\s*\(\s*\))?|CURRENT_ROLE(?:\s*\(\s*\))?|` +
		part + `(?:\s*@\s*` + part + `)?)`)
	re.Longest()
	return re
}()

// definerLookahead bounds a DEFINER clause: a user is at most 80 characters
// and a host 255, quoted and doubled far below this.
const definerLookahead = 2048

// isIdent reports whether c can be part of an unquoted identifier.
func isIdent(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// lexer states.
const (
	inCode = iota
	inSingle
	inDouble
	inBacktick
	inLineComment
	inBlockComment
)

// RewriteDefiners copies the SQL of src to dst with the user of every
// DEFINER clause replaced by definer (SR-T7): `site_N`@`localhost` when
// the import runs as the site's own account, CURRENT_USER for a scratch
// database. It follows quotes and comments, so a DEFINER= inside a string
// (a post that quotes SQL) is left as it is; executable comments (/*! … */,
// /*M! … */), where mariadb-dump puts trigger and view definers, are code.
//
// It is not the security boundary: a clause it does not see is refused by
// the server, since the importing account has no SET USER. It returns the
// number of clauses replaced.
func RewriteDefiners(dst io.Writer, src io.Reader, definer string) (int, error) {
	in := bufio.NewReaderSize(src, 64<<10)
	out := bufio.NewWriterSize(dst, 64<<10)
	n, err := rewriteDefiners(out, in, definer)
	if ferr := out.Flush(); err == nil {
		err = ferr
	}
	return n, err
}

func rewriteDefiners(out *bufio.Writer, in *bufio.Reader, definer string) (int, error) {
	state := inCode
	prevIdent := false // the last code byte could end an identifier
	replaced := 0
	for {
		c, err := in.ReadByte()
		if err == io.EOF {
			return replaced, nil
		}
		if err != nil {
			return replaced, err
		}
		switch state {
		case inCode:
			switch {
			case c == '\'':
				state = inSingle
			case c == '"':
				state = inDouble
			case c == '`':
				state = inBacktick
			case c == '#':
				state = inLineComment
			case c == '-':
				if next, _ := in.Peek(2); len(next) == 2 && next[0] == '-' && (next[1] == ' ' || next[1] == '\t' || next[1] == '\n' || next[1] == '\r') {
					state = inLineComment
				}
			case c == '/':
				if next, _ := in.Peek(2); len(next) >= 1 && next[0] == '*' {
					// An executable comment (/*!, /*M!) is code; any other
					// is skipped. Its "/*" goes out as is either way.
					if !(len(next) == 2 && (next[1] == '!' || next[1] == 'M')) {
						state = inBlockComment
					}
					if err := out.WriteByte(c); err != nil {
						return replaced, err
					}
					b, _ := in.ReadByte()
					c = b
				}
			case (c == 'D' || c == 'd') && !prevIdent:
				ahead, _ := in.Peek(definerLookahead)
				clause := append([]byte{c}, ahead...)
				if m := definerRe.Find(clause); m != nil {
					if _, err := in.Discard(len(m) - 1); err != nil {
						return replaced, err
					}
					if _, err := out.WriteString("DEFINER=" + definer); err != nil {
						return replaced, err
					}
					replaced++
					prevIdent = false
					continue
				}
			}
			if state == inCode {
				prevIdent = isIdent(c)
			} else {
				prevIdent = false
			}
		case inSingle, inDouble, inBacktick:
			quote := byte('\'')
			if state == inDouble {
				quote = '"'
			} else if state == inBacktick {
				quote = '`'
			}
			if c == '\\' && state != inBacktick {
				if err := out.WriteByte(c); err != nil {
					return replaced, err
				}
				b, err := in.ReadByte()
				if err == io.EOF {
					return replaced, nil
				}
				if err != nil {
					return replaced, err
				}
				c = b
			} else if c == quote {
				if next, _ := in.Peek(1); len(next) == 1 && next[0] == quote {
					// A doubled quote stays inside.
					if err := out.WriteByte(c); err != nil {
						return replaced, err
					}
					c, _ = in.ReadByte()
				} else {
					state = inCode
					prevIdent = false
				}
			}
		case inLineComment:
			if c == '\n' {
				state = inCode
				prevIdent = false
			}
		case inBlockComment:
			if c == '*' {
				if next, _ := in.Peek(1); len(next) == 1 && next[0] == '/' {
					if err := out.WriteByte(c); err != nil {
						return replaced, err
					}
					c, _ = in.ReadByte()
					state = inCode
					prevIdent = false
				}
			}
		}
		if err := out.WriteByte(c); err != nil {
			return replaced, err
		}
	}
}
