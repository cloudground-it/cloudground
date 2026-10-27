package nginx

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/cloudground-it/cloudground/internal/functions"
	"github.com/cloudground-it/cloudground/internal/site"
)

// The functions' files in the sites directory (SPEC §8.1).
const (
	FunctionsDomainFile   = "cloudground-functions.conf"
	FunctionsInternalFile = "cloudground-functions-internal.conf"
)

// FunctionFile is a function's own domains' file.
func FunctionFile(id int64) string { return "cloudground-fn-" + strconv.FormatInt(id, 10) + ".conf" }

// IsFunctionsFile reports whether a file name in the sites directory is
// one of the three kinds FunctionsFiles writes.
func IsFunctionsFile(name string) bool {
	if name == FunctionsDomainFile || name == FunctionsInternalFile {
		return true
	}
	var id int64
	n, err := fmt.Sscanf(name, "cloudground-fn-%d.conf", &id)
	return err == nil && n == 1 && id > 0 && FunctionFile(id) == name
}

// A function's bounds in nginx (SPEC §8.4): each client at most
// FunctionRatePerSecond requests a second (FunctionBurst at once) and
// FunctionConnsPerClient at a time, each function FunctionConnsPerFunction
// at a time — the socket's MaxConnections, so a socket never refuses.
const (
	FunctionRatePerSecond    = 10
	FunctionBurst            = 20
	FunctionConnsPerClient   = 8
	FunctionConnsPerFunction = functions.MaxConnections
)

// FunctionsInput is what the functions' files are rendered from (SPEC §8).
type FunctionsInput struct {
	// Domain is the functions domain; "" is none.
	Domain                string
	DomainCert, DomainKey string
	ACMEWebroot           string
	// InternalSocket is the agent's invocation socket.
	InternalSocket string
	HTTP2Directive bool
	Functions      []FunctionServer
}

// FunctionServer is one function as nginx serves it.
type FunctionServer struct {
	ID          int64
	Name        string
	Socket      string
	TimeoutSec  int
	AccessLog   string
	Token       string
	CORSOrigins []string
	// Domains are its own; CertFile and KeyFile their certificate.
	Domains           []string
	CertFile, KeyFile string
}

// checkFunctions refuses what SPEC §8 refuses.
func checkFunctions(in FunctionsInput) error {
	claimed := map[string]string{}
	claim := func(d, by string) error {
		if err := site.ValidateDomain(d); err != nil {
			return err
		}
		if other, ok := claimed[d]; ok {
			return fmt.Errorf("%s is claimed by %s and %s", d, other, by)
		}
		claimed[d] = by
		return nil
	}
	paths := map[string]string{"ACME webroot": in.ACMEWebroot, "internal socket": in.InternalSocket}
	if in.Domain != "" {
		if err := claim(in.Domain, "the functions domain"); err != nil {
			return err
		}
		paths["functions domain certificate"], paths["functions domain key"] = in.DomainCert, in.DomainKey
	}
	names, ids := map[string]bool{}, map[int64]bool{}
	for _, f := range in.Functions {
		if !functions.ValidName(f.Name) || f.ID <= 0 || names[f.Name] || ids[f.ID] {
			return fmt.Errorf("invalid function %q", f.Name)
		}
		names[f.Name], ids[f.ID] = true, true
		if f.Socket != functions.SocketPath(f.ID) || f.AccessLog != functions.AccessLog(f.Name) {
			return fmt.Errorf("function %s: its socket or its log is not where functions live", f.Name)
		}
		if f.TimeoutSec < 1 || f.TimeoutSec > functions.MaxTimeout {
			return fmt.Errorf("function %s: invalid timeout", f.Name)
		}
		if f.Token != "" && !functions.ValidToken(f.Token) {
			return fmt.Errorf("function %s: invalid token", f.Name)
		}
		if err := functions.CheckOrigins(f.CORSOrigins); err != nil {
			return fmt.Errorf("function %s: %w", f.Name, err)
		}
		for _, d := range f.Domains {
			if err := claim(d, "function "+f.Name); err != nil {
				return err
			}
		}
		if len(f.Domains) > 0 {
			paths["certificate of "+f.Name], paths["key of "+f.Name] = f.CertFile, f.KeyFile
		}
	}
	for what, p := range paths {
		if !safeConfPath(p) {
			return fmt.Errorf("functions: %s path %q is not a clean absolute path", what, p)
		}
	}
	return nil
}

