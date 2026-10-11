package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// A user's interface language is saved with their profile, validated against
// the five supported codes, returned by /api/session, and survives a new
// sign-in. Changing it alone leaves the name as it is.
func TestProfileLocale(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)

	session := func() state.User {
		t.Helper()
		_, body := c.do("GET", "/api/session", nil)
		var out struct{ User state.User }
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("session: %v (%s)", err, body)
		}
		return out.User
	}
	if !strings.Contains(func() string { _, b := c.do("GET", "/api/session", nil); return string(b) }(), `"locale":""`) {
		t.Fatal("a new account should report an empty locale (not chosen yet)")
	}

	if resp, body := c.do("PUT", "/api/account/profile", map[string]string{"first_name": "Grace", "last_name": "Hopper"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("profile = %d %s", resp.StatusCode, body)
	}
	for _, code := range state.Locales {
		if resp, body := c.do("PUT", "/api/account/profile", map[string]string{"locale": code}); resp.StatusCode != http.StatusOK {
			t.Fatalf("locale %s = %d %s", code, resp.StatusCode, body)
		}
		if u := session(); u.Locale != code {
			t.Fatalf("session locale = %q, want %q", u.Locale, code)
		}
	}
	if u := session(); u.FirstName != "Grace" || u.LastName != "Hopper" {
		t.Fatalf("setting the locale changed the name: %+v", u)
	}

	for _, bad := range []string{"", "pt", "de-DE", "DE", "en_US", "../it"} {
		if resp, _ := c.do("PUT", "/api/account/profile", map[string]string{"locale": bad}); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("locale %q accepted (%d)", bad, resp.StatusCode)
		}
	}
	if u := session(); u.Locale != "de" {
		t.Fatalf("a rejected locale changed the saved one: %q", u.Locale)
	}

	// A new sign-in sees the saved choice.
	fresh := &client{t: t, base: ts.URL}
	if resp, body := fresh.do("POST", "/api/login", map[string]string{"email": "admin@example.com", "password": "a-long-password!"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d %s", resp.StatusCode, body)
	}
	if _, body := fresh.do("GET", "/api/session", nil); !strings.Contains(string(body), `"locale":"de"`) {
		t.Fatalf("session after a new login = %s", body)
	}
}

// The alert webhook speaks the admins' language: the first enabled admin
// who chose one, Italian otherwise.
func TestWebhookFollowsAdminLocale(t *testing.T) {
	s, _, _ := testServer(t)
	got := make(chan string, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(b, &m)
		got <- m["text"]
	}))
	defer hook.Close()
	site, err := s.Store.CreateSite(state.Site{Domain: "hook.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced, Engine: state.EngineNginx, SystemUser: "cg-site-1", RootPath: "/srv/sites/hook.example.com", Status: state.SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetCertExpiry(site.ID, time.Now().Add(10*24*time.Hour+time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.webhookClient = hook.Client() // the guarded client refuses loopback
	if err := s.Store.SetSecret(state.SecretNotifyWebhook, hook.URL); err != nil {
		t.Fatal(err)
	}
	if lang := s.notifyLocale(); lang != "it" {
		t.Fatalf("no admin: webhook language = %q, want it", lang)
	}
	admin, err := s.Store.CreateUser("admin@example.com", "x", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpdateUserLocale(admin.ID, "de"); err != nil {
		t.Fatal(err)
	}
	if err := s.runAlertsNotify(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-got:
		if want := "CloudGround WARNUNG bei hook.example.com: Zertifikat läuft in 10 Tagen ab"; text != want {
			t.Fatalf("webhook text = %q, want %q", text, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no webhook delivered")
	}

	a, ok := certAlert(t, tableAlerts(t, s))
	if !ok {
		t.Fatal("no certificate alert")
	}
	stored, err := s.Store.GetAlert(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Message != "certificate expires in 10 days" {
		t.Fatalf("the dashboard message must stay English (the UI parses it): %q", a.Message)
	}
	for lang, want := range map[string]string{
		"it": "CloudGround AVVISO su hook.example.com: il certificato scade tra 10 giorni",
		"en": "CloudGround WARNING on hook.example.com: certificate expires in 10 days",
		"fr": "CloudGround AVERTISSEMENT sur hook.example.com\u00a0: le certificat expire dans 10 jours",
		"es": "CloudGround AVISO en hook.example.com: el certificado caduca en 10 días",
	} {
		if got := alertText(stored, lang); got != want {
			t.Errorf("%s: %q, want %q", lang, got, want)
		}
	}
}

// The dashboard gets each alert's text in the interface's language (sent as
// Accept-Language), or in the user's saved one when the client sends none it
// knows.
func TestAlertTextFollowsLanguage(t *testing.T) {
	s, _, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	site, err := s.Store.CreateSite(state.Site{Domain: "txt.example.com", Type: state.SiteStatic, Profile: state.ProfileBalanced, Engine: state.EngineNginx, SystemUser: "cg-site-1", RootPath: "/srv/sites/txt.example.com", Status: state.SiteActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetCertExpiry(site.ID, time.Now().Add(5*24*time.Hour+time.Hour)); err != nil {
		t.Fatal(err)
	}
	evaluateAt(t, s, time.Now()) // GET reads the table the evaluator writes
	fetch := func(acceptLanguage string) Alert {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/api/system/alerts", nil)
		for _, ck := range c.cookies {
			req.AddCookie(ck)
		}
		if acceptLanguage != "" {
			req.Header.Set("Accept-Language", acceptLanguage)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var alerts []Alert
		if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
			t.Fatal(err)
		}
		a, ok := certAlert(t, alerts)
		if !ok {
			t.Fatal("no certificate alert")
		}
		return a
	}
	if a := fetch("es"); a.Text != "el certificado caduca en 5 días" || a.Message != "certificate expires in 5 days" {
		t.Fatalf("es: text %q, message %q", a.Text, a.Message)
	}
	if a := fetch(""); a.Text != "il certificato scade tra 5 giorni" {
		t.Fatalf("no language, none saved: %q, want Italian", a.Text)
	}
	if resp, body := c.do("PUT", "/api/account/profile", map[string]string{"locale": "de"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("locale = %d %s", resp.StatusCode, body)
	}
	if a := fetch("pt-BR"); a.Text != "Zertifikat läuft in 5 Tagen ab" {
		t.Fatalf("unsupported language, de saved: %q", a.Text)
	}
}
