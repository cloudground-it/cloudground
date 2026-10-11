package state

import (
	"errors"
	"path/filepath"
	"testing"
)

// Update and delete share one rule: the panel keeps an enabled admin, and a
// disabled admin does not count towards it.
func TestLastEnabledAdminIsKept(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.CreateUser("a@example.com", "h", "admin")
	b, _ := s.CreateUser("b@example.com", "h", "admin")
	op, _ := s.CreateUser("op@example.com", "h", "operator")

	if err := s.UpdateUserAccess(b.ID, "admin", true); err != nil {
		t.Fatalf("disabling one of two enabled admins: %v", err)
	}
	// b is disabled, so a is the last enabled admin.
	for name, err := range map[string]error{
		"delete":  s.DeleteUser(a.ID),
		"demote":  s.UpdateUserAccess(a.ID, "readonly", false),
		"disable": s.UpdateUserAccess(a.ID, "admin", true),
	} {
		if !errors.Is(err, ErrLastAdmin) {
			t.Errorf("%s of the last enabled admin = %v, want ErrLastAdmin", name, err)
		}
	}
	if got, _ := s.GetUserByEmail("a@example.com"); got.Role != "admin" || got.Disabled {
		t.Fatalf("a refused change was applied: %+v", got)
	}
	// Everything else still works.
	if err := s.UpdateUserAccess(a.ID, "admin", false); err != nil {
		t.Errorf("re-saving the last admin unchanged: %v", err)
	}
	if err := s.UpdateUserAccess(op.ID, "readonly", true); err != nil {
		t.Errorf("editing a non-admin: %v", err)
	}
	if err := s.DeleteUser(b.ID); err != nil {
		t.Errorf("deleting a disabled admin: %v", err)
	}
	if err := s.DeleteUser(op.ID); err != nil {
		t.Errorf("deleting a non-admin: %v", err)
	}
	if err := s.DeleteUser(9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a missing user = %v", err)
	}
}
