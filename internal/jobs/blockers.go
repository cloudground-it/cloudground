package jobs

import "strings"

// BlocksPanelChange reports whether a running job of this kind must finish
// before the panel's own services are restarted or removed (SPEC §11): an
// update or an uninstall would cut it. Backups and restores (every
// `backup.` kind) hold restic's repository lock and leave a half-made
// snapshot or a half-restored site; the others are not resumable and would
// fail with their work half done.
func BlocksPanelChange(kind string) bool {
	if strings.HasPrefix(kind, "backup.") {
		return true
	}
	switch kind {
	case "site.create", "site.delete", "migration.import", "staging.database-push",
		"database.import", "database.export", "dbstudio.import", "dbstudio.export",
		"database.create", "database.drop", "wp.update", "wp.update.scheduled":
		return true
	}
	return false
}
