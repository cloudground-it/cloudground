package certs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"golang.org/x/sys/unix"
)

// Preflight proves that each domain reaches this server's ACME webroot over
// plain HTTP, before certbot spends a rate-limited attempt on it.
func (o *Ops) Preflight(ctx context.Context, domains []string) error {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("ACME preflight token: %w", err)
	}
	token := "cloudground-" + hex.EncodeToString(raw[:])
	wellKnown := filepath.Join(o.Paths.ACMEWebroot, ".well-known")
	dir := filepath.Join(wellKnown, "acme-challenge")
	// nginx (www-data) must traverse both and read the token. The agent runs
	// with UMask=0027, which would leave them closed to it, so every mode is
	// set explicitly.
	for _, d := range []string{wellKnown, dir} {
		if err := rootfile.MkdirMode(d, 0o755); err != nil {
			return fmt.Errorf("prepare ACME webroot: %w", err)
		}
	}
	probe := filepath.Join(dir, token)
	f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o644)
	if err != nil {
		return fmt.Errorf("write ACME preflight: %w", err)
	}
	defer os.Remove(probe)
	_, err = f.WriteString(token)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write ACME preflight: %w", err)
	}

	client := o.HTTP
	if client == nil {
		client = &http.Client{
			Timeout:       12 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	for _, d := range domains {
		if err := o.probe(ctx, client, d, token); err != nil {
			return err
		}
	}
	return nil
}

func (o *Ops) probe(ctx context.Context, client *http.Client, domain, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+domain+"/.well-known/acme-challenge/"+token, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return agentrpc.Wrap(agentrpc.CodeInvalid, fmt.Sprintf("ACME preflight for %s failed: it could not be reached%s", domain, o.dnsHint(ctx, domain)), err)
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if rerr == nil && resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == token {
		return nil
	}
	return agentrpc.Invalid("ACME preflight for %s failed: the challenge answered HTTP %d%s; point its A/AAAA records to this server and disable any proxy that intercepts /.well-known/acme-challenge/",
		domain, resp.StatusCode, o.dnsHint(ctx, domain))
}

// dnsHint says where domain points, for the preflight's error.
func (o *Ops) dnsHint(ctx context.Context, domain string) string {
	lookup := o.LookupHost
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	addrs, err := lookup(ctx, domain)
	if err != nil || len(addrs) == 0 {
		return " (DNS does not resolve)"
	}
	return " (DNS resolves to " + strings.Join(addrs, ", ") + ")"
}