// FunctionsFiles renders the functions' servers (SPEC §8): path → content,
// in the sites directory. With neither a domain nor a function, none.
func (r Renderer) FunctionsFiles(in FunctionsInput) (map[string]string, error) {
	if err := checkFunctions(in); err != nil {
		return nil, err
	}
	dir := orDefault(r.SitesDir, DefaultSitesDir)
	files := map[string]string{}
	fns := slices.Clone(in.Functions)
	slices.SortFunc(fns, func(a, b FunctionServer) int { return cmp.Compare(a.ID, b.ID) })
	if len(fns) > 0 {
		files[filepath.Join(dir, FunctionsInternalFile)] = internalFile(in, fns)
	}
	if in.Domain != "" {
		var c conf
		c.put(0, "# Managed by CloudGround — do not edit. Changes are detected as drift.",
			"# The functions domain (SPEC §8.3): every function at https://"+in.Domain+"/<name>.")
		publicServers(&c, in, []string{in.Domain}, in.DomainCert, in.DomainKey, func(c *conf) {
			for _, f := range fns {
				for _, loc := range []string{"= /" + f.Name, "^~ /" + f.Name + "/"} {
					location(c, f, loc, "/"+f.Name, false)
				}
			}
			notFound(c)
		})
		files[filepath.Join(dir, FunctionsDomainFile)] = c.String()
	}
	for _, f := range fns {
		if len(f.Domains) == 0 {
			continue
		}
		var c conf
		c.put(0, "# Managed by CloudGround — do not edit. Changes are detected as drift.",
			fmt.Sprintf("# The function %s (id %d) on its own domains (SPEC §8.3).", f.Name, f.ID))
		publicServers(&c, in, f.Domains, f.CertFile, f.KeyFile, func(c *conf) {
			location(c, f, "/", "", false)
		})
		files[filepath.Join(dir, FunctionFile(f.ID))] = c.String()
	}
	return files, nil
}

// internalFile is the zones, the CORS maps and the internal server (SPEC
// §8.2, §8.4, §8.5).
func internalFile(in FunctionsInput, fns []FunctionServer) string {
	var c conf
	c.put(0, "# Managed by CloudGround — do not edit. Changes are detected as drift.",
		"# Every function's bounds (SPEC §8.4): keyed per function by $cg_fn_id and",
		"# per client and function by $cg_fn_client, both set in its locations.")
	c.putf(0, "limit_req_zone $cg_fn_client zone=cg_fn_req:4m rate=%dr/s;", FunctionRatePerSecond)
	c.put(0, "limit_conn_zone $cg_fn_client zone=cg_fn_conn:4m;", "limit_conn_zone $cg_fn_id zone=cg_fn_all:1m;")
	for _, f := range fns {
		if len(f.CORSOrigins) == 0 {
			continue
		}
		c.put(0, "")
		c.putf(0, "# The origins function %s answers (SPEC §8.5).", f.Name)
		c.putf(0, "map $http_origin $cg_fn_cors_%d {", f.ID)
		if slices.Contains(f.CORSOrigins, "*") {
			c.put(1, `default "*";`)
		} else {
			c.put(1, `default "";`)
			for _, o := range f.CORSOrigins {
				c.putf(1, "%q %q;", o, o)
			}
		}
		c.put(0, "}")
	}
	c.put(0, "", "server {",
		"    # The agent's own invocations (test runs and schedules): plain HTTP on a",
		"    # unix socket in a directory only root can enter (SPEC §8.2). No token.")
	c.put(1, "listen unix:"+in.InternalSocket+";", "server_name "+functions.InternalHost+";", "server_tokens off;", "access_log off;")
	for _, f := range fns {
		for _, loc := range []string{"= /" + f.Name, "^~ /" + f.Name + "/"} {
			location(&c, f, loc, "/"+f.Name, true)
		}
	}
	notFound(&c)
	c.put(0, "}")
	return c.String()
}

