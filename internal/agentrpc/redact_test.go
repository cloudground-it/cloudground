package agentrpc

import (
	"errors"
	"strings"
	"testing"
)

// A URL's password never reaches an error's detail, which both journals
// keep.
func TestDetailRedactsURLPasswords(t *testing.T) {
	for in, want := range map[string]string{
		"Fatal: unable to open repository at rest:https://backup:s3cr3t@host:8000/repo": "rest:https://backup:***@host:8000/repo",
		"s3:https://AKIA:wJalrXUtn@s3.example.com/bucket: 403":                          "s3:https://AKIA:***@s3.example.com/bucket",
		"no credentials in https://example.com/a@b":                                     "https://example.com/a@b",
	} {
		if got := RedactCredentials(in); !strings.Contains(got, want) {
			t.Errorf("%q -> %q, want it to contain %q", in, got, want)
		}
	}
	e := toError(Spec{Name: "RunBackup", Fail: "The backup failed"}, errors.New("restic: rest:https://u:pw-secret@h/r: exit status 1"))
	if strings.Contains(e.Detail, "pw-secret") {
		t.Fatalf("detail keeps the password: %q", e.Detail)
	}
}
