package traffic

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func line(addr, ts, req string, status int, bytes int64, cache, ua string) string {
	return fmt.Sprintf(`%s - shop.example.com [%s] "%s" %d %d 0.004 cache:%s "%s"`, addr, ts, req, status, bytes, cache, ua)
}

const firefox = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"

func TestParseLine(t *testing.T) {
	h, ok := ParseLine(line("203.0.113.9", "27/Oct/2026:10:15:00 +0100", "GET /blog/post/?utm_source=x HTTP/2.0", 200, 5120, "HIT", firefox) + "\n")
	if !ok || h.Addr != "203.0.113.9" || h.Method != "GET" || h.Path != "/blog/post/" || h.Status != 200 || h.Bytes != 5120 || h.Cache != "HIT" || h.UA != firefox {
		t.Fatalf("parsed %+v %v", h, ok)
	}
	if h.Time.UTC().Hour() != 9 {
		t.Fatalf("time %v", h.Time)
	}
	// A quote in a variable is \x22; garbage as the request still counts.
	h, ok = ParseLine(line("2001:db8::1", "27/Oct/2026:10:15:00 +0000", `\x16\x03\x01`, 400, 0, "-", `x\x22y`))
	if !ok || h.Method != "" || h.Path != "" || h.UA != `x\x22y` {
		t.Fatalf("garbage request: %+v %v", h, ok)
	}
	if h, ok = ParseLine(line("1.2.3.4", "27/Oct/2026:10:15:00 +0000", "GET /"+strings.Repeat("a", 300), 200, 1, "MISS", firefox)); !ok || len(h.Path) != maxPath {
		t.Fatalf("long path: %d %v", len(h.Path), ok)
	}
	for _, bad := range []string{
		"", "junk", `1.2.3.4 - h [nope] "GET / HTTP/1.1" 200 1 0.1 cache:- "ua"`,
		`1.2.3.4 - h [27/Oct/2026:10:15:00 +0000] "GET / HTTP/1.1" 999 1 0.1 cache:- "ua"`,
		`1.2.3.4 - h [27/Oct/2026:10:15:00 +0000] "GET / HTTP/1.1" 200 x 0.1 cache:- "ua"`,
		`1.2.3.4 - h [27/Oct/2026:10:15:00 +0000] "GET / HTTP/1.1" 200 1 0.1 "ua"`,
	} {
		if _, ok := ParseLine(bad); ok {
			t.Errorf("took %q", bad)
		}
	}
}

func TestKinds(t *testing.T) {
	for _, c := range []struct {
		method, path, ua string
		status, want     int
	}{
		{"GET", "/", firefox, 200, kindPage},
		{"GET", "/shop/", firefox, 304, kindPage},
		{"GET", "/a.css", firefox, 200, kindOther},
		{"GET", "/wp-admin/edit.php", firefox, 200, kindOther},
		{"GET", "/robots.txt", firefox, 200, kindOther},
		{"POST", "/", firefox, 200, kindOther},
		{"GET", "/missing", firefox, 404, kindOther},
		{"GET", "/", "Googlebot/2.1", 200, kindBot},
		{"GET", "/", "curl/8.5.0", 200, kindBot},
		{"GET", "/", "", 200, kindBot},
		{"GET", "/", "CloudGround-CacheWarmer/1.0", 200, kindInternal},
		{"GET", "/", "CloudGround-Uptime/v0.7.0", 200, kindInternal},
	} {
		if got := kind(Hit{Method: c.method, Path: c.path, UA: c.ua, Status: c.status}); got != c.want {
			t.Errorf("%+v = %d", c, got)
		}
	}
}

