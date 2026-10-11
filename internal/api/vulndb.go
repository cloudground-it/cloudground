package api

// The public vulnerability database the WordPress check reads (#31, SPEC.md
// §11.6): WPVulnerability (https://www.wpvulnerability.com), whose data is
// CC0 1.0 (public domain, commercial use included) and whose API needs no
// key. It is asked one component at a time: a WordPress version, or a
// plugin or theme slug, never the site's name.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

const (
	// vulnDBBase is the API's address; vulnDBName and vulnDBSite are how
	// the panel credits it.
	vulnDBBase = "https://www.wpvulnerability.net"
	vulnDBName = "WPVulnerability"
	vulnDBSite = "https://www.wpvulnerability.com"
	// vulnDBTimeout bounds one request; vulnDBMaxBody one answer (the
	// largest plugin record, Elementor's, is about 170 KB).
	vulnDBTimeout = 20 * time.Second
	vulnDBMaxBody = 4 << 20
	// vulnCacheFor is how long an answer is reused, by every site that
	// has the same component; vulnCacheMax bounds the entries kept.
	vulnCacheFor = 6 * time.Hour
	vulnCacheMax = 4096
	// vulnLookups is how many requests one check sends at a time.
	vulnLookups = 4
)

// vulnSlugRe is a plugin or theme slug as WordPress.org names them (the
// directory name); vulnVersionRe a version the database can be asked about.
var (
	vulnSlugRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,199}$`)
	vulnVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+[0-9A-Za-z.+-]{0,40}$`)
)

// vulnRecord is one vulnerability as the database lists it for a
// component, reduced to what a finding needs.
type vulnRecord struct {
	id, title, severity, cve, link string
	// The affected range: each bound a version and an operator (lt, le,
	// gt, ge, eq); a missing bound is "". always: the record was listed
	// for the exact version asked about (core), so no range applies.
	minV, minOp, maxV, maxOp string
	unfixed, always          bool
}

// vulnDB asks the database and caches its answers.
type vulnDB struct {
	base   string
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]vulnEntry
}

type vulnEntry struct {
	at   time.Time
	list []vulnRecord
}

func newVulnDB() *vulnDB {
	return &vulnDB{base: vulnDBBase, client: &http.Client{Timeout: vulnDBTimeout}, now: time.Now}
}

// lookup is what the database knows about one component: "core" with a
// version, or "plugin"/"theme" with a slug.
func (d *vulnDB) lookup(ctx context.Context, component, name string) ([]vulnRecord, error) {
	key := component + ":" + name
	d.mu.Lock()
	if e, ok := d.cache[key]; ok && d.now().Sub(e.at) < vulnCacheFor {
		d.mu.Unlock()
		return e.list, nil
	}
	d.mu.Unlock()
	list, err := d.fetch(ctx, component, name)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cache == nil || len(d.cache) >= vulnCacheMax {
		d.prune()
	}
	d.cache[key] = vulnEntry{at: d.now(), list: list}
	return list, nil
}

// prune drops the stale entries, and everything when that is not enough:
// the cache only saves requests, so losing it costs one more round.
func (d *vulnDB) prune() {
	if d.cache == nil {
		d.cache = map[string]vulnEntry{}
		return
	}
	for k, e := range d.cache {
		if d.now().Sub(e.at) >= vulnCacheFor {
			delete(d.cache, k)
		}
	}
	if len(d.cache) >= vulnCacheMax {
		d.cache = map[string]vulnEntry{}
	}
}

// wpvAnswer is the database's envelope; Data is null for a component it
// does not know.
type wpvAnswer struct {
	Error   json.RawMessage `json:"error"`
	Message json.RawMessage `json:"message"`
	Data    *struct {
		Vulnerability []wpvVuln `json:"vulnerability"`
	} `json:"data"`
}

type wpvVuln struct {
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	Operator *struct {
		MinVersion  *string         `json:"min_version"`
		MinOperator *string         `json:"min_operator"`
		MaxVersion  *string         `json:"max_version"`
		MaxOperator *string         `json:"max_operator"`
		Unfixed     json.RawMessage `json:"unfixed"`
	} `json:"operator"`
	Source []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Link string `json:"link"`
	} `json:"source"`
	// Impact is an object, or an empty list when the database has none.
	Impact json.RawMessage `json:"impact"`
}

