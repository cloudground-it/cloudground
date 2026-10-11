package host

import (
	"context"
	"fmt"
	"sync"
)

// FakeACME is an in-memory ACME. It validates requests as the real adapter
// does (CertOnlyArgs), records them, and keeps the set of lineages so Delete
// answers ErrNoSuchCert for one that was never issued.
type FakeACME struct {
	// OnCertOnly runs inside CertOnly, after validation; its error is
	// returned. Tests use it to fail an issuance, to write the lineage's
	// files, or to act while "certbot" runs.
	OnCertOnly func(ctx context.Context, r CertRequest) error

	mu       sync.Mutex
	requests []CertRequest
	deleted  []string
	lineages map[string]bool
}

func (f *FakeACME) CertOnly(ctx context.Context, r CertRequest) error {
	if _, err := CertOnlyArgs(r); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.requests = append(f.requests, r)
	hook := f.OnCertOnly
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, r); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lineages == nil {
		f.lineages = map[string]bool{}
	}
	f.lineages[r.Name] = true
	return nil
}

func (f *FakeACME) Delete(ctx context.Context, name string) error {
	if !certNameRe.MatchString(name) {
		return fmt.Errorf("certificate name %q: %w", name, ErrInvalidArgument)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	if !f.lineages[name] {
		return fmt.Errorf("certbot delete %s: %w", name, ErrNoSuchCert)
	}
	delete(f.lineages, name)
	return nil
}

// Requests returns every CertOnly request so far.
func (f *FakeACME) Requests() []CertRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CertRequest(nil), f.requests...)
}

// Deleted returns every name Delete was asked for.
func (f *FakeACME) Deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

var _ ACME = (*FakeACME)(nil)
