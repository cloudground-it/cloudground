package panel

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
	"sync"
	"testing"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/version"
)

type release struct {
	t     *testing.T
	priv  ed25519.PrivateKey
	files map[string][]byte
	mu    sync.Mutex
	srv   *httptest.Server
	elf   []byte
	// servedAs publishes the next release under another tag's name.
	servedAs string
}

// newRelease serves a release over TLS and points the build's update origin
// and key at it, running as v0.3.0.
func newRelease(t *testing.T) *release {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable() // an ELF for this machine, as a release binary is
	if err != nil {
		t.Fatal(err)
	}
	elf, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	r := &release{t: t, priv: priv, elf: elf}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		b, ok := r.files[strings.TrimPrefix(req.URL.Path, "/")]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(r.srv.Close)
	oldURL, oldKey, oldVer := version.UpdateURL, version.UpdatePublicKey, version.Version
	t.Cleanup(func() { version.UpdateURL, version.UpdatePublicKey, version.Version = oldURL, oldKey, oldVer })
	version.UpdateURL = r.srv.URL
	version.UpdatePublicKey = base64.StdEncoding.EncodeToString(pub)
	version.Version = "v0.3.0"
	return r
}

// publish serves tag with these files (name -> bytes), the manifest signed
// by key (nil: the release key). sums overrides a file's signed checksum.
func (r *release) publish(tag string, files map[string][]byte, key ed25519.PrivateKey, sums map[string]string) {
	if key == nil {
		key = r.priv
	}
	var b strings.Builder
	fmt.Fprintf(&b, "version %s\n", tag)
	for name, data := range files {
		s := sha256.Sum256(data)
		sum := hex.EncodeToString(s[:])
		if o, ok := sums[name]; ok {
			sum = o
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
	}
	body := []byte(b.String())
	path := tag
	if r.servedAs != "" {
		path = r.servedAs
	}
	dir := "releases/download/" + path + "/"
	out := map[string][]byte{
		dir + "SHA256SUMS":     body,
		dir + "SHA256SUMS.sig": []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))),
		"releases.atom": []byte(`<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>tag:example,2008:Repository/1/` + path +
			`</id><link rel="alternate" href="https://example.com/o/r/releases/tag/` + path + `"/></entry></feed>`),
	}
	for k, v := range files {
		out[dir+k] = v
	}
	r.mu.Lock()
	r.files = out
	r.mu.Unlock()
}

func (r *release) binaries() map[string][]byte {
	return map[string][]byte{"panel-api": r.elf, "panel-agent": r.elf, "cgctl": r.elf}
}

type updFixture struct {
	*Ops
	sd *host.FakeSystemd
}

func newUpdFixture(t *testing.T, r *release) *updFixture {
	f := newCertFixture(t)
	f.UpdateClient = r.srv.Client()
	for _, b := range Binaries {
		os.WriteFile(filepath.Join(f.Paths.BinDir, b), []byte("old "+b), 0o755)
	}
	os.WriteFile(filepath.Join(f.Paths.SystemdDir, AgentUnit), []byte("[Service]\nold\n"), 0o644)
	os.WriteFile(filepath.Join(f.Paths.SystemdDir, APIUnit), []byte("[Service]\nsame\n"), 0o644)
	return &updFixture{Ops: f.Ops, sd: f.sd}
}

// untouched reports whether the live binaries are still the old ones, with
// no backup, staged file or guard left behind.
func (f *updFixture) untouched(t *testing.T) {
	t.Helper()
	f.oldBinaries(t)
	if _, ok := f.sd.TransientArgv(GuardUnit); ok {
		t.Fatal("the guard was started")
	}
}

// oldBinaries checks the previous binaries are live, with no backup or
// staged file left.
func (f *updFixture) oldBinaries(t *testing.T) {
	t.Helper()
	for _, b := range Binaries {
		live := filepath.Join(f.Paths.BinDir, b)
		if got, _ := os.ReadFile(live); string(got) != "old "+b {
			t.Fatalf("%s was replaced", b)
		}
		for _, p := range []string{live + ".bak", stagingName(live)} {
			if _, err := os.Lstat(p); err == nil {
				t.Fatalf("%s left behind", p)
			}
		}
	}
}

