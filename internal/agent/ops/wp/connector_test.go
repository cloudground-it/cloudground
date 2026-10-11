package wp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudground-it/cloudground/connector"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
)

// The connector lands next to the mu-plugins a tree already has, and in a
// tree with no wp-content at all (the usual imported site). Without it a
// site serves cached pages that nothing ever purges.
func TestInstallConnectorKeepsOtherMuPlugins(t *testing.T) {
	own := sitefs.Owner{UID: os.Getuid(), GID: os.Getgid()}
	withPlugins := t.TempDir()
	mu := filepath.Join(withPlugins, "wp-content", "mu-plugins")
	if err := os.MkdirAll(mu, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mu, "theirs.php"), []byte("<?php // theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{withPlugins, t.TempDir()} {
		r, err := sitefs.Open(dir, own)
		if err != nil {
			t.Skip(err)
		}
		err = InstallConnector(r, ".")
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(dir, ConnectorPlugin)); err != nil || string(b) != connector.PHP {
			t.Fatalf("%s: connector %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(mu, "theirs.php")); err != nil {
		t.Fatal("another mu-plugin was removed")
	}
}
