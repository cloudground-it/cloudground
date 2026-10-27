package traffic

import (
	"cmp"
	"slices"
	"time"
)

// Bounds (SPEC §5, §6).
const (
	topCounters = 100
	topAnswer   = 20
	keepDays    = 90
	dateLayout  = "2006-01-02"
)

// Top is a bounded count of paths: space-saving (SPEC §5).
type Top map[string]int64

// add counts one path.
func (t Top) add(path string, n int64) {
	if _, ok := t[path]; ok || len(t) < topCounters {
		t[path] += n
		return
	}
	minPath, minN := "", int64(-1)
	for p, c := range t {
		if minN < 0 || c < minN || (c == minN && p > minPath) {
			minPath, minN = p, c
		}
	}
	delete(t, minPath)
	t[path] = minN + n
}

// Counts are a day's (or a range's) totals (SPEC §3).
type Counts struct {
	Requests    int64 `json:"requests"`
	Pageviews   int64 `json:"pageviews"`
	Visitors    int64 `json:"visitors"`
	Bots        int64 `json:"bots"`
	Bytes       int64 `json:"bytes"`
	Status2xx   int64 `json:"status_2xx"`
	Status3xx   int64 `json:"status_3xx"`
	Status4xx   int64 `json:"status_4xx"`
	Status5xx   int64 `json:"status_5xx"`
	CacheHit    int64 `json:"cache_hit"`
	CacheMiss   int64 `json:"cache_miss"`
	CacheBypass int64 `json:"cache_bypass"`
}

func (c *Counts) plus(o Counts) {
	c.Requests += o.Requests
	c.Pageviews += o.Pageviews
	c.Visitors += o.Visitors
	c.Bots += o.Bots
	c.Bytes += o.Bytes
	c.Status2xx += o.Status2xx
	c.Status3xx += o.Status3xx
	c.Status4xx += o.Status4xx
	c.Status5xx += o.Status5xx
	c.CacheHit += o.CacheHit
	c.CacheMiss += o.CacheMiss
	c.CacheBypass += o.CacheBypass
}

// Day is one UTC day of a site (SPEC §3).
type Day struct {
	Date string `json:"date"`
	Counts
	HourRequests  [24]int64 `json:"hour_requests"`
	HourPageviews [24]int64 `json:"hour_pageviews"`
	Pages         Top       `json:"pages"`
	NotFound      Top       `json:"not_found"`
	// Sketch counts the day's visitors until the day is two days old.
	Sketch *Sketch `json:"sketch,omitempty"`
}

// Site is one site's aggregate: its days, oldest first (SPEC §6).
type Site struct {
	Days []*Day `json:"days"`
}

// day finds or makes the day of date.
func (s *Site) day(date string) *Day {
	i, found := slices.BinarySearchFunc(s.Days, date, func(d *Day, t string) int { return cmp.Compare(d.Date, t) })
	if found {
		return s.Days[i]
	}
	d := &Day{Date: date, Pages: Top{}, NotFound: Top{}}
	s.Days = slices.Insert(s.Days, i, d)
	return d
}

// Add counts one hit in its day; it reports whether it was counted.
func (s *Site) Add(h Hit, now time.Time) bool {
	k := kind(h)
	if k == kindInternal {
		return false
	}
	t := h.Time.UTC()
	now = now.UTC()
	if t.Before(startOfDay(now).AddDate(0, 0, -(keepDays-1))) || t.After(now.Add(48*time.Hour)) {
		return false
	}
	d := s.day(t.Format(dateLayout))
	if d.Pages == nil {
		d.Pages = Top{}
	}
	if d.NotFound == nil {
		d.NotFound = Top{}
	}
	d.Requests++
	d.HourRequests[t.Hour()]++
	d.Bytes += h.Bytes
	switch h.Status / 100 {
	case 2:
		d.Status2xx++
	case 3:
		d.Status3xx++
	case 4:
		d.Status4xx++
	case 5:
		d.Status5xx++
	}
	switch h.Cache {
	case "HIT", "STALE", "UPDATING", "REVALIDATED":
		d.CacheHit++
	case "MISS", "EXPIRED":
		d.CacheMiss++
	case "BYPASS":
		d.CacheBypass++
	}
	if h.Status == 404 && h.Path != "" {
		d.NotFound.add(h.Path, 1)
	}
	switch k {
	case kindBot:
		d.Bots++
	case kindPage:
		d.Pageviews++
		d.HourPageviews[t.Hour()]++
		d.Pages.add(h.Path, 1)
		if d.Sketch == nil {
			if t.Before(startOfDay(now).AddDate(0, 0, -1)) {
				return true // its sketch is gone: the count stays as it was
			}
			d.Sketch = NewSketch()
		}
		d.Sketch.Add(h.Addr, h.UA)
		d.Visitors = d.Sketch.Estimate()
	}
	return true
}

