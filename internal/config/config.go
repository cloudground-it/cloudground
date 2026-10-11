// Package config is the one configuration loader of panel-api, panel-agent
// and cgctl: defaults, then a file in /etc/cloudground, then the
// environment, validated as a whole. SPEC.md is the contract.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Component selects which keys apply and which file is read.
type Component int

const (
	API Component = iota
	Agent
	CLI
)

// Config is the validated configuration of one binary. Fields of the other
// components are left at their zero values.
type Config struct {
	// Shared by the API and the agent.
	AgentSocket    string
	AgentTokenFile string

	// API.
	StatePath     string
	SecretKeyPath string
	Listen        string
	LocalListen   string
	TLSCert       string
	TLSKey        string
	Dev           bool
	DefaultPHP    string
	DrainTimeout  time.Duration

	// BackupPrivateEndpoint lets a backup repository's s3:/rest: endpoint
	// be on a private or local address (internal/backuprepo).
	BackupPrivateEndpoint bool

	// Agent.
	SocketGroup string
	APIUser     string

	// CLI.
	URL      string
	Token    string
	Insecure bool
	Email    string
	Password string
	TOTP     string

	// Warnings lists unknown CLOUDGROUND_ keys found in the file.
	Warnings []string
	// File is the file that was read, "" when there was none.
	File string
}

// key describes one setting: who reads it, its default, and how it is
// validated and stored.
type key struct {
	name  string
	for_  []Component
	def   string
	apply func(c *Config, v string) error
}

var keys = []key{
	{"CLOUDGROUND_AGENT_SOCKET", []Component{API, Agent}, "/run/cloudground/agent.sock", absPath(func(c *Config) *string { return &c.AgentSocket })},
	{"CLOUDGROUND_AGENT_TOKEN_FILE", []Component{API, Agent}, "/etc/cloudground/agent.token", absPath(func(c *Config) *string { return &c.AgentTokenFile })},
	{"CLOUDGROUND_STATE", []Component{API}, "/var/lib/cloudground/state.db", absPath(func(c *Config) *string { return &c.StatePath })},
	{"CLOUDGROUND_SECRET_KEY", []Component{API}, "/var/lib/cloudground/secret.key", absPath(func(c *Config) *string { return &c.SecretKeyPath })},
	{"CLOUDGROUND_LISTEN", []Component{API}, "127.0.0.1:5252", hostPort(false, func(c *Config) *string { return &c.Listen })},
	{"CLOUDGROUND_LOCAL_LISTEN", []Component{API}, "127.0.0.1:9080", hostPort(true, func(c *Config) *string { return &c.LocalListen })},
	{"CLOUDGROUND_TLS_CERT", []Component{API}, "/etc/cloudground/certs/panel.pem", absPath(func(c *Config) *string { return &c.TLSCert })},
	{"CLOUDGROUND_TLS_KEY", []Component{API}, "/etc/cloudground/certs/panel.key", absPath(func(c *Config) *string { return &c.TLSKey })},
	{"CLOUDGROUND_DEV", []Component{API}, "", flag(func(c *Config) *bool { return &c.Dev })},
	{"CLOUDGROUND_PHP_VERSION", []Component{API}, "8.4", phpVersion},
	{"CLOUDGROUND_DRAIN_TIMEOUT", []Component{API}, "30s", drainTimeout},
	{"CLOUDGROUND_BACKUP_PRIVATE_ENDPOINT", []Component{API, Agent}, "", flag(func(c *Config) *bool { return &c.BackupPrivateEndpoint })},
	{"CLOUDGROUND_SOCKET_GROUP", []Component{Agent}, "cloudground", account(func(c *Config) *string { return &c.SocketGroup })},
	{"CLOUDGROUND_API_USER", []Component{Agent}, "cloudground", account(func(c *Config) *string { return &c.APIUser })},
	{"CLOUDGROUND_URL", []Component{CLI}, "unix:///run/cloudground/api.sock", panelURL},
	{"CLOUDGROUND_TOKEN", []Component{CLI}, "", nil}, // resolved as a pair: resolveToken
	{"CLOUDGROUND_TOKEN_FILE", []Component{CLI}, "", nil},
	{"CLOUDGROUND_INSECURE", []Component{CLI}, "", flag(func(c *Config) *bool { return &c.Insecure })},
	{"CLOUDGROUND_EMAIL", []Component{CLI}, "", text(func(c *Config) *string { return &c.Email })},
	{"CLOUDGROUND_PASSWORD", []Component{CLI}, "", text(func(c *Config) *string { return &c.Password })},
	{"CLOUDGROUND_TOTP", []Component{CLI}, "", text(func(c *Config) *string { return &c.TOTP })},
}

