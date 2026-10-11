package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// The agent serves every method of the table, and nothing else (the server
// refuses a handler for a method not in the table at registration).
func TestEveryTableMethodIsRegistered(t *testing.T) {
	a, _ := testAgent(t)
	if missing := a.NewServer(agentrpc.Config{}).Missing(); len(missing) != 0 {
		t.Fatalf("methods in the table with no handler: %v", missing)
	}
}

// A site's root is confined before any handler runs, whichever method is
// called: no command runs for a site rooted outside SitesRoot.
func TestEverySiteMethodIsConfinedCentrally(t *testing.T) {
	a, h := testAgent(t)
	site := staticSite(a)
	site.RootPath = "/etc"
	n := 0
	for _, spec := range agentrpc.Specs() {
		if !spec.Sites {
			continue
		}
		n++
		raw := []byte(`{"site":` + mustJSON(t, site) + `}`)
		if strings.Contains(spec.Name, "Staging") {
			raw = []byte(`{"production":` + mustJSON(t, site) + `,"staging":` + mustJSON(t, site) + `}`)
		}
		_, e := a.NewServer(agentrpc.Config{}).Invoke(t.Context(), spec.Name, raw)
		if e == nil || e.Code != agentrpc.CodeInvalid || e.Message != "invalid site root" {
			t.Errorf("%s: a root outside SitesRoot was not refused: %v", spec.Name, e)
		}
	}
	if n < 30 || h.touched() {
		t.Fatalf("%d site methods; the host was touched: %v %v %v", n, h.exec.Calls(), h.accounts.Ops(), h.systemd.Ops())
	}
}

// A staging copy whose provisioning fails half way rolls back and returns.
// It used to wait forever: CreateStaging holds the staging site's lock, and
// the provisioning rollback took it again through DeleteSite.
func TestStagingProvisionFailureDoesNotDeadlock(t *testing.T) {
	a, h := testAgent(t)
	prod := staticSite(a)
	if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: prod}); err != nil {
		t.Fatal(err)
	}
	stg := prod
	stg.ID, stg.Domain, stg.SystemUser, stg.StagingOf = 4, "staging.docs.example.com", "cg-site-4", prod.ID
	stg.RootPath = filepath.Join(a.Paths.SitesRoot, stg.Domain)
	// nginx joining the site's group comes after the user, inside the
	// part that is undone: without www-data it fails.
	if err := h.accounts.DeleteUser(t.Context(), "www-data", false); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := rpcCall(a, agentrpc.CreateStaging, rpc.StagingParams{Production: prod, Staging: stg})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a failed staging provision reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CreateStaging deadlocked on its own site lock during rollback")
	}
	if _, ok := h.accounts.User("cg-site-4"); ok {
		t.Fatal("the half-created staging user was not rolled back")
	}
	// The staging site's lock was released.
	unlock := a.lockSite(stg.Domain)
	unlock()
}

// An error from a handler reaches the API as the method's safe message, the
// command line and paths only in the detail.
func TestHandlerErrorsAreSafeForUsers(t *testing.T) {
	a, _ := testAgent(t)
	site := staticSite(a)
	if _, err := rpcCall(a, agentrpc.CreateSite, rpc.CreateSiteParams{Site: site}); err != nil {
		t.Fatal(err)
	}
	_, err := rpcCall(a, agentrpc.ListFiles, rpc.ListFilesParams{Site: site, RelPath: "nope1/nope2"})
	e := agentrpc.AsError(err)
	if e == nil {
		t.Fatalf("listing a missing directory: %v", err)
	}
	if strings.Contains(e.Message, a.Paths.SitesRoot) || strings.Contains(e.Message, "lstat") {
		t.Fatalf("the user message names internals: %q", e.Message)
	}
	if !strings.Contains(e.Detail, "nope1") {
		t.Fatalf("the detail lost the cause: %q", e.Detail)
	}
	// A typed refusal keeps its own message.
	_, err = rpcCall(a, agentrpc.ManageFile, rpc.ManageFileParams{Site: site, Operation: "chmod", RelPath: "x"})
	if e := agentrpc.AsError(err); e == nil || e.Code != agentrpc.CodeInvalid || e.Message != `unsupported file operation "chmod"` {
		t.Fatalf("typed refusal: %#v", err)
	}
}
