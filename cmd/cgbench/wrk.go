package main

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"
)

// row is one scenario's line in summary.tsv (bench/SPEC.md §3). The text
// fields keep wrk's own spelling; rps is parsed for arithmetic.
type row struct {
	scenario string
	rpsText  string
	p50      string
	p99      string
	errors   string
	// non2xx is wrk's "Non-2xx or 3xx responses" count. It is not a
	// summary.tsv column, so it is zero for a row read back from one.
	non2xx int
}

const missing = "na"

// rps returns the req/s as a number, and false when the run produced none.
func (r row) rps() (float64, bool) {
	v, err := strconv.ParseFloat(r.rpsText, 64)
	if err != nil || r.rpsText == missing {
		return 0, false
	}
	return v, true
}

// hasErrors reports whether wrk logged any socket error for the run.
func (r row) hasErrors() bool {
	return r.errors != "" && r.errors != "none"
}

var non2xxLine = regexp.MustCompile(`^\s*Non-2xx or 3xx responses:\s*(\d+)`)

// parseWrk reads one scenario's wrk output. Fields wrk did not print (a run
// that could not connect prints almost nothing) come back as "na", and the
// errors field as "none" when there is no Socket errors line.
func parseWrk(name, out string) row {
	r := row{scenario: name, rpsText: missing, p50: missing, p99: missing, errors: "none"}
	sawP50, sawP99, sawErr := false, false, false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		f := strings.Fields(line)
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "Requests/sec:") && len(f) >= 2:
			r.rpsText = f[1]
		case len(f) >= 2 && f[0] == "50%" && !sawP50:
			r.p50, sawP50 = f[1], true
		case len(f) >= 2 && f[0] == "99%" && !sawP99:
			r.p99, sawP99 = f[1], true
		case strings.HasPrefix(strings.TrimSpace(line), "Socket errors:") && !sawErr:
			r.errors = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Socket errors:"))
			sawErr = true
		default:
			if m := non2xxLine.FindStringSubmatch(line); m != nil {
				r.non2xx, _ = strconv.Atoi(m[1])
			}
		}
	}
	return r
}
