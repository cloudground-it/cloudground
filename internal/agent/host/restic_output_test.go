package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeSummary = `{"message_type":"summary","files_new":3,"total_bytes_processed":42,"snapshot_id":"0123456789abcdef0123456789abcdef"}`

// A backup that reports its progress 100,000 times keeps only its summary
// (and the other messages), and the summary still decodes.
func TestResticBackupProgressIsNotKept(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n" +
		"i=0\nwhile [ $i -lt 100000 ]; do\n" +
		`  echo '{"message_type":"status","percent_done":0.5,"total_files":10,"files_done":5,"total_bytes":100,"bytes_done":50,"current_files":["/srv/www/a/very/long/path/to/some/file.php"]}'` + "\n" +
		"  i=$((i+1))\ndone\n" +
		`echo '{"message_type":"verbose_status","action":"new","item":"/x"}'` + "\n" +
		"echo '" + fakeSummary + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "restic"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	r := NewRestic(OSExec{})
	out, err := r.Run(context.Background(), Repo{URL: "/repo", Password: "pw"}, "backup", "--json", "--quiet", "/srv/www/a")
	if err != nil {
		t.Fatal(err)
	}
	// 100k status lines are ~16 MB; what is kept is the two other lines.
	if len(out) > 1024 || strings.Contains(string(out), `"status"`) {
		t.Fatalf("kept %d bytes of progress: %.200q", len(out), out)
	}
	sum, err := DecodeBackupSummary(out)
	if err != nil || sum.SnapshotID != "0123456789abcdef0123456789abcdef" || sum.TotalBytesProcessed != 42 {
		t.Fatalf("summary %+v %v", sum, err)
	}
}

func TestResticOutputSplitsLinesAcrossWrites(t *testing.T) {
	o := newResticOutput([]string{"backup", "--stdin", "--json"}, 1<<10)
	in := `{"message_type":"status","percent_done":0}` + "\n" + `{"message_type":"error","item":"/a"}` + "\n" +
		`{"message_type":"status","percent_done":1}` + "\n" + fakeSummary // no final newline
	for i := 0; i < len(in); i += 7 {
		end := min(i+7, len(in))
		if n, err := o.Write([]byte(in[i:end])); n != end-i || err != nil {
			t.Fatal(n, err)
		}
	}
	want := `{"message_type":"error","item":"/a"}` + "\n" + fakeSummary
	if got := string(o.Bytes()); got != want || o.Err() != nil {
		t.Fatalf("kept %q %v", got, o.Err())
	}
}

// Another command's output is kept whole, up to the limit; past it the call
// fails rather than hand back a truncated list as if it were the whole.
func TestResticOutputOverLimitFails(t *testing.T) {
	o := newResticOutput([]string{"snapshots", "--json"}, 16)
	o.Write([]byte(`[{"message_type":"status"}]`))
	if !errors.Is(o.Err(), ErrOutputTooLarge) || len(o.Bytes()) != 0 {
		t.Fatalf("%q %v", o.Bytes(), o.Err())
	}
	small := newResticOutput([]string{"snapshots", "--json"}, 64)
	small.Write([]byte(`[{"message_type":"status"}]`))
	if string(small.Bytes()) != `[{"message_type":"status"}]` || small.Err() != nil {
		t.Fatalf("%q %v", small.Bytes(), small.Err())
	}
	// A status line is dropped for a backup only.
	line := newResticOutput([]string{"backup", "--json"}, 16)
	line.Write([]byte(`{"message_type":"error","error":"x"}` + "\n"))
	if !errors.Is(line.Err(), ErrOutputTooLarge) {
		t.Fatal("an error line past the limit must fail the backup")
	}
}

func TestResticRunReportsOutputTooLarge(t *testing.T) {
	big := strings.Repeat("x", ResticOutputLimit+1)
	fx := &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Stdout: big} }}
	_, err := NewRestic(fx).Run(context.Background(), Repo{URL: "/r", Password: "pw"}, "snapshots", "--json")
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("err %v", err)
	}
	fx = &FakeExec{Respond: func(FakeCall) FakeReply { return FakeReply{Stdout: big, Code: 11} }}
	_, err = NewRestic(fx).Run(context.Background(), Repo{URL: "/r", Password: "pw"}, "snapshots", "--json")
	if !errors.Is(err, ErrOutputTooLarge) || !errors.Is(err, ErrRepoLocked) {
		t.Fatalf("err %v", err)
	}
}
