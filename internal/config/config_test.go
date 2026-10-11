package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// env is a fake environment pointing CLOUDGROUND_CONFIG at a file (or at a
// missing one, so the host's /etc never leaks into a test).
func env(t *testing.T, file string, vars map[string]string) func(string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "missing.env")
	if file != "" {
		path = filepath.Join(t.TempDir(), "panel.env")
		if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return func(k string) string {
		if k == "CLOUDGROUND_CONFIG" {
			return path
		}
		return vars[k]
	}
}

func TestDefaults(t *testing.T) {
	c, err := load(API, env(t, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		AgentSocket: "/run/cloudground/agent.sock", AgentTokenFile: "/etc/cloudground/agent.token",
		StatePath: "/var/lib/cloudground/state.db", SecretKeyPath: "/var/lib/cloudground/secret.key",
		Listen: "127.0.0.1:5252", LocalListen: "127.0.0.1:9080",
		TLSCert: "/etc/cloudground/certs/panel.pem", TLSKey: "/etc/cloudground/certs/panel.key",
		DefaultPHP: "8.4", DrainTimeout: 30 * time.Second,
	}
	if c.File != "" || len(c.Warnings) != 0 {
		t.Fatalf("no file expected: %+v", c)
	}
	c.File, c.Warnings = "", nil
	if c.AgentSocket != want.AgentSocket || c.StatePath != want.StatePath || c.LocalListen != want.LocalListen ||
		c.DefaultPHP != want.DefaultPHP || c.DrainTimeout != want.DrainTimeout || c.TLSKey != want.TLSKey || c.Dev {
		t.Fatalf("defaults:\n%+v\nwant\n%+v", *c, want)
	}
	a, err := load(Agent, env(t, "", nil))
	if err != nil || a.SocketGroup != "cloudground" || a.APIUser != "cloudground" || a.StatePath != "" {
		t.Fatalf("agent defaults: %+v %v", a, err)
	}
	cli, err := load(CLI, env(t, "", nil))
	if err != nil || cli.URL != "unix:///run/cloudground/api.sock" || cli.Token != "" {
		t.Fatalf("cli defaults: %+v %v", cli, err)
	}
}

func TestPrecedenceDefaultsFileEnvironment(t *testing.T) {
	file := `# installer
CLOUDGROUND_PHP_VERSION=8.5
CLOUDGROUND_STATE = "/srv/state.db"
CLOUDGROUND_DRAIN_TIMEOUT='45s'
OTHER_TOOL=1
CLOUDGROUND_TYPO=x
`
	c, err := load(API, env(t, file, map[string]string{"CLOUDGROUND_PHP_VERSION": "8.3"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultPHP != "8.3" || c.StatePath != "/srv/state.db" || c.DrainTimeout != 45*time.Second {
		t.Fatalf("precedence: %+v", c)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "CLOUDGROUND_TYPO") {
		t.Fatalf("warnings: %v", c.Warnings)
	}
	// The file is shared: the API's keys are no typo for the agent.
	a, err := load(Agent, env(t, file, nil))
	if err != nil || len(a.Warnings) != 1 {
		t.Fatalf("agent warnings: %v %v", a.Warnings, err)
	}
}

func TestValidationReportsEveryProblem(t *testing.T) {
	_, err := load(API, env(t, "CLOUDGROUND_LOCAL_LISTEN=0.0.0.0:9080\n", map[string]string{
		"CLOUDGROUND_STATE":         "state.db",
		"CLOUDGROUND_LISTEN":        "127.0.0.1:99999",
		"CLOUDGROUND_DEV":           "maybe",
		"CLOUDGROUND_PHP_VERSION":   "latest",
		"CLOUDGROUND_DRAIN_TIMEOUT": "1h",
	}))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"CLOUDGROUND_STATE (environment)", "CLOUDGROUND_LISTEN", "CLOUDGROUND_LOCAL_LISTEN (", "loopback",
		"CLOUDGROUND_DEV", "CLOUDGROUND_PHP_VERSION", "CLOUDGROUND_DRAIN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
	if _, err := load(Agent, env(t, "", map[string]string{"CLOUDGROUND_API_USER": "Root;rm"})); err == nil {
		t.Error("bad user name accepted")
	}
}

func TestMalformedFile(t *testing.T) {
	_, err := load(API, env(t, "CLOUDGROUND_STATE=/a\njust words\n", nil))
	if err == nil || !strings.Contains(err.Error(), ":2:") {
		t.Fatalf("got %v", err)
	}
}

func TestCLIURL(t *testing.T) {
	ok := map[string]string{
		"unix:///run/cloudground/api.sock": "unix:///run/cloudground/api.sock",
		"https://panel.example.com:8443/":  "https://panel.example.com:8443",
		"http://127.0.0.1:5252":            "http://127.0.0.1:5252",
	}
	for in, want := range ok {
		c, err := load(CLI, env(t, "", map[string]string{"CLOUDGROUND_URL": in}))
		if err != nil || c.URL != want {
			t.Errorf("%s: %v %+v", in, err, c)
		}
	}
	for _, bad := range []string{"http://panel.example.com", "unix://relative", "ftp://x", "https://u:p@x", "https://x/api", "https://"} {
		if _, err := load(CLI, env(t, "", map[string]string{"CLOUDGROUND_URL": bad})); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestCLIToken(t *testing.T) {
	tok := "cgt_" + strings.Repeat("A", 43)
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	os.WriteFile(tf, []byte(tok+"\n"), 0o600)
	c, err := load(CLI, env(t, "", map[string]string{"CLOUDGROUND_TOKEN_FILE": tf}))
	if err != nil || c.Token != tok {
		t.Fatalf("token file: %v %+v", err, c)
	}
	other := "cgt_" + strings.Repeat("B", 43)
	c, _ = load(CLI, env(t, "", map[string]string{"CLOUDGROUND_TOKEN_FILE": tf, "CLOUDGROUND_TOKEN": other}))
	if c.Token != other {
		t.Fatal("CLOUDGROUND_TOKEN must win over the token file")
	}
	_, err = load(CLI, env(t, "", map[string]string{"CLOUDGROUND_TOKEN": "hunter2"}))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("a malformed token must be refused without echoing it: %v", err)
	}
	if _, err := load(CLI, env(t, "", map[string]string{"CLOUDGROUND_TOKEN_FILE": filepath.Join(dir, "nope")})); err == nil {
		t.Fatal("a missing token file must be an error")
	}
}

func TestUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	path := filepath.Join(t.TempDir(), "cgctl.env")
	os.WriteFile(path, []byte("CLOUDGROUND_URL=https://x\n"), 0)
	get := func(k string) string {
		if k == "CLOUDGROUND_CONFIG" {
			return path
		}
		return ""
	}
	if _, err := load(CLI, get); err != nil {
		t.Fatalf("cgctl must skip a file it may not read: %v", err)
	}
	if _, err := load(API, get); err == nil {
		t.Fatal("a daemon must not skip an unreadable file")
	}
}

func TestReadSecret(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.token")
	os.WriteFile(p, []byte("  "+strings.Repeat("x", 40)+"\n"), 0o600)
	if s, err := ReadSecret(p, 32); err != nil || s != strings.Repeat("x", 40) {
		t.Fatalf("%q %v", s, err)
	}
	if _, err := ReadSecret(p, 64); err == nil {
		t.Fatal("short secret accepted")
	}
	os.WriteFile(p, []byte("\n"), 0o600)
	if _, err := ReadSecret(p, 0); err == nil {
		t.Fatal("empty secret accepted")
	}
}

// The token pair is one setting: the environment beats the file whichever
// key each uses, and only the chosen value is validated.
func TestTokenPrecedenceAcrossSources(t *testing.T) {
	envTok := "cgt_" + strings.Repeat("E", 43)
	fileTok := "cgt_" + strings.Repeat("F", 43)
	tf := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tf, []byte(envTok+"\n"), 0o600)

	// A token file from the environment beats a token written in the file.
	c, err := load(CLI, env(t, "CLOUDGROUND_TOKEN="+fileTok+"\n", map[string]string{"CLOUDGROUND_TOKEN_FILE": tf}))
	if err != nil || c.Token != envTok {
		t.Fatalf("env token file vs file token: %v %q", err, c.Token)
	}
	// A malformed token in the file does not fail a command the environment
	// gives a good one; nor does a missing token file named in the file.
	for _, file := range []string{"CLOUDGROUND_TOKEN=garbage\n", "CLOUDGROUND_TOKEN_FILE=/nonexistent/token\n"} {
		c, err = load(CLI, env(t, file, map[string]string{"CLOUDGROUND_TOKEN": envTok}))
		if err != nil || c.Token != envTok {
			t.Fatalf("%q with a good env token: %v", file, err)
		}
	}
	// Without the environment, the file decides, its token before its file.
	c, err = load(CLI, env(t, "CLOUDGROUND_TOKEN="+fileTok+"\nCLOUDGROUND_TOKEN_FILE="+tf+"\n", nil))
	if err != nil || c.Token != fileTok {
		t.Fatalf("file pair: %v %q", err, c.Token)
	}
	// The chosen one is validated and named with its source.
	_, err = load(CLI, env(t, "CLOUDGROUND_TOKEN=garbage\n", nil))
	if err == nil || !strings.Contains(err.Error(), "CLOUDGROUND_TOKEN (") || strings.Contains(err.Error(), "environment") {
		t.Fatalf("bad file token: %v", err)
	}
}

// DEV and INSECURE keep their old meaning: only "1" turns them on.
func TestSwitchesOnlyOne(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "0": false, "": false} {
		c, err := load(API, env(t, "", map[string]string{"CLOUDGROUND_DEV": v}))
		if err != nil || c.Dev != want {
			t.Errorf("DEV=%q: %v %v", v, err, c)
		}
		cli, err := load(CLI, env(t, "", map[string]string{"CLOUDGROUND_INSECURE": v}))
		if err != nil || cli.Insecure != want {
			t.Errorf("INSECURE=%q: %v %v", v, err, cli)
		}
	}
	for _, v := range []string{"true", "yes", "on", "TRUE", "2"} {
		if _, err := load(API, env(t, "", map[string]string{"CLOUDGROUND_DEV": v})); err == nil {
			t.Errorf("DEV=%q accepted", v)
		}
		if _, err := load(CLI, env(t, "", map[string]string{"CLOUDGROUND_INSECURE": v})); err == nil {
			t.Errorf("INSECURE=%q accepted", v)
		}
	}
}
