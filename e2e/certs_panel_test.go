//go:build e2e

package e2e

import (
	"bufio"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const panelCertFile = "/etc/cloudground/certs/panel.pem"

// certsPanel (R-3.9): the agent's certificate and panel methods.
func (h *harness) certsPanel(t *testing.T) {
	h.panelCertStatus(t)
	h.agentDrainsOnStop(t)
}

// agentCall posts a raw JSON body to an agent method over its socket, as
// root, and returns the status and body.
func (h *harness) agentCall(method, body string) (int, []byte, error) {
	tok, err := os.ReadFile("/etc/cloudground/agent.token")
	if err != nil {
		return 0, nil, err
	}
	h.step("agent " + method + " " + body)
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Dial: func(string, string) (net.Conn, error) { return net.Dial("unix", agentSock) },
	}}
	req, _ := http.NewRequest(http.MethodPost, "http://agent/rpc/"+method, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// panelCertStatus: the panel's certificate status is the certificate nginx
// serves for the panel (panel.pem), not certbot's lineage. certbot's timer
// renews the lineage; the copy in panel.pem then aged unseen and expired.
// On a box with no panel domain the old agent reported no certificate at
// all (the lineage does not exist), so this fails on it there.
func (h *harness) panelCertStatus(t *testing.T) {
	raw, err := os.ReadFile(panelCertFile)
	block, _ := pem.Decode(raw)
	if err != nil || block == nil {
		h.fail(t, "R-3.9: control: panel.pem is readable", "R-3.9: %s: %v", panelCertFile, err)
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		h.fail(t, "R-3.9: control: panel.pem parses", "R-3.9: %v", err)
		return
	}
	code, body, err := h.agentCall("CertStatus", `{"names":["cloudground-panel"]}`)
	var res struct {
		Certs map[string]struct {
			Exists   bool      `json:"exists"`
			NotAfter time.Time `json:"not_after"`
		} `json:"certs"`
	}
	if err == nil {
		err = json.Unmarshal(body, &res)
	}
	got := res.Certs["cloudground-panel"]
	h.assert(t, "R-3.9: the panel's certificate status is the panel.pem nginx serves",
		code == 200 && got.Exists && got.NotAfter.Equal(cert.NotAfter),
		"R-3.9: CertStatus(cloudground-panel) = %d %s (%v); panel.pem expires %s", code, head(body, 200), err, cert.NotAfter.UTC())
}

// agentDrainsOnStop: a request the agent is in the middle of when systemd
// stops it is still answered. The old agent died on SIGTERM at once, and a
// self-update's reply could be lost to the restart its guard issues.
//
// The request's headers go out first and its body after the stop has begun,
// so the request is in flight when SIGTERM arrives, whatever the timing.
func (h *harness) agentDrainsOnStop(t *testing.T) {
	tok, err := os.ReadFile("/etc/cloudground/agent.token")
	if err != nil {
		h.fail(t, "R-3.9: control: the agent token is readable", "R-3.9: %v", err)
		return
	}
	pidBefore := h.mainPID("cloudground-agent.service")
	conn, err := net.Dial("unix", agentSock)
	if err != nil {
		h.fail(t, "R-3.9: control: the agent socket answers", "R-3.9: %v", err)
		return
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /rpc/Ping HTTP/1.1\r\nHost: agent\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n",
		strings.TrimSpace(string(tok)))
	// Let the agent read the headers: the connection is now active.
	time.Sleep(500 * time.Millisecond)
	h.run("systemctl", "restart", "--no-block", "cloudground-agent.service")
	time.Sleep(time.Second)
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	conn.Write([]byte("{}"))
	status, _ := bufio.NewReader(conn).ReadString('\n')
	h.assert(t, "R-3.9: the agent answers the request in flight when it is stopped",
		strings.HasPrefix(status, "HTTP/1.1 200"), "R-3.9: the request in flight got %q", strings.TrimSpace(status))

	// And it comes back.
	var pidAfter string
	for i := 0; i < 30; i++ {
		pidAfter = h.mainPID("cloudground-agent.service")
		if pidAfter != "" && pidAfter != "0" && pidAfter != pidBefore {
			if c, _, err := h.agentCall("Ping", "{}"); err == nil && c == 200 {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.assert(t, "R-3.9: the agent is back after the restart", pidAfter != pidBefore && h.run("systemctl", "is-active", "cloudground-agent.service").Trim() == "active",
		"R-3.9: agent MainPID %s -> %s", pidBefore, pidAfter)
}

// jsonBool is a top-level boolean of a JSON object, as "true"/"false", or ""
// when it is missing or not a boolean.
func jsonBool(b []byte, key string) string {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	if v, ok := m[key].(bool); ok {
		return fmt.Sprint(v)
	}
	return ""
}
