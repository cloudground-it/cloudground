package api

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"text/template"
)

// The API reference, docs/api.md, is this template with the endpoint tables
// filled in from the route table (SPEC.md §1.9).
//
//go:embed apidoc.md.tmpl
var apiDocTemplate string

// policyRequires is what each policy's marker means, for the roles table.
var policyRequires = map[policy]string{
	policyOpen:     "anyone; no session needed",
	policySignedIn: "every signed-in role",
	policyManage:   "`admin`, or an `operator` on a site assigned to it",
	policyGlobal:   "`admin` and `readonly`; server-wide data is closed to `operator`",
	policyAdmin:    "`admin` alone",
	policyToken:    "the site's connector token, in place of a session",
	// Quarry's own session (its cookie), never the panel's.
	policyStudio:     "a Quarry session (see Quarry below), never a panel session or an API token",
	policyStudioData: "a Quarry session that may read the rows: a database user's, or one opened from the panel by an `admin` or `operator`",
}

// Reference renders docs/api.md from the route table.
func Reference() ([]byte, error) {
	table := (&Server{}).routeTable()
	placed := map[string]bool{}
	funcs := template.FuncMap{
		"routes": func(section string) (string, error) {
			if placed[section] {
				return "", fmt.Errorf("section %q placed twice", section)
			}
			placed[section] = true
			var b strings.Builder
			b.WriteString("| Method | Path | Access | What it does |\n| :-- | :-- | :-- | :-- |\n")
			n := 0
			for _, rt := range table {
				if rt.Section != section {
					continue
				}
				method, path := "any", rt.Path
				if rt.Method != "" {
					method = "`" + rt.Method + "`"
				}
				if rt.Query != "" {
					path += "?" + rt.Query
				}
				role := rt.Policy.marker()
				if rt.SessionOnly {
					role += ", session only"
				}
				fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n", method, path, role, rt.Doc)
				n++
			}
			if n == 0 {
				return "", fmt.Errorf("section %q has no routes", section)
			}
			return strings.TrimSuffix(b.String(), "\n"), nil
		},
		"roles": func() string {
			var b strings.Builder
			b.WriteString("| Marker | Who may call |\n| :-- | :-- |")
			for _, p := range policies {
				fmt.Fprintf(&b, "\n| **%s** | %s |", p.marker(), policyRequires[p])
			}
			return b.String()
		},
		"statementsPerUser": func() int { return maxStatementsPerUser },
		"statementsPerSite": func() int { return maxStatementsPerSite },
		"statementRoutes": func() string {
			var lines []string
			for _, rt := range table {
				if rt.Statements {
					lines = append(lines, fmt.Sprintf("  - `%s %s`", rt.Method, rt.Path))
				}
			}
			return strings.Join(lines, "\n")
		},
		"limits": func() string {
			var lines []string
			for _, rt := range table {
				if rt.MaxBody != 0 {
					lines = append(lines, fmt.Sprintf("  - `%s %s`: %s", rt.Method, rt.Path, byteSize(rt.MaxBody)))
				}
			}
			return strings.Join(lines, "\n")
		},
	}
	tmpl, err := template.New("api.md").Funcs(funcs).Parse(apiDocTemplate)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, nil); err != nil {
		return nil, err
	}
	for _, rt := range table {
		if !placed[rt.Section] {
			return nil, fmt.Errorf("%s: section %q is not in apidoc.md.tmpl", rt.pattern(), rt.Section)
		}
	}
	return out.Bytes(), nil
}

// RouteSummary is a row of the route table as code outside the package sees
// it: the e2e suite walks it against a live panel.
type RouteSummary struct {
	Method, Path string
	// Role is the policy's docs marker: open, site token, auth, global,
	// manage, admin, dbstudio or dbstudio data.
	Role string
	// Resource is what the {id} loads and checks against the caller: site,
	// backup, deployment, cron job, or "" when the router loads nothing.
	Resource string
	// OperatorRefusal is an admin route's 403 message to an operator.
	OperatorRefusal string
}

// RouteSummaries lists the route table.
func RouteSummaries() []RouteSummary {
	var out []RouteSummary
	for _, rt := range (&Server{}).routeTable() {
		sum := RouteSummary{Method: rt.Method, Path: rt.Path, Role: rt.Policy.marker(), Resource: rt.Load.noun()}
		if rt.Policy == policyAdmin {
			sum.OperatorRefusal = operatorRefusal(rt.AdminOnly)
		}
		out = append(out, sum)
	}
	return out
}

// byteSize writes a body limit as the docs do: 8 KiB, 16 MiB.
func byteSize(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
