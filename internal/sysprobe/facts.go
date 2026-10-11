// Package sysprobe measures the machine the agent runs on: CPUs, memory,
// load, uptime and disk. The tuning (PHP pools, MariaDB) is computed from
// these numbers, and the dashboard shows them.
package sysprobe

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Facts is one reading of the machine.
type Facts struct {
	CPUCount       int // CPUs online on the host
	MemTotalMB     int64
	MemAvailableMB int64
	Load1          float64 // 1-minute load average
	DiskTotalMB    int64   // the filesystem holding the probed directory
	DiskFreeMB     int64   // free for unprivileged users
	UptimeSeconds  int64
}

// Probe reads the machine; dir picks the filesystem for the disk numbers.
// Only memory is required: a tuning computed without it would be a guess.
func Probe(dir string) (Facts, error) { return probeAt("/", dir) }

// probeAt reads /proc and /sys below root, so tests can supply both.
func probeAt(root, dir string) (Facts, error) {
	var f Facts
	mem, err := os.ReadFile(root + "proc/meminfo")
	if err != nil {
		return f, err
	}
	f.MemTotalMB = meminfoMB(string(mem), "MemTotal")
	f.MemAvailableMB = meminfoMB(string(mem), "MemAvailable")
	f.CPUCount = onlineCPUs(root)
	f.Load1 = firstNumber(root + "proc/loadavg")
	f.UptimeSeconds = int64(firstNumber(root + "proc/uptime"))
	var st unix.Statfs_t
	if dir != "" && unix.Statfs(dir, &st) == nil {
		f.DiskTotalMB = int64(st.Blocks) * int64(st.Bsize) >> 20
		f.DiskFreeMB = int64(st.Bavail) * int64(st.Bsize) >> 20
	}
	return f, nil
}

// meminfoMB returns one "<key>: <n> kB" field of /proc/meminfo in MiB.
func meminfoMB(text, key string) int64 {
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || name != key {
			continue
		}
		if fields := strings.Fields(rest); len(fields) > 0 {
			kb, _ := strconv.ParseInt(fields[0], 10, 64)
			return kb >> 10
		}
	}
	return 0
}

// onlineCPUs counts the host's online CPUs from the kernel's range list
// ("0-3,6"). runtime.NumCPU reports the agent's own affinity mask instead,
// and is only the fallback.
func onlineCPUs(root string) int {
	b, err := os.ReadFile(root + "sys/devices/system/cpu/online")
	if err != nil {
		return runtime.NumCPU()
	}
	n := 0
	for _, part := range strings.Split(strings.TrimSpace(string(b)), ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(lo)
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.Atoi(hi)
		}
		if err1 != nil || err2 != nil || b < a {
			return runtime.NumCPU()
		}
		n += b - a + 1
	}
	if n == 0 {
		return runtime.NumCPU()
	}
	return n
}

// firstNumber is the first field of a one-line /proc file, 0 if unreadable.
func firstNumber(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

// HeadroomPct is the share of a resource still free, 0 when total is unknown.
func HeadroomPct(free, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(free * 100 / total)
}
