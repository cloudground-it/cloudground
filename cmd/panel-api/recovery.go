package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/config"
	"github.com/cloudground-it/cloudground/internal/state"
)

const recoverHelp = `panel-api recover-admin: get back into the panel from the server itself.

Run it as root. Root on the machine is the credential here, as for every
other way of recovering a server: root can read the database and replace
the binaries anyway.

  panel-api recover-admin                              list the accounts
  panel-api recover-admin --email you@example.com      reset that account
  panel-api recover-admin --email you@example.com --disable-2fa

A new password is generated and printed once; it is never an argument,
which would leave it in the shell history and in ps. A reset also ends the
account's open sessions and revokes its API tokens.

`

// recoverAdmin opens the database directly, not through the API: it is for
// when the API will not let anyone in, or is not running at all.
func recoverAdmin(args []string) error {
	fs := flag.NewFlagSet("recover-admin", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	email := fs.String("email", "", "the account to reset (or to create, when the panel has none)")
	disable2FA := fs.Bool("disable-2fa", false, "also remove every second factor (authenticator app, passkeys, recovery codes), when they are lost")
	statePath := fs.String("state", "", "the panel database (default: from the configuration)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, recoverHelp)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	// Root first: as another user the database would not open, and the
	// error said so in the database's words, not this one.
	if geteuid() != 0 {
		return errNotRoot
	}

	path := *statePath
	if path == "" {
		// A broken configuration must not lock the owner out as well: fall
		// back to the default database and say so.
		cfg, err := config.Load(config.API)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\nusing the default database; pass --state to choose another\n", err)
			path = "/var/lib/cloudground/state.db"
		} else {
			path = cfg.StatePath
		}
	}

	// No secret key: recovery touches only accounts, and must neither create
	// a key file as root (which the API could not then read) nor fail on a
	// box whose key is lost.
	store, err := state.OpenWith(path, state.Options{})
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer store.Close()
	return accounts.Recover(store, os.Stdout, accounts.Recovery{Email: *email, DisableTOTP: *disable2FA, Root: geteuid() == 0})
}

// geteuid is os.Geteuid (tests replace it).
var geteuid = os.Geteuid

// errNotRoot is recover-admin run by another user.
var errNotRoot = errors.New("run as root: recover-admin reads and changes the panel's database directly (sudo panel-api recover-admin …)")
