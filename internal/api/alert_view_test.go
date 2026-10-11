package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A task alert carries its job's kind and its error apart, the error in the
// reader's language when the catalogue has it: the interface parsed both
// out of the English message, so an Italian reader got an English error
// under an Italian title.
func TestTaskAlertCarriesKindAndLocalizedDetail(t *testing.T) {
	a := state.Alert{Kind: "task", Level: "error", Message: "migration import failed: The archive expands to more than 1 GiB, ...",
		Text: state.AlertText{Key: "alert.taskFailed", Args: []string{"migration import", "The archive expands to more than 1 GiB, ..."},
			DetailKey: "migration.tooLargeSize", DetailArgs: []string{"1 GiB"}}}
	v := alertView(a, "it")
	if v.TaskKind != "migration.import" {
		t.Fatalf("task_kind = %q", v.TaskKind)
	}
	if !strings.HasPrefix(v.Detail, "L'archivio si espande oltre 1 GiB") {
		t.Fatalf("detail = %q, want the Italian catalogue's", v.Detail)
	}
	// Without a key the error is the job's own.
	a.Text.DetailKey, a.Text.DetailArgs = "", nil
	a.Text.Args = []string{"deploy safe-push", "the guard blocked it"}
	if v := alertView(a, "it"); v.TaskKind != "deploy.safe-push" || v.Detail != "the guard blocked it" {
		t.Fatalf("view = %+v", v)
	}
	// Other alerts carry neither.
	if v := alertView(state.Alert{Kind: "drift", Text: state.AlertText{Key: "alert.drift", Counted: true, Count: 2}}, "it"); v.TaskKind != "" || v.Detail != "" {
		t.Fatalf("drift view = %+v", v)
	}
}

// A task alert names its job in words in every reader's language, the same
// name the task list uses, and the webhook's body keeps the raw kind apart
// for machines (#13). An alert stored before names (kind with spaces) reads
// the same.
func TestTaskAlertNamesTheJob(t *testing.T) {
	now := time.Now()
	set := taskConditions([]state.Job{{ID: 7, Kind: "backup.verify", SiteID: 3, Status: state.JobFailed,
		Error: "restic: boom", CreatedAt: now}}, map[int64]bool{3: true}, now)
	if len(set) != 1 {
		t.Fatalf("conditions = %+v", set)
	}
	a := set[0]
	a.ID, a.Kind, a.Site = "abc", "task", "shop.example.com"
	if a.Message != "Restore test failed: restic: boom" {
		t.Fatalf("message = %q", a.Message)
	}
	if v := alertView(a, "it"); v.TaskKind != "backup.verify" || v.TaskName != "Ripristino di prova" {
		t.Fatalf("view = %+v", v)
	}
	if got := alertMessage(a, "it"); got != "operazione non riuscita (Ripristino di prova): restic: boom" {
		t.Fatalf("it text = %q", got)
	}
	old := a
	old.Text.Args = []string{"backup verify", "restic: boom"}
	if alertMessage(old, "en") != alertMessage(a, "en") || alertView(old, "en").TaskKind != "backup.verify" {
		t.Fatalf("an old alert reads %q", alertMessage(old, "en"))
	}

	raw, err := json.Marshal(webhookBody(a, "en"))
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Text        string `json:"text"`
		Content     string `json:"content"`
		CloudGround struct {
			Kind     string `json:"kind"`
			TaskKind string `json:"task_kind"`
			Site     string `json:"site"`
		} `json:"cloudground"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Text != "CloudGround ERROR on shop.example.com: Restore test failed: restic: boom" || body.Content != body.Text {
		t.Fatalf("text = %q", body.Text)
	}
	if body.CloudGround.Kind != "task" || body.CloudGround.TaskKind != "backup.verify" || body.CloudGround.Site != "shop.example.com" {
		t.Fatalf("machine fields = %+v", body.CloudGround)
	}
	if strings.Contains(body.Text, "backup.verify") {
		t.Fatalf("the text names the raw kind: %q", body.Text)
	}
}