// fetch asks the database about one component.
func (d *vulnDB) fetch(ctx context.Context, component, name string) ([]vulnRecord, error) {
	target := d.base + "/" + component + "/" + url.PathEscape(name) + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "CloudGround/"+version.Version+" (+https://cloudground.it)")
	req.Header.Set("Accept", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%s did not answer: %w", vulnDBName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", vulnDBName, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, vulnDBMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", vulnDBName, err)
	}
	if len(body) > vulnDBMaxBody {
		return nil, fmt.Errorf("%s sent more than %d MB for %s %s", vulnDBName, vulnDBMaxBody>>20, component, name)
	}
	return parseVulnAnswer(body, component == "core")
}

// parseVulnAnswer reads one answer. exact: the list is for the version asked
// about (core), not a list of ranges.
func parseVulnAnswer(body []byte, exact bool) ([]vulnRecord, error) {
	var a wpvAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("%s sent an answer that is not its JSON: %w", vulnDBName, err)
	}
	if code := looseText(a.Error); code != "" && code != "0" {
		msg := looseText(a.Message)
		if msg == "" {
			msg = "error " + code
		}
		return nil, fmt.Errorf("%s: %s", vulnDBName, firstLine(msg))
	}
	if a.Data == nil {
		return nil, nil
	}
	out := make([]vulnRecord, 0, len(a.Data.Vulnerability))
	for _, v := range a.Data.Vulnerability {
		r := vulnRecord{id: v.UUID, always: exact}
		if op := v.Operator; op != nil {
			r.minV, r.minOp = deref(op.MinVersion), deref(op.MinOperator)
			r.maxV, r.maxOp = deref(op.MaxVersion), deref(op.MaxOperator)
			r.unfixed = looseText(op.Unfixed) == "1"
		}
		r.title, r.link, r.cve = describeVuln(v)
		r.severity = vulnSeverity(v.Impact)
		if r.id == "" {
			r.id = r.cve + "|" + r.title
		}
		out = append(out, r)
	}
	return out, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// looseText is a JSON string or number as text; null or absent is "".
func looseText(v json.RawMessage) string {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || string(v) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	return string(v)
}

// describeVuln picks a vulnerability's title, link and CVE from its
// sources: the first source with a title of its own (a vendor's
// advisory: "Contact Form 7 <= 5.3.1 - Arbitrary File Upload"), else the
// database's name for it; the CVE is the first source that is one.
func describeVuln(v wpvVuln) (title, link, cve string) {
	for _, s := range v.Source {
		if cve == "" && strings.HasPrefix(s.ID, "CVE-") {
			cve = s.ID
		}
		if title == "" && s.Name != "" && s.Name != s.ID && !strings.HasPrefix(s.Name, "CVE-") {
			title, link = s.Name, s.Link
		}
	}
	if title == "" {
		title = v.Name
	}
	if link == "" && len(v.Source) > 0 {
		link = v.Source[0].Link
	}
	if !strings.HasPrefix(link, "https://") {
		link = "" // shown as a link in the panel: nothing but https
	}
	title = strings.Join(strings.Fields(html.UnescapeString(title)), " ")
	if len(title) > 300 {
		title = title[:300] + "…"
	}
	return title, link, cve
}

// vulnSeverity is the CVSS severity the database gives (critical, high,
// medium, low), "" when it gives none.
func vulnSeverity(raw json.RawMessage) string {
	var im struct {
		CVSS *struct {
			Severity string `json:"severity"`
		} `json:"cvss"`
		CVSS3 *struct {
			Severity string `json:"severity"`
		} `json:"cvss3"`
	}
	if json.Unmarshal(raw, &im) != nil { // an empty list: no impact given
		return ""
	}
	words := map[string]string{"c": "critical", "h": "high", "m": "medium", "l": "low",
		"critical": "critical", "high": "high", "medium": "medium", "low": "low"}
	for _, c := range []*struct {
		Severity string `json:"severity"`
	}{im.CVSS, im.CVSS3} {
		if c != nil {
			if w := words[strings.ToLower(c.Severity)]; w != "" {
				return w
			}
		}
	}
	return ""
}

