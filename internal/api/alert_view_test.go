package api

import (
	"strings"
	"testing"

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
