package version

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testKey struct {
	pub  string
	priv ed25519.PrivateKey
}

func newKey(t *testing.T) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{base64.StdEncoding.EncodeToString(pub), priv}
}

func (k testKey) sign(body string) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, []byte(body))) + "\n")
}

// trust makes this build trust exactly keys for the test.
func trust(t *testing.T, keys ...testKey) {
	t.Helper()
	oldList, oldLink := releaseKeys, UpdatePublicKey
	t.Cleanup(func() { releaseKeys, UpdatePublicKey = oldList, oldLink })
	releaseKeys = nil
	for _, k := range keys {
		releaseKeys = append(releaseKeys, k.pub)
	}
	UpdatePublicKey = ""
}

func sumOf(b string) string {
	s := sha256.Sum256([]byte(b))
	return hex.EncodeToString(s[:])
}

// The release keys in the source are well formed and at most two: the
// current key and, during a rotation, the next one.
func TestReleaseKeys(t *testing.T) {
	if len(releaseKeys) > 2 {
		t.Fatalf("%d release keys: a rotation window holds two, the old key must go", len(releaseKeys))
	}
	old := UpdatePublicKey
	UpdatePublicKey = ""
	defer func() { UpdatePublicKey = old }()
	if _, err := TrustedKeys(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyManifestSignature(t *testing.T) {
	current, next, stranger := newKey(t), newKey(t), newKey(t)
	sum := strings.Repeat("ab", 32)
	good := "version v0.4.0\n" + sum + "  panel-api\n"

	t.Run("good", func(t *testing.T) {
		trust(t, current)
		m, err := VerifyManifest([]byte(good), current.sign(good))
		if err != nil || m.Version != "v0.4.0" || m.Sums["panel-api"] != sum {
			t.Fatalf("a valid manifest was refused or misread: %+v, %v", m, err)
		}
	})
	t.Run("bad", func(t *testing.T) {
		trust(t, current)
		for name, tc := range map[string]struct {
			body string
			sig  []byte
		}{
			"tampered body":    {strings.Replace(good, "v0.4.0", "v0.2.0", 1), current.sign(good)},
			"garbage sig":      {good, []byte("not base64!")},
			"short sig":        {good, []byte(base64.StdEncoding.EncodeToString([]byte("short")))},
			"empty sig":        {good, nil},
			"no version line":  {sum + "  panel-api\n", current.sign(sum + "  panel-api\n")},
			"path as a name":   {"version v0.4.0\n" + sum + "  ../panel-api\n", current.sign("version v0.4.0\n" + sum + "  ../panel-api\n")},
			"dot as a name":    {"version v0.4.0\n" + sum + "  ..\n", current.sign("version v0.4.0\n" + sum + "  ..\n")},
			"short checksum":   {"version v0.4.0\nabcd  panel-api\n", current.sign("version v0.4.0\nabcd  panel-api\n")},
			"dev version line": {"version dev\n" + sum + "  panel-api\n", current.sign("version dev\n" + sum + "  panel-api\n")},
		} {
			if _, err := VerifyManifest([]byte(tc.body), tc.sig); err == nil {
				t.Errorf("%s: manifest accepted", name)
			}
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		trust(t, current)
		if _, err := VerifyManifest([]byte(good), stranger.sign(good)); err == nil {
			t.Fatal("a manifest signed by an untrusted key was accepted")
		}
	})
	t.Run("no key", func(t *testing.T) {
		trust(t)
		if _, err := VerifyManifest([]byte(good), current.sign(good)); err == nil {
			t.Fatal("a build with no key accepted a manifest")
		}
	})
	t.Run("malformed key in the build", func(t *testing.T) {
		trust(t, current)
		releaseKeys = append(releaseKeys, "c2hvcnQ=")
		if _, err := VerifyManifest([]byte(good), current.sign(good)); err == nil {
			t.Fatal("a build with a malformed key verified anything")
		}
	})
	t.Run("rotation", func(t *testing.T) {
		// During the window the build trusts both keys: the last release
		// signed with the current key and the first signed with the next.
		trust(t, current, next)
		for _, k := range []testKey{current, next} {
			if _, err := VerifyManifest([]byte(good), k.sign(good)); err != nil {
				t.Fatalf("a key of the rotation window was refused: %v", err)
			}
		}
		if _, err := VerifyManifest([]byte(good), stranger.sign(good)); err == nil {
			t.Fatal("a third key was accepted during the rotation")
		}
		// After the window, the retired key no longer verifies.
		trust(t, next)
		if _, err := VerifyManifest([]byte(good), current.sign(good)); err == nil {
			t.Fatal("the retired key still verifies")
		}
		if _, err := VerifyManifest([]byte(good), next.sign(good)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("link-time key", func(t *testing.T) {
		trust(t)
		UpdatePublicKey = stranger.pub + "," + next.pub
		if _, err := VerifyManifest([]byte(good), next.sign(good)); err != nil {
			t.Fatalf("a key added at link time was refused: %v", err)
		}
	})
}

// writeRelease lays out a signed release directory.
func writeRelease(t *testing.T, k testKey, tag string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	fmt.Fprintf(&b, "version %s\n", tag)
	for name, data := range files {
		fmt.Fprintf(&b, "%s  %s\n", sumOf(data), name)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, ManifestName), []byte(b.String()), 0o644)
	os.WriteFile(filepath.Join(dir, SignatureName), k.sign(b.String()), 0o644)
	return dir
}

func TestVerifyRelease(t *testing.T) {
	k := newKey(t)
	trust(t, k)
	files := map[string]string{"cgctl": "cgctl bytes", "panel-api": "api bytes", "cloudground-v1.0.0-linux-amd64.tar.gz": "tar"}

	dir := writeRelease(t, k, "v1.0.0", files)
	if m, err := VerifyRelease(dir); err != nil || m.Version != "v1.0.0" {
		t.Fatalf("a good release was refused: %v", err)
	}
	if _, err := VerifyRelease(dir, "cgctl"); err != nil {
		t.Fatalf("a named file was refused: %v", err)
	}
	if _, err := VerifyRelease(dir, "panel-agent"); err == nil {
		t.Error("a file the manifest does not list was accepted")
	}

	os.WriteFile(filepath.Join(dir, "cgctl"), []byte("swapped"), 0o755)
	if _, err := VerifyRelease(dir, "cgctl"); err == nil {
		t.Error("a file that does not match its checksum was accepted")
	}
	if _, err := VerifyRelease(dir, "panel-api"); err != nil {
		t.Errorf("checking only panel-api still looked at cgctl: %v", err)
	}

	dir = writeRelease(t, k, "v1.0.0", files)
	os.Remove(filepath.Join(dir, "panel-api"))
	if _, err := VerifyRelease(dir); err == nil {
		t.Error("a release missing a listed file was accepted")
	}

	dir = writeRelease(t, k, "v1.0.0", files)
	real := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(real, []byte("api bytes"), 0o644)
	os.Remove(filepath.Join(dir, "panel-api"))
	if err := os.Symlink(real, filepath.Join(dir, "panel-api")); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRelease(dir, "panel-api"); err == nil {
		t.Error("a symlink was followed")
	}

	dir = writeRelease(t, newKey(t), "v1.0.0", files)
	if _, err := VerifyRelease(dir); err == nil {
		t.Error("a release signed by another key was accepted")
	}
	os.Remove(filepath.Join(dir, SignatureName))
	if _, err := VerifyRelease(dir); err == nil {
		t.Error("an unsigned release was accepted")
	}
}

func TestReleaseTagsAndOrder(t *testing.T) {
	for tag, want := range map[string]string{
		"v1.2.3": Stable, "v0.0.0": Stable, "v10.20.30": Stable, "v1.2.3-beta.1": Beta, "v1.2.3-beta.12": Beta,
		"1.2.3": "", "v1.2": "", "v01.2.3": "", "v1.2.3-beta": "", "v1.2.3-beta.0": "", "v1.2.3-rc.1": "",
		"v1.2.3-4-gabc": "", "v1.2.3-beta.1-2-gabc": "", "dev": "", "": "", " v1.2.3": "",
	} {
		got, ok := ReleaseTag(tag)
		if got != want || ok != (want != "") {
			t.Errorf("ReleaseTag(%q) = %q %v, want %q", tag, got, ok, want)
		}
	}
	order := []string{"v0.9.0", "v1.0.0-beta.1", "v1.0.0-beta.2", "v1.0.0-beta.10", "v1.0.0", "v1.0.1-beta.1", "v1.0.1", "v1.1.0"}
	for i := range order {
		for j := range order {
			c, ok := Compare(order[i], order[j])
			if want := sign(i - j); !ok || c != want {
				t.Errorf("Compare(%q, %q) = %d %v, want %d", order[i], order[j], c, ok, want)
			}
		}
	}
	if c, _ := Compare("v1.0.0-3-gabc123", "v1.0.0"); c != 0 {
		t.Error("a build described from a tag no longer orders as the tag")
	}
	if !Newer("v1.0.0", "v1.0.0-beta.3") || Newer("v1.0.0-beta.3", "v1.0.0") {
		t.Error("a release does not supersede its betas")
	}
	for _, tc := range []struct {
		channel, tag string
		ok           bool
	}{
		{"stable", "v1.0.0", true}, {"", "v1.0.0", true}, {"beta", "v1.0.0", true}, {"beta", "v1.0.0-beta.1", true},
		{"stable", "v1.0.0-beta.1", false}, {"", "v1.0.0-beta.1", false}, {"nightly", "v1.0.0", false}, {"beta", "v1.0", false},
	} {
		if err := CheckChannel(tc.channel, tc.tag); (err == nil) != tc.ok {
			t.Errorf("CheckChannel(%q, %q) = %v, want ok=%v", tc.channel, tc.tag, err, tc.ok)
		}
	}
	if err := CheckUpdate("v1.0.0", "", "v1.0.0-beta.2"); err != nil {
		t.Errorf("a beta server refused the release its beta led to: %v", err)
	}
	if err := CheckUpdate("v1.0.0-beta.1", "v1.0.0", "v0.9.0"); err == nil {
		t.Error("a beta manifest was accepted for the release that was requested")
	}
}

// feedServer publishes releases the way GitHub does, at the paths Feed reads.
type feedServer struct {
	files map[string][]byte
	tags  []string
}

func (f *feedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/releases.atom" {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom"><id>tag:github.com,2008:/o/r/releases</id>`)
		for _, tag := range f.tags {
			fmt.Fprintf(&b, `<entry><id>tag:github.com,2008:Repository/1/%s</id><link rel="alternate" type="text/html" href="https://github.com/o/r/releases/tag/%s"/><title>%s</title></entry>`, tag, tag, tag)
		}
		b.WriteString(`</feed>`)
		w.Write([]byte(b.String()))
		return
	}
	b, ok := f.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(b)
}

func (f *feedServer) publish(k testKey, tag, signedAs string) {
	body := "version " + signedAs + "\n" + sumOf("x") + "  cgctl\n"
	f.files["/releases/download/"+tag+"/SHA256SUMS"] = []byte(body)
	f.files["/releases/download/"+tag+"/SHA256SUMS.sig"] = k.sign(body)
	f.tags = append(f.tags, tag)
}

func TestFeed(t *testing.T) {
	k := newKey(t)
	trust(t, k)
	fs := &feedServer{files: map[string][]byte{}}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	old := UpdateURL
	UpdateURL = srv.URL
	defer func() { UpdateURL = old }()
	ctx := context.Background()
	feed := Feed{Client: srv.Client()}

	if _, err := feed.Latest(ctx, Stable); err == nil {
		t.Error("an empty feed named a release")
	}
	fs.publish(k, "v1.0.0", "v1.0.0")
	fs.publish(k, "v1.1.0-beta.1", "v1.1.0-beta.1")
	fs.publish(k, "v0.9.0", "v0.9.0")
	fs.tags = append(fs.tags, "nightly", "v2.0") // not release tags: ignored
	if tag, err := feed.Latest(ctx, Stable); err != nil || tag != "v1.0.0" {
		t.Errorf("stable: %q %v, want v1.0.0", tag, err)
	}
	if tag, err := feed.Latest(ctx, Beta); err != nil || tag != "v1.1.0-beta.1" {
		t.Errorf("beta: %q %v, want v1.1.0-beta.1", tag, err)
	}
	if _, err := feed.Latest(ctx, "nightly"); err == nil {
		t.Error("an unknown channel was read")
	}
	if m, err := feed.FetchManifest(ctx, "v1.0.0"); err != nil || m.Version != "v1.0.0" {
		t.Errorf("FetchManifest: %+v %v", m, err)
	}

	// A validly signed old manifest served under a newer release's name.
	fs.publish(k, "v1.2.0", "v0.9.0")
	if _, err := feed.FetchManifest(ctx, "v1.2.0"); err == nil {
		t.Error("a manifest signed for another release was accepted under this tag")
	}
	fs.publish(newKey(t), "v1.3.0", "v1.3.0")
	if _, err := feed.FetchManifest(ctx, "v1.3.0"); err == nil {
		t.Error("a manifest signed by a stranger was accepted")
	}
	if _, err := feed.FetchManifest(ctx, "v9.9.9"); err == nil {
		t.Error("a release that does not exist was fetched")
	}

	UpdateURL = ""
	if _, err := feed.Latest(ctx, Stable); err == nil {
		t.Error("a build without an origin read a feed")
	}
}

// A pinned key narrows the check to that key, and a build with keys of its
// own takes a pin only from among them.
func TestVerifyManifestPinned(t *testing.T) {
	a, b := newKey(t), newKey(t)
	body := "version v1.0.0\n" + sumOf("x") + "  cgctl\n"
	trust(t)
	if m, err := VerifyManifestPinned([]byte(body), a.sign(body), a.pub); err != nil || m.Version != "v1.0.0" {
		t.Fatalf("a pin on a build without keys: %+v, %v", m, err)
	}
	if _, err := VerifyManifestPinned([]byte(body), a.sign(body), ""); !errors.Is(err, ErrNoTrustedKey) {
		t.Fatalf("no pin, no keys: %v", err)
	}
	trust(t, a, b)
	if _, err := VerifyManifestPinned([]byte(body), b.sign(body), a.pub); err == nil {
		t.Fatal("a release signed by another trusted key passed a pin")
	}
	if _, err := VerifyManifestPinned([]byte(body), b.sign(body), ""); err != nil {
		t.Fatalf("no pin, a trusted signer: %v", err)
	}
	trust(t, b)
	if _, err := VerifyManifestPinned([]byte(body), a.sign(body), a.pub); !errors.Is(err, ErrKeyNotTrusted) {
		t.Fatalf("a pin the build does not trust: %v", err)
	}
	if _, err := VerifyManifestPinned([]byte(body), a.sign(body), "short"); err == nil {
		t.Fatal("a malformed pin was taken")
	}
}
