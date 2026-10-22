//go:build !e2e

package e2e

// The e2e sections as the source declares them, checked against the files
// that describe them: e2e/sections.map (which code changes affect which
// sections), e2e/SECTIONS.md (each section's fixtures) and
// e2e/sections.index (which sections reach each function of the suite,
// generated). The suite's sources carry the e2e build tag, so they are read
// as source, with go/parser, which ignores build tags; this file has the
// opposite tag, so it never runs on a server inside cg-e2e.

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type declaredSection struct {
	name     string
	run      string // harness method
	fixtures []string
	lane     string
}

// parseE2E parses every .go file of this directory.
func parseE2E(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files[p] = f
	}
	return fset, files
}

// stringConsts are the package's string constants (the fixture names).
func stringConsts(files map[string]*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if bl, ok := vs.Values[i].(*ast.BasicLit); ok && bl.Kind == token.STRING {
							out[n.Name], _ = strconv.Unquote(bl.Value)
						}
					}
				}
			}
		}
	}
	return out
}

// declaredSections reads the sections table (var sections = []section{…}).
func declaredSections(t *testing.T) []declaredSection {
	t.Helper()
	_, files := parseE2E(t)
	consts := stringConsts(files)
	str := func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.BasicLit:
			s, err := strconv.Unquote(v.Value)
			if err != nil {
				t.Fatalf("sections: %s: %v", v.Value, err)
			}
			return s
		case *ast.Ident:
			if s, ok := consts[v.Name]; ok {
				return s
			}
		}
		t.Fatalf("sections: %T is not a string literal or a string constant", e)
		return ""
	}
	var out []declaredSection
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR {
				continue
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				if len(vs.Names) != 1 || vs.Names[0].Name != "sections" || len(vs.Values) != 1 {
					continue
				}
				lit, ok := vs.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Fatal("sections is not a composite literal")
				}
				for _, el := range lit.Elts {
					cl, ok := el.(*ast.CompositeLit)
					if !ok {
						t.Fatalf("sections: element %T", el)
					}
					var ds declaredSection
					for _, kv := range cl.Elts {
						kv, ok := kv.(*ast.KeyValueExpr)
						if !ok {
							t.Fatal("sections: elements must use field names")
						}
						switch kv.Key.(*ast.Ident).Name {
						case "name":
							ds.name = str(kv.Value)
						case "lane":
							ds.lane = str(kv.Value)
						case "run":
							sel, ok := kv.Value.(*ast.SelectorExpr)
							if !ok {
								t.Fatalf("sections: %q: run must be (*harness).<method>", ds.name)
							}
							ds.run = sel.Sel.Name
						case "fixtures":
							for _, e := range kv.Value.(*ast.CompositeLit).Elts {
								ds.fixtures = append(ds.fixtures, str(e))
							}
						}
					}
					out = append(out, ds)
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no sections table found (var sections = []section{…})")
	}
	return out
}

func TestSectionsDeclared(t *testing.T) {
	_, files := parseE2E(t)
	methods := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && recvType(fd) == "harness" {
				methods[fd.Name.Name] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, s := range declaredSections(t) {
		tn := strings.Join(strings.Fields(s.name), "_")
		if s.name == "" || seen[tn] {
			t.Errorf("section %q: empty or duplicate name", s.name)
		}
		seen[tn] = true
		if !methods[s.run] {
			t.Errorf("section %q runs harness.%s, which is not defined", s.name, s.run)
		}
		if strings.ContainsAny(s.name, ",=|/@#") {
			t.Errorf("section %q: the name must not contain , = | / @ #", s.name)
		}
	}
}

// No section declares a fixture after the section that deletes it
// (h.dropFixture): run in the table's order, it would only ever find the
// fixture gone ("deleted earlier in this run"), and fail.
func TestSectionsFixtureAfterTeardown(t *testing.T) {
	_, files := parseE2E(t)
	consts := stringConsts(files)
	drops := map[string][]string{} // harness method -> fixtures it deletes
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || recvType(fd) != "harness" {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "dropFixture" {
					if id, ok := call.Args[1].(*ast.Ident); ok {
						if v, ok := consts[id.Name]; ok {
							drops[fd.Name.Name] = append(drops[fd.Name.Name], v)
						}
					}
				}
				return true
			})
		}
	}
	if len(drops) == 0 {
		t.Fatal("no section deletes a fixture: the dropFixture calls were not found")
	}
	dropped := map[string]string{} // fixture -> the section that deleted it
	for _, s := range declaredSections(t) {
		for _, fx := range s.fixtures {
			if by, ok := dropped[fx]; ok {
				t.Errorf("section %q declares the fixture %q after %q deleted it: move it before %q", s.name, fx, by, by)
			}
		}
		for _, fx := range drops[s.run] {
			dropped[fx] = s.name
		}
	}
}

