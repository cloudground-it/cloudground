package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Typed account errors. Every one is decided by an exit code or by the
// account databases, never by a tool's stderr.
var (
	ErrInvalidName     = errors.New("invalid user or group name")
	ErrInvalidArgument = errors.New("invalid argument to an account tool")
	ErrInvalidPassword = errors.New("password must be non-empty, without a newline or NUL")
	ErrUserExists      = errors.New("user already exists")
	ErrGroupExists     = errors.New("group already exists")
	ErrNoSuchUser      = errors.New("no such user")
	ErrNoSuchGroup     = errors.New("no such group")
	ErrNotMember       = errors.New("user is not a member of the group")
	ErrIDTaken         = errors.New("uid or gid already in use")
	ErrUserBusy        = errors.New("user is logged in or has running processes")
	ErrPrimaryGroup    = errors.New("group is a user's primary group")
	ErrHome            = errors.New("cannot create or remove the home directory")
)

// accountNameRe is what the agent accepts as a user or group name: shadow's
// own default rule, minus anything that could read as an option.
var accountNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidAccountName reports whether name is acceptable as a user or group.
func ValidAccountName(name string) bool { return accountNameRe.MatchString(name) }

// UserSpec describes a user to create.
type UserSpec struct {
	Name         string
	System       bool
	Home         string // "" leaves useradd's default
	CreateHome   bool
	Shell        string // "" leaves useradd's default
	PrimaryGroup string // "" lets useradd create the user's own group
	Groups       []string
	// UID, when above 0, is the uid to give the user (--uid); NonUnique
	// lets it be one another user has already (--non-unique), as a site's
	// shell login shares the site user's (R-4.5).
	UID       int
	NonUnique bool
}

// Accounts manages Unix users and groups.
type Accounts interface {
	CreateUser(ctx context.Context, u UserSpec) error
	DeleteUser(ctx context.Context, name string, removeHome bool) error
	LockUser(ctx context.Context, name string) error
	UnlockUser(ctx context.Context, name string) error
	// ExpireUser expires the account (true) or clears the expiry (false).
	ExpireUser(ctx context.Context, name string, expired bool) error
	SetPassword(ctx context.Context, name, password string) error
	// EnsureGroup creates the group unless it exists (groupadd -f).
	EnsureGroup(ctx context.Context, name string, system bool) error
	CreateGroup(ctx context.Context, name string, system bool) error
	DeleteGroup(ctx context.Context, name string) error
	// AddToGroup is idempotent: adding a member again succeeds.
	AddToGroup(ctx context.Context, user, group string) error
	RemoveFromGroup(ctx context.Context, user, group string) error
	IsMember(ctx context.Context, user, group string) (bool, error)
	// UserExists reports whether the user database has the user.
	UserExists(ctx context.Context, name string) (bool, error)
}

// ShadowAccounts drives the shadow tools.
type ShadowAccounts struct {
	Exec Exec
	// PasswdFile and GroupFile are the account databases read to classify
	// gpasswd's ambiguous exit 3; empty means /etc/passwd and /etc/group.
	PasswdFile string
	GroupFile  string
}

// NewAccounts returns the real Accounts.
func NewAccounts(e Exec) *ShadowAccounts { return &ShadowAccounts{Exec: e} }

// codeMap turns a tool's exit code into a typed error.
type codeMap map[int]error

func (s *ShadowAccounts) run(ctx context.Context, codes codeMap, stdin, name string, args ...string) error {
	c := Cmd{Name: name, Args: args}
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	_, err := s.Exec.Run(ctx, c)
	if err == nil {
		return nil
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		if typed, ok := codes[ee.Code]; ok {
			return fmt.Errorf("%s: %w", name, typed)
		}
	}
	return err
}

func checkNames(names ...string) error {
	for _, n := range names {
		if !ValidAccountName(n) {
			return fmt.Errorf("%q: %w", n, ErrInvalidName)
		}
	}
	return nil
}

// useradd(8) EXIT VALUES; 19 is E_BAD_NAME (shadow >= 4.15, measured on
// Ubuntu 26.04).
var useraddCodes = codeMap{3: ErrInvalidArgument, 4: ErrIDTaken, 6: ErrNoSuchGroup, 9: ErrUserExists, 12: ErrHome, 19: ErrInvalidName}

