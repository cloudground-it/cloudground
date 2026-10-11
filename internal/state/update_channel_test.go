package state

import "testing"

// Migration 21: the release channel defaults to stable, keeps beta, and the
// column refuses anything else.
func TestUpdateChannel(t *testing.T) {
	s := testStore(t)
	ps, err := s.PanelSettings()
	if err != nil || ps.UpdateChannel != "stable" {
		t.Fatalf("default channel = %q, %v", ps.UpdateChannel, err)
	}
	if _, err := s.UpdatePanelSettings(func(p *PanelSettings) error { p.UpdateChannel = "beta"; return nil }); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.PanelSettings(); ps.UpdateChannel != "beta" {
		t.Fatalf("channel = %q after choosing beta", ps.UpdateChannel)
	}
	if _, err := s.UpdatePanelSettings(func(p *PanelSettings) error { p.UpdateChannel = "nightly"; return nil }); err == nil {
		t.Fatal("the store kept a channel that does not exist")
	}
	if ps, _ := s.PanelSettings(); ps.UpdateChannel != "beta" {
		t.Fatalf("a refused write changed the channel to %q", ps.UpdateChannel)
	}
}
