package backup

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// H-4-2: the folder a full restore swapped out is removed, tried again
// when a try fails, and named in the log when it stays.
func TestRemoveReplacedRetriesAndLogs(t *testing.T) {
	var logs bytes.Buffer
	stage := filepath.Join(t.TempDir(), ".cg-restore-1")
	if err := os.MkdirAll(filepath.Join(stage, "new", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	busy := errors.New("device or resource busy")
	tries, waits := 0, 0
	o := &Ops{Log: slog.New(slog.NewTextHandler(&logs, nil)), RetryWait: func(int) { waits++ },
		RemoveAll: func(p string) error {
			if tries++; tries < 2 {
				return busy
			}
			return os.RemoveAll(p)
		}}
	o.removeReplaced("a.example", stage)
	if _, err := os.Stat(stage); !os.IsNotExist(err) || tries != 2 || waits != 1 {
		t.Fatalf("stage %v after %d tries, %d waits", err, tries, waits)
	}
	if !strings.Contains(logs.String(), "trying again") || strings.Contains(logs.String(), "left on disk") {
		t.Fatalf("log: %s", logs.String())
	}

	logs.Reset()
	tries, waits = 0, 0
	o.RemoveAll = func(string) error { tries++; return busy }
	o.removeReplaced("a.example", "/srv/sites/.cg-restore-2")
	if tries != removeTries || waits != removeTries-1 {
		t.Fatalf("%d tries, %d waits", tries, waits)
	}
	if !strings.Contains(logs.String(), "left on disk") || !strings.Contains(logs.String(), "/srv/sites/.cg-restore-2") {
		t.Fatalf("the folder left is not named: %s", logs.String())
	}
}
