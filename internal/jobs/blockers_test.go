package jobs

import "testing"

func TestBlocksPanelChange(t *testing.T) {
	for kind, want := range map[string]bool{
		"backup.run": true, "backup.scheduled": true, "backup.restore": true, "backup.verify": true, "backup.prune": true,
		"site.create": true, "migration.import": true, "database.import": true,
		"cache.warm": false, "certificate.issue": false, "site.update": false, "": false,
	} {
		if got := BlocksPanelChange(kind); got != want {
			t.Errorf("BlocksPanelChange(%q) = %v, want %v", kind, got, want)
		}
	}
}