// ------------------------------------------------------------ sections.map --

type mapLine struct {
	n        int
	glob     string
	sections []string // or the single word ALL / NONE
}

func readSectionsMap(t *testing.T) []mapLine {
	t.Helper()
	f, err := os.Open("sections.map")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	groups := map[string][]string{}
	var out []mapLine
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		expand := func(rhs string) []string {
			var out []string
			for _, item := range strings.Split(rhs, ",") {
				item = strings.TrimSpace(item)
				if strings.HasPrefix(item, "@") {
					g, ok := groups[item]
					if !ok {
						t.Errorf("sections.map:%d: group %s is not defined above", n, item)
					}
					out = append(out, g...)
				} else if item != "" {
					out = append(out, item)
				}
			}
			return out
		}
		if strings.HasPrefix(line, "@") {
			name, rhs, ok := strings.Cut(line, "=")
			if !ok {
				t.Errorf("sections.map:%d: a group line is @name = sections", n)
				continue
			}
			groups[strings.TrimSpace(name)] = expand(rhs)
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Errorf("sections.map:%d: want <glob> <sections>", n)
			continue
		}
		rhs := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		out = append(out, mapLine{n: n, glob: fields[0], sections: expand(rhs)})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Every section the map names exists, and every section is named by at
// least one line (ALL does not count): a section no change can select would
// only ever run in a full run.
func TestSectionsMap(t *testing.T) {
	known := map[string]bool{}
	for _, s := range declaredSections(t) {
		known[s.name] = true
	}
	named := map[string]bool{}
	for _, l := range readSectionsMap(t) {
		for _, s := range l.sections {
			switch {
			case s == "ALL" || s == "NONE":
				if len(l.sections) != 1 {
					t.Errorf("sections.map:%d: %s stands alone", l.n, s)
				}
			case !known[s]:
				t.Errorf("sections.map:%d: no section %q (sections in e2e_test.go)", l.n, s)
			default:
				named[s] = true
			}
		}
	}
	for s := range known {
		if !named[s] {
			t.Errorf("section %q is not named by any line of sections.map: map the code it exercises to it", s)
		}
	}
}

// --------------------------------------------------------------- SECTIONS.md --

// SECTIONS.md has, under "## Sections", a "| `<section>` | <fixtures> |
// <lane> | …" row per section, with exactly the fixtures and the lane the
// section declares ("—" for none), and no row for anything else.
func TestSectionsDocumented(t *testing.T) {
	b, err := os.ReadFile("SECTIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	type doc struct {
		fixtures []string
		lane     string
	}
	rows := map[string]doc{}
	row := regexp.MustCompile("^\\| `([^`]+)` \\| ([^|]*)\\| ([^|]*)\\|")
	fx := regexp.MustCompile("`([^`]+)`")
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "## ") {
			in = line == "## Sections"
		}
		m := row.FindStringSubmatch(line)
		if !in || m == nil {
			continue
		}
		d := doc{lane: strings.TrimSpace(m[3])}
		for _, f := range fx.FindAllStringSubmatch(m[2], -1) {
			d.fixtures = append(d.fixtures, f[1])
		}
		rows[m[1]] = d
	}
	for _, s := range declaredSections(t) {
		d, ok := rows[s.name]
		if !ok {
			t.Errorf("SECTIONS.md has no row for section %q", s.name)
			continue
		}
		want := append([]string(nil), s.fixtures...)
		sort.Strings(want)
		sort.Strings(d.fixtures)
		if strings.Join(d.fixtures, ",") != strings.Join(want, ",") {
			t.Errorf("SECTIONS.md: section %q lists fixtures %v, the code declares %v", s.name, d.fixtures, want)
		}
		lane := s.lane
		if lane == "" {
			lane = "—"
		}
		if d.lane != lane {
			t.Errorf("SECTIONS.md: section %q is in lane %q, the code says %q", s.name, d.lane, lane)
		}
		delete(rows, s.name)
	}
	for name := range rows {
		t.Errorf("SECTIONS.md documents %q, which is not a section", name)
	}
}