// fileFor is each component's file (SPEC.md §2).
func fileFor(c Component) string {
	if c == CLI {
		return "/etc/cloudground/cgctl.env"
	}
	return "/etc/cloudground/panel.env"
}

// Load reads the configuration of component c from the real environment.
func Load(c Component) (*Config, error) { return load(c, os.Getenv) }

// load is Load over an environment lookup (tests).
func load(c Component, getenv func(string) string) (*Config, error) {
	cfg := &Config{}
	path := getenv("CLOUDGROUND_CONFIG")
	if path == "" {
		path = fileFor(c)
	}
	fromFile, err := readFile(path)
	switch {
	case err == nil:
		cfg.File = path
	case errors.Is(err, fs.ErrNotExist):
		fromFile = nil
	case c == CLI && errors.Is(err, fs.ErrPermission):
		fromFile = nil
	default:
		return nil, err
	}

	var problems []string
	known := map[string]bool{"CLOUDGROUND_CONFIG": true}
	for _, k := range keys {
		known[k.name] = true // panel.env is shared: another component's key is no typo
		if !slices.Contains(k.for_, c) || k.apply == nil {
			continue
		}
		value, source := k.def, "default"
		if v, ok := fromFile[k.name]; ok {
			value, source = v, path
		}
		if v := getenv(k.name); v != "" {
			value, source = v, "environment"
		}
		if err := k.apply(cfg, value); err != nil {
			problems = append(problems, fmt.Sprintf("%s (%s): %v", k.name, source, err))
		}
	}
	if c == CLI {
		if src, err := resolveToken(cfg, fromFile, path, getenv); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", src, err))
		}
	}
	for name := range fromFile {
		if !known[name] {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("%s: unknown key %s ignored", path, name))
		}
	}
	slices.Sort(cfg.Warnings)
	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  %s", strings.Join(problems, "\n  "))
	}
	return cfg, nil
}

// readFile parses an EnvironmentFile-style file into its CLOUDGROUND_ keys.
func readFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if strings.HasPrefix(name, "CLOUDGROUND_") {
			out[name] = value
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// ReadSecret reads a shared-secret file (SPEC.md §4).
func ReadSecret(path string, min int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if len(s) < max(min, 1) {
		return "", fmt.Errorf("%s: the secret is shorter than %d bytes", path, max(min, 1))
	}
	return s, nil
}

// --- validators ---

func absPath(field func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error {
		if !filepath.IsAbs(v) {
			return fmt.Errorf("%q is not an absolute path", v)
		}
		*field(c) = filepath.Clean(v)
		return nil
	}
}

func hostPort(loopback bool, field func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error {
		host, port, err := net.SplitHostPort(v)
		if err != nil {
			return fmt.Errorf("%q is not host:port", v)
		}
		if !validPort(port) {
			return fmt.Errorf("%q: port must be 1-65535", v)
		}
		if loopback {
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("%q must be on a loopback address", v)
			}
		}
		*field(c) = v
		return nil
	}
}

func validPort(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	n := 0
	for _, r := range p {
		if r < '0' || r > '9' {
			return false
		}
		n = n*10 + int(r-'0')
	}
	return n >= 1 && n <= 65535
}

