package accounts

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// Host recovery, `panel-api recover-admin` (SPEC §2.6): the way back in for
// whoever owns the machine, when the password or the phone is gone.
//
// Root on the box is the credential, the boundary every server recovery
// has: root can already read the database and replace the binaries, so
// refusing to help it protects nothing and only strands the owner. The
// password is generated rather than taken as an argument, which would put
// it in the shell history and in every user's view of ps.

const recoveryActor = "root (console)"

// Recovery is what recover-admin was asked to do.
type Recovery struct {
	// Email is the account to reset, or to create on a panel with none;
	// empty lists the accounts.
	Email string
	// DisableTOTP also removes the account's second factors: TOTP,
	// passkeys and recovery codes.
	DisableTOTP bool
	// Root: the process runs as root. Nothing happens otherwise.
	Root bool
}

// Recover carries out a recovery on store, writing the new credential and
// any notes to out. The store may have no secret key: recovery reads
// whether 2FA is on, never a secret.
func Recover(store *state.Store, out io.Writer, req Recovery) error {
	if !req.Root {
		return errors.New("must be run as root: physical access to the server is the credential")
	}
	email := strings.TrimSpace(req.Email)
	if email != "" {
		norm, err := NormalizeEmail(email)
		if err != nil {
			return fmt.Errorf("%q is not a bare e-mail address", email)
		}
		email = norm
	}
	all, err := store.ListUsers()
	if err != nil {
		return fmt.Errorf("reading accounts: %w", err)
	}
	switch {
	case len(all) == 0 && email == "":
		return errors.New("no accounts exist: pass --email to create the first admin")
	case len(all) == 0:
		return createFirstAdmin(store, out, email)
	case email == "":
		// Never guess which account: resetting the wrong one looks exactly
		// like a recovery that worked.
		var b strings.Builder
		b.WriteString("no --email given. Accounts on this panel:\n")
		for _, u := range all {
			fmt.Fprintf(&b, "  %-40s %s\n", u.Email, u.Role)
		}
		b.WriteString("\nRe-run with --email <address> to reset one of them.")
		return errors.New(b.String())
	}
	target, err := store.GetUserByEmail(email)
	if errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("no account with the address %q. Run without --email to list them", email)
	}
	if err != nil {
		return err
	}
	return resetAccount(store, out, target, req.DisableTOTP)
}

// createFirstAdmin is recovery on a panel whose setup never finished, or
// whose last account was deleted: the installer's token is long gone.
func createFirstAdmin(store *state.Store, out io.Writer, email string) error {
	password, hash, err := newCredential()
	if err != nil {
		return err
	}
	u, err := store.CreateUser(email, hash, "admin")
	if err != nil {
		return fmt.Errorf("creating the account: %w", err)
	}
	markWhatsNewSeen(store, &u)
	_ = store.Audit(recoveryActor, "user.create", email, "first admin created by host recovery")
	printCredential(out, u.Email, password, "created")
	return nil
}

func resetAccount(store *state.Store, out io.Writer, target state.User, disableTOTP bool) error {
	password, hash, err := newCredential()
	if err != nil {
		return err
	}
	// The store ends the account's sessions and API tokens with it: if the
	// reason for the reset is that someone else has the old password, what
	// they opened with it must close too.
	if err := store.UpdatePassword(target.ID, hash); err != nil {
		return fmt.Errorf("setting the password: %w", err)
	}
	var notes []string
	if !target.PasswordSignIn {
		notes = append(notes, "password sign-in was off for this account: it is on again")
	}
	factors := secondFactors(target)
	// Taking the second factors away is asked for, never assumed: silently
	// dropping them would be worse than the lockout.
	switch {
	case disableTOTP && factors != "":
		if err := store.ResetSecondFactors(target.ID); err != nil {
			return fmt.Errorf("removing the second factors: %w", err)
		}
		notes = append(notes, "two-factor sign-in was removed: "+factors)
		_ = store.Audit(recoveryActor, "user.2fa.disable", target.Email, "removed by host recovery: "+factors)
	case disableTOTP:
		notes = append(notes, "two-factor was already off")
	case factors != "":
		notes = append(notes, "two-factor is still ON for this account ("+factors+"): re-run with --disable-2fa if they are lost")
	}
	// Disabled admins do not count, as everywhere else: a panel whose only
	// admin is disabled has none.
	admins, err := store.CountActiveAdmins()
	if err != nil {
		return fmt.Errorf("counting admin accounts: %w", err)
	}
	switch {
	case target.Disabled && target.Role == "admin" && admins == 0:
		// The new password would open nothing and nobody could enable the
		// account again: recovery restores the rule instead.
		if err := store.UpdateUserAccess(target.ID, "admin", false); err != nil {
			return fmt.Errorf("re-enabling the account: %w", err)
		}
		admins = 1
		notes = append(notes, "the account was disabled and no other admin was enabled: it has been re-enabled")
		_ = store.Audit(recoveryActor, "user.enable", target.Email, "re-enabled by host recovery: no enabled admin remained")
	case target.Disabled:
		notes = append(notes, "this account is disabled: it cannot sign in until an admin enables it again")
	}
	if target.Role != "admin" {
		notes = append(notes, fmt.Sprintf("this account is %q, not an admin", target.Role))
	}
	if admins == 0 {
		// Every admin gone, operators left who cannot make one: getting in
		// is not getting out of trouble.
		notes = append(notes, "there is no enabled admin account on this panel: create one from the UI once you are in, or delete the accounts and re-run this to make a fresh admin")
	}
	_ = store.Audit(recoveryActor, "user.password.reset", target.Email, "reset by host recovery; sessions and API tokens revoked")
	printCredential(out, target.Email, password, "reset")
	for _, n := range notes {
		fmt.Fprintf(out, "  note: %s\n", n)
	}
	return nil
}

// secondFactors names an account's second factors for recovery's notes,
// "" for none.
func secondFactors(u state.User) string {
	var parts []string
	if u.TOTPEnabled {
		parts = append(parts, "an authenticator app")
	}
	switch {
	case u.Passkeys == 1:
		parts = append(parts, "1 passkey")
	case u.Passkeys > 1:
		parts = append(parts, fmt.Sprintf("%d passkeys", u.Passkeys))
	}
	if len(parts) > 0 && u.RecoveryCodesLeft > 0 {
		parts = append(parts, fmt.Sprintf("%d recovery codes", u.RecoveryCodesLeft))
	}
	return strings.Join(parts, ", ")
}

func newCredential() (password, hash string, err error) {
	if password, err = GeneratePassword(); err != nil {
		return "", "", err
	}
	hash, err = HashPassword(password)
	return password, hash, err
}

// printCredential shows the password once and says it is kept nowhere.
func printCredential(out io.Writer, email, password, what string) {
	fmt.Fprintf(out, "\n  account:  %s\n  password: %s\n\n", email, password)
	fmt.Fprintf(out, "  Password %s. Existing sessions and API tokens of this account were revoked.\n", what)
	fmt.Fprintf(out, "  It is shown once and is not written anywhere. Sign in and change it.\n")
}
