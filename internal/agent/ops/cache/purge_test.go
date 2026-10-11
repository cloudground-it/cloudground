package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/velocity"
)

func seed(t *testing.T, dir, host, uri string) string {
	t.Helper()
	p := velocity.CacheFilePath(dir, velocity.CacheKey("https", "GET", host, uri))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A URL purge removes the page under the domain and every alias; an empty
// list empties the site's cache; another site's cache is untouched.
func TestPurge(t *testing.T) {
	root := t.TempDir()
	c := Purger{Root: root}
	site := state.Site{Domain: "Docs.Example.com", Aliases: []string{"www.docs.example.com"}}
	dir := filepath.Join(root, "docs.example.com")
	other := seed(t, filepath.Join(root, "other.example.com"), "other.example.com", "/post/")
	seed(t, dir, "docs.example.com", "/post/")
	seed(t, dir, "www.docs.example.com", "/post/")
	keep := seed(t, dir, "docs.example.com", "/keep/")

	res, err := c.Purge(context.Background(), rpc.PurgeParams{Site: site, URLs: []string{"https://docs.example.com/post/"}})
	if err != nil || res.Removed != 2 {
		t.Fatalf("by URL: %+v %v", res, err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("an unrelated page was purged")
	}
	res, err = c.Purge(context.Background(), rpc.PurgeParams{Site: site})
	if err != nil || res.Removed != 1 {
		t.Fatalf("all: %+v %v", res, err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("another site's cache was purged")
	}
	if _, err := c.Purge(context.Background(), rpc.PurgeParams{Site: site, URLs: []string{"not a url"}}); err == nil {
		t.Fatal("an invalid URL was accepted")
	}
}
