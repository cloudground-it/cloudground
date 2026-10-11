#!/usr/bin/env bash
# The cost of a site's security checks (bench/security-scans.md): the CPU the
# panel, the agent and the site's slice spend on one check, the site slice's
# memory peak over its resting level, and the wall time.
#
#   sudo PANEL_EMAIL=... PANEL_PW=... IP=<public-ip> bash bench/security-cost.sh vuln [runs]
#
# vuln: the WordPress vulnerability check (#31) of the bench site
# (setup-workload.sh). The first run after a restart of the API looks every
# component up; the next ones within six hours answer from its cache: set
# COLD=1 to restart cloudground-api before the first run and see both.
#
# malware: the malware scan (#76) of the bench site, with the setting
# malware_scan on. The first run of the day also updates the signatures
# (freshclam and the set's rebuild, in the agent); FORCE_SIGS=1 removes the
# set's stamp first so run 1 includes that update. The scanner's peak memory
# is the slice's sampled peak over its resting level; the scan's own unit
# (cg-scan-*) is also read for MemoryPeak when systemd keeps it.
set -euo pipefail

MODE="${1:?vuln}"
RUNS="${2:-5}"
IP="${IP:?set IP=<public ip>}"
EMAIL="${PANEL_EMAIL:?set PANEL_EMAIL}"
PW="${PANEL_PW:?set PANEL_PW}"
SOCK="${API_SOCK:-/run/cloudground/api.sock}"
BASE=http://localhost
DOMAIN="bench.${IP//./-}.sslip.io"

case "$MODE" in
  vuln) path=vulnerabilities/check ;;
  malware) path=malware/scan ;;
  *) echo "unknown mode $MODE" >&2; exit 2 ;;
esac

cookies=$(mktemp)
samples=$(mktemp)
trap 'rm -f "$cookies" "$samples"' EXIT
api() { curl -sk --unix-socket "$SOCK" -b "$cookies" -c "$cookies" "$@"; }
field() { python3 -c 'import json,sys; v=json.load(sys.stdin)
for k in sys.argv[1:]: v=v[k]
print(v)' "$@" 2>/dev/null; }
login() {
  api -o /dev/null -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"email": sys.argv[1], "password": sys.argv[2]}))' "$EMAIL" "$PW")" \
    "$BASE/api/login"
}

if [[ "${COLD:-}" == 1 ]]; then
  systemctl restart cloudground-api
  for _ in $(seq 30); do [[ -S "$SOCK" ]] && curl -sk --unix-socket "$SOCK" -o /dev/null "$BASE/healthz" && break; sleep 1; done
fi
login
if [[ "$MODE" == malware ]]; then
  api -o /dev/null -X PUT -H 'Content-Type: application/json' -d '{"malware_scan":"1"}' "$BASE/api/settings"
  [[ "${FORCE_SIGS:-}" == 1 ]] && rm -f /var/lib/cloudground-signatures/cloudground.json
  du -sk /var/lib/cloudground-signatures /var/lib/clamav 2>/dev/null || true
fi
site=$(api "$BASE/api/sites" | python3 -c 'import json,sys
d=json.load(sys.stdin)
for s in (d if isinstance(d, list) else d.get("sites", [])):
    if s.get("domain")==sys.argv[1]: print(s["id"])' "$DOMAIN")
[[ -n "$site" ]] || { echo "no bench site $DOMAIN: run setup-workload.sh" >&2; exit 1; }
slice="/sys/fs/cgroup/cg.slice/cg-site.slice/cg-site-$site.slice"

svc_cpu() { systemctl show -p CPUUsageNSec --value "$1"; }
slice_cpu() { awk '$1 == "usage_usec" { print $2 * 1000 }' "$slice/cpu.stat"; }

printf '| run | wall s | API CPU ms | agent CPU ms | site slice CPU ms | slice memory over rest MB |\n'
printf '| ---: | ---: | ---: | ---: | ---: | ---: |\n'
for run in $(seq "$RUNS"); do
  rest=$(cat "$slice/memory.current")
  a0=$(svc_cpu cloudground-api) g0=$(svc_cpu cloudground-agent) s0=$(slice_cpu)
  t0=$(date +%s.%N)
  task=$(api -X POST "$BASE/api/sites/$site/$path" | field task id)
  [[ -n "$task" ]] || { echo "the check did not start" >&2; exit 1; }
  : >"$samples"
  while :; do
    cat "$slice/memory.current" >>"$samples"
    status=$(api "$BASE/api/tasks/$task" | field status || true)
    [[ "$status" == succeeded || "$status" == failed ]] && break
    sleep 0.1
  done
  t1=$(date +%s.%N)
  a1=$(svc_cpu cloudground-api) g1=$(svc_cpu cloudground-agent) s1=$(slice_cpu)
  peak=$(sort -n "$samples" | tail -1)
  python3 -c 'import sys
r, t0, t1, a0, a1, g0, g1, s0, s1, rest, peak, st = sys.argv[1:]
f = lambda a, b: (int(b) - int(a)) / 1e6
if st != "succeeded":
    r += " (failed)"
print(f"| {r} | {float(t1) - float(t0):.2f} | {f(a0, a1):.0f} | {f(g0, g1):.0f} | {f(s0, s1):.0f} | {max(0, int(peak) - int(rest)) / 2**20:.0f} |")' \
    "$run" "$t0" "$t1" "$a0" "$a1" "$g0" "$g1" "$s0" "$s1" "$rest" "$peak" "$status"
done
