# `internal/traffic` — visitor statistics from the access log (specification)

Issue #33: per-site statistics built on the server from nginx's access
log, with no third party and no cookie. This package is pure: it reads
lines and keeps aggregates; ops/traffic feeds it the logs and keeps its
state. Nothing here stores an address.

## 1. The line (`ParseLine`)

The `cloudground` log format (internal/engine SPEC §3.1):

```
$remote_addr - $host [$time_local] "$request" $status $body_bytes_sent $request_time cache:$upstream_cache_status "$http_user_agent"
```

nginx writes `"` and control bytes inside a variable as `\xHH`, so the
quoted fields hold no raw quote. A line is taken when the address, the
time (`02/Jan/2006:15:04:05 -0700`), the status (100–599) and the bytes
parse; anything else is skipped and counted. From `$request`, a method
of capital letters and a target that starts with `/` give the method and
the path (the query string cut off, at most 200 bytes kept); any other
request (`-`, a scanner's garbage) has neither and still counts as a
request.

## 2. What a request is

- **Internal**: a user agent starting `CloudGround` (the uptime check, the
  warmer, the guard's probes): not counted at all.
- **Bot**: an empty user agent (or `-`), or one containing, in any case,
  `bot`, `crawl`, `spider`, `slurp`, `curl`, `wget`, `python`,
  `go-http-client`, `java/`, `libwww`, `httpclient`, `okhttp`, `axios`,
  `node-fetch`, `headless`, `scrapy`, `facebookexternalhit`, `preview`,
  `monitor`, `uptime`, `lighthouse`, `feedfetcher`. Counted as a request
  and a bot, never as a page view.
- **Page view**: not a bot, `GET`, status 2xx or 304, a path whose last
  segment has none of the asset extensions (`css js mjs map png jpg jpeg
  gif webp avif svg ico bmp woff woff2 ttf otf eot mp4 webm mp3 ogg wav
  pdf zip gz xml txt json webmanifest`) and that is not under `/wp-admin/`,
  `/wp-content/`, `/wp-includes/`, `/wp-json/`, `/.well-known/` or
  `/.cg-maintenance/`, nor `/wp-login.php`, `/wp-cron.php`, `/xmlrpc.php`,
  `/favicon.ico` or `/robots.txt`.
- **Visitor**: the distinct (address, user agent) pairs among the day's
  page views, counted by a HyperLogLog sketch (§4).

## 3. The day (`Day`)

One per UTC day: `requests`, `pageviews`, `visitors`, `bots`, `bytes`
(`$body_bytes_sent`), `status_2xx` … `status_5xx`, `cache_hit` (`HIT`,
`STALE`, `UPDATING`, `REVALIDATED`), `cache_miss` (`MISS`, `EXPIRED`),
`cache_bypass` (`BYPASS`), each hour's requests and page views, the pages
viewed (`pages`) and the paths answered 404 (`not_found`), each a bounded
top list (§5). A line is counted on its own date, whatever the order the
lines come in.

## 4. Visitors (`Sketch`)

HyperLogLog with 2^12 one-byte registers (about 1.6 % standard error,
4 KiB). The element is FNV-1a 64 of the day's salt, the address and the
user agent, mixed by the SplitMix64 finaliser. The salt is 16 random bytes
made with the day's sketch. The estimate is the usual one, with linear
counting under 2.5 × 2^12 when a register is still zero. The sketch, and
its salt, are dropped once the day is two days old (§6): from then on the
day keeps only its count, and no hash of an address exists any more.

## 5. Top lists (`Top`)

Space-saving with 100 counters: a known path adds to its counter; a new
one takes a free counter, or the smallest counter's place with that count
plus one (an over-estimate, never an under-estimate, and the paths that
matter stay). Merging days adds counters path by path; an answer gives the
20 largest, ties by path.

## 6. The aggregate (`Site`)

The days of one site, newest 90 kept. `Add(hit, now)` counts a hit in its
day: a day older than the 90 kept, or two days ahead of `now`, is dropped.
`Settle(now)` drops days past 90 and sketches older than yesterday.
`Stats(days, now)` answers the range: every day zero-filled, the last 48
hours, the totals, and the two top lists merged over the range.

## 7. Tests

`traffic_test.go`: lines of every shape (a query string cut, `\x22` in a
user agent, `-` as the request, garbage skipped); internal, bot, asset and
page; the cache classes; days in any order and out of range; the sketch's
estimate within 5 % at 10 000 and exact-ish under 100, and gone after two
days; the top list's bound and over-estimate; the range's zero-filled days,
48 hours and merged tops.
