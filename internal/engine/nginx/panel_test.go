package nginx

import (
	"strings"
	"testing"
)

func panelInput() PanelInput {
	return PanelInput{
		Domain:        "panel.example.com",
		ACMEWebroot:   "/var/www/cloudground-acme",
		CertFile:      "/etc/cloudground/certs/panel.pem",
		KeyFile:       "/etc/cloudground/certs/panel.key",
		AccessInclude: "/etc/cloudground/panel-access.conf",
		APISocket:     "/run/cloudground/api.sock",
	}
}

func TestPanelVhost(t *testing.T) {
	in := panelInput()
	v, err := PanelVhost(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"server_name panel.example.com;",
		"location /.well-known/acme-challenge/ { root /var/www/cloudground-acme; }",
		"return 301 https://$host$request_uri;",
		"listen 443 ssl http2;",
		"listen [::]:443 ssl http2;",
		"ssl_certificate /etc/cloudground/certs/panel.pem;",
		"ssl_certificate_key /etc/cloudground/certs/panel.key;",
		// Operator access rules survive a re-render (they used to be
		// reverted by every panel certificate issuance when edited here).
		"include /etc/cloudground/panel-access.conf*;",
		"client_max_body_size 16m;",
		"proxy_pass http://unix:/run/cloudground/api.sock;",
		"proxy_set_header X-Forwarded-Proto https;",
		// GET /api/events: messages pass at once, and a stream with a
		// heartbeat every 25 s is not cut by the read timeout.
		"proxy_buffering off;",
		"proxy_read_timeout 1h;",
		`add_header Strict-Transport-Security "max-age=31536000" always;`,
	} {
		if !strings.Contains(v, want) {
			t.Errorf("panel vhost lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "http2 on;") {
		t.Error("http2 directive rendered for an old nginx")
	}
	// The HSTS header is declared at server level and no location declares
	// its own add_header, so nginx does not drop it (AGENTS.md).
	if strings.Count(v, "add_header") != 1 {
		t.Errorf("add_header outside the server level:\n%s", v)
	}

	in.HTTP2Directive = true
	v, _ = PanelVhost(in)
	if !strings.Contains(v, "    listen 443 ssl;\n    listen [::]:443 ssl;\n    http2 on;\n") {
		t.Errorf("no http2 directive on a new nginx:\n%s", v)
	}
}

func TestPanelVhostRefusesInjection(t *testing.T) {
	for name, mut := range map[string]func(*PanelInput){
		"domain":       func(p *PanelInput) { p.Domain = "panel.example.com; include /etc/shadow" },
		"empty domain": func(p *PanelInput) { p.Domain = "" },
		"webroot":      func(p *PanelInput) { p.ACMEWebroot = "/var/www/x; }" },
		"relative":     func(p *PanelInput) { p.CertFile = "panel.pem" },
		"unclean":      func(p *PanelInput) { p.KeyFile = "/etc/../etc/shadow" },
		"socket":       func(p *PanelInput) { p.APISocket = "/run/x sock" },
		"no include":   func(p *PanelInput) { p.AccessInclude = "" },
	} {
		in := panelInput()
		mut(&in)
		if _, err := PanelVhost(in); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestHasHTTP2Directive(t *testing.T) {
	for info, want := range map[string]bool{
		"nginx version: nginx/1.24.0 (Ubuntu)\nbuilt with OpenSSL": false,
		"nginx version: nginx/1.25.0":                              false,
		"nginx version: nginx/1.25.1":                              true,
		"nginx version: nginx/1.26.3 (Ubuntu)":                     true,
		"nginx version: nginx/2.0.0":                               true,
		"":                                                         false,
		"nginx: command not found":                                 false,
	} {
		if got := HasHTTP2Directive(info); got != want {
			t.Errorf("%q: %v, want %v", info, got, want)
		}
	}
}
