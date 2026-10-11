package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/i18n"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Every task kind the panel can list has a name in the server's catalogue
// (internal/i18n, TaskName; its test holds the other four languages to
// English), which the task list, the alerts and the webhook share (#13):
// backup.retention and backup.forget once showed as their raw kind. The
// kinds are read from this package's source, the job requests and the
// registered kinds; the quiet (maintenance) ones are never listed and need
// none.
func TestEveryListedTaskKindIsNamed(t *testing.T) {
	s, _, _ := testServer(t)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	kindRe := regexp.MustCompile(`(?:kind|Kind|Name):\s+"([a-z]+\.[a-z_.-]+)"`)
	kinds := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range kindRe.FindAllStringSubmatch(string(b), -1) {
			if k, ok := s.Jobs.Kind(m[1]); ok && k.Quiet {
				continue
			}
			kinds[m[1]] = true
		}
	}
	if len(kinds) < 20 {
		t.Fatalf("only %d kinds found: %v", len(kinds), kinds)
	}
	var missing []string
	for k := range kinds {
		if !i18n.HasTaskName(k) {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("task kinds with no name: %v", missing)
	}
}

// The interface names tasks from the server alone: a second catalogue in
// its locales would drift from the one the alerts and the webhook use.
func TestInterfaceHasNoTaskNamesOfItsOwn(t *testing.T) {
	for _, lang := range []string{"it", "en", "fr", "es", "de"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "ui", "i18n", "locales", lang+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var loc struct {
			Tasks struct {
				Kind map[string]string `json:"kind"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(raw, &loc); err != nil {
			t.Fatal(err)
		}
		if len(loc.Tasks.Kind) > 0 {
			t.Errorf("%s.json still names %d task kinds (tasks.kind): they live in internal/i18n", lang, len(loc.Tasks.Kind))
		}
	}
}

// GET /api/tasks/names answers in the reader's language, and a task carries
// its name beside its raw kind.
func TestTaskNamesFollowTheReader(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	users, err := s.Store.ListUsers()
	if err != nil || len(users) != 1 {
		t.Fatalf("users %v %v", users, err)
	}
	if err := s.Store.UpdateUserLocale(users[0].ID, "de"); err != nil {
		t.Fatal(err)
	}
	resp, body := c.do(http.MethodGet, "/api/tasks/names", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	var names map[string]string
	if err := json.Unmarshal(body, &names); err != nil {
		t.Fatal(err)
	}
	if names["backup.verify"] != i18n.TaskName("de", "backup.verify") || names["backup.verify"] == "backup.verify" {
		t.Fatalf("backup.verify = %q", names["backup.verify"])
	}
	task := jobs.View(state.Job{Kind: "backup.verify"})
	localizeTask(&task, "en")
	if task.Kind != "backup.verify" || task.Name != "Restore test" {
		t.Fatalf("task = %q / %q", task.Kind, task.Name)
	}
}
