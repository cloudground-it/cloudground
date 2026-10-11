package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const summaryHeader = "scenario\treq_per_sec\tp50\tp99\terrors"

// summary is one run: its rows by scenario name.
type summary map[string]row

// writeSummary writes the rows in summary order, skipping scenarios the run
// does not have.
func writeSummary(w io.Writer, s summary) error {
	if _, err := fmt.Fprintln(w, summaryHeader); err != nil {
		return err
	}
	for _, name := range summaryOrder {
		r, ok := s[name]
		if !ok {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.scenario, r.rpsText, r.p50, r.p99, r.errors); err != nil {
			return err
		}
	}
	return nil
}

// readSummary parses a summary.tsv. Rows of unknown scenarios are kept: a
// file written by a later version may have more.
func readSummary(rd io.Reader) (summary, error) {
	s := summary{}
	sc := bufio.NewScanner(rd)
	first := true
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		if first {
			first = false
			if line != summaryHeader {
				return nil, fmt.Errorf("not a summary.tsv: header is %q", line)
			}
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			return nil, fmt.Errorf("summary line %q has %d fields, want 5", line, len(f))
		}
		s[f[0]] = row{scenario: f[0], rpsText: f[1], p50: f[2], p99: f[3], errors: f[4]}
	}
	if first {
		return nil, fmt.Errorf("empty summary")
	}
	return s, sc.Err()
}

// loadRun reads <dir>/summary.tsv, and takes the non-2xx counts from the raw
// wrk files beside it when they are there.
func loadRun(dir string) (summary, error) {
	f, err := os.Open(filepath.Join(dir, "summary.tsv"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := readSummary(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	for name, r := range s {
		if raw, err := os.ReadFile(filepath.Join(dir, name+".txt")); err == nil {
			r.non2xx = parseWrk(name, string(raw)).non2xx
			s[name] = r
		}
	}
	return s, nil
}
