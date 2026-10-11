package version

import "testing"

// The origin is https, or plain http that only this machine can answer
// (SPEC §4): the upgrade test's local feed.
func TestCheckOrigin(t *testing.T) {
	for origin, ok := range map[string]bool{
		"https://github.com/cloudground-it/cloudground": true,
		"http://127.0.0.1:18931":                        true,
		"http://127.1.2.3/feed":                         true,
		"http://[::1]:8080":                             true,
		"http://localhost:9000":                         true,
		"http://github.com/cloudground-it/cloudground":  false,
		"http://10.0.0.5":                               false,
		"http://127.0.0.1.example.com":                  false,
		"ftp://127.0.0.1":                               false,
		"https://user:pw@example.com":                   false,
		"https://example.com/?x=1":                      false,
		"github.com/cloudground-it/cloudground":         false,
		"":                                              false,
	} {
		if err := CheckOrigin(origin); (err == nil) != ok {
			t.Errorf("CheckOrigin(%q) = %v, want ok=%v", origin, err, ok)
		}
	}
}

// A build whose origin is plain http to another machine cannot update.
func TestCanUpdateChecksTheOrigin(t *testing.T) {
	old := UpdateURL
	defer func() { UpdateURL = old }()
	UpdateURL = "http://example.com/o/r"
	if CanUpdate() == nil {
		t.Fatal("a plain-http origin was accepted")
	}
}