// ---------------------------------------------------------- sections.index --

func recvType(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	e := fd.Recv.List[0].Type
	for {
		switch v := e.(type) {
		case *ast.StarExpr:
			e = v.X
		case *ast.IndexExpr:
			e = v.X
		case *ast.IndexListExpr:
			e = v.X
		case *ast.Ident:
			return v.Name
		default:
			return ""
		}
	}
}

// funcKey is how the index (and the scripts, from a `func` line) name a
// function: Recv.Name for a method, Name for a function.
func funcKey(fd *ast.FuncDecl) string {
	if r := recvType(fd); r != "" {
		return r + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// sectionIndex is, for every function of the suite, the sections that reach
// it: "ALL" when TestE2E itself (or TestMain, or an init) does, "NONE" when
// nothing does. A call is resolved by name only, so a selector x.f reaches
// every method f and an identifier f the function f: that errs towards more
// sections, never fewer.
func sectionIndex(t *testing.T) []string {
	fset, files := parseE2E(t)
	type fn struct {
		file string
		decl *ast.FuncDecl
	}
	funcs := map[string]fn{}
	methodsNamed := map[string][]string{}
	funcNamed := map[string]string{}
	var keys []string
	for p, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			k := funcKey(fd)
			if fd.Name.Name == "init" {
				k = "init@" + p + ":" + strconv.Itoa(fset.Position(fd.Pos()).Line)
			}
			funcs[k] = fn{p, fd}
			keys = append(keys, k)
			if fd.Recv != nil {
				methodsNamed[fd.Name.Name] = append(methodsNamed[fd.Name.Name], k)
			} else {
				funcNamed[fd.Name.Name] = k
			}
		}
	}
	edges := map[string][]string{}
	for k, f := range funcs {
		if f.decl.Body == nil {
			continue
		}
		seen := map[string]bool{}
		add := func(to string) {
			if !seen[to] {
				seen[to] = true
				edges[k] = append(edges[k], to)
			}
		}
		ast.Inspect(f.decl.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				for _, m := range methodsNamed[v.Sel.Name] {
					add(m)
				}
			case *ast.Ident:
				if to, ok := funcNamed[v.Name]; ok {
					add(to)
				}
			}
			return true
		})
	}
	reach := func(root string, mark func(string)) {
		seen := map[string]bool{}
		stack := []string{root}
		for len(stack) > 0 {
			k := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[k] {
				continue
			}
			seen[k] = true
			mark(k)
			stack = append(stack, edges[k]...)
		}
	}
	all := map[string]bool{}
	for k := range funcs {
		if k == "TestE2E" || k == "TestMain" || strings.HasPrefix(k, "init@") {
			reach(k, func(r string) { all[r] = true })
		}
	}
	secs := declaredSections(t)
	by := map[string]map[string]bool{}
	for _, s := range secs {
		reach("harness."+s.run, func(r string) {
			if by[r] == nil {
				by[r] = map[string]bool{}
			}
			by[r][s.name] = true
		})
	}
	var lines []string
	for _, k := range keys {
		var v string
		switch {
		case all[k] || len(by[k]) == len(secs):
			v = "ALL"
		case len(by[k]) == 0:
			v = "NONE"
		default:
			var names []string
			for _, s := range secs { // in the table's order
				if by[k][s.name] {
					names = append(names, s.name)
				}
			}
			v = strings.Join(names, "|")
		}
		name := k
		if strings.HasPrefix(k, "init@") {
			name = "init"
		}
		lines = append(lines, "e2e/"+funcs[k].file+"\t"+name+"\t"+v)
	}
	sort.Strings(lines)
	return lines
}