func (s *ShadowAccounts) CreateUser(ctx context.Context, u UserSpec) error {
	if err := checkNames(append([]string{u.Name}, u.Groups...)...); err != nil {
		return err
	}
	if u.PrimaryGroup != "" {
		if err := checkNames(u.PrimaryGroup); err != nil {
			return err
		}
	}
	var args []string
	if u.System {
		args = append(args, "--system")
	}
	if u.Home != "" {
		if !strings.HasPrefix(u.Home, "/") {
			return fmt.Errorf("home %q: %w", u.Home, ErrInvalidArgument)
		}
		args = append(args, "--home-dir", u.Home)
	}
	if u.CreateHome {
		args = append(args, "--create-home")
	} else {
		args = append(args, "--no-create-home")
	}
	if u.Shell != "" {
		if !strings.HasPrefix(u.Shell, "/") {
			return fmt.Errorf("shell %q: %w", u.Shell, ErrInvalidArgument)
		}
		args = append(args, "--shell", u.Shell)
	}
	if u.PrimaryGroup != "" {
		args = append(args, "--gid", u.PrimaryGroup)
	}
	if len(u.Groups) > 0 {
		args = append(args, "--groups", strings.Join(u.Groups, ","))
	}
	if u.UID > 0 {
		args = append(args, "--uid", strconv.Itoa(u.UID))
	}
	if u.NonUnique {
		if u.UID <= 0 {
			return fmt.Errorf("a non-unique user needs a uid: %w", ErrInvalidArgument)
		}
		args = append(args, "--non-unique")
	}
	args = append(args, u.Name)
	return s.run(ctx, useraddCodes, "", "useradd", args...)
}

// userdel(8) EXIT VALUES.
var userdelCodes = codeMap{6: ErrNoSuchUser, 8: ErrUserBusy, 12: ErrHome}

func (s *ShadowAccounts) DeleteUser(ctx context.Context, name string, removeHome bool) error {
	if err := checkNames(name); err != nil {
		return err
	}
	args := []string{name}
	if removeHome {
		args = []string{"--remove", name}
	}
	return s.run(ctx, userdelCodes, "", "userdel", args...)
}

// usermod(8) EXIT VALUES.
var usermodCodes = codeMap{3: ErrInvalidArgument, 6: ErrNoSuchUser, 8: ErrUserBusy}

func (s *ShadowAccounts) usermod(ctx context.Context, name string, args ...string) error {
	if err := checkNames(name); err != nil {
		return err
	}
	return s.run(ctx, usermodCodes, "", "usermod", append(args, name)...)
}

func (s *ShadowAccounts) LockUser(ctx context.Context, name string) error {
	return s.usermod(ctx, name, "--lock")
}

func (s *ShadowAccounts) UnlockUser(ctx context.Context, name string) error {
	return s.usermod(ctx, name, "--unlock")
}

func (s *ShadowAccounts) ExpireUser(ctx context.Context, name string, expired bool) error {
	date := ""
	if expired {
		date = "1" // 1970-01-02: expired
	}
	return s.usermod(ctx, name, "--expiredate", date)
}

// SetPassword feeds "name:password" to chpasswd on stdin. chpasswd exits 1
// for every failure, so the user is checked first.
func (s *ShadowAccounts) SetPassword(ctx context.Context, name, password string) error {
	if err := checkNames(name); err != nil {
		return err
	}
	if password == "" || strings.ContainsAny(password, "\n\r\x00") {
		return ErrInvalidPassword
	}
	ok, err := s.userExists(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("chpasswd: %w", ErrNoSuchUser)
	}
	return s.run(ctx, nil, name+":"+password+"\n", "chpasswd")
}

// groupadd(8) EXIT VALUES.
var groupaddCodes = codeMap{3: ErrInvalidArgument, 4: ErrIDTaken, 9: ErrGroupExists}

func (s *ShadowAccounts) groupadd(ctx context.Context, name string, system, force bool) error {
	if err := checkNames(name); err != nil {
		return err
	}
	var args []string
	if force {
		args = append(args, "--force")
	}
	if system {
		args = append(args, "--system")
	}
	return s.run(ctx, groupaddCodes, "", "groupadd", append(args, name)...)
}

