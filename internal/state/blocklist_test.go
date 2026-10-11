package state

import (
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/blocklist"
)

// #21: rows are unique per kind and prefix (a second block replaces the
// first), expired blocks are pruned and permanent ones kept, and the
// settings read as the defaults until saved.
func TestBlocklistStore(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	in := t0.Add(time.Hour)
	auto, err := s.PutBlockEntry(BlockEntry{Kind: BlockKindBlock, Prefix: "203.0.113.9/32", Source: "ssh", Failures: 6, CreatedAt: t0, ExpiresAt: &in})
	if err != nil || auto.ID == 0 {
		t.Fatalf("put = %+v, %v", auto, err)
	}
	if _, err := s.PutBlockEntry(BlockEntry{Kind: BlockKindBlock, Prefix: "198.51.100.0/24", Source: "manual", Note: "scanner", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBlockEntry(BlockEntry{Kind: BlockKindAllow, Prefix: "203.0.113.9/32", Source: "manual", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	// The same address blocked again by hand: one row, now permanent.
	again, err := s.PutBlockEntry(BlockEntry{Kind: BlockKindBlock, Prefix: "203.0.113.9/32", Source: "manual", Note: "by hand", CreatedAt: t0})
	if err != nil || again.ID != auto.ID {
		t.Fatalf("re-put = %+v, %v (first id %d)", again, err, auto.ID)
	}
	list, err := s.BlockEntries()
	if err != nil || len(list) != 3 || list[0].Source != "manual" || list[0].ExpiresAt != nil || list[0].Note != "by hand" {
		t.Fatalf("entries = %+v, %v", list, err)
	}
	if _, err := s.PutBlockEntry(BlockEntry{Kind: "other", Prefix: "x"}); err == nil {
		t.Fatal("an unknown kind was stored")
	}

	ends := t0.Add(10 * time.Minute)
	if _, err := s.PutBlockEntry(BlockEntry{Kind: BlockKindBlock, Prefix: "192.0.2.1/32", Source: "panel", CreatedAt: t0, ExpiresAt: &ends}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneBlockEntries(t0.Add(9 * time.Minute)); err != nil || n != 0 {
		t.Fatalf("pruned early: %d %v", n, err)
	}
	if n, err := s.PruneBlockEntries(ends); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v; want 1", n, err)
	}
	if ok, err := s.DeleteBlockEntry(BlockKindAllow, "203.0.113.9/32"); !ok || err != nil {
		t.Fatalf("delete = %v %v", ok, err)
	}
	if ok, _ := s.DeleteBlockEntry(BlockKindAllow, "203.0.113.9/32"); ok {
		t.Fatal("deleted twice")
	}

	if v, err := s.BlocklistSettings(); err != nil || v != blocklist.DefaultSettings() {
		t.Fatalf("settings before saving = %+v, %v", v, err)
	}
	want := blocklist.Settings{Enabled: false, MaxFailures: 4, WindowMinutes: 30, BanMinutes: 120}
	if err := s.SetBlocklistSettings(want); err != nil {
		t.Fatal(err)
	}
	if v, err := s.BlocklistSettings(); err != nil || v != want {
		t.Fatalf("settings = %+v, %v", v, err)
	}
}
