package db

import (
	"context"
	"sync"
)

// Request-scoped import options (SR-T7). An import into a database on an
// external server runs as the site's own account, whose password the agent
// cannot borrow there; the panel sends it in the request and it travels to
// importAs through the context, so ImportFile and Replace keep their
// signatures. A fallback to a throwaway account (no password) collects a
// warning the handler shows the operator.

type accountsKey struct{}
type warningsKey struct{}

// WithImportAccounts carries the site's database-account passwords (user →
// password) for this request. The agent uses one only for the account its
// own registry picks for the database being imported.
func WithImportAccounts(ctx context.Context, accounts map[string]string) context.Context {
	if len(accounts) == 0 {
		return ctx
	}
	return context.WithValue(ctx, accountsKey{}, accounts)
}

func importAccounts(ctx context.Context) map[string]string {
	m, _ := ctx.Value(accountsKey{}).(map[string]string)
	return m
}

// importWarnings collects the warnings an import raises (a fallback to a
// throwaway account that imported triggers or views).
type importWarnings struct {
	mu   sync.Mutex
	msgs []string
}

// WithImportWarnings attaches a collector to the context; ImportWarnings
// reads what accumulated. The handler shows them to the operator.
func WithImportWarnings(ctx context.Context) (context.Context, *importWarnings) {
	w := &importWarnings{}
	return context.WithValue(ctx, warningsKey{}, w), w
}

func addImportWarning(ctx context.Context, msg string) {
	w, _ := ctx.Value(warningsKey{}).(*importWarnings)
	if w == nil {
		return
	}
	w.mu.Lock()
	w.msgs = append(w.msgs, msg)
	w.mu.Unlock()
}

// Messages returns the warnings collected so far.
func (w *importWarnings) Messages() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.msgs...)
}
