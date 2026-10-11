package db

import (
	"errors"
	"io"
	"os"
	"regexp"
)

// fileStatements are statements that read or write a file of a machine:
// the server refuses them (local_infile off, and no account of a site has
// FILE), the client too (--local-infile=0). A dump that has them fails, and
// the person is told why before the import starts (SPEC §5).
var fileStatements = []struct {
	re   *regexp.Regexp
	warn string
}{
	{regexp.MustCompile(`(?i)\bLOAD\s+DATA\s+(?:LOW_PRIORITY\s+|CONCURRENT\s+)?(?:LOCAL\s+)?INFILE\b`),
		"the file has LOAD DATA … INFILE statements: this server refuses them (local files are off), so the import will fail on them"},
	{regexp.MustCompile(`(?i)\bLOAD\s+XML\s+(?:LOW_PRIORITY\s+|CONCURRENT\s+)?(?:LOCAL\s+)?INFILE\b`),
		"the file has LOAD XML … INFILE statements: this server refuses them (local files are off), so the import will fail on them"},
	{regexp.MustCompile(`(?i)\bINTO\s+(?:OUTFILE|DUMPFILE)\s*['"]`),
		"the file has SELECT … INTO OUTFILE/DUMPFILE statements: a site's account may not write files on the server, so the import will fail on them"},
}

// scanChunk is how much of the file is matched at once; scanOverlap is
// kept from the previous chunk, so a statement split across two chunks is
// still seen (the patterns are far shorter).
const (
	scanChunk   = 1 << 20
	scanOverlap = 256
)

// fileStatementWarnings reads the SQL file at path and returns a warning for
// each kind of file statement in it. It is a warning, not a filter: the
// refusal is the server's and the client's, and a match may be in a string.
func fileStatementWarnings(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return scanFileStatements(f)
}

func scanFileStatements(r io.Reader) ([]string, error) {
	found := make([]bool, len(fileStatements))
	buf := make([]byte, scanOverlap+scanChunk)
	kept := 0
	for {
		n, err := io.ReadFull(r, buf[kept:])
		window := buf[:kept+n]
		for i, s := range fileStatements {
			if !found[i] && s.re.Match(window) {
				found[i] = true
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		kept = copy(buf, window[len(window)-scanOverlap:])
	}
	var out []string
	for i, s := range fileStatements {
		if found[i] {
			out = append(out, s.warn)
		}
	}
	return out, nil
}
