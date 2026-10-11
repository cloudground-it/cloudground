package tune

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden files pin the MariaDB tuning over a matrix of machines (Phase 5
// gate). Captured from the code before the rewrite; regenerate with
// CG_GOLDEN_UPDATE=1 only for an intended change.

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
		wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
		for i := 0; i < len(wl) || i < len(gl); i++ {
			var w, g string
			if i < len(wl) {
				w = wl[i]
			}
			if i < len(gl) {
				g = gl[i]
			}
			if w != g {
				t.Fatalf("%s differs from the golden at line %d:\nwant %q\ngot  %q", name, i+1, w, g)
			}
		}
	}
}

func TestGoldenMariaDB(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, "ConfigPath=%s\n", ConfigPath)
	for _, mem := range []int64{-1, 0, 256, 511, 512, 1024, 2048, 4095, 4096, 8191, 8192, 16383, 16384, 32768, 65536, 262144} {
		for _, cpu := range []int{-1, 0, 1, 2, 8, 64} {
			for _, role := range []Role{RoleShared, RoleDedicated, ""} {
				for _, commerce := range []bool{false, true} {
					c := CalculateMariaDB(mem, cpu, role, commerce)
					out, err := c.Render()
					sum := sha256.Sum256([]byte(out))
					fmt.Fprintf(&b, "mem=%d cpu=%d role=%q commerce=%v %#v str=%q err=%v sha=%s\n",
						mem, cpu, role, commerce, c, c.String(), err, hex.EncodeToString(sum[:8]))
				}
			}
		}
	}
	checkGolden(t, "mariadb", b.String())

	var full strings.Builder
	for _, c := range []MariaDBConfig{
		CalculateMariaDB(2048, 2, RoleShared, false),
		CalculateMariaDB(16384, 8, RoleDedicated, true),
	} {
		out, err := c.Render()
		fmt.Fprintf(&full, "===== %s err=%v\n%s", c, err, out)
	}
	checkGolden(t, "mariadb-samples", full.String())
}
