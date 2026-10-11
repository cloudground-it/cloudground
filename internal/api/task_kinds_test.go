package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every task kind the panel can list has a name in the interface
// (ui/i18n/locales/en.json, tasks.kind.<kind with dots as _>; the locale
// test holds the other four languages to it): backup.retention and
// backup.forget showed as their raw kind. The kinds are read from this
// package's source, the job requests and the registered kinds; the quiet
// (maintenance) ones are never listed and need none.
func TestEveryListedTaskKindIsTranslated(t *testing.T) {
	s, _, _ := testServer(t)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	kindRe := regexp.MustCompile(`(?:kind|Kind|Name):\s+"([a-z]+\.[a-z_.-]+)"`)
	kinds := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range kindRe.FindAllStringSubmatch(string(b), -1) {
			if k, ok := s.Jobs.Kind(m[1]); ok && k.Quiet {
				continue
			}
			kinds[m[1]] = true
		}
	}
	if len(kinds) < 20 {
		t.Fatalf("only %d kinds found: %v", len(kinds), kinds)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "ui", "i18n", "locales", "en.json"))
	if err != nil {
		t.Fatal(err)
	}
	var en struct {
		Tasks struct {
			Kind map[string]string `json:"kind"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &en); err != nil {
		t.Fatal(err)
	}
	var missing []string
	for k := range kinds {
		if en.Tasks.Kind[strings.ReplaceAll(k, ".", "_")] == "" {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("task kinds with no name in the interface: %v", missing)
	}
}
