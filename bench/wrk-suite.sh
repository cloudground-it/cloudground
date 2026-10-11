#!/bin/sh
# The full scenario suite, once, against one target (bench/SPEC.md §2–3).
#
#   bash bench/wrk-suite.sh https://bench.<ip>.sslip.io [label]
#
# Results go to bench/results/<label>/ (default label: run): one wrk output
# per scenario and summary.tsv, which is also printed. PATH_CACHED,
# PATH_STATIC, DURATION, SHORT and LAT_TIMEOUT change the defaults.
# For repetitions and A/B alternation use bench/cgbench repeat or ab.
set -eu
here=$(cd -- "$(dirname -- "$0")" && pwd)
exec "$here/cgbench" suite -out "$here/results" "$@"