func TestSiteCounts(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	var s Site
	add := func(addr, ts, req string, status int, cache, ua string) {
		h, ok := ParseLine(line(addr, ts, req, status, 100, cache, ua))
		if !ok {
			t.Fatalf("line %s %s", ts, req)
		}
		s.Add(h, now)
	}
	add("203.0.113.1", "27/Oct/2026:10:00:00 +0000", "GET / HTTP/2.0", 200, "HIT", firefox)
	add("203.0.113.1", "27/Oct/2026:10:01:00 +0000", "GET /about/ HTTP/2.0", 200, "MISS", firefox)
	add("203.0.113.2", "27/Oct/2026:11:00:00 +0000", "GET / HTTP/2.0", 200, "BYPASS", firefox)
	add("203.0.113.2", "27/Oct/2026:11:00:01 +0000", "GET /style.css HTTP/2.0", 200, "-", firefox)
	add("203.0.113.3", "27/Oct/2026:11:00:02 +0000", "GET /gone HTTP/2.0", 404, "-", firefox)
	add("66.249.66.1", "27/Oct/2026:11:00:03 +0000", "GET / HTTP/1.1", 200, "HIT", "Googlebot/2.1")
	add("127.0.0.1", "27/Oct/2026:11:00:04 +0000", "GET / HTTP/1.1", 200, "HIT", "CloudGround-CacheWarmer/1.0")
	// Out of order: yesterday comes after today; too old and too far ahead are dropped.
	add("203.0.113.4", "26/Oct/2026:23:59:00 +0000", "GET / HTTP/2.0", 500, "-", firefox)
	add("203.0.113.5", "01/Jan/2026:00:00:00 +0000", "GET / HTTP/2.0", 200, "-", firefox)
	add("203.0.113.6", "01/Jan/2027:00:00:00 +0000", "GET / HTTP/2.0", 200, "-", firefox)

	if len(s.Days) != 2 || s.Days[0].Date != "2026-10-26" {
		t.Fatalf("days %v", len(s.Days))
	}
	d := s.Days[1].Counts
	want := Counts{Requests: 6, Pageviews: 3, Visitors: 2, Bots: 1, Bytes: 600, Status2xx: 5, Status4xx: 1, CacheHit: 2, CacheMiss: 1, CacheBypass: 1}
	if d != want {
		t.Fatalf("today\n got %+v\nwant %+v", d, want)
	}
	if s.Days[1].HourPageviews[10] != 2 || s.Days[1].HourRequests[11] != 4 {
		t.Fatalf("hours %v %v", s.Days[1].HourPageviews, s.Days[1].HourRequests)
	}

	st := s.Stats(7, now)
	if len(st.Days) != 7 || st.Days[6].Date != "2026-10-27" || st.Days[0].Date != "2026-10-21" || len(st.Hours) != 48 {
		t.Fatalf("range %d days, %d hours", len(st.Days), len(st.Hours))
	}
	if st.Totals.Requests != 7 || st.Totals.Status5xx != 1 {
		t.Fatalf("totals %+v", st.Totals)
	}
	if len(st.TopPages) != 2 || st.TopPages[0] != (PathCount{"/", 2}) || len(st.TopNotFound) != 1 || st.TopNotFound[0].Path != "/gone" {
		t.Fatalf("tops %v %v", st.TopPages, st.TopNotFound)
	}
	if last := st.Hours[47]; !last.Time.Equal(now) {
		t.Fatalf("last hour %v", last.Time)
	}

	// No address in what is stored, beyond the salted sketch.
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "203.0.113") {
		t.Fatal("an address is stored")
	}
	// Two days later the sketches are gone, the counts stay.
	s.Settle(now.AddDate(0, 0, 2))
	for _, d := range s.Days {
		if d.Sketch != nil {
			t.Fatalf("%s keeps its sketch", d.Date)
		}
	}
	if s.Days[1].Visitors != 2 {
		t.Fatal("the visitors went with the sketch")
	}
	s.Settle(now.AddDate(0, 0, 100))
	if len(s.Days) != 0 {
		t.Fatal("days past 90 kept")
	}
}

func TestSketchEstimate(t *testing.T) {
	for _, n := range []int{1, 50, 10000} {
		// A fixed salt: the standard error is 1.6 %, so a fresh salt's
		// estimate falls outside 5 % about once in 500 runs.
		s := NewSketch()
		s.Salt = "0123456789abcdef0123456789abcdef"
		for i := range n {
			s.Add(fmt.Sprintf("10.0.%d.%d", i/256, i%256), firefox)
			s.Add(fmt.Sprintf("10.0.%d.%d", i/256, i%256), firefox) // twice: counted once
		}
		got := s.Estimate()
		if diff := float64(got-int64(n)) / float64(n); diff > 0.05 || diff < -0.05 {
			t.Errorf("%d pairs estimated %d", n, got)
		}
	}
	// Every sketch has a salt of its own.
	a, b := NewSketch(), NewSketch()
	a.Add("1.2.3.4", firefox)
	b.Add("1.2.3.4", firefox)
	if a.Salt == b.Salt {
		t.Fatal("salts repeat")
	}
}

func TestTopIsBounded(t *testing.T) {
	top := Top{}
	for i := range 500 {
		top.add(fmt.Sprintf("/p%d", i), 1)
	}
	for range 50 {
		top.add("/hot", 1)
	}
	if len(top) != topCounters || top["/hot"] < 50 {
		t.Fatalf("%d counters, /hot %d", len(top), top["/hot"])
	}
	if got := topOf(top); len(got) != topAnswer || got[0].Path != "/hot" {
		t.Fatalf("top %v", got[:3])
	}
}
