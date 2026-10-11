package main

// cgctl sites create (SPEC.md §6): the site's database flags, and the
// summary it prints once the site is created, which holds the database
// password the panel answers only this once.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// siteCreateArgs is the usage of sites create.
const siteCreateArgs = "<domain> <type> [flags-json] [--with-database] [--db-name <name>] [--db-user <user>] [--db-password-stdin | --db-password-file <file> | --db-password <password>] [--json]"

// siteCreateStdin is what --db-password-stdin, sftp password and a public
// key file "-" read (a variable for the tests).
var siteCreateStdin io.Reader = os.Stdin

// maxPasswordLine bounds the line --db-password-stdin reads.
const maxPasswordLine = 4 << 10

// argvPasswordWarning is printed for --db-password: argv is visible to
// every local user in ps and stays in the shell's history.
const argvPasswordWarning = "--db-password puts the password where ps and the shell history show it; use --db-password-stdin or --db-password-file"

// readPasswordLine reads one line from r, its line ending dropped. An empty
// one is refused: the panel would generate a password, which is not what
// the caller asked for.
func readPasswordLine(r io.Reader) (string, error) {
	line, err := bufio.NewReaderSize(io.LimitReader(r, maxPasswordLine+1), maxPasswordLine+1).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	if len(line) > maxPasswordLine {
		return "", fmt.Errorf("the line is longer than %d bytes", maxPasswordLine)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("no password on stdin")
	}
	return line, nil
}

// buildSiteCreate maps sites create's arguments to POST /api/sites. The
// flags win over the same fields in flags-json; domain and type always do.
// Its request prints the summary (not with --json).
func buildSiteCreate(a []string) (request, error) {
	body := map[string]any{}
	var pos []string
	summary := true
	var warnings []string
	pwFrom := ""
	pwSource := func(name string) error {
		if pwFrom != "" && pwFrom != name {
			return fmt.Errorf("%s and %s both give the database password", pwFrom, name)
		}
		pwFrom = name
		return nil
	}
	for i := 0; i < len(a); i++ {
		arg := a[i]
		name, val, hasVal := strings.Cut(arg, "=")
		value := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(a) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return a[i], nil
		}
		switch name {
		case "--json":
			summary = false
		case "--with-database":
			body["create_database"] = true
		case "--db-password-stdin":
			if hasVal {
				return request{}, errors.New("--db-password-stdin takes no value")
			}
			if err := pwSource(name); err != nil {
				return request{}, err
			}
			pw, err := readPasswordLine(siteCreateStdin)
			if err != nil {
				return request{}, fmt.Errorf("--db-password-stdin: %w", err)
			}
			body["database_password"] = pw
		case "--db-name", "--db-user", "--db-password", "--db-password-file":
			v, err := value()
			if err != nil {
				return request{}, err
			}
			if name == "--db-password" || name == "--db-password-file" {
				if err := pwSource(name); err != nil {
					return request{}, err
				}
			}
			switch name {
			case "--db-name":
				body["database_name"] = v
			case "--db-user":
				body["database_user"] = v
			case "--db-password":
				body["database_password"] = v
				warnings = append(warnings, argvPasswordWarning)
			case "--db-password-file":
				raw, err := os.ReadFile(v)
				if err != nil {
					return request{}, fmt.Errorf("--db-password-file: %w", err)
				}
				body["database_password"] = strings.TrimRight(string(raw), "\r\n")
			}
		default:
			if strings.HasPrefix(arg, "--") {
				return request{}, fmt.Errorf("unknown flag %s", name)
			}
			pos = append(pos, arg)
		}
	}
	if len(pos) < 2 || len(pos) > 3 {
		return request{}, errors.New("usage: cgctl sites create " + siteCreateArgs)
	}
	if len(pos) == 3 {
		extra := map[string]any{}
		if err := json.Unmarshal([]byte(pos[2]), &extra); err != nil {
			return request{}, fmt.Errorf("flags-json: %w", err)
		}
		for k, v := range extra {
			if _, set := body[k]; !set {
				body[k] = v
			}
		}
	}
	body["domain"], body["type"] = pos[0], pos[1]
	return request{method: "POST", path: "/api/sites", body: body, siteSummary: summary, warnings: warnings}, nil
}

// createdSite is the part of POST /api/sites's answer the summary reads.
type createdSite struct {
	Site struct {
		ID     int64  `json:"id"`
		Domain string `json:"domain"`
		Type   string `json:"type"`
	} `json:"site"`
	Task struct {
		ID int64 `json:"id"`
	} `json:"task"`
	Summary struct {
		SiteURL   string `json:"site_url"`
		PanelPath string `json:"panel_path"`
		PanelURL  string `json:"panel_url"`
		WordPress *struct {
			AdminURL  string `json:"admin_url"`
			AdminUser string `json:"admin_user"`
		} `json:"wordpress"`
		Database *struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Socket   string `json:"socket"`
			Name     string `json:"name"`
			User     string `json:"user"`
			Password string `json:"password"`
		} `json:"database"`
		SFTP struct {
			Enabled bool   `json:"enabled"`
			User    string `json:"user"`
			Host    string `json:"host"`
			Port    int    `json:"port"`
		} `json:"sftp"`
	} `json:"summary"`
}

// printSiteSummary writes the summary of a creation's answer. status is
// how the creation stands: "" when cgctl did not wait for it.
func printSiteSummary(w io.Writer, raw []byte, status string) {
	var c createdSite
	if json.Unmarshal(raw, &c) != nil || c.Site.ID == 0 {
		printJSON(w, raw)
		return
	}
	s := c.Summary
	row := func(label, value string) { fmt.Fprintf(w, "  %-12s %s\n", label+":", value) }
	fmt.Fprintf(w, "%s (%s, site %d)\n", c.Site.Domain, c.Site.Type, c.Site.ID)
	switch status {
	case "":
		fmt.Fprintf(w, "Being created: task %d (cgctl task %d; --wait follows it).\n", c.Task.ID, c.Task.ID)
	case "succeeded":
		fmt.Fprintln(w, "Created.")
	default:
		fmt.Fprintf(w, "The creation %s (task %d).\n", status, c.Task.ID)
	}
	fmt.Fprintln(w, "\nSite")
	row("URL", s.SiteURL)
	panel := s.PanelURL
	if panel == "" {
		panel = s.PanelPath
	}
	row("Panel", panel)
	if wp := s.WordPress; wp != nil {
		fmt.Fprintln(w, "\nWordPress")
		row("Admin URL", wp.AdminURL)
		row("Admin user", wp.AdminUser)
	}
	if db := s.Database; db != nil {
		fmt.Fprintln(w, "\nDatabase")
		row("Host", db.Host)
		row("Port", fmt.Sprint(db.Port))
		if db.Socket != "" {
			row("Socket", db.Socket)
		}
		row("Name", db.Name)
		row("User", db.User)
		row("Password", db.Password)
	}
	fmt.Fprintln(w, "\nSFTP")
	if s.SFTP.Enabled {
		row("Host", s.SFTP.Host)
		row("Port", fmt.Sprint(s.SFTP.Port))
		row("User", s.SFTP.User)
	} else {
		fmt.Fprintf(w, "  Off: turn it on from the site's SFTP tab (user %s).\n", s.SFTP.User)
	}
	if s.Database != nil {
		fmt.Fprintln(w, "\nThe database password is shown only now: keep it safe. A new one can be set from the site's Database tab.")
	}
}