// Settle drops the days past those kept and the sketches older than
// yesterday, with their salts (SPEC §4, §6).
func (s *Site) Settle(now time.Time) {
	today := startOfDay(now.UTC())
	oldest := today.AddDate(0, 0, -(keepDays - 1)).Format(dateLayout)
	yesterday := today.AddDate(0, 0, -1).Format(dateLayout)
	kept := s.Days[:0]
	for _, d := range s.Days {
		if d.Date < oldest {
			continue
		}
		if d.Date < yesterday && d.Sketch != nil {
			d.Visitors = d.Sketch.Estimate()
			d.Sketch = nil
		}
		kept = append(kept, d)
	}
	s.Days = kept
}

// Hour is one hour's requests and page views.
type Hour struct {
	Time      time.Time `json:"time"`
	Requests  int64     `json:"requests"`
	Pageviews int64     `json:"pageviews"`
}

// DayCounts is a day of a range.
type DayCounts struct {
	Date string `json:"date"`
	Counts
}

// PathCount is one path of a top list.
type PathCount struct {
	Path  string `json:"path"`
	Count int64  `json:"count"`
}

// Stats answers a range of days (SPEC §6).
type Stats struct {
	Days        []DayCounts `json:"days"`
	Hours       []Hour      `json:"hours"`
	Totals      Counts      `json:"totals"`
	TopPages    []PathCount `json:"top_pages"`
	TopNotFound []PathCount `json:"top_not_found"`
}

// Stats is the last days days up to now's, every one present.
func (s *Site) Stats(days int, now time.Time) Stats {
	now = now.UTC()
	today := startOfDay(now)
	byDate := map[string]*Day{}
	for _, d := range s.Days {
		byDate[d.Date] = d
	}
	out := Stats{Days: make([]DayCounts, 0, days), Hours: make([]Hour, 0, 48)}
	pages, missing := Top{}, Top{}
	for i := days - 1; i >= 0; i-- {
		date := today.AddDate(0, 0, -i).Format(dateLayout)
		dc := DayCounts{Date: date}
		if d := byDate[date]; d != nil {
			dc.Counts = d.Counts
			for p, n := range d.Pages {
				pages[p] += n
			}
			for p, n := range d.NotFound {
				missing[p] += n
			}
		}
		out.Totals.plus(dc.Counts)
		out.Days = append(out.Days, dc)
	}
	hour := now.Truncate(time.Hour)
	for i := 47; i >= 0; i-- {
		t := hour.Add(-time.Duration(i) * time.Hour)
		h := Hour{Time: t}
		if d := byDate[t.Format(dateLayout)]; d != nil {
			h.Requests, h.Pageviews = d.HourRequests[t.Hour()], d.HourPageviews[t.Hour()]
		}
		out.Hours = append(out.Hours, h)
	}
	out.TopPages, out.TopNotFound = topOf(pages), topOf(missing)
	return out
}

// topOf is the largest counts, ties by path.
func topOf(t Top) []PathCount {
	out := make([]PathCount, 0, len(t))
	for p, n := range t {
		out = append(out, PathCount{p, n})
	}
	slices.SortFunc(out, func(a, b PathCount) int { return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Path, b.Path)) })
	if len(out) > topAnswer {
		out = out[:topAnswer]
	}
	return out
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
