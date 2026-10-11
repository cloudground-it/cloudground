package host

import (
	"context"
	"fmt"
)

// ForceDeleteUser removes a user with `userdel --force`: also while
// processes of its uid run. For a login that shares its uid with another
// user (a site's shell login, R-4.5), whose processes a plain userdel
// would count as the login's own. It removes no home and no group of the
// same name unless one exists; the caller makes sure none does.
func (s *ShadowAccounts) ForceDeleteUser(ctx context.Context, name string) error {
	if err := checkNames(name); err != nil {
		return err
	}
	return s.run(ctx, userdelCodes, "", "userdel", "--force", name)
}

// ForceDeleteUser removes the user whatever it runs.
func (f *FakeAccounts) ForceDeleteUser(ctx context.Context, name string) error {
	if err := checkNames(name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "userdel --force", name); err != nil {
		return err
	}
	if _, ok := f.users[name]; !ok {
		return fmt.Errorf("userdel: %w", ErrNoSuchUser)
	}
	delete(f.users, name)
	for _, m := range f.groups {
		delete(m, name)
	}
	delete(f.primary, name)
	return nil
}
