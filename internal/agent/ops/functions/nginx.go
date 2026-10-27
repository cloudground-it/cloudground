package functions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/ops/certs"
	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/engine/nginx"
	fn "github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// certFor is a lineage's certificate and key when certbot has it, else the
// fallback certificate.
func (o *Ops) certFor(lineage string) (string, string) {
	dir := filepath.Join(o.CertLiveDir, lineage)
	chain, key := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	if fi, err := os.Stat(chain); err == nil && fi.Mode().IsRegular() {
		if fi, err := os.Stat(key); err == nil && fi.Mode().IsRegular() {
			return chain, key
		}
	}
	return o.FallbackCert, o.FallbackKey
}

// input is the renderer's input for p, with the paths only the agent knows.
func (o *Ops) input(ctx context.Context, p rpc.RenderFunctionsParams) nginx.FunctionsInput {
	info, err := o.Nginx.BuildInfo(ctx)
	if err != nil {
		o.log().Warn("nginx -V failed; rendering the functions for an older nginx", "err", err)
	}
	in := nginx.FunctionsInput{
		Domain: p.Domain, ACMEWebroot: o.ACMEWebroot, InternalSocket: or(o.NginxSocket, fn.InvokeSocket),
		HTTP2Directive: nginx.HasHTTP2Directive(info),
	}
	if p.Domain != "" {
		in.DomainCert, in.DomainKey = o.certFor(fn.DomainLineage)
	}
	for _, f := range p.Functions {
		s := nginx.FunctionServer{
			ID: f.ID, Name: f.Name, Socket: fn.SocketPath(f.ID), TimeoutSec: f.TimeoutSec, AccessLog: fn.AccessLog(f.Name),
			Token: f.Token, CORSOrigins: f.CORSOrigins, Domains: f.Domains,
		}
		if len(f.Domains) > 0 {
			s.CertFile, s.KeyFile = o.certFor(fn.FunctionLineage(f.ID))
		}
		in.Functions = append(in.Functions, s)
	}
	return in
}

// Render is RenderFunctions (SPEC §7).
func (o *Ops) Render(ctx context.Context, p rpc.RenderFunctionsParams) (rpc.RenderFunctionsResult, error) {
	if err := p.Validate(); err != nil {
		return rpc.RenderFunctionsResult{}, agentrpc.Invalid("%s", err.Error())
	}
	files, err := nginx.Renderer{SitesDir: o.SitesDir}.FunctionsFiles(o.input(ctx, p))
	if err != nil {
		return rpc.RenderFunctionsResult{}, agentrpc.Invalid("%s", err.Error())
	}
	o.NginxMu.Lock()
	defer o.NginxMu.Unlock()
	type prior struct {
		data []byte
		had  bool
	}
	before := map[string]prior{}
	entries, err := os.ReadDir(o.SitesDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return rpc.RenderFunctionsResult{}, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !nginx.IsFunctionsFile(e.Name()) {
			continue
		}
		p := filepath.Join(o.SitesDir, e.Name())
		b, err := rootfile.ReadNoFollow(p)
		if err != nil {
			return rpc.RenderFunctionsResult{}, err
		}
		before[p] = prior{b, true}
	}
	for p := range files {
		if _, ok := before[p]; !ok {
			before[p] = prior{}
		}
	}
	restore := func() {
		for p, b := range before {
			var err error
			if b.had {
				err = rootfile.Replace(p, b.data, 0o600)
			} else {
				err = os.Remove(p)
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				o.log().Error("could not put a functions file back", "path", p, "err", err)
			}
		}
	}
	changed := false
	var res rpc.RenderFunctionsResult
	for p, b := range before {
		content, keep := files[p]
		switch {
		case !keep:
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				restore()
				return res, err
			}
			changed = true
		case !b.had || string(b.data) != content:
			// A file may hold a token: root's alone (SPEC §7).
			if err := rootfile.Replace(p, []byte(content), 0o600); err != nil {
				restore()
				return res, err
			}
			changed = true
		}
		if keep {
			sum := sha256.Sum256([]byte(content))
			res.Files = append(res.Files, rpc.ManagedFile{Path: p, SHA256: hex.EncodeToString(sum[:])})
		}
	}
	if !changed {
		return res, nil
	}
	if err := o.Nginx.Test(ctx); err != nil {
		restore()
		return rpc.RenderFunctionsResult{}, fmt.Errorf("nginx refused the functions' configuration: %w", err)
	}
	if err := o.Nginx.Reload(ctx); err != nil {
		restore()
		rctx := context.WithoutCancel(ctx)
		if rerr := o.Nginx.Reload(rctx); rerr != nil {
			o.log().Error("nginx reload after restoring the functions' files failed", "err", rerr)
		}
		return rpc.RenderFunctionsResult{}, fmt.Errorf("reload nginx with the functions' configuration: %w", err)
	}
	return res, nil
}

// IssueCertificate is IssueFunctionsCertificate (SPEC §8).
func (o *Ops) IssueCertificate(ctx context.Context, p rpc.FunctionsCertParams) error {
	if err := p.Validate(); err != nil {
		return agentrpc.Invalid("%s", err.Error())
	}
	if len(p.Domains) == 0 {
		return agentrpc.Invalid("a certificate needs at least one domain")
	}
	if err := certs.CheckEmail(p.Email); err != nil {
		return err
	}
	o.CertMu.Lock()
	defer o.CertMu.Unlock()
	if o.Preflight != nil {
		if err := o.Preflight(ctx, p.Domains); err != nil {
			return err
		}
	}
	if err := o.ACME.CertOnly(ctx, host.CertRequest{Name: p.Name, Domains: p.Domains, Email: p.Email, Webroot: o.ACMEWebroot}); err != nil {
		if errors.Is(err, host.ErrInvalidArgument) {
			return agentrpc.Wrap(agentrpc.CodeInvalid, "the certificate request names something certbot cannot take", err)
		}
		return fmt.Errorf("certificate issuance: %w", err)
	}
	// A renewed certificate is read at a reload; a first one once the next
	// render points the files at it.
	o.NginxMu.Lock()
	defer o.NginxMu.Unlock()
	if err := o.Nginx.Test(ctx); err != nil {
		return err
	}
	return o.Nginx.Reload(ctx)
}

// deleteCert is DeleteFunctionsCertificate (SPEC §8): no lineage, nothing
// to do.
func (o *Ops) deleteCert(ctx context.Context, name string) error {
	if !fn.ValidLineage(name) {
		return agentrpc.Invalid("invalid certificate name %q", name)
	}
	o.CertMu.Lock()
	defer o.CertMu.Unlock()
	if err := o.ACME.Delete(ctx, name); err != nil && !errors.Is(err, host.ErrNoSuchCert) {
		return err
	}
	return nil
}
