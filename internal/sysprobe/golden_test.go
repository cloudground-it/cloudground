package sysprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The golden file pins what the probe reads from /proc and /sys over a
// matrix of machine files (Phase 5 gate). Regenerate with CG_GOLDEN_UPDATE=1
// only for an intended change.

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("CG_GOLDEN_UPDATE") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (CG_GOLDEN_UPDATE=1 writes it)", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from the golden:\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}

func TestGoldenProbe(t *testing.T) {
	var b strings.Builder
	cpus := func(n int) string {
		if n == runtime.NumCPU() {
			return "NumCPU"
		}
		return fmt.Sprint(n)
	}
	for _, online := range []string{"0-3,6\n", "0\n", "0-63", "0,2,4", "", "3-1", "x", "0-", "-", "1-1,1-1"} {
		root := fakeRoot(t, map[string]string{
			"proc/meminfo":                  "MemTotal:        2014520 kB\nMemAvailable:    1048576 kB\n",
			"sys/devices/system/cpu/online": online,
		})
		f, err := probeAt(root, "")
		fmt.Fprintf(&b, "online=%q cpus=%s err=%v\n", online, cpus(f.CPUCount), err)
	}
	for _, mem := range []string{
		"MemTotal:        2014520 kB\nMemFree:  100 kB\nMemAvailable:    1048576 kB\n",
		"MemTotal: 1023 kB\n",
		"MemTotalX: 999999 kB\nMemTotal:\nMemAvailable: abc kB\n",
		"",
		"MemAvailable:    1048576 kB\nMemTotal:        67108864 kB",
	} {
		for _, extra := range []map[string]string{
			{},
			{"proc/loadavg": "1.50 0.80 0.40 2/300 12345\n", "proc/uptime": "86461.97 170000.00\n"},
			{"proc/loadavg": "\n", "proc/uptime": "garbage"},
		} {
			files := map[string]string{"proc/meminfo": mem, "sys/devices/system/cpu/online": "0-1"}
			for k, v := range extra {
				files[k] = v
			}
			f, err := probeAt(fakeRoot(t, files), "")
			fmt.Fprintf(&b, "meminfo=%q extra=%q %+v err=%v\n", mem, extra, f, err)
		}
	}
	_, err := probeAt(fakeRoot(t, map[string]string{}), "")
	fmt.Fprintf(&b, "no meminfo: err=%v\n", err != nil)
	for _, c := range [][2]int64{{0, 0}, {5, 0}, {5, -1}, {0, 100}, {50, 100}, {1, 3}, {2, 3}, {100, 100}, {150, 100}} {
		fmt.Fprintf(&b, "HeadroomPct(%d,%d)=%d\n", c[0], c[1], HeadroomPct(c[0], c[1]))
	}
	checkGolden(t, "probe", b.String())
}
