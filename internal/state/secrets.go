package state

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Names of the panel-wide secrets. Per-site secrets are named by SiteSecret.
const (
	SecretBackupPassword    = "backup_password"
	SecretBackupS3SecretKey = "backup_s3_secret_key"
	SecretCloudflareToken   = "cloudflare_api_token"
	// SecretCloudflarePurgeToken empties Cloudflare's cache of a site
	// (#22): a token with the zone permission Cache Purge only, never the
	// DNS one.
	SecretCloudflarePurgeToken = "cloudflare_purge_token"
	// The alert webhook URL is a credential: whoever has it can post as the
	// panel.
	SecretNotifyWebhook = "notify_webhook"
	// SecretMailPassword is the server-wide mail relay's (#77).
	SecretMailPassword = "mail_password"
)

// Kinds of per-site secret.
const (
	SiteSecretDBPassword      = "db_password"
	SiteSecretConnectorToken  = "connector_token"
	SiteSecretStagingPassword = "staging_password"
	// SiteSecretMailPassword is the site's own mail relay's (#77).
	SiteSecretMailPassword = "mail_password"
)

// panelSecrets are the panel-wide secret names, also as the settings keys
// they were stored under before v1.
var panelSecrets = []string{SecretBackupPassword, SecretBackupS3SecretKey, SecretCloudflareToken, SecretNotifyWebhook}

// SiteSecret names one of a site's secrets: site.<id>.<kind>.
func SiteSecret(siteID int64, kind string) string {
	return SiteSecretPrefix(siteID) + kind
}

// SiteSecretPrefix is the prefix of every secret of one site.
func SiteSecretPrefix(siteID int64) string {
	return "site." + strconv.FormatInt(siteID, 10) + "."
}

// ErrNoSecretKey is returned by the secret calls of a Store opened without a
// key file.
var ErrNoSecretKey = errors.New("secret store unavailable: no key")

const (
	secretKeyLen   = 32
	sealVersion    = 1
	keyCheckLabel  = "cloudground secret key check v1"
	metaKeyCheck   = "secret_key_check"
	secretSiteName = "site."
)

// siteOfSecret returns the site a secret belongs to, from its name: the id
// in site.<id>.<kind>, or 0 for a panel-wide secret.
func siteOfSecret(name string) (int64, error) {
	if !strings.HasPrefix(name, secretSiteName) {
		return 0, nil
	}
	rest := strings.TrimPrefix(name, secretSiteName)
	idStr, kind, ok := strings.Cut(rest, ".")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if !ok || kind == "" || err != nil || id <= 0 {
		return 0, fmt.Errorf("malformed site secret name %q", name)
	}
	return id, nil
}

// loadOrCreateKey reads the key file, or creates it when the database has
// never had one. It refuses to create a key for a database whose secrets
// were sealed with another: that would orphan every one of them.
func (s *Store) loadOrCreateKey(keyPath, dbPath string) error {
	var check string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, metaKeyCheck).Scan(&check)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var sealed int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&sealed); err != nil {
		return err
	}
	raw, err := os.ReadFile(keyPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if check != "" || sealed > 0 {
			return fmt.Errorf("secret key %s is missing, but this database holds secrets sealed with one: restore the key file (a new key cannot open them)", keyPath)
		}
		key := make([]byte, secretKeyLen)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		if err := writeKeyFile(keyPath, dbPath, key); err != nil {
			return err
		}
		s.key = key
	case err != nil:
		return fmt.Errorf("read secret key: %w", err)
	default:
		key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != secretKeyLen {
			return fmt.Errorf("secret key %s is malformed (want %d hex-encoded bytes)", keyPath, secretKeyLen)
		}
		s.key = key
	}
	want := keyCheck(s.key)
	if check == "" {
		_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, metaKeyCheck, want)
		return err
	}
	if !hmac.Equal([]byte(check), []byte(want)) {
		s.key = nil
		return fmt.Errorf("secret key %s is not the key this database's secrets were sealed with", keyPath)
	}
	return nil
}

// writeKeyFile creates the key file, 0600, never overwriting one. Run as
// root (recover-admin), it hands the file to the state database's owner, the
// API user, who must be able to read it.
func writeKeyFile(keyPath, dbPath string, key []byte) error {
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create secret key: %w", err)
	}
	_, werr := f.WriteString(hex.EncodeToString(key) + "\n")
	if werr == nil {
		werr = f.Sync()
	}
	if os.Geteuid() == 0 && werr == nil {
		if info, err := os.Stat(dbPath); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				werr = f.Chown(int(st.Uid), int(st.Gid))
			}
		}
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(keyPath)
		return fmt.Errorf("write secret key: %w", werr)
	}
	return nil
}

func keyCheck(key []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(keyCheckLabel))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Store) aead() (cipher.AEAD, error) {
	if s.key == nil {
		return nil, ErrNoSecretKey
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal encrypts value for name; the name is authenticated with it, so a
// sealed value moved to another name does not open.
func (s *Store) seal(name, value string) ([]byte, error) {
	a, err := s.aead()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+a.NonceSize(), 1+a.NonceSize()+len(value)+a.Overhead())
	out[0] = sealVersion
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, err
	}
	return a.Seal(out, out[1:], []byte(value), []byte(name)), nil
}

