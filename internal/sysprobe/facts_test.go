package sysprobe

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func fakeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root + "/"
}

func TestProbeReadsProcAndSys(t *testing.T) {
	root := fakeRoot(t, map[string]string{
		"proc/meminfo":                  "MemTotal:        2014520 kB\nMemFree:  100 kB\nMemAvailable:    1048576 kB\n",
		"proc/loadavg":                  "1.50 0.80 0.40 2/300 12345\n",
		"proc/uptime":                   "86461.27 170000.00\n",
		"sys/devices/system/cpu/online": "0-3,6\n",
	})
	f, err := probeAt(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if f.MemTotalMB != 1967 || f.MemAvailableMB != 1024 || f.Load1 != 1.5 || f.UptimeSeconds != 86461 || f.CPUCount != 5 {
		t.Fatalf("facts = %+v", f)
	}
	if f.DiskTotalMB <= 0 || f.DiskFreeMB > f.DiskTotalMB {
		t.Fatalf("disk = %d free of %d", f.DiskFreeMB, f.DiskTotalMB)
	}
}

// Memory is the one required reading; the CPU count falls back to the
// runtime's, and the rest to zero.
func TestProbeFallbacks(t *testing.T) {
	if _, err := probeAt(fakeRoot(t, nil), ""); err == nil {
		t.Fatal("no error without /proc/meminfo")
	}
	f, err := probeAt(fakeRoot(t, map[string]string{
		"proc/meminfo":                  "MemTotal: 1024 kB\n",
		"sys/devices/system/cpu/online": "garbage",
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	if f.CPUCount != runtime.NumCPU() || f.Load1 != 0 || f.UptimeSeconds != 0 || f.DiskTotalMB != 0 || f.MemTotalMB != 1 {
		t.Fatalf("facts = %+v", f)
	}
}

func TestHeadroomPct(t *testing.T) {
	for _, c := range []struct {
		free, total int64
		want        int
	}{{50, 200, 25}, {0, 0, 0}, {10, -1, 0}, {200, 200, 100}} {
		if got := HeadroomPct(c.free, c.total); got != c.want {
			t.Errorf("HeadroomPct(%d, %d) = %d, want %d", c.free, c.total, got, c.want)
		}
	}
}
