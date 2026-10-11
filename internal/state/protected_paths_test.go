package state

import "testing"

// #26: the site as panel users see it keeps each protected path and its
// username but never the password hash, and the stored site keeps it.
func TestPublicHidesProtectedPathHashes(t *testing.T) {
	s := Site{Config: SiteConfig{ProtectedPaths: []ProtectedPath{{Path: "/wp-admin", Username: "boss", PasswordHash: "$2a$10$x"}}}}
	p := s.Public()
	if len(p.Config.ProtectedPaths) != 1 || p.Config.ProtectedPaths[0] != (ProtectedPath{Path: "/wp-admin", Username: "boss"}) {
		t.Fatalf("public = %+v", p.Config.ProtectedPaths)
	}
	if s.Config.ProtectedPaths[0].PasswordHash != "$2a$10$x" {
		t.Fatal("Public changed the stored site's hash")
	}
}