const indexHeader = `# e2e/sections.index: for each function of the e2e suite, the sections that
# reach it (|-separated), ALL (TestE2E itself runs it) or NONE. Generated from
# the source by TestSectionsIndex; do not edit. Regenerate after changing the
# suite: make e2e-index (or make docker-e2e-index).
# scripts/e2e-affected.sh and scripts/e2e-new-checks.sh map a changed line to
# its function, and the function to its sections, with it.
`

// The committed index is the one the source gives. CG_E2E_UPDATE_INDEX=1
// rewrites it.
func TestSectionsIndex(t *testing.T) {
	want := indexHeader + strings.Join(sectionIndex(t), "\n") + "\n"
	if os.Getenv("CG_E2E_UPDATE_INDEX") != "" {
		if err := os.WriteFile("sections.index", []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile("sections.index")
	if err != nil || string(got) != want {
		t.Fatalf("e2e/sections.index is out of date with the suite's source: run make e2e-index (or make docker-e2e-index) and commit it")
	}
}

// --------------------------------------------------------------- the scripts --

// runScript runs a script of scripts/ from the repository root with stdin,
// skipping when a tool it needs is missing.
func runScript(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	for _, tool := range []string{"bash", "awk", "sed", "perl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	cmd := exec.Command("bash", args...)
	cmd.Dir = ".."
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// The scripts read the section table with awk: they see the sections the
// source declares, in its order.
func TestScriptsSections(t *testing.T) {
	out, errs, err := runScript(t, "", "scripts/e2e-affected.sh", "--sections")
	if err != nil {
		t.Fatalf("%v\n%s", err, errs)
	}
	var want []string
	for _, s := range declaredSections(t) {
		want = append(want, s.name)
	}
	if got := strings.TrimSpace(out); got != strings.Join(want, "\n") {
		t.Errorf("e2e-affected.sh --sections:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// The affected-sections script, on paths given on stdin: the map's first
// matching line wins, e2e functions go through the index, unknown paths
// select everything.
func TestAffectedScript(t *testing.T) {
	run := func(paths ...string) string {
		out, errs, err := runScript(t, strings.Join(paths, "\n")+"\n", "scripts/e2e-affected.sh", "--paths")
		if err != nil && !strings.Contains(errs, "no e2e section") {
			t.Fatalf("e2e-affected.sh --paths %v: %v\n%s", paths, err, errs)
		}
		return strings.TrimSpace(out)
	}
	if got := run("docs/api.md", "bench/README.md", "internal/api/maintenance_test.go"); got != "" {
		t.Errorf("docs and unit tests select %q, want nothing", got)
	}
	if got := run("internal/guard/guard.go"); got != `^TestE2E$/^(wordpress_safe_push|wordpress_updates)$` {
		t.Errorf("internal/guard: %q", got)
	}
	if got := run("internal/agent/ops/db/provision.go"); !strings.Contains(got, "woocommerce_teardown") || !strings.Contains(got, "site_types") {
		t.Errorf("ops/db/provision.go (a specific line above ops/db/**): %q", got)
	}
	if got := run("internal/agent/ops/db/console.go"); strings.Contains(got, "site_types") || !strings.Contains(got, "woocommerce_security") {
		t.Errorf("ops/db/console.go: %q", got)
	}
	if got := run("somewhere/new.txt"); got != `^TestE2E$` {
		t.Errorf("an unmapped path selects %q, want every section", got)
	}
	if got := run("internal/state/store.go"); got != `^TestE2E$` {
		t.Errorf("internal/state selects %q, want every section", got)
	}
	// R-0.7 has a "." that must be escaped, and both sections are kept in
	// the table's order.
	if got := run("internal/agent/ops/system/services.go", "internal/api/maintenance.go"); got != `^TestE2E$/^(wordpress_certificates|R-0\.7_reconcile|R-2\.4_services)$` {
		t.Errorf("services and scheduler: %q", got)
	}
}

// Under pipefail, a pipe into grep -q fails now and then although grep
// matched: grep quits at its first match, and a writer still writing dies
// of SIGPIPE (on a loaded machine e2e-affected.sh dropped a section so).
// The scripts give grep -q a here-string or a file instead.
func TestScriptsDoNotPipeIntoGrepQuiet(t *testing.T) {
	files, err := filepath.Glob("../scripts/*.sh")
	if err != nil || len(files) == 0 {
		t.Fatalf("no scripts: %v", err)
	}
	pipeIntoQuiet := regexp.MustCompile(`\|\s*grep\s+-[A-Za-z]*q`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "pipefail") {
			continue
		}
		for i, line := range strings.Split(string(src), "\n") {
			if l := strings.TrimSpace(line); !strings.HasPrefix(l, "#") && pipeIntoQuiet.MatchString(l) {
				t.Errorf("%s:%d pipes into grep -q under pipefail: %s", filepath.Base(f), i+1, l)
			}
		}
	}
}

// The function lookup the scripts do in awk agrees with funcKey.
func TestScriptsFunctionLookup(t *testing.T) {
	out, errs, err := runScript(t, "", "scripts/e2e-affected.sh", "--func-at", "e2e/features_test.go")
	if err != nil {
		t.Fatalf("%v\n%s", err, errs)
	}
	got := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if _, k, ok := strings.Cut(line, "\t"); ok {
			got[k] = true
		}
	}
	for _, line := range sectionIndex(t) {
		f := strings.Split(line, "\t")
		if f[0] == "e2e/features_test.go" && !got[f[1]] {
			t.Errorf("the scripts do not find function %s in features_test.go (they found %v)", f[1], got)
		}
	}
}

// The new-checks script turns check calls into name patterns that match the
// check's test name, as `go test` matches it: the part before the first
// "/" (which it splits on), up to its first run-time part.
func TestNewChecksPatterns(t *testing.T) {
	cases := []struct{ call, pattern, name string }{
		{`h.check(t, "R-2.4: nginx restart is accepted", code(x), "200")`,
			`R-2\.4:_nginx_restart_is_accepted`, "R-2.4: nginx restart is accepted"},
		{`h.assert(t, "R-3.7: "+p+" is removed with its site", ok, "x")`,
			`R-3\.7:_.*`, "R-3.7: /srv/sites/r37a.example is removed with its site"},
		{`h.check(t, fmt.Sprintf("R-0.7 (%s): nginx reloads during the site-count reconcile", what), a, "0")`,
			`R-0\.7_\(.*`, "R-0.7 (create): nginx reloads during the site-count reconcile"},
		{`h.fail(t, "fixture (a.b)",
			"x")`, `fixture_\(a\.b\)`, "fixture (a.b)"},
		{`h.check(t, "woocommerce /shop resolves", sc, "200")`, `woocommerce_`, "woocommerce /shop resolves"},
		{`f.objectCacheSet(t, true, "R-3.4: object cache on before a database restore")`,
			`.*R-3\.4:_object_cache_on_before_a_database_restore.*`, "R-3.4: object cache on before a database restore"},
		{`f.ensureObjectCache(t, "the backup checks")`,
			`.*the_backup_checks.*`, "control: the object cache is turned on before the backup checks"},
		{`h.check(t, label+" cache warm completes", x, "y")`, `.*`, "R-3.8: woocommerce cache warm completes"},
	}
	var src []string
	for _, c := range cases {
		src = append(src, "\t"+c.call)
	}
	out, errs, err := runScript(t, strings.Join(src, "\n")+"\n", "scripts/e2e-new-checks.sh", "--patterns")
	if err != nil {
		t.Fatalf("%v\n%s", err, errs)
	}
	got := strings.Split(strings.TrimSpace(out), "\n")
	if len(got) != len(cases) {
		t.Fatalf("%d patterns for %d calls:\n%s", len(got), len(cases), out)
	}
	for i, c := range cases {
		if got[i] != c.pattern {
			t.Errorf("%s: pattern %s, want %s", c.call, got[i], c.pattern)
		}
		elem := strings.SplitN(strings.Join(strings.Fields(c.name), "_"), "/", 2)[0]
		if !regexp.MustCompile("^(" + got[i] + ")(#[0-9]+)?$").MatchString(elem) {
			t.Errorf("pattern %s does not select the check %q", got[i], c.name)
		}
	}
}
