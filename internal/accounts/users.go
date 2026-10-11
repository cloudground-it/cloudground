package accounts

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/cloudground-it/cloudground/internal/state"
)

// User management by an admin (SPEC §2.5). Each method takes the admin
// making the change (me) and the actor to audit.

// Roles are the account roles.
var Roles = []string{"admin", "operator", "readonly"}

const roleOperator = "operator"

func validRole(r string) bool { return slices.Contains(Roles, r) }

// UserForm is an account as an admin fills it in.
type UserForm struct {
	Email, Password     string
	Role                string
	SiteIDs             []int64
	FirstName, LastName string
	Disabled            bool
}

func (f UserForm) names() (string, string, error) {
	first, ok1 := CleanName(f.FirstName)
	last, ok2 := CleanName(f.LastName)
	if !ok1 || !ok2 {
		return "", "", refuse(Invalid, msgBadName)
	}
	return first, last, nil
}

func (f UserForm) needsSites() error {
	if f.Role == roleOperator && len(f.SiteIDs) == 0 {
		return refuse(Invalid, "assign at least one site to an operator")
	}
	return nil
}

// ListUsers returns every account.
func (sv *Service) ListUsers() ([]state.User, error) {
	list, err := sv.Store.ListUsers()
	if list == nil && err == nil {
		list = []state.User{}
	}
	return list, err
}

// CreateUser creates an account. A role is never assumed: a typo once fell
// through to admin, which on this panel is root.
func (sv *Service) CreateUser(ctx context.Context, actor string, f UserForm) (state.User, error) {
	email, err := NormalizeEmail(f.Email)
	if err != nil {
		return state.User{}, refuse(Invalid, err.Error())
	}
	if err := CheckNewPassword(f.Password); err != nil {
		return state.User{}, refuse(Invalid, err.Error())
	}
	switch {
	case f.Role == "":
		return state.User{}, refuse(Invalid, "role required: admin, operator or readonly")
	case !validRole(f.Role):
		return state.User{}, refuse(Invalid, "unknown role "+strconv.Quote(f.Role)+": use admin, operator or readonly")
	}
	if err := f.needsSites(); err != nil {
		return state.User{}, err
	}
	first, last, err := f.names()
	if err != nil {
		return state.User{}, err
	}
	hash, err := hashPassword(ctx, f.Password)
	if err != nil {
		return state.User{}, busy(err)
	}
	nu := state.NewUser{Email: email, PasswordHash: hash, Role: f.Role, FirstName: first, LastName: last}
	if f.Role == roleOperator {
		nu.SiteIDs = f.SiteIDs
	}
	u, err := sv.Store.CreateAccount(nu)
	switch {
	case errors.Is(err, state.ErrEmailTaken):
		return state.User{}, refuse(Conflict, err.Error())
	case errors.Is(err, state.ErrBadSite):
		return state.User{}, refuse(Invalid, msgBadSites)
	case err != nil:
		return state.User{}, err
	}
	sv.audit(actor, "user.create", u.Email, f.Role)
	return u, nil
}

// UpdateUser applies an admin's edit of account id: role, disabled flag,
// names and sites, all or nothing. An admin cannot demote or disable
// themselves, and the store keeps an enabled admin.
func (sv *Service) UpdateUser(me state.User, actor string, id int64, f UserForm) (state.User, error) {
	if !validRole(f.Role) {
		return state.User{}, refuse(Invalid, "unknown role: use admin, operator or readonly")
	}
	if err := f.needsSites(); err != nil {
		return state.User{}, err
	}
	first, last, err := f.names()
	if err != nil {
		return state.User{}, err
	}
	target, err := sv.Store.GetUser(id)
	if errors.Is(err, state.ErrNotFound) {
		return state.User{}, refuse(NotFound, msgNoUser)
	}
	if err != nil {
		return state.User{}, err
	}
	if me.ID == id && (f.Role != target.Role || f.Disabled) {
		return state.User{}, refuse(Invalid, "you cannot change your own role or disable your own account")
	}
	sites := f.SiteIDs
	if f.Role != roleOperator {
		sites = nil
	}
	err = sv.Store.UpdateUser(id, state.UserUpdate{Role: f.Role, Disabled: f.Disabled, FirstName: first, LastName: last, SiteIDs: sites})
	switch {
	case errors.Is(err, state.ErrLastAdmin):
		return state.User{}, refuse(Invalid, err.Error())
	case errors.Is(err, state.ErrBadSite):
		return state.User{}, refuse(Invalid, msgBadSites)
	case errors.Is(err, state.ErrNotFound):
		return state.User{}, refuse(NotFound, msgNoUser)
	case err != nil:
		return state.User{}, err
	}
	detail := f.Role
	if f.Disabled {
		detail += ", disabled"
	}
	sv.audit(actor, "user.update", target.Email, detail)
	target.Role, target.Disabled, target.FirstName, target.LastName, target.SiteIDs = f.Role, f.Disabled, first, last, sites
	return target, nil
}

// ResetPassword sets another account's password, ending its sessions and
// API tokens. One's own goes through ChangePassword, which asks for the
// current one.
func (sv *Service) ResetPassword(ctx context.Context, me state.User, actor string, id int64, password string) error {
	if err := CheckNewPassword(password); err != nil {
		return refuse(Invalid, err.Error())
	}
	if me.ID == id {
		return refuse(Invalid, "change your own password from Security")
	}
	hash, err := hashPassword(ctx, password)
	if err != nil {
		return busy(err)
	}
	if err := sv.Store.SetUserPassword(id, hash); errors.Is(err, state.ErrNotFound) {
		return refuse(NotFound, msgNoUser)
	} else if err != nil {
		return err
	}
	sv.audit(actor, "user.password_reset", strconv.FormatInt(id, 10), "")
	return nil
}

// DeleteUser removes account id, never one's own nor the last enabled
// admin. The audit names who was removed.
func (sv *Service) DeleteUser(me state.User, actor string, id int64) error {
	if me.ID == id {
		return refuse(Invalid, "you cannot delete your own account")
	}
	target, err := sv.Store.GetUser(id)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return err
	}
	switch err := sv.Store.DeleteUser(id); {
	case errors.Is(err, state.ErrLastAdmin):
		return refuse(Invalid, err.Error())
	case errors.Is(err, state.ErrNotFound):
		return refuse(NotFound, msgNoUser)
	case err != nil:
		return err
	}
	sv.audit(actor, "user.delete", target.Email, "")
	return nil
}
