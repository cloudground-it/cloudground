package backuprepo

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func resolver(m map[string][]string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, h string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range m[h] {
			out = append(out, netip.MustParseAddr(a))
		}
		if out == nil {
			return nil, errors.New("no such host")
		}
		return out, nil
	}
}

func TestValidate(t *testing.T) {
	dns := resolver(map[string][]string{
		"s3.example.com":   {"203.0.113.10"},
		"rest.example.com": {"198.51.100.7"},
		"inside.example":   {"10.1.2.3"},
		"mixed.example":    {"203.0.113.10", "127.0.0.1"},
		"meta.example":     {"169.254.169.254"},
	})
	o := Options{Resolve: dns, Rclone: func() bool { return true }}
	ok := []string{
		"/var/backups/cloudground/repo",
		"/var/backups/cloudground/e2e-r10-123",
		"/mnt/usb/restic",
		"s3:s3.example.com/bucket/path",
		"s3:https://s3.example.com/bucket",
		"s3:203.0.113.10:9000/bucket",
		"rest:https://user:pw@rest.example.com:8000/repo/",
		"b2:my-bucket:path/x",
		"sftp:backup@host.example.com:/srv/restic",
		"sftp://backup@host.example.com:2222//srv/restic",
		"rclone:remote:path/to/repo",
	}
	for _, r := range ok {
		if err := Validate(context.Background(), r, o); err != nil {
			t.Errorf("%q refused: %v", r, err)
		}
	}
	bad := []string{
		"", "relative/path", "/var/backups/other", "/var/backups/cloudground", "/var/backups/cloudground/", "/etc/cloudground",
		"/var/backups/cloudground/../x", "/var/backups/cloudground/a b", "/mnt/", "/srv/sites/a.example.com",
		"rest:http://rest.example.com/repo", "rest:https://127.0.0.1:8000/", "rest:https://[::1]/", "rest:https://localhost/",
		"s3:inside.example/bucket", "s3:https://mixed.example/bucket", "s3:http://169.254.169.254/latest", "s3:meta.example/b",
		"s3:192.168.1.10/bucket", "s3:100.64.1.1/b", "s3:nowhere.example/b",
		"sftp:-oProxyCommand=x@host:/r", "sftp:user@-host:/r", "sftp://-oProxyCommand=x@host//r", "sftp:us er@host:/r",
		"sftp:user\x00@host:/r", "s3:s3.example.com/bucket\n", "swift:container", "gs:bucket:/", "azure:c:/",
		"b2:", "rclone:", "sftp://u:pw@host//r",
	}
	for _, r := range bad {
		err := Validate(context.Background(), r, o)
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%q accepted (%v)", r, err)
		}
	}
}

// The administrator's flag lets a private endpoint through; rclone needs
// rclone installed.
func TestValidateOptions(t *testing.T) {
	o := Options{AllowPrivate: true, Resolve: resolver(nil)}
	for _, r := range []string{"rest:https://127.0.0.1:8000/", "s3:http://minio.lan:9000/b", "s3:10.0.0.5/b"} {
		if err := Validate(context.Background(), r, o); err != nil {
			t.Errorf("%q with AllowPrivate: %v", r, err)
		}
	}
	if err := Validate(context.Background(), "rest:http://127.0.0.1/", o); err == nil {
		t.Error("plain http rest: accepted with AllowPrivate")
	}
	if err := Validate(context.Background(), "rclone:remote:x", Options{}); err == nil {
		t.Error("rclone: accepted without rclone")
	}
}
