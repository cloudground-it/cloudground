package phpfpm

import (
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/state"
)

// #77: mail() goes to the panel's mail gate on every site but a staging
// copy and a site whose mail is off, and the connector gets the sender
// only when it is a plain address.
func TestPoolMail(t *testing.T) {
	const gate = "php_admin_value[sendmail_path] = " + SendmailPath + "\n"
	for _, tc := range []struct {
		name    string
		staging int64
		mail    state.SiteMail
		want    string
	}{
		{"default", 0, state.SiteMail{}, gate},
		{"a sender", 0, state.SiteMail{From: "shop@example.com"}, gate + "env[CLOUDGROUND_MAIL_FROM] = shop@example.com\n"},
		{"a sender that is not plain", 0, state.SiteMail{From: "a@b.c\nphp_admin_value[x] = y"}, gate},
		{"off", 0, state.SiteMail{Off: true, From: "shop@example.com"}, ""},
		{"staging", 3, state.SiteMail{From: "shop@example.com"}, ""},
	} {
		s := state.Site{ID: 7, Domain: "a.example.com", Type: state.SiteWordPress, SystemUser: "cg-site-7", PHPVersion: "8.4",
			RootPath: "/srv/sites/a.example.com", StagingOf: tc.staging}
		s.Config.Mail = tc.mail
		_, conf, err := RenderPool(s, 1024)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, line := range strings.SplitAfter(conf, "\n") {
			if strings.Contains(line, "sendmail_path") || strings.HasPrefix(line, "env[CLOUDGROUND_MAIL") {
				got += line
			}
		}
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