// flag is an on/off switch as these settings always were: "1" is on, empty
// or "0" off. Anything else ("true", "yes") is refused rather than given a
// meaning it never had.
func flag(field func(*Config) *bool) func(*Config, string) error {
	return func(c *Config, v string) error {
		switch strings.TrimSpace(v) {
		case "1":
			*field(c) = true
		case "", "0":
			*field(c) = false
		default:
			return fmt.Errorf("%q: set 1 to turn it on, 0 or nothing to leave it off", v)
		}
		return nil
	}
}

func text(field func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error { *field(c) = v; return nil }
}

var (
	phpRe     = regexp.MustCompile(`^[0-9]\.[0-9]{1,2}$`)
	accountRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	tokenRe   = regexp.MustCompile(`^cgt_[A-Za-z0-9_-]{43}$`)
)

func phpVersion(c *Config, v string) error {
	if !phpRe.MatchString(v) {
		return fmt.Errorf("%q is not a PHP version such as 8.4", v)
	}
	c.DefaultPHP = v
	return nil
}

func drainTimeout(c *Config, v string) error {
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Second || d > 10*time.Minute {
		return fmt.Errorf("%q must be a duration between 1s and 10m", v)
	}
	c.DrainTimeout = d
	return nil
}

func account(field func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error {
		if !accountRe.MatchString(v) {
			return fmt.Errorf("%q is not a user or group name", v)
		}
		*field(c) = v
		return nil
	}
}

func panelURL(c *Config, v string) error {
	u, err := url.Parse(strings.TrimSuffix(v, "/"))
	if err != nil {
		return fmt.Errorf("%q is not a URL", v)
	}
	switch u.Scheme {
	case "unix":
		if !filepath.IsAbs(u.Path) || u.Host != "" {
			return fmt.Errorf("%q: give the socket as unix:///absolute/path", v)
		}
	case "https":
		if u.Host == "" {
			return fmt.Errorf("%q has no host", v)
		}
	case "http":
		if !IsLoopback(u.Hostname()) {
			return fmt.Errorf("%q: plain http only to a loopback address; use https", v)
		}
	default:
		return fmt.Errorf("%q: the scheme must be unix, https or http", v)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "unix" && u.Path != "") {
		return fmt.Errorf("%q: give only the panel's address, without a path, query or credentials", v)
	}
	c.URL = u.String()
	return nil
}

// IsLoopback reports whether host names this machine over loopback.
func IsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// resolveToken picks the CLI's token (SPEC.md §3): the two keys are one
// setting, so the source decides first (environment over file), and within
// a source the token over the token file. Only the chosen one is read and
// validated, so a stale token in the file cannot fail a command the
// environment gives a good token. It returns the chosen key and source.
func resolveToken(c *Config, fromFile map[string]string, path string, getenv func(string) string) (string, error) {
	type candidate struct{ key, source, value string }
	var cands []candidate
	for _, k := range []string{"CLOUDGROUND_TOKEN", "CLOUDGROUND_TOKEN_FILE"} {
		cands = append(cands, candidate{k, "environment", getenv(k)})
	}
	for _, k := range []string{"CLOUDGROUND_TOKEN", "CLOUDGROUND_TOKEN_FILE"} {
		cands = append(cands, candidate{k, path, fromFile[k]})
	}
	for _, cd := range cands {
		if cd.value == "" {
			continue
		}
		where := fmt.Sprintf("%s (%s)", cd.key, cd.source)
		secret := cd.value
		if cd.key == "CLOUDGROUND_TOKEN_FILE" {
			b, err := os.ReadFile(cd.value)
			if err != nil {
				return where, err
			}
			secret = strings.TrimSpace(string(b))
		}
		if !tokenRe.MatchString(secret) {
			return where, errors.New("not an API token (cgt_ followed by 43 characters)")
		}
		c.Token = secret
		return where, nil
	}
	return "", nil
}
