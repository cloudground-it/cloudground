package host

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// FakeAccounts is an in-memory user and group database that fails exactly
// where ShadowAccounts does, with the same typed errors.
type FakeAccounts struct {
	mu       sync.Mutex
	users    map[string]*FakeUser
	groups   map[string]map[string]bool // group -> supplementary members
	primary  map[string]string          // user -> primary group
	ops      []string
	FailNext error // returned once by the next mutating call, then cleared
}

// FakeUser is a user's state in FakeAccounts.
type FakeUser struct {
	Spec     UserSpec
	Locked   bool
	Expired  bool
	Password string
}

// NewFakeAccounts returns an empty fake.
func NewFakeAccounts() *FakeAccounts {
	return &FakeAccounts{users: map[string]*FakeUser{}, groups: map[string]map[string]bool{}, primary: map[string]string{}}
}

// User returns a copy of a user's state.
func (f *FakeAccounts) User(name string) (FakeUser, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[name]
	if !ok {
		return FakeUser{}, false
	}
	return *u, true
}

// HasGroup reports whether a group exists.
func (f *FakeAccounts) HasGroup(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.groups[name]
	return ok
}

// Ops lists every mutating call as "<op> <args>".
func (f *FakeAccounts) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func (f *FakeAccounts) begin(ctx context.Context, op string, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.ops = append(f.ops, strings.TrimSpace(op+" "+strings.Join(args, " ")))
	if err := f.FailNext; err != nil {
		f.FailNext = nil
		return err
	}
	return nil
}

func (f *FakeAccounts) CreateUser(ctx context.Context, u UserSpec) error {
	if err := checkNames(append([]string{u.Name}, u.Groups...)...); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "useradd", u.Name); err != nil {
		return err
	}
	if _, ok := f.users[u.Name]; ok {
		return fmt.Errorf("useradd: %w", ErrUserExists)
	}
	for _, g := range u.Groups {
		if _, ok := f.groups[g]; !ok {
			return fmt.Errorf("useradd: %w", ErrNoSuchGroup)
		}
	}
	prim := u.PrimaryGroup
	if prim == "" {
		prim = u.Name
		if _, ok := f.groups[prim]; ok {
			return fmt.Errorf("useradd: %w", ErrGroupExists)
		}
		f.groups[prim] = map[string]bool{}
	} else if _, ok := f.groups[prim]; !ok {
		return fmt.Errorf("useradd: %w", ErrNoSuchGroup)
	}
	f.users[u.Name] = &FakeUser{Spec: u, Locked: true}
	f.primary[u.Name] = prim
	for _, g := range u.Groups {
		f.groups[g][u.Name] = true
	}
	return nil
}

func (f *FakeAccounts) DeleteUser(ctx context.Context, name string, removeHome bool) error {
	if err := checkNames(name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "userdel", name); err != nil {
		return err
	}
	if _, ok := f.users[name]; !ok {
		return fmt.Errorf("userdel: %w", ErrNoSuchUser)
	}
	delete(f.users, name)
	for _, m := range f.groups {
		delete(m, name)
	}
	// userdel removes the user's own group only when it has no other
	// member left (USERGROUPS_ENAB); a site's group keeps www-data.
	if prim := f.primary[name]; prim == name && len(f.groups[name]) == 0 {
		delete(f.groups, name)
	}
	delete(f.primary, name)
	return nil
}

func (f *FakeAccounts) withUser(ctx context.Context, op, name string, fn func(u *FakeUser)) error {
	if err := checkNames(name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, op, name); err != nil {
		return err
	}
	u, ok := f.users[name]
	if !ok {
		return fmt.Errorf("%s: %w", strings.Fields(op)[0], ErrNoSuchUser)
	}
	fn(u)
	return nil
}

func (f *FakeAccounts) LockUser(ctx context.Context, name string) error {
	return f.withUser(ctx, "usermod --lock", name, func(u *FakeUser) { u.Locked = true })
}

func (f *FakeAccounts) UnlockUser(ctx context.Context, name string) error {
	return f.withUser(ctx, "usermod --unlock", name, func(u *FakeUser) { u.Locked = false })
}

func (f *FakeAccounts) ExpireUser(ctx context.Context, name string, expired bool) error {
	return f.withUser(ctx, fmt.Sprintf("usermod --expire=%v", expired), name, func(u *FakeUser) { u.Expired = expired })
}

func (f *FakeAccounts) SetPassword(ctx context.Context, name, password string) error {
	if password == "" || strings.ContainsAny(password, "\n\r\x00") {
		return ErrInvalidPassword
	}
	// chpasswd sets a new hash, which also clears a lock ("!" prefix).
	return f.withUser(ctx, "chpasswd", name, func(u *FakeUser) { u.Password, u.Locked = password, false })
}

func (f *FakeAccounts) group(ctx context.Context, name string, force bool) error {
	if err := checkNames(name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "groupadd", name); err != nil {
		return err
	}
	if _, ok := f.groups[name]; ok {
		if force {
			return nil
		}
		return fmt.Errorf("groupadd: %w", ErrGroupExists)
	}
	f.groups[name] = map[string]bool{}
	return nil
}

func (f *FakeAccounts) EnsureGroup(ctx context.Context, name string, _ bool) error {
	return f.group(ctx, name, true)
}

func (f *FakeAccounts) CreateGroup(ctx context.Context, name string, _ bool) error {
	return f.group(ctx, name, false)
}

func (f *FakeAccounts) DeleteGroup(ctx context.Context, name string) error {
	if err := checkNames(name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "groupdel", name); err != nil {
		return err
	}
	if _, ok := f.groups[name]; !ok {
		return fmt.Errorf("groupdel: %w", ErrNoSuchGroup)
	}
	for _, g := range f.primary {
		if g == name {
			return fmt.Errorf("groupdel: %w", ErrPrimaryGroup)
		}
	}
	delete(f.groups, name)
	return nil
}

func (f *FakeAccounts) membership(ctx context.Context, op, user, group string) (map[string]bool, error) {
	if err := checkNames(user, group); err != nil {
		return nil, err
	}
	if err := f.begin(ctx, "gpasswd "+op, user, group); err != nil {
		return nil, err
	}
	m, ok := f.groups[group]
	if !ok {
		return nil, fmt.Errorf("gpasswd: %w", ErrNoSuchGroup)
	}
	if _, ok := f.users[user]; !ok {
		return nil, fmt.Errorf("gpasswd: %w", ErrNoSuchUser)
	}
	return m, nil
}

func (f *FakeAccounts) AddToGroup(ctx context.Context, user, group string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.membership(ctx, "-a", user, group)
	if err != nil {
		return err
	}
	m[user] = true
	return nil
}

func (f *FakeAccounts) RemoveFromGroup(ctx context.Context, user, group string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.membership(ctx, "-d", user, group)
	if err != nil {
		return err
	}
	if !m[user] {
		return fmt.Errorf("gpasswd: %w", ErrNotMember)
	}
	delete(m, user)
	return nil
}

func (f *FakeAccounts) IsMember(ctx context.Context, user, group string) (bool, error) {
	if err := checkNames(user, group); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.groups[group]
	if !ok {
		return false, fmt.Errorf("%q: %w", group, ErrNoSuchGroup)
	}
	return m[user], nil
}

func (f *FakeAccounts) UserExists(ctx context.Context, name string) (bool, error) {
	if err := checkNames(name); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.users[name]
	return ok, nil
}

var _ Accounts = (*FakeAccounts)(nil)
