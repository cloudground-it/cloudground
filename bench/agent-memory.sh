#!/bin/bash
# The agent's memory and its helpers', sampled while a scenario runs
# (bench/agent-memory.md, issue #66).
#
#   sudo bash bench/agent-memory.sh <label> [seconds]
#
# Samples every second, until [seconds] have passed (default: until Ctrl-C):
# cloudground-agent.service's cgroup (memory.current, its anon and file
# parts, memory.events high/max/oom_kill), the agent process's VmRSS, and
# cg.slice/cg-helper.slice (the helpers: restic, mariadb-dump, mariadb,
# certbot). Writes agent-memory-<label>.csv and prints the peaks; the
# lifetime peaks (memory.peak) cover the cgroup since it was made, so
# restart the agent before a scenario to measure that scenario alone.
set -euo pipefail
label=${1:?usage: agent-memory.sh <label> [seconds]}
secs=${2:-0}
agent=/sys/fs/cgroup/system.slice/cloudground-agent.service
helpers=/sys/fs/cgroup/cg.slice/cg-helper.slice
[[ -d $agent ]] || { echo "no cgroup at $agent: is the agent running?" >&2; exit 1; }
out=agent-memory-$label.csv

mib() { awk -v b="${1:-0}" 'BEGIN { if (b == "max" || b == "") print b; else printf "%.1f", b / 1048576 }'; }
stat() { awk -v k="$2" '$1 == k { print $2 }' "$1/memory.stat" 2>/dev/null || echo 0; }
event() { awk -v k="$2" '$1 == k { print $2 }' "$1/memory.events" 2>/dev/null || echo 0; }
read1() { cat "$1" 2>/dev/null || echo 0; }

echo "t,agent_current_mib,agent_anon_mib,agent_file_mib,agent_rss_mib,agent_high_events,agent_max_events,agent_oom_kills,helpers_current_mib" > "$out"
peak_cur=0 peak_anon=0 peak_rss=0 peak_help=0
start=$(date +%s)
trap 'done=1' INT
done=0
while [[ $done == 0 ]]; do
	now=$(date +%s)
	(( secs > 0 && now - start >= secs )) && break
	cur=$(read1 $agent/memory.current)
	anon=$(stat $agent anon)
	file=$(stat $agent file)
	pid=$(systemctl show -p MainPID --value cloudground-agent)
	rss=$(awk '/^VmRSS:/ { print $2 * 1024 }' /proc/"$pid"/status 2>/dev/null || echo 0)
	help=$(read1 $helpers/memory.current)
	(( cur > peak_cur )) && peak_cur=$cur
	(( anon > peak_anon )) && peak_anon=$anon
	(( rss > peak_rss )) && peak_rss=$rss
	(( help > peak_help )) && peak_help=$help
	echo "$((now - start)),$(mib "$cur"),$(mib "$anon"),$(mib "$file"),$(mib "$rss"),$(event $agent high),$(event $agent max),$(event $agent oom_kill),$(mib "$help")" >> "$out"
	sleep 1
done

echo "== $label ($(($(date +%s) - start)) s, $out)"
echo "agent memory.current peak (sampled): $(mib $peak_cur) MiB, anon $(mib $peak_anon) MiB, process VmRSS $(mib $peak_rss) MiB"
echo "agent memory.peak (cgroup lifetime): $(mib "$(read1 $agent/memory.peak)") MiB"
echo "agent memory.events: high $(event $agent high), max $(event $agent max), oom_kill $(event $agent oom_kill)"
echo "agent limits: MemoryHigh $(mib "$(read1 $agent/memory.high)") MiB, MemoryMax $(mib "$(read1 $agent/memory.max)") MiB"
echo "helpers memory.current peak (sampled): $(mib $peak_help) MiB"