// publicServers writes the port 80 and the port 443 servers of names.
func publicServers(c *conf, in FunctionsInput, names []string, cert, key string, body func(*conf)) {
	sn := "server_name"
	for _, n := range names {
		sn += " " + n
	}
	c.put(0, "server {")
	c.put(1, "listen 80;", "listen [::]:80;", sn+";", "server_tokens off;",
		"location /.well-known/acme-challenge/ { root "+in.ACMEWebroot+"; }",
		"location / { return 301 https://$host$request_uri; }")
	c.put(0, "}", "server {")
	if in.HTTP2Directive {
		c.put(1, "listen 443 ssl;", "listen [::]:443 ssl;", "http2 on;")
	} else {
		c.put(1, "listen 443 ssl http2;", "listen [::]:443 ssl http2;")
	}
	c.put(1, sn+";", "server_tokens off;", "ssl_certificate "+cert+";", "ssl_certificate_key "+key+";")
	body(c)
	c.put(0, "}")
}

// notFound is the JSON 404 for a path that names no function.
func notFound(c *conf) {
	c.put(1, "location / {")
	c.put(2, "default_type application/json;", `return 404 '{"error":"no such function"}\n';`)
	c.put(1, "}")
}

// location writes one of a function's locations (SPEC §8.4): prefix is
// what the shim cuts from the path, internal the agent's server.
func location(c *conf, f FunctionServer, loc, prefix string, internal bool) {
	c.put(1, "location "+loc+" {")
	c.putf(2, "set $cg_fn_id %d;", f.ID)
	c.putf(2, `set $cg_fn_client "$binary_remote_addr %d";`, f.ID)
	cors := !internal && len(f.CORSOrigins) > 0
	corsVar := fmt.Sprintf("$cg_fn_cors_%d", f.ID)
	if cors {
		// A preflight carries no token: answered before the check.
		c.put(2, "if ($request_method = OPTIONS) {")
		c.put(3, "add_header Access-Control-Allow-Origin "+corsVar+" always;",
			`add_header Access-Control-Allow-Methods "GET, POST, PUT, PATCH, DELETE, OPTIONS" always;`,
			"add_header Access-Control-Allow-Headers $http_access_control_request_headers always;",
			"add_header Access-Control-Max-Age 600 always;",
			"add_header Vary Origin always;",
			"return 204;")
		c.put(2, "}")
	}
	c.put(2, "default_type application/json;")
	if !internal && f.Token != "" {
		c.putf(2, `if ($http_authorization != "Bearer %s") {`, f.Token)
		c.put(3, `return 401 '{"error":"a valid bearer token is required"}\n';`)
		c.put(2, "}")
	}
	source := `""`
	if internal {
		source = "$http_x_cloudground_source"
	}
	if prefix == "" {
		prefix = `""`
	}
	c.put(2, "access_log "+f.AccessLog+" cloudground buffer=4k flush=1s;",
		"proxy_pass http://unix:"+f.Socket+":;",
		"proxy_set_header Host $host;",
		"proxy_set_header X-Real-IP $remote_addr;",
		"proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;",
		"proxy_set_header X-Forwarded-Proto $scheme;",
		"proxy_set_header X-CloudGround-Source "+source+";",
		"proxy_set_header X-CloudGround-Prefix "+prefix+";",
		fmt.Sprintf("proxy_read_timeout %ds;", f.TimeoutSec+5),
		"client_max_body_size 6m;",
		fmt.Sprintf("limit_req zone=cg_fn_req burst=%d nodelay;", FunctionBurst),
		fmt.Sprintf("limit_conn cg_fn_conn %d;", FunctionConnsPerClient),
		fmt.Sprintf("limit_conn cg_fn_all %d;", FunctionConnsPerFunction),
		"limit_req_status 429;",
		"limit_conn_status 503;")
	if cors {
		c.put(2, "add_header Access-Control-Allow-Origin "+corsVar+" always;", "add_header Vary Origin always;")
	}
	c.put(1, "}")
}
