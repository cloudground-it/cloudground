package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSFTPTarget(t *testing.T) {
	for _, tc := range []struct {
		repo, user, host string
		port             int
		ok               bool
	}{
		{"sftp:backup@203.0.113.9:/srv/repo", "backup", "203.0.113.9", 22, true},
		{"sftp:host.example.com:repo", "", "host.example.com", 22, true},
		{"sftp://cg-site-4@127.0.0.1:2222//shared/repo", "cg-site-4", "127.0.0.1", 2222, true},
		{"sftp:-oProxyCommand=x@host:/r", "", "", 0, false},
		{"sftp:user@-host:/r", "", "", 0, false},
		{"sftp://u@h:99999//r", "", "", 0, false},
		{"s3:s3.example.com/bucket", "", "", 0, false},
	} {
		u, h, p, err := SFTPTarget(tc.repo)
		if (err == nil) != tc.ok || (tc.ok && (u != tc.user || h != tc.host || p != tc.port)) {
			t.Errorf("%s: %q %q %d %v", tc.repo, u, h, p, err)
		}
	}
}

func TestSFTPArgsWriteTheKeyRootOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backup-ssh")
	r := &ResticCLI{SSHDir: dir}
	args, err := r.sftpArgs(Repo{URL: "sftp://backup@203.0.113.9:2200//srv/repo", Password: "pw", SSHKey: "-----BEGIN KEY-----\nabc\n-----END KEY-----"})
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "-o" || !strings.HasPrefix(args[1], "sftp.command=ssh backup@203.0.113.9 -p 2200 -i "+dir+"/id_ed25519 ") ||
		!strings.Contains(args[1], "UserKnownHostsFile="+dir+"/known_hosts") || !strings.HasSuffix(args[1], "-s sftp") {
		t.Fatalf("args %q", args)
	}
	info, err := os.Stat(filepath.Join(dir, "id_ed25519"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", info, err)
	}
	if d, _ := os.Stat(dir); d.Mode().Perm() != 0o700 {
		t.Fatalf("key folder mode %v", d.Mode())
	}
	if none, err := r.sftpArgs(Repo{URL: "/var/backups/cloudground/r", Password: "pw"}); err != nil || none != nil {
		t.Fatalf("no key: %v %v", none, err)
	}
	if _, err := r.sftpArgs(Repo{URL: "s3:s3.example.com/b", Password: "pw", SSHKey: "k"}); err == nil {
		t.Fatal("a key for a repository that is not sftp")
	}
}
