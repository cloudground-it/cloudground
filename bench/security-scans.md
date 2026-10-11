# A site's security checks: what they cost (issue #31)

**Not measured yet.** This page lists what is to be measured on the reference VPS and how. The
numbers come with the next run there. Until then, the bounds below are the design's, not measurements.

## 1. The WordPress vulnerability check (#31, api SPEC §11.6)

What one check does: one wp-cli run in the site's slice and sandbox (`core version`,
`plugin list`, `theme list`), then one HTTPS request per component to the vulnerability
database, four at a time. Each answer is reused for six hours by every site that has the same
component.

Bounded by design:

- **One check at a time on the server** (the `security:vulncheck` lock). Each site is checked
  once a day, at the time it was first seen, so the checks spread over the day.
- **Memory in the panel:** one answer read at a time per request, each at most 4 MB (the largest
  plugin record, Elementor's, is about 170 KB), parsed into compact records. The cache holds at
  most 4 096 components.
- **The site:** wp-cli runs in the site's own slice, under its memory and task limits
  (`site-resources.md`).

To measure, on the bench site (`setup-workload.sh`, WooCommerce with its default theme):

```sh
sudo COLD=1 PANEL_EMAIL=... PANEL_PW=... IP=<public-ip> bash bench/security-cost.sh vuln 5
```

| | Run 1 (cold cache) | Runs 2–5 (cached) |
| --- | ---: | ---: |
| wall time | to measure | to measure |
| panel-api CPU | to measure | to measure |
| panel-agent CPU | to measure | to measure |
| the site slice's CPU (wp-cli) | to measure | to measure |
| the site slice's memory over its resting level | to measure | to measure |
| panel-api resident memory after the check (`systemctl show -p MemoryCurrent cloudground-api`) | to measure | — |

The issue's bound is the same as the usage collection's: far under 1 % of the 2-CPU box. That
is 864 s of CPU a day, so the check must stay under that even with a hundred WordPress sites.