func (s *Store) open(name string, sealed []byte) (string, error) {
	a, err := s.aead()
	if err != nil {
		return "", err
	}
	if len(sealed) < 1+a.NonceSize() || sealed[0] != sealVersion {
		return "", fmt.Errorf("secret %s: unknown format", name)
	}
	nonce := sealed[1 : 1+a.NonceSize()]
	plain, err := a.Open(nil, nonce, sealed[1+a.NonceSize():], []byte(name))
	if err != nil {
		return "", fmt.Errorf("secret %s: cannot be decrypted with this key", name)
	}
	return string(plain), nil
}

// Secret returns the named secret, or "" when it is not set.
func (s *Store) Secret(name string) (string, error) {
	return s.secretFrom(s.db, name)
}

// rowQuerier reads one row: the database, a transaction or a connection
// (connExec).
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// secretFrom is Secret read through q.
func (s *Store) secretFrom(q rowQuerier, name string) (string, error) {
	if s.key == nil {
		return "", ErrNoSecretKey
	}
	var sealed []byte
	err := q.QueryRow(`SELECT sealed FROM secrets WHERE name=?`, name).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return s.open(name, sealed)
}

// SetSecret stores the named secret; an empty value deletes it. A site's
// secret (site.<id>.<kind>) requires the site to exist, and goes with it.
func (s *Store) SetSecret(name, value string) error {
	return s.setSecret(s.db, name, value)
}

func (s *Store) setSecret(q execer, name, value string) error {
	return s.setSecretLookup(q, name, value, secretLookup(name, value))
}

// setSecretLookup is setSecret with the lookup column given (nil: none).
func (s *Store) setSecretLookup(q execer, name, value string, lookup any) error {
	if value == "" {
		if s.key == nil {
			return ErrNoSecretKey
		}
		_, err := q.Exec(`DELETE FROM secrets WHERE name=?`, name)
		return err
	}
	siteID, err := siteOfSecret(name)
	if err != nil {
		return err
	}
	sealed, err := s.seal(name, value)
	if err != nil {
		return err
	}
	var site any
	if siteID != 0 {
		site = siteID
	}
	_, err = q.Exec(`INSERT INTO secrets (name, site_id, sealed, updated_at, lookup) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET site_id=excluded.site_id, sealed=excluded.sealed, updated_at=excluded.updated_at, lookup=excluded.lookup`,
		name, site, sealed, now(), lookup)
	if err != nil {
		return fmt.Errorf("store secret %s: %w", name, err)
	}
	return nil
}

// DeleteSecret removes the named secret; removing an absent one is not an
// error.
func (s *Store) DeleteSecret(name string) error {
	_, err := s.db.Exec(`DELETE FROM secrets WHERE name=?`, name)
	return err
}

// DeleteSecretsWithPrefix removes every secret whose name starts with
// prefix, e.g. SiteSecretPrefix(id). An empty prefix is refused.
func (s *Store) DeleteSecretsWithPrefix(prefix string) error {
	return deleteSecretsWithPrefix(s.db, prefix)
}

func deleteSecretsWithPrefix(q execer, prefix string) error {
	if prefix == "" {
		return errors.New("refusing to delete every secret")
	}
	_, err := q.Exec(`DELETE FROM secrets WHERE substr(name, 1, ?) = ?`, len(prefix), prefix)
	return err
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// moveLegacySecrets seals every secret still in the settings table into
// secrets and deletes the settings rows, in one transaction. A site secret
// of a site that no longer exists, or an empty value (how earlier versions
// "deleted" one), is dropped. An existing secrets row wins over a legacy
// value. It reports whether anything was removed from settings.
func (s *Store) moveLegacySecrets() (moved bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.Query(`SELECT key, value FROM settings WHERE key LIKE 'site.%' OR key IN (?, ?, ?, ?)`,
		panelSecrets[0], panelSecrets[1], panelSecrets[2], panelSecrets[3])
	if err != nil {
		return false, err
	}
	type kv struct{ k, v string }
	var legacy []kv
	for rows.Next() {
		var r kv
		if err := rows.Scan(&r.k, &r.v); err != nil {
			rows.Close()
			return false, err
		}
		legacy = append(legacy, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(legacy) == 0 {
		return false, tx.Rollback()
	}
	for _, r := range legacy {
		if r.v != "" {
			keep, err := legacyWanted(tx, r.k)
			if err != nil {
				return false, err
			}
			if keep {
				err := s.setSecret(tx, r.k, r.v)
				if err != nil && strings.Contains(err.Error(), "secrets.lookup") {
					// Two sites with one connector token (never minted so):
					// the second is kept unindexed, its purges refused,
					// rather than the panel refusing to open.
					err = s.setSecretLookup(tx, r.k, r.v, nil)
				}
				if err != nil {
					return false, err
				}
			}
		}
		if _, err := tx.Exec(`DELETE FROM settings WHERE key=?`, r.k); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// legacyWanted reports whether a legacy secret should move: its site, if
// any, exists, and no sealed value already exists under its name.
func legacyWanted(tx *sql.Tx, name string) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM secrets WHERE name=?`, name).Scan(&n); err != nil || n > 0 {
		return false, err
	}
	siteID, err := siteOfSecret(name)
	if err != nil {
		return false, nil // not a name the panel ever wrote: drop it
	}
	if siteID == 0 {
		return true, nil
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sites WHERE id=?`, siteID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
