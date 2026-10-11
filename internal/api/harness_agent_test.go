package api

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
)

// fakeAgent stands in for the agent (AgentCaller): every call is recorded
// and answered from canned values by method name. Tests change results and
// errs, and read calls, lastParams and history, holding mu, since jobs and
// monitors call from their own goroutines.
type fakeAgent struct {
	mu sync.Mutex
	// calls are the method names called, in order.
	calls []string
	// lastParams holds the latest params sent to each method.
	lastParams map[string]any
	// results answer a method (JSON round-tripped into the caller's out);
	// errs fail it, and win over a result.
	results map[string]any
	errs    map[string]error
	// history is every call with its params, in order.
	history []agentCall
	// hook, set with setHook, runs as each call starts, outside mu.
	hook func(method string)
}

// agentCall is one recorded call.
type agentCall struct {
	method string
	params any
}

// setHook installs h to run at the start of every later call.
func (f *fakeAgent) setHook(h func(method string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = h
}

// Call records the call, runs the hook, then answers: the method's error if
// one is set, else its result decoded into out, else nothing.
func (f *fakeAgent) Call(_ context.Context, method string, params any, out any) error {
	if hook := f.note(method, params); hook != nil {
		hook(method)
	}
	// The answer is read after the hook and under the lock: a test may
	// swap it while a background caller is mid-call.
	f.mu.Lock()
	fail := f.errs[method]
	answer, canned := f.results[method]
	f.mu.Unlock()
	switch {
	case fail != nil:
		return fail
	case !canned || out == nil:
		return nil
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// note records one call and returns the hook to run for it.
func (f *fakeAgent) note(method string, params any) func(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastParams == nil {
		f.lastParams = make(map[string]any)
	}
	f.calls = append(f.calls, method)
	f.lastParams[method] = params
	f.history = append(f.history, agentCall{method: method, params: params})
	return f.hook
}

// called reports whether method was called since calls was last cleared.
func (f *fakeAgent) called(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, method)
}

// forget clears the list of calls, so called sees only later ones.
// (setResult, setErr, last and count are in capabilities_test.go and
// dbstudio_test.go.)
func (f *fakeAgent) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}