func codeOf(err error) agentrpc.Code {
	var e *agentrpc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return agentrpc.CodeInternal
}

// Nothing live changes unless the release is signed by the build's key, is
// newer than what runs and than anything installed before, is the one
// asked for, and every byte matches the signed manifest.
func TestSelfUpdateRefusals(t *testing.T) {
	r := newRelease(t)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	notELF := map[string][]byte{"panel-api": []byte("<html>"), "panel-agent": r.elf, "cgctl": r.elf}
	missing := map[string][]byte{"panel-api": r.elf, "panel-agent": r.elf}
	for name, tc := range map[string]struct {
		tag       string
		files     map[string][]byte
		key       ed25519.PrivateKey
		sums      map[string]string
		requested string
		servedAs  string
		channel   string
		highest   string
		want      agentrpc.Code
	}{
		"signed by another key":           {tag: "v0.4.0", key: otherKey, want: agentrpc.CodeInvalid},
		"older release":                   {tag: "v0.2.9", want: agentrpc.CodeConflict},
		"same release":                    {tag: "v0.3.0", want: agentrpc.CodeConflict},
		"other than requested":            {tag: "v0.4.0", requested: "v0.5.0", servedAs: "v0.5.0", want: agentrpc.CodeConflict},
		"other than the feed named":       {tag: "v0.4.0", servedAs: "v0.5.0", want: agentrpc.CodeConflict},
		"beta on the stable channel":      {tag: "v0.4.0-beta.1", requested: "v0.4.0-beta.1", want: agentrpc.CodeConflict},
		"beta named to a stable server":   {tag: "v0.4.0-beta.1", servedAs: "v0.4.0", channel: "stable", want: agentrpc.CodeConflict},
		"unknown channel":                 {tag: "v0.4.0", channel: "nightly", want: agentrpc.CodeInvalid},
		"not a release tag":               {tag: "v0.4.0", requested: "v0.4", want: agentrpc.CodeInvalid},
		"below the newest installed here": {tag: "v0.4.0", highest: "v0.4.1", want: agentrpc.CodeConflict},
		"bytes differ from the manifest":  {tag: "v0.4.0", sums: map[string]string{"cgctl": strings.Repeat("ab", 32)}, want: agentrpc.CodeInvalid},
		"not an executable":               {tag: "v0.4.0", files: notELF, want: agentrpc.CodeInvalid},
		"a binary missing":                {tag: "v0.4.0", files: missing, want: agentrpc.CodeInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			files := tc.files
			if files == nil {
				files = r.binaries()
			}
			r.servedAs = tc.servedAs
			r.publish(tc.tag, files, tc.key, tc.sums)
			r.servedAs = ""
			f := newUpdFixture(t, r)
			if tc.highest != "" {
				os.WriteFile(filepath.Join(f.Paths.StateDir(), highestName), []byte(tc.highest+"\n"), 0o600)
			}
			_, err := f.SelfUpdate(context.Background(), rpc.SelfUpdateParams{Version: tc.requested, Channel: tc.channel})
			if err == nil || codeOf(err) != tc.want {
				t.Fatalf("err = %v (%s), want %s", err, codeOf(err), tc.want)
			}
			f.untouched(t)
		})
	}
	t.Run("no key in the build", func(t *testing.T) {
		r.publish("v0.4.0", r.binaries(), nil, nil)
		f := newUpdFixture(t, r)
		key := version.UpdatePublicKey
		version.UpdatePublicKey = ""
		defer func() { version.UpdatePublicKey = key }()
		if _, err := f.SelfUpdate(context.Background(), rpc.SelfUpdateParams{}); codeOf(err) != agentrpc.CodeInvalid {
			t.Fatalf("err = %v", err)
		}
		f.untouched(t)
	})
}

