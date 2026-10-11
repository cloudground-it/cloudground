package state

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/api/events"
	_ "modernc.org/sqlite"
)

// Store is the panel's state database. Every method is safe for concurrent
// use: the one connection queues the callers (SPEC.md, Concurrency).
type Store struct {
	db *sql.DB
	// key seals and opens the secrets table; nil without a key file.
	key []byte
	// events, when set, is told about every committed change a browser
	// shows (internal/api/events/SPEC.md §2).
	events *events.Bus
}

// SetEvents connects the store to the panel's event bus, once, before the
// store is shared.
func (s *Store) SetEvents(b *events.Bus) { s.events = b }

// publish announces one committed change; without a bus it does nothing.
func (s *Store) publish(typ events.Type, siteID int64, data any) {
	s.events.Publish(typ, siteID, data)
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Options are what OpenWith takes beyond the database path.
type Options struct {
	// SecretKeyPath is the key file of the secret store, created the
	// first time. Empty opens the store with no secrets at all: the
	// secret calls answer ErrNoSecretKey, and plaintext waiting to be
	// sealed (legacy settings, migration 16's TOTP rows) stays where it
	// is until an Open with a key.
	SecretKeyPath string
}

// Open opens the state database at path, with the secret key kept beside
// it as secret.key. See OpenWith.
func Open(path string) (*Store, error) {
	return OpenWith(path, Options{SecretKeyPath: filepath.Join(filepath.Dir(path), "secret.key")})
}

// OpenWith opens the state database at path, creating it when it does not
// exist, and brings it up to date: the schema to its latest version, and,
// with a key, every secret still in plaintext into the sealed store, after
// which the file is compacted so the plaintext is gone from it.
func OpenWith(path string, opt Options) (*Store, error) {
	db, err := sql.Open("sqlite", stateDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// One connection: SQLite has one writer at a time, and a pool would
	// only turn concurrent task updates into SQLITE_BUSY retries.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.bringUpToDate(path, opt); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// stateDSN is the connection string: write-ahead logging, foreign keys,
// a five-second wait for the lock, normal syncing (safe under WAL), and
// secure_delete, so a secret deleted or overwritten is zeroed in the file
// instead of lingering in a free page.
func stateDSN(path string) string {
	pragmas := []string{
		"journal_mode(WAL)",
		"foreign_keys(ON)",
		"busy_timeout(5000)",
		"synchronous(NORMAL)",
		"secure_delete(ON)",
	}
	return "file:" + path + "?_pragma=" + strings.Join(pragmas, "&_pragma=")
}

// dbFileMode is the mode of the database and its sidecars: they hold
// authentication material and infrastructure credentials, so it is set
// explicitly on every Open instead of trusting the umask.
const dbFileMode = 0o640

func (s *Store) bringUpToDate(path string, opt Options) error {
	ranStateV1, err := s.applySchema(latestSchema)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, dbFileMode); err != nil {
		return fmt.Errorf("secure state db: %w", err)
	}
	// Plaintext left the tables: the v1 step deleted settings rows, or
	// the moves below sealed some.
	plaintextRemoved := ranStateV1
	holdingTOTP := false
	if opt.SecretKeyPath != "" {
		if err := s.loadOrCreateKey(opt.SecretKeyPath, path); err != nil {
			return err
		}
		moved, err := s.moveLegacySecrets()
		if err != nil {
			return fmt.Errorf("move secrets out of settings: %w", err)
		}
		plaintextRemoved = plaintextRemoved || moved
		if err := s.indexConnectorTokens(); err != nil {
			return fmt.Errorf("index connector tokens: %w", err)
		}
		// While migration 16's holding table exists it may have held
		// plaintext, even if this Open moved nothing (a crash after an
		// earlier move): it always means a compaction, and the table
		// goes only after it, so a crash in between compacts next time.
		if holdingTOTP, err = s.moveLegacyTOTP(); err != nil {
			return fmt.Errorf("move TOTP secrets out of users: %w", err)
		}
	}
	if plaintextRemoved || holdingTOTP {
		if err := s.compact(path); err != nil {
			return err
		}
	}
	if holdingTOTP {
		if err := s.dropTOTPImport(); err != nil {
			return fmt.Errorf("drop the TOTP holding table: %w", err)
		}
	}
	return secureSidecars(path)
}

// secureSidecars gives the WAL and shared-memory files the database's mode,
// when they exist.
func secureSidecars(path string) error {
	for _, side := range []string{path + "-wal", path + "-shm"} {
		err := os.Chmod(side, dbFileMode)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("secure state db sidecar: %w", err)
		}
	}
	return nil
}

// compact rewrites the file and empties the WAL, so that nothing deleted
// survives in a free page or in an old WAL frame.
func (s *Store) compact(path string) error {
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("compact state db: %w", err)
	}
	if err := s.emptyWAL(path); err != nil {
		return fmt.Errorf("checkpoint state db: %w", err)
	}
	return nil
}

// emptyWAL checkpoints the WAL into the database and truncates it. While a
// reader holds old frames SQLite answers "busy" rather than failing, and
// the frames would stay on disk: it tries five times, waiting a little
// longer each time, and then logs loudly and goes on (a later checkpoint
// removes them).
func (s *Store) emptyWAL(path string) error {
	const tries = 5
	for n := 1; n <= tries; n++ {
		var busy, walFrames, moved int
		if err := s.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &walFrames, &moved); err != nil {
			return err
		}
		if busy == 0 {
			return nil
		}
		time.Sleep(time.Duration(n) * 100 * time.Millisecond)
	}
	slog.Error("state db: the WAL could not be truncated after removing plaintext secrets; its old frames stay on disk until a later checkpoint",
		"path", path+"-wal")
	return nil
}
