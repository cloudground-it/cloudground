package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// #29: an administrator installs a common extension as a job, which asks
// the agent; anything off the list is refused before a job starts.
func TestInstallPHPExtensionJob(t *testing.T) {
	s, agent, ts := testServer(t)
	admin := setupAdmin(t, s, ts)

	for _, body := range []map[string]any{{"extension": "xdebug"}, {"extension": ""}} {
		if resp, b := admin.do("POST", "/api/php/8.3/extensions", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%v = %d %s", body, resp.StatusCode, b)
		}
	}
	if resp, b := admin.do("POST", "/api/php/7.4/extensions", map[string]any{"extension": "redis"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("php 7.4 = %d %s", resp.StatusCode, b)
	}
	if agent.called(agentrpc.PHPExtInstall.Name()) {
		t.Fatal("a refused install reached the agent")
	}

	resp, b := admin.do("POST", "/api/php/8.3/extensions", map[string]any{"extension": "redis"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("install = %d %s", resp.StatusCode, b)
	}
	waitForTasks(t, s)
	deadline := time.Now().Add(5 * time.Second)
	for !agent.called(agentrpc.PHPExtInstall.Name()) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	agent.mu.Lock()
	sent, _ := agent.lastParams[agentrpc.PHPExtInstall.Name()].(rpc.PHPExtensionInstallParams)
	agent.mu.Unlock()
	if sent.Version != "8.3" || sent.Extension != "redis" {
		t.Fatalf("the agent got %+v", sent)
	}
}
