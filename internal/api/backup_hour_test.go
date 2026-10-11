package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// #29: a daily or weekly backup at a chosen hour. Without an hour the
// schedule is the spacing since the last backup, as before; with one, the
// backup runs at the first sweep after the hour, once per day or week, a
// weekly one on the same weekday, and a slot missed while the panel was
// down is caught up.
func TestBackupDueAtTheHour(t *testing.T) {
	at := func(day, hour, min int) time.Time { return time.Date(2026, 10, day, hour, min, 0, 0, time.UTC) }
	three := 3
	daily := state.BackupPolicy{Schedule: "daily", Hour: &three}
	weekly := state.BackupPolicy{Schedule: "weekly", Hour: &three}
	for _, c := range []struct {
		name      string
		p         state.BackupPolicy
		last, now time.Time
		want      bool
	}{
		{"no hour: a day after", state.BackupPolicy{Schedule: "daily"}, at(10, 14, 0), at(11, 14, 1), true},
		{"no hour: not yet a day", state.BackupPolicy{Schedule: "daily"}, at(10, 14, 0), at(11, 13, 0), false},
		{"hourly ignores the hour", state.BackupPolicy{Schedule: "hourly", Hour: &three}, at(11, 10, 0), at(11, 11, 0), true},
		{"before the hour", daily, at(10, 3, 5), at(11, 2, 55), false},
		{"at the hour", daily, at(10, 3, 5), at(11, 3, 0), true},
		{"just after the hour", daily, at(10, 3, 5), at(11, 3, 4), true},
		{"already ran today", daily, at(11, 3, 2), at(11, 9, 0), false},
		{"never ran", daily, time.Time{}, at(11, 9, 0), true},
		{"missed slot caught up", daily, at(9, 3, 0), at(11, 1, 0), true},
		{"hour changed after today's: not twice a day", daily, at(11, 1, 0), at(11, 3, 5), false},
		{"weekly: six days on", weekly, at(4, 3, 5), at(10, 3, 5), false},
		{"weekly: a week on, at the hour", weekly, at(4, 3, 5), at(11, 3, 0), true},
		{"weekly: a week on, before the hour", weekly, at(4, 3, 5), at(11, 2, 50), false},
	} {
		if got := backupDue(c.p, c.last, c.now); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}

// The policy route takes the hour, clears it with -1, and refuses one
// outside 0–23.
func TestBackupPolicyHour(t *testing.T) {
	s, _, ts := testServer(t)
	admin := setupAdmin(t, s, ts)
	site := seedActiveSite(t, s, "hour.example.com", state.SiteStatic, "", 0)
	path := fmt.Sprintf("/api/sites/%d/backups/policy", site.ID)

	resp, body := admin.do("PUT", path, map[string]any{"enabled": true, "schedule": "weekly", "hour": 4})
	var got state.BackupPolicy
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Hour == nil || *got.Hour != 4 || got.Schedule != "weekly" {
		t.Fatalf("PUT hour = %d %s", resp.StatusCode, body)
	}
	if stored, _ := s.Store.GetSite(site.ID); stored.Config.Backups.Hour == nil || *stored.Config.Backups.Hour != 4 {
		t.Fatalf("stored = %+v", stored.Config.Backups)
	}
	// Another field leaves the hour alone.
	resp, body = admin.do("PUT", path, map[string]any{"schedule": "daily"})
	got = state.BackupPolicy{}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Hour == nil || *got.Hour != 4 {
		t.Fatalf("PUT schedule = %d %s", resp.StatusCode, body)
	}
	for _, bad := range []int{24, -2} {
		resp, body := admin.do("PUT", path, map[string]any{"hour": bad})
		var a patchAnswer
		_ = json.Unmarshal(body, &a)
		if resp.StatusCode != http.StatusBadRequest || a.Fields["hour"] == "" {
			t.Errorf("hour %d = %d %s", bad, resp.StatusCode, body)
		}
	}
	resp, body = admin.do("PUT", path, map[string]any{"hour": -1})
	got = state.BackupPolicy{}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Hour != nil {
		t.Fatalf("clear = %d %s", resp.StatusCode, body)
	}
}
