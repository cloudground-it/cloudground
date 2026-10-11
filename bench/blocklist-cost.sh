#!/bin/bash
# What the block list's netfilter table costs cached traffic (issue #21,
# bench/blocklist.md). Three arms on the same box, alternated:
#
#   none   no table inet cloudground at all (the build before #21)
#   empty  the table as the panel leaves it with nothing blocked
#   full   the table with 10,000 blocked addresses, 1,000 blocked /24s and
#          100 allowed networks (far more than a busy server collects)
#
# The load comes from a network namespace on the box: loopback traffic is
# accepted by the table's first rule (iif lo) and would not measure the set
# lookups. The panel's API is stopped during the run so that it does not put
# its own list back; cached pages do not need it. Run as root:
#
#   bash bench/blocklist-cost.sh https://bench.<ip>.sslip.io [rounds]
set -euo pipefail

url=${1:?usage: blocklist-cost.sh https://bench.<ip>.sslip.io [rounds]}
rounds=${2:-3}
here=$(cd -- "$(dirname -- "$0")" && pwd)
host=${url#https://}
host=${host%%/*}
ip=$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}')
ns=cgbenchbl
out=bench/results/blocklist-$(date -u +%Y%m%dT%H%M%SZ)

cleanup() {
  ip netns del "$ns" 2>/dev/null || true
  ip link del cgbenchbl0 2>/dev/null || true
  rm -rf "/etc/netns/$ns"
  systemctl start cloudground-api.socket cloudground-api || true
}
trap cleanup EXIT

# The client: 198.18.78.2 behind a veth pair, the bench site's name pointing
# at the public address (ip netns exec bind-mounts /etc/netns/<ns>/hosts).
ip netns add "$ns"
ip link add cgbenchbl0 type veth peer name cgbenchbl1
ip link set cgbenchbl1 netns "$ns"
ip addr add 198.18.78.1/30 dev cgbenchbl0
ip link set cgbenchbl0 up
ip netns exec "$ns" ip addr add 198.18.78.2/30 dev cgbenchbl1
ip netns exec "$ns" ip link set cgbenchbl1 up
ip netns exec "$ns" ip link set lo up
ip netns exec "$ns" ip route add default via 198.18.78.1
mkdir -p "/etc/netns/$ns"
printf '127.0.0.1 localhost\n%s %s\n' "$ip" "$host" >"/etc/netns/$ns/hosts"

systemctl stop cloudground-api.socket cloudground-api

table() { # table <blocked /32s> <blocked /24s> <allowed /16s>
  {
    echo "table inet cloudground"
    echo "delete table inet cloudground"
    echo "table inet cloudground {"
    printf '\tset allow4 { type ipv4_addr; flags interval;'
    if [ "$3" -gt 0 ]; then printf ' elements = { '; seq 0 $(($3 - 1)) | awk '{printf "%s10.%d.0.0/16", (NR > 1 ? ", " : ""), $1}'; printf ' }'; fi
    printf ' }\n\tset allow6 { type ipv6_addr; flags interval; }\n'
    printf '\tset block4 { type ipv4_addr; flags interval;'
    if [ "$1" -gt 0 ]; then
      printf ' elements = { '
      { seq 0 $(($1 - 1)) | awk '{printf "45.%d.%d.%d/32\n", int($1 / 65536) % 256, int($1 / 256) % 256, $1 % 256}'
        seq 0 $(($2 - 1)) | awk '{printf "46.%d.%d.0/24\n", int($1 / 256) % 256, $1 % 256}'; } | paste -sd, - | sed 's/,/, /g'
      printf ' }'
    fi
    printf ' }\n\tset block6 { type ipv6_addr; flags interval; }\n'
    printf '\tchain input {\n\t\ttype filter hook input priority -10; policy accept;\n\t\tiif "lo" accept\n'
    printf '\t\tip saddr @allow4 accept\n\t\tip6 saddr @allow6 accept\n\t\tip saddr @block4 drop\n\t\tip6 saddr @block6 drop\n\t}\n}\n'
  } | nft -f -
}

arm() {
  case $1 in
  none) nft delete table inet cloudground 2>/dev/null || true ;;
  empty) table 0 0 0 ;;
  full) table 10000 1000 100 ;;
  esac
}

for r in $(seq 1 "$rounds"); do
  for a in none empty full; do
    arm "$a"
    ip netns exec "$ns" "$here/cgbench" repeat -n 1 -out "$out" \
      -scenarios cached-sustained,cached-single,static "$url" "$a-$r"
  done
done

join() { local IFS=,; echo "$*"; }
runs() { local d=(); for r in $(seq 1 "$rounds"); do d+=("$out/$1-$r/run1"); done; join "${d[@]}"; }
echo "== none against empty"
"$here/cgbench" compare -a "$(runs none)" -b "$(runs empty)" -a-name none -b-name empty
echo "== none against full"
"$here/cgbench" compare -a "$(runs none)" -b "$(runs full)" -a-name none -b-name full