func TestSelfUpdateSwapsAndHandsOverToTheGuard(t *testing.T) {
	r := newRelease(t)
	files := r.binaries()
	files[AgentUnit] = []byte("[Service]\nnew\n")
	files[APIUnit] = []byte("[Service]\nsame\n")
	files[APISocketUnit] = []byte("[Socket]\nnew\n")
	r.publish("v0.4.0", files, nil, nil)
	f := newUpdFixture(t, r)

	res, err := f.SelfUpdate(context.Background(), rpc.SelfUpdateParams{Version: "v0.4.0"})
	if err != nil {
		t.Fatal(err)
	}
	if res.UpdatedTo != "v0.4.0" || !res.Restarted {
		t.Fatalf("result %+v", res)
	}
	for _, b := range Binaries {
		live := filepath.Join(f.Paths.BinDir, b)
		if got, _ := os.ReadFile(live); string(got) != string(r.elf) {
			t.Fatalf("%s not installed", b)
		}
		if fi, _ := os.Stat(live); fi.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode %v", b, fi.Mode().Perm())
		}
		if got, _ := os.ReadFile(live + ".bak"); string(got) != "old "+b {
			t.Fatalf("%s.bak is not the previous binary", b)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(f.Paths.SystemdDir, AgentUnit)); string(b) != "[Service]\nnew\n" {
		t.Fatal("the changed unit was not installed")
	}
	argv, ok := f.sd.TransientArgv(GuardUnit)
	planPath := filepath.Join(f.Paths.WorkDir, planName)
	if !ok || strings.Join(argv, " ") != filepath.Join(f.Paths.BinDir, "panel-agent.bak")+" update-guard "+planPath {
		t.Fatalf("guard argv %v", argv)
	}
	plan, err := ReadPlan(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Version != "v0.4.0" || plan.Previous != "v0.3.0" || len(plan.Backed) != 3 ||
		strings.Join(plan.Units, ",") != AgentUnit || strings.Join(plan.FreshUnits, ",") != APISocketUnit || !plan.SocketChanged {
		t.Fatalf("plan %+v", plan)
	}
	// The floor rises only once the guard has seen the release healthy.
	if HighestInstalled(f.Paths.StateDir()) != "" {
		t.Fatal("the release was recorded as installed before the guard checked it")
	}
	// The guard is still running: a second update is refused.
	if _, err := f.SelfUpdate(context.Background(), rpc.SelfUpdateParams{}); codeOf(err) != agentrpc.CodeConflict {
		t.Fatalf("second update: %v", err)
	}
	// The guard, run on that plan, rolls everything back.
	if err := plan.restoreFiles(); err != nil {
		t.Fatal(err)
	}
	f.oldBinaries(t)
	if b, _ := os.ReadFile(filepath.Join(f.Paths.SystemdDir, AgentUnit)); string(b) != "[Service]\nold\n" {
		t.Fatal("the unit was not restored")
	}
	if _, err := os.Lstat(filepath.Join(f.Paths.SystemdDir, APISocketUnit)); !os.IsNotExist(err) {
		t.Fatal("a unit that did not exist before survived the rollback")
	}
}

// If the guard cannot be started, the swap is undone: nothing was restarted.
func TestSelfUpdateUndoesTheSwapWhenTheGuardCannotStart(t *testing.T) {
	r := newRelease(t)
	r.publish("v0.4.0", r.binaries(), nil, nil)
	f := newUpdFixture(t, r)
	f.sd.FailJob[GuardUnit] = "failed"
	if _, err := f.SelfUpdate(context.Background(), rpc.SelfUpdateParams{}); err == nil {
		t.Fatal("accepted")
	}
	for _, b := range Binaries {
		if got, _ := os.ReadFile(filepath.Join(f.Paths.BinDir, b)); string(got) != "old "+b {
			t.Fatalf("%s was not restored", b)
		}
	}
	for _, op := range f.sd.Ops() {
		if strings.HasPrefix(op, "restart") {
			t.Fatalf("restarted something: %v", f.sd.Ops())
		}
	}
}
