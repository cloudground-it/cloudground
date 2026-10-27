package rpc

import "testing"

func TestRestorePath(t *testing.T) {
	for raw, want := range map[string]string{
		"shared/wp-config.php":    "shared/wp-config.php",
		" shared//uploads/2026/ ": "shared/uploads/2026",
		"releases/r1/index.php":   "releases/r1/index.php",
		"logs/app.log":            "logs/app.log",
		"":                        "",
		".":                       "",
		"/etc/passwd":             "",
		"../outside":              "",
		"shared/../../outside":    "",
		".ssh":                    "",
		".ssh/authorized_keys":    "",
		"tmp/sessions":            "",
		"current/wp-config.php":   "",
		"releases":                "",
		"shared/a\nb":             "",
	} {
		got, err := RestorePath(raw)
		if (want == "") != (err != nil) || got != want {
			t.Errorf("%q: %q %v, want %q", raw, got, err, want)
		}
	}
}

func TestRestoreParamsPaths(t *testing.T) {
	ok := RestoreParams{Mode: "files", Paths: []string{"shared/wp-config.php"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []RestoreParams{
		{Mode: "full", Paths: []string{"shared/x"}},
		{Mode: "files", Paths: []string{"../x"}},
		{Mode: "files", Paths: make([]string, 21)},
	} {
		if bad.Validate() == nil {
			t.Errorf("%+v passed", bad)
		}
	}
	if !ValidArchiveName(ArchiveName("ABCDEF1234567890", "database")) || ValidArchiveName("backup-x-full.tar.gz") {
		t.Fatal("archive names")
	}
}
