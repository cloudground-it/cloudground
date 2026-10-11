package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
)

// A write the agent refuses because the file is binary on disk is the
// client's mistake: 409, not the 502 of an agent failure.
func TestWriteFileOverBinaryIsConflict(t *testing.T) {
	s, agent, ts := testServer(t)
	c := setupAdmin(t, s, ts)
	if resp, _ := c.do("POST", "/api/sites", map[string]any{"domain": "files.example.com", "type": "static"}); resp.StatusCode != http.StatusAccepted {
		t.Fatal("site create failed")
	}
	waitForTasks(t, s)
	site, _ := s.Store.GetSiteByDomain("files.example.com")
	base := fmt.Sprintf("/api/sites/%d/files", site.ID)

	agent.results[agentrpc.ReadFile.Name()] = rpc.ReadFileResult{RelPath: "logo.png", Size: 1234, Binary: true}
	resp, body := c.do("GET", base+"/read?path=logo.png", nil)
	var got map[string]any
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("read = %d %s", resp.StatusCode, body)
	}
	if got["binary"] != true || got["content"] != "" || got["size"] != float64(1234) {
		t.Fatalf("read body = %s, want binary, no content, size", body)
	}

	agent.errs[agentrpc.WriteFile.Name()] = agentrpc.Conflict("this file is binary and cannot be edited as text; download it instead")
	if resp, body := c.do("POST", base+"/write", map[string]string{"path": "logo.png", "content": "x"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("write over binary = %d %s, want 409", resp.StatusCode, body)
	}
	agent.errs[agentrpc.WriteFile.Name()] = errors.New("path escapes site root")
	if resp, _ := c.do("POST", base+"/write", map[string]string{"path": "../x", "content": "x"}); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("other agent error = %d, want 502", resp.StatusCode)
	}
}
