package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/api"
	"github.com/cloudground-it/cloudground/internal/config"
	"github.com/cloudground-it/cloudground/internal/state"
)

// noAgent answers every agent call with nothing: the token routes do not
// reach the agent.
type noAgent struct{}

func (noAgent) Call(context.Context, string, any, any) error { return nil }

// realPanelOnSocket serves the real router, with its production cookies
// (__Host-…; Secure), on a unix socket, with one admin account.
func realPanelOnSocket(t *testing.T) (sockURL, email, password string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cgs") // short: a socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	email, password = "admin@example.com", "a-long-password-123"
	hash, err := accounts.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(email, hash, "admin"); err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{Store: store, Agent: noAgent{}, Log: slog.New(slog.DiscardHandler)}
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv.Routes())
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.DrainJobs(ctx)
	})
	return "unix://" + sock, email, password
}

// TestPasswordSessionOverTheSocket: a password sign-in on the default
// transport keeps its session for the next request, with the panel's real
// Secure __Host- cookie (the standard jar dropped it: every tokens command
// answered 401 after a successful sign-in).
func TestPasswordSessionOverTheSocket(t *testing.T) {
	sock, email, pw := realPanelOnSocket(t)
	cfg := config.Config{URL: sock, Email: email, Password: pw}
	run := func(args ...string) (int, string, string) {
		inv, err := parseArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		var out, errb strings.Builder
		code := execute(&cfg, inv, &out, &errb)
		return code, out.String(), errb.String()
	}
	code, out, errs := run("tokens", "create", "ci")
	if code != exitOK || !strings.Contains(out, `"token": "cgt_`) {
		t.Fatalf("tokens create: code %d\nout %s\nerr %s", code, out, errs)
	}
	code, out, errs = run("tokens", "list")
	if code != exitOK || !strings.Contains(out, `"ci"`) {
		t.Fatalf("tokens list: code %d\nout %s\nerr %s", code, out, errs)
	}
}

func TestSocketJarKeepsSecureCookiesAndForgetsDeleted(t *testing.T) {
	j := newSocketJar()
	u, _ := url.Parse("http://cloudground/api/login")
	j.SetCookies(u, []*http.Cookie{{Name: "__Host-s", Value: "v", Secure: true, Path: "/"}})
	got := j.Cookies(u)
	if len(got) != 1 || got[0].Value != "v" {
		t.Fatalf("cookies %v", got)
	}
	j.SetCookies(u, []*http.Cookie{{Name: "__Host-s", MaxAge: -1}})
	if got := j.Cookies(u); len(got) != 0 {
		t.Fatalf("deleted cookie still sent: %v", got)
	}
	j.SetCookies(u, []*http.Cookie{{Name: "old", Value: "x", Expires: time.Now().Add(-time.Hour)}})
	if got := j.Cookies(u); len(got) != 0 {
		t.Fatalf("expired cookie sent: %v", got)
	}
}
