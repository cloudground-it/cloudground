package agentrpc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// checkedParams has rules of its own (SPEC §3, step 4a).
type checkedParams struct {
	Site state.Site `json:"site"`
	Mode string     `json:"mode"`
}

func (p *checkedParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

func (p *checkedParams) Validate() error {
	if p.Mode != "on" && p.Mode != "off" {
		return errors.New("mode must be on or off")
	}
	return nil
}

var tChecked = define[checkedParams, string](Spec{Name: "TChecked", Timeout: 5 * time.Second, Sites: true, Lock: LockSite, Fail: "Checked failed"})

// A value that breaks its type's rules is invalid, with the rule's text, and
// neither the lock nor the handler runs; the sites are checked first.
func TestParameterRulesRunBeforeTheHandler(t *testing.T) {
	locker := &fakeLocker{}
	srv := NewServer(Config{SitesRoot: "/srv/sites", Locker: locker})
	var ran atomic.Int32
	Handle(srv, tChecked, func(_ context.Context, p checkedParams) (string, error) { ran.Add(1); return p.Mode, nil })
	good := `{"domain":"a.example.com","system_user":"cg-site-1","root_path":"/srv/sites/a.example.com","type":"static"}`
	_, e := srv.Invoke(t.Context(), "TChecked", []byte(`{"site":`+good+`,"mode":"maybe"}`))
	if e == nil || e.Code != CodeInvalid || e.Message != "mode must be on or off" {
		t.Fatalf("bad mode: %+v", e)
	}
	bad := `{"domain":"a.example.com","system_user":"cg-site-1","root_path":"/etc","type":"static"}`
	if _, e = srv.Invoke(t.Context(), "TChecked", []byte(`{"site":`+bad+`,"mode":"maybe"}`)); e == nil || e.Message != "invalid site root" {
		t.Fatalf("the site is checked first: %+v", e)
	}
	if ran.Load() != 0 || len(locker.locks) != 0 {
		t.Fatalf("handler ran %d times, %d locks taken", ran.Load(), len(locker.locks))
	}
	res, e := srv.Invoke(t.Context(), "TChecked", []byte(`{"site":`+good+`,"mode":"on"}`))
	if e != nil || res != "on" || ran.Load() != 1 {
		t.Fatalf("a valid value: %v %+v", res, e)
	}
}
