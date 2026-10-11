package tune

import (
	"strings"
	"testing"
)

// SPEC §2: the buffer pool's share follows the role, with a floor.
func TestBufferPoolShare(t *testing.T) {
	for _, c := range []struct {
		mem  int64
		role Role
		want int64
	}{
		{4096, RoleShared, 1024},
		{4096, RoleDedicated, 2867},
		{256, RoleShared, 128},
		{0, RoleShared, 512}, // unknown RAM is read as 2 GB
		{4096, "", 1024},
	} {
		if got := CalculateMariaDB(c.mem, 2, c.role, false).BufferPoolMB; got != c.want {
			t.Errorf("%d MB %q: buffer pool %d, want %d", c.mem, c.role, got, c.want)
		}
	}
}

// SPEC §2: a store trades the last second of writes for none lost.
func TestDurabilityForStores(t *testing.T) {
	if CalculateMariaDB(4096, 2, RoleShared, true).FlushLogAtCommit != 1 ||
		CalculateMariaDB(4096, 2, RoleShared, false).FlushLogAtCommit != 2 {
		t.Fatal("innodb_flush_log_at_trx_commit must be 1 for stores and 2 otherwise")
	}
}

func TestBoundsAndSteps(t *testing.T) {
	big := CalculateMariaDB(262144, 64, RoleDedicated, false)
	if big.LogFileMB != 2048 || big.TmpTableMB != 256 || big.MaxConnections != 500 || big.ThreadCacheSize != 512 {
		t.Errorf("large machine: %+v", big)
	}
	small := CalculateMariaDB(512, 1, RoleShared, false)
	if small.LogFileMB != 64 || small.TmpTableMB != 32 || small.MaxConnections != 100 {
		t.Errorf("small machine: %+v", small)
	}
	if got := CalculateMariaDB(8192, 2, RoleShared, false).MaxConnections; got != 300 {
		t.Errorf("8 GB: %d connections", got)
	}
}

func TestRenderedFile(t *testing.T) {
	out, err := CalculateMariaDB(4096, 2, RoleShared, true).Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[mysqld]\n", "innodb_buffer_pool_size = 1024M\n", "innodb_flush_log_at_trx_commit = 1\n",
		"innodb_flush_method = O_DIRECT\n", "table_definition_cache = 2000\n", "tmp_table_size = 128M\n", "max_heap_table_size = 128M\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", strings.TrimSpace(want))
		}
	}
}
