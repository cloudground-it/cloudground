# internal/guard — behaviour spec (R-4.5)

Performance Guard is the gate of a staging safe-push: before staging's code
goes live, it measures the staging site (the **candidate**) against
production (the **baseline**) on the same server and refuses a candidate
that is measurably slower, heavier or broken. After the promotion it checks
production once more and asks for a rollback when production now fails.

There is exactly **one measurement engine** (`Engine`). The comparison
before the promotion and the check after it send their requests through the
same function, the same HTTP client and the same rules; nothing else in the
package makes a request.

## 1. Inputs

| Input | Meaning |
| --- | --- |
| `Engine.Target` | scheme and authority every request goes to (the panel's probe target, `https://127.0.0.1` by default). Its path, query and user info are ignored. |
| `Endpoint{Host, ProbeKey, InternalKey}` | one side: the virtual host (`Host` header), the key that unlocks the connector's metrics footer, and a staging site's password header |
| paths | the pages to measure; `DefaultProbePaths` (`/` and `/?cloudground-probe=uncached`) when the caller has none |
| `Engine.Rounds` | paired rounds per path (default 200, §5) |
| `Engine.Workers` | PHP-FPM workers of the sites (the larger of the two); sizes the warm-up (§3) |
| `Engine.Seed` | randomness for the round order, the cache-busting nonces and the bootstrap; 0 = time-based |
| `Thresholds` | the policy (§5); `DefaultThresholds()` is the shipped one |

### 1.1 Probe paths

`ValidateProbePaths` accepts at most `MaxProbePaths` (10) paths. Each must be
at most 512 bytes, start with exactly one `/`, contain no `@`, `\`, space,
tab, CR or LF, and parse as a request URI. A path can therefore never name
another host: the request URL is built from the target's scheme and host plus
the path's own path and query, never by string concatenation.

## 2. One request

`GET <target><path>` with a nonce appended to the query (`cg-guard=<random
base-36>`, joined with `&` when the path already has a query), so every
request misses the full-page cache.

Headers: `Host` = the endpoint's host; `X-CloudGround-Probe` = its probe key
when set; `X-CloudGround-Internal` = its internal key when set. The vhost
never caches a response to a request carrying the probe header
(connector/SPEC.md §2.7).

The client:

- is **one process-wide client over one shared transport** (keep-alive,
  at most 16 idle connections per host, TLS verification off: the target is
  the server itself over loopback and the certificate may be the fallback
  one). A TLS handshake therefore lands on no particular sample, and a run
  does not open a connection pool of its own;
- **never follows a redirect**: the site under test chooses the `Location`,
  and following it would carry the probe key wherever the site says;
- times out a request after 60 s; reads at most 4 MiB of body.

What a response yields (an **observation**):

| Field | From |
| --- | --- |
| status | the HTTP status; 0 when no response arrived |
| failed | transport error, body read error, or status ≥ 500 |
| cached | header `X-CloudGround-Cache` (nginx's `$upstream_cache_status`, case-insensitive) is present and not `MISS` or `BYPASS`: `HIT`, `STALE`, `UPDATING`, `REVALIDATED`, `EXPIRED` and anything else |
| server time | the **last** connector footer in the body `<!-- cloudground queries=<n> time=<seconds> mem=<bytes> -->`, `time` × 1000 ms; 0 when there is no footer. The last, because `wp_footer` prints last and a page's own content could carry a fake footer earlier |
| queries, peak memory | the same footer |
| client time | wall time from sending to the end of the body, ms |

The footer is printed only for the right probe key and only by PHP, so its
presence means "this response is the PHP path, rendered for us".

## 3. Warm-up

Before measuring a path, each side gets `max(8, 2 × Workers)` requests whose
results are discarded, sent `Workers` at a time (at least one), alternating
sides. Twice the worker count reaches every PHP-FPM worker at least once
under the pool's own scheduling, so no measured request is a worker's first
(cold opcache, cold connections).

## 4. Comparison (before the promotion)

### 4.1 Rounds

For each path, `Rounds` rounds, one at a time. A round requests both sides,
in a random order chosen per round. Whatever the server does during the run
(a backup, a cron job, a noisy neighbour) falls on both halves of the same
round, so the per-round ratio isolates the candidate.

### 4.2 Errors and status

Per round, the **baseline** errs when its observation failed. The
**candidate** errs when its observation failed, or when its status is a 4xx
that differs from the baseline's status in that round (a page that answers
404 on staging but 200 in production is broken, not fast).

In that order, the first rule that fires decides the path:

1. **status change** — among the rounds where both sides answered, the
   statuses differ in more than 10%: **block** ("candidate answers HTTP x
   where production answers HTTP y", with each side's most common status).
2. **error rate** — the candidate's error share is above the baseline's and
   above `MaxErrorRatePct`: **block**.

### 4.3 Timing

Only rounds where neither side erred and neither response was served from
the cache are **pairs**. The time used is the **server time** (the connector's own
render time, free of TLS, connection reuse and client scheduling). When a
path has at least one round where both sides carry the footer, only rounds
where both carry it are pairs (only the PHP path is measured). When neither
side ever carries it (a site without the connector), the client time is
used and the comparison says so (`server_timed: false`).

Fewer than 20 pairs: the path is **inconclusive** (too few measurable
responses: cache hits, errors or a missing footer).

Otherwise, with `r_i = ln(candidate_i / baseline_i)`:

- `median_ratio = exp(median(r))`;
- a 95% interval from a percentile bootstrap of the median (2000
  resamples): `ci_low`, `ci_high` (both as ratios);
- `median_delta_ms = median(candidate_i − baseline_i)`.

With `T = ln(1 + MaxSlowdownPct/100)`, in order:

| Condition | Verdict for the path |
| --- | --- |
| `median_delta_ms < MinSlowdownMs` | nothing: too small to matter, whatever the ratio |
| `ln(ci_low) > T` | **block**: 95% sure the candidate is slower than the threshold |
| `ln(ci_high) − ln(ci_low) > ln 3` and `ln(median_ratio) > T` | **inconclusive**: the server moved too much to tell |
| `ln(ci_low) > 0` and `ln(median_ratio) > T` | **warn**: probably slower than the threshold, not proven |
| otherwise | nothing |

### 4.4 Queries and memory

From the footer, per side: the median query count and the peak memory over
the observations that carry it. An increase counts only when it is at least
5 queries, or at least 32 MiB. Above the threshold (`MaxQueryIncreasePct`,
`MaxMemIncreasePct`) it **blocks**, above half of it it **warns**. A
decrease is never reported.

### 4.5 Report

The verdict is the worst over all paths: block > inconclusive > warn > pass.
Each finding adds a reason prefixed `BLOCK: `, `INCONCLUSIVE: ` or `WARN: `.
The report carries per path a `Sample` for each side (requests, errors as
defined in §4.2, p50/p95 of the time used, median queries, peak memory, most
common status) and a `PathComparison` (§4.3). Its JSON is stored on the
deployment record and shown by the UI (`ui/app/types` `GuardReport`).

The caller (the safe-push) promotes on pass; on warn or inconclusive only
when forced; never on block.

## 5. Policy

`DefaultThresholds()`:

| Field (JSON) | Default | Meaning |
| --- | --- | --- |
| `max_slowdown_pct` | 10 | block when the interval's lower bound is above this slowdown |
| `min_slowdown_ms` | 25 | ignore slowdowns whose median per-round difference is smaller |
| `max_query_increase_pct` | 50 | |
| `max_mem_increase_pct` | 30 | |
| `max_error_rate_pct` | 1 | |

A zero field disables its check (`min_slowdown_ms` 0 means no floor).

**Reliability, enforced by unit tests on a deterministic fake target**
(log-normal render times, σ = 0.25, plus a 3× spike on 5% of requests, which
is what an uncached WordPress render looks like on a small box):

- 50 A/A comparisons (identical sides) give fewer than 5% blocks;
- an injected 30% regression is blocked in at least 95% of 50 comparisons.

**The trade-off.** The threshold is what a real, systematic gap between
production and staging must stay under to pass: the two are different sites
(their own database, autoloaded options, APCu and opcache), so identical
code can render a few percent apart on every request. Pairing removes the
noise, not that gap. A low threshold catches smaller regressions but blocks
pushes on that gap; so the threshold is 10% and the sample is made large
enough to still catch 30%.

Measured on the fake over 2 × 300 seeds when the defaults were chosen:

| Threshold | Rounds | A/A blocked | 30% blocked |
| --- | --- | --- | --- |
| 5% | 100 | 0.3–0.7% | 98.3–99.7% |
| 10% | 100 | 0–0.3% | 90–96% (too close to the bound) |
| 10% | 150 | 0% | 97.7–98.7% |
| **10%** | **200** | **0%** | **99.7–100%** |

200 rounds cost time: about 2 × 200 renders per path (≈ 2 minutes per path at
300 ms a render). The e2e safe push also runs an A/A comparison on a heavy
page (many blocks and posts), which must neither block nor warn.

## 6. Check after the promotion

`Engine.Verify(ctx, production, baseline, paths, t)`: right after the
promotion PHP has reloaded and the cache was purged, so latency is not
comparable and is not judged. Per path: the same warm-up (§3), then 20
requests to production, one at a time, through the same request function
(§2). An observation errs when it failed, or when it is a 4xx different from
the baseline's most common status. Per path, in order:

1. the most common status differs from the baseline's: **block**;
2. the error share is above the baseline's and above `MaxErrorRatePct`:
   **block**.

A path the baseline does not have is skipped. Block asks the caller to roll
back; anything else keeps the release.

### 6.1 Measure (before a change in place)

`Engine.Measure(ctx, side, paths)` is §6's sampling with no baseline: per
path the same warm-up and 20 requests, nothing judged, the samples
returned. It is the baseline of a change made in place, where there is no
second site to compare with: a WordPress update (internal/api SPEC §6.2.3)
measures the site before it and `Verify`s it after against those samples.

## 7. Probe key

`ProbeKeyFor(token)` = lowercase hex of HMAC-SHA256 with the site's
connector token as key over the message `guard-probe`; empty for an empty
token. The connector compares it in constant time (connector/SPEC.md §2.7).