// affects reports whether the record covers version v. A bound with an
// operator this code does not know covers nothing: a guess either way
// would be wrong, and the next answer may be clearer.
func (r vulnRecord) affects(v string) bool {
	if r.always {
		return true
	}
	if r.minV == "" && r.maxV == "" {
		return false
	}
	for _, b := range [][2]string{{r.minV, r.minOp}, {r.maxV, r.maxOp}} {
		if b[0] == "" {
			continue
		}
		ok, known := versionOp(compareVersions(v, b[0]), b[1])
		if !known || !ok {
			return false
		}
	}
	return true
}

// fixedIn is the first version without the vulnerability, when the record
// says ("< 5.3.2" is fixed in 5.3.2).
func (r vulnRecord) fixedIn() string {
	if r.unfixed || r.always || r.maxOp != "lt" {
		return ""
	}
	return r.maxV
}

func versionOp(c int, op string) (ok, known bool) {
	switch op {
	case "lt":
		return c < 0, true
	case "le":
		return c <= 0, true
	case "gt":
		return c > 0, true
	case "ge":
		return c >= 0, true
	case "eq":
		return c == 0, true
	}
	return false, false
}

// compareVersions compares two versions as PHP's version_compare does,
// which is how WordPress and the database order them: parts split at . -
// _ + and between digits and letters; numbers compare as numbers; the
// words order dev < alpha = a < beta = b < RC = rc < (a number) < pl = p;
// a missing part is lower than a number and higher than a pre-release word
// (1.0 < 1.0.1, 1.0rc1 < 1.0).
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		switch {
		case i >= len(pa):
			return -partRank(pb[i])
		case i >= len(pb):
			return partRank(pa[i])
		}
		if c := comparePart(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	return 0
}

// partRank is how a part left over at the end of the longer version moves
// it against the shorter: a number or a patch level up, a pre-release down.
func partRank(p string) int {
	if isNumber(p) || specialRank(p) > specialRank("#") {
		return 1
	}
	return -1
}

func versionParts(v string) []string {
	var parts []string
	var cur []rune
	kind := 0 // 0 none, 1 digit, 2 other
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range strings.TrimSpace(v) {
		switch {
		case r == '.' || r == '-' || r == '_' || r == '+':
			flush()
			kind = 0
		case unicode.IsDigit(r):
			if kind == 2 {
				flush()
			}
			kind = 1
			cur = append(cur, r)
		default:
			if kind == 1 {
				flush()
			}
			kind = 2
			cur = append(cur, r)
		}
	}
	flush()
	return parts
}

func isNumber(p string) bool {
	_, err := strconv.ParseUint(p, 10, 64)
	return err == nil
}

// specialRank orders the words PHP knows; "#" stands for a number. A word
// it does not know is below them all.
func specialRank(p string) int {
	switch strings.ToLower(p) {
	case "dev":
		return 0
	case "alpha", "a":
		return 1
	case "beta", "b":
		return 2
	case "rc":
		return 3
	case "#":
		return 4
	case "pl", "p":
		return 5
	}
	return -1
}

func comparePart(a, b string) int {
	na, nb := isNumber(a), isNumber(b)
	switch {
	case na && nb:
		x, _ := strconv.ParseUint(a, 10, 64)
		y, _ := strconv.ParseUint(b, 10, 64)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case na:
		a = "#"
	case nb:
		b = "#"
	}
	ra, rb := specialRank(a), specialRank(b)
	switch {
	case ra < rb:
		return -1
	case ra > rb:
		return 1
	}
	return 0
}

// finding is the record as a finding about component name at version v.
func (r vulnRecord) finding(component, slug, v string) state.VulnFinding {
	return state.VulnFinding{ID: r.id, Component: component, Slug: slug, Version: v, Title: r.title,
		Severity: r.severity, CVE: r.cve, FixedIn: r.fixedIn(), Unfixed: r.unfixed, Link: r.link}
}