func (s *ShadowAccounts) EnsureGroup(ctx context.Context, name string, system bool) error {
	return s.groupadd(ctx, name, system, true)
}

func (s *ShadowAccounts) CreateGroup(ctx context.Context, name string, system bool) error {
	return s.groupadd(ctx, name, system, false)
}

// groupdel(8) EXIT VALUES.
var groupdelCodes = codeMap{6: ErrNoSuchGroup, 8: ErrPrimaryGroup}

func (s *ShadowAccounts) DeleteGroup(ctx context.Context, name string) error {
	if err := checkNames(name); err != nil {
		return err
	}
	return s.run(ctx, groupdelCodes, "", "groupdel", name)
}

func (s *ShadowAccounts) AddToGroup(ctx context.Context, user, group string) error {
	return s.gpasswd(ctx, "-a", user, group)
}

func (s *ShadowAccounts) RemoveFromGroup(ctx context.Context, user, group string) error {
	return s.gpasswd(ctx, "-d", user, group)
}

// gpasswd exits 3 alike for a missing user, a missing group and (on -d) a
// user that is not a member (measured; its man page lists no exit values).
// The account databases tell them apart.
func (s *ShadowAccounts) gpasswd(ctx context.Context, op, user, group string) error {
	if err := checkNames(user, group); err != nil {
		return err
	}
	err := s.run(ctx, nil, "", "gpasswd", op, user, group)
	if ExitCode(err) != 3 {
		return err
	}
	if ok, lerr := s.groupExists(group); lerr != nil {
		return errors.Join(err, lerr)
	} else if !ok {
		return fmt.Errorf("gpasswd: %w", ErrNoSuchGroup)
	}
	if ok, lerr := s.userExists(user); lerr != nil {
		return errors.Join(err, lerr)
	} else if !ok {
		return fmt.Errorf("gpasswd: %w", ErrNoSuchUser)
	}
	if op == "-d" {
		if member, lerr := s.IsMember(ctx, user, group); lerr == nil && !member {
			return fmt.Errorf("gpasswd: %w", ErrNotMember)
		}
	}
	return err
}

// IsMember reports whether user is listed as a supplementary member of
// group in the group database.
func (s *ShadowAccounts) IsMember(_ context.Context, user, group string) (bool, error) {
	if err := checkNames(user, group); err != nil {
		return false, err
	}
	members, ok, err := s.groupMembers(group)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("%q: %w", group, ErrNoSuchGroup)
	}
	for _, m := range members {
		if m == user {
			return true, nil
		}
	}
	return false, nil
}

func (s *ShadowAccounts) passwdFile() string {
	if s.PasswdFile != "" {
		return s.PasswdFile
	}
	return "/etc/passwd"
}

func (s *ShadowAccounts) groupFile() string {
	if s.GroupFile != "" {
		return s.GroupFile
	}
	return "/etc/group"
}

// scanDB returns the fields of the entry named name in a colon-separated
// account database.
func scanDB(path, name string) ([]string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) > 0 && fields[0] == name {
			return fields, true, nil
		}
	}
	return nil, false, sc.Err()
}

// UserExists reads the user database (/etc/passwd), where useradd writes.
func (s *ShadowAccounts) UserExists(_ context.Context, name string) (bool, error) {
	if err := checkNames(name); err != nil {
		return false, err
	}
	return s.userExists(name)
}

func (s *ShadowAccounts) userExists(name string) (bool, error) {
	_, ok, err := scanDB(s.passwdFile(), name)
	return ok, err
}

func (s *ShadowAccounts) groupExists(name string) (bool, error) {
	_, ok, err := scanDB(s.groupFile(), name)
	return ok, err
}

func (s *ShadowAccounts) groupMembers(name string) ([]string, bool, error) {
	fields, ok, err := scanDB(s.groupFile(), name)
	if err != nil || !ok {
		return nil, ok, err
	}
	if len(fields) < 4 || fields[3] == "" {
		return nil, true, nil
	}
	return strings.Split(fields[3], ","), true, nil
}

var _ Accounts = (*ShadowAccounts)(nil)
