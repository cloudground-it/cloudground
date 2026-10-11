package state

import (
	"fmt"
	"time"
)

// The schema is a numbered list of steps (schema_*.go). schema_migrations
// holds a row for every version a database has run; Open runs the ones it
// lacks in order, each in a transaction together with its row. A released
// step is history, since installed panels have run it: its number and what
// it leaves in the file never change (testdata/schema.golden pins the
// result). A change to the schema is a new step at the end.

// schemaStep takes a database from version-1 to version.
type schemaStep struct {
	version int
	stmts   []string
}

// schemaSteps is the whole history, oldest first; entry i is version i+1.
var schemaSteps = []schemaStep{
	stepInitial,           // 1
	stepAuthCronSchedule,  // 2
	stepOperatorSites,     // 3
	stepMetricHistory,     // 4
	stepTOTPReplay,        // 5
	stepTOTPEnrolment,     // 6
	stepSessionIdle,       // 7
	stepSessionClient,     // 8
	stepUserProfile,       // 9
	stepDropWriteOnly,     // 10
	stepStateV1,           // 11
	stepUserLocale,        // 12
	stepAPITokens,         // 13
	stepTaskErrorKeys,     // 14
	stepJobQueue,          // 15
	stepTOTPToSecrets,     // 16
	stepReservedSites,     // 17
	stepDeploymentMachine, // 18
	stepAlertsTable,       // 19
	stepReservedUI,        // 20
	stepUpdateChannel,     // 21
	stepSiteDatabases,     // 22
	stepBackupDatabases,   // 23
	stepSiteUsage,         // 24
	stepBlocklist,         // 25
}

// latestSchema is the version Open brings every database to.
var latestSchema = len(schemaSteps)

// versionStateV1 is the step that empties the legacy settings table
// (SPEC.md, Migrations): after it, Open compacts the file.
const versionStateV1 = 11

// applySchema runs every step up to version target that the database has
// not run yet. It reports whether the state v1 step was among them.
func (s *Store) applySchema(target int) (ranStateV1 bool, err error) {
	const ledger = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL
)`
	if _, err := s.db.Exec(ledger); err != nil {
		return false, fmt.Errorf("create migrations table: %w", err)
	}
	var have int
	if err := s.db.QueryRow(`SELECT IFNULL(MAX(version), 0) FROM schema_migrations`).Scan(&have); err != nil {
		return false, fmt.Errorf("read schema version: %w", err)
	}
	if have > len(schemaSteps) {
		return false, &SchemaNewerError{Have: have, Known: len(schemaSteps)}
	}
	for i, step := range schemaSteps[:target] {
		if step.version != i+1 {
			return ranStateV1, fmt.Errorf("schema step %d is listed as version %d", i+1, step.version)
		}
		if step.version <= have {
			continue
		}
		if err := s.runStep(step); err != nil {
			return ranStateV1, err
		}
		if step.version == versionStateV1 {
			ranStateV1 = true
		}
	}
	return ranStateV1, nil
}

// SchemaNewerError refuses a database a newer CloudGround has migrated:
// migrations only go forward, and this build would read tables it does not
// know as if they were its own (SPEC.md, Migrations). The way back is the
// snapshot the update took before the newer release first started.
type SchemaNewerError struct{ Have, Known int }

func (e *SchemaNewerError) Error() string {
	return fmt.Sprintf("the state database is at schema %d, newer than this build's %d: it was written by a newer CloudGround. "+
		"Install that release again, or put back the snapshot taken before it (/var/lib/cloudground-agent/update-snapshots)", e.Have, e.Known)
}

// runStep applies one step and records it, all or nothing.
func (s *Store) runStep(step schemaStep) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // a no-op once committed
	for _, q := range step.stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migration %d: %w", step.version, err)
		}
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, step.version, stamp); err != nil {
		return fmt.Errorf("record migration %d: %w", step.version, err)
	}
	return tx.Commit()
}
