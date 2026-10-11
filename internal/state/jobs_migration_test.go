package state

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The jobs migration from a database at schema 12 (before API tokens), 13
// (before the task error key) and 14: every task becomes a job with its ID;
// one left pending or running is failed; a failure's translation key and
// values come along; a log over the bound keeps its head and tail, counted
// in bytes (a log of two-byte characters is cut as its bytes say, not its
// characters).
func TestJobsMigrationFromTasks(t *testing.T) {
	for _, from := range []int{12, 13, 14} {
		t.Run(fmt.Sprintf("from %d", from), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			old, err := openAtVersion(path, from)
			if err != nil {
				t.Fatal(err)
			}
			long := strings.Repeat("é", 35000) // 70000 bytes, 35000 characters
			for _, row := range []struct {
				id                    int64
				kind, status, log, er string
			}{
				{1, "backup.run", "succeeded", "Snapshot taken\n", ""},
				{2, "site.create", "running", long, ""},
				{3, "wp.update", "pending", "", ""},
				{4, "site.php", "failed", "bad\n", "no such PHP"},
			} {
				if _, err := old.db.Exec(`INSERT INTO tasks (id, kind, site_id, status, progress, message, log, error, created_at, started_at)
					VALUES (?, ?, 7, ?, 40, 'msg', ?, ?, '2026-10-01T00:00:00Z', '2026-10-01T00:00:01Z')`,
					row.id, row.kind, row.status, row.log, row.er); err != nil {
					t.Fatal(err)
				}
			}
			if from >= 14 {
				if _, err := old.db.Exec(`UPDATE tasks SET error_key='migration.tooLargeRatio', error_args='["12.0 MiB","200"]' WHERE id=4`); err != nil {
					t.Fatal(err)
				}
			}
			old.Close()

			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var tables int
			s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='tasks'`).Scan(&tables)
			if tables != 0 {
				t.Fatal("the tasks table survived the migration")
			}
			want := map[int64]JobStatus{1: JobSucceeded, 2: JobFailed, 3: JobFailed, 4: JobFailed}
			for id, st := range want {
				j, err := s.GetJob(id)
				if err != nil || j.Status != st {
					t.Fatalf("job %d = %+v %v, want %s", id, j, err, st)
				}
			}
			if j, _ := s.GetJob(2); j.Error != "interrupted by panel restart" || j.FinishedAt == nil {
				t.Fatalf("a task left running = %q finished %v", j.Error, j.FinishedAt)
			}
			if j, _ := s.GetJob(4); j.Error != "no such PHP" || j.LogHead != "bad\n" {
				t.Fatalf("a failed task kept %q / %q", j.Error, j.LogHead)
			}
			if j, _ := s.GetJob(4); from >= 14 && (j.ErrorKey != "migration.tooLargeRatio" || strings.Join(j.ErrorArgs, ",") != "12.0 MiB,200") {
				t.Fatalf("the failure's key did not come along: %q %v", j.ErrorKey, j.ErrorArgs)
			}
			j, _ := s.GetJob(2)
			if len(j.LogHead) != 16384 || len(j.LogTail) != 49152 || j.LogDropped != 70000-65536 {
				t.Fatalf("long log: head %d, tail %d bytes, %d dropped; want 16384, 49152, %d",
					len(j.LogHead), len(j.LogTail), j.LogDropped, 70000-65536)
			}
			// The IDs go on from the tasks'.
			next, _, err := s.CreateJob(Job{Kind: "cache.warm"})
			if err != nil || next.ID != 5 {
				t.Fatalf("first new job = %d %v, want ID 5", next.ID, err)
			}
		})
	}
}

type keyed struct{ key string }

func (k keyed) Error() string                  { return "the English text" }
func (k keyed) MessageKey() (string, []string) { return k.key, []string{"12.0 MiB", "200"} }

// A failure that names its message keeps the key and the values beside the
// text, also when wrapped; any other failure stores the text alone.
func TestFinishJobKeepsTheErrorKey(t *testing.T) {
	s := testStore(t)
	for _, c := range []struct {
		err  error
		key  string
		args string
	}{
		{fmt.Errorf("wrapped: %w", keyed{"migration.tooLargeRatio"}), "migration.tooLargeRatio", "12.0 MiB,200"},
		{keyed{""}, "", ""},
		{errors.New("plain"), "", ""},
	} {
		j, _, err := s.CreateJob(Job{Kind: "migration.import"})
		if err != nil {
			t.Fatal(err)
		}
		key, args := ErrorKeyOf(c.err)
		if _, err := s.FinishJob(j.ID, JobFailed, c.err.Error(), key, args, 0, "", "", 0); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetJob(j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Error != c.err.Error() || got.ErrorKey != c.key || strings.Join(got.ErrorArgs, ",") != c.args {
			t.Errorf("%v: stored %q key %q args %v", c.err, got.Error, got.ErrorKey, got.ErrorArgs)
		}
	}
}
