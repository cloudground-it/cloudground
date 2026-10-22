package mailgate

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want Request
		bad  bool
	}{
		{[]string{"-t", "-i"}, Request{FromHeaders: true}, false},
		{[]string{"-t", "-i", "-f", "x@example.com"}, Request{FromHeaders: true}, false},
		{[]string{"-i", "-fx@example.com", "-oi", "a@example.com", "b@example.com"}, Request{To: []string{"a@example.com", "b@example.com"}}, false},
		{[]string{"-i"}, Request{}, true},
		{[]string{"-t", "-X", "/tmp/log"}, Request{}, true},
	} {
		got, err := ParseArgs(tc.args)
		if (err != nil) != tc.bad || (!tc.bad && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("%v: %+v %v", tc.args, got, err)
		}
	}
}

// The client posts the message and the recipients to the socket, and
// reads the gate's refusal.
func TestSend(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mail.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		query string
		body  []byte
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.query = r.URL.RawQuery
		got.body, _ = io.ReadAll(r.Body)
		if strings.Contains(string(got.body), "refuse") {
			w.WriteHeader(http.StatusPreconditionFailed)
			io.WriteString(w, `{"error":"no mail relay is configured"}`)
			return
		}
		io.WriteString(w, `{"sent":1}`)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	msg := []byte("To: a@example.com\nSubject: hi\n\nhello\n")
	if err := Send(context.Background(), sock, Request{To: []string{"b@example.com"}, FromHeaders: true}, msg); err != nil {
		t.Fatal(err)
	}
	if got.query != "t=1&to=b%40example.com" || !bytes.Equal(got.body, msg) {
		t.Fatalf("posted %q %q", got.query, got.body)
	}
	err = Send(context.Background(), sock, Request{FromHeaders: true}, []byte("Subject: refuse\n\n"))
	if err == nil || !strings.Contains(err.Error(), "no mail relay is configured") {
		t.Fatalf("refusal = %v", err)
	}
	var stderr bytes.Buffer
	if code := Main([]string{"-i"}, strings.NewReader(""), &stderr); code != exitUsage {
		t.Fatalf("no recipient: exit %d %s", code, stderr.String())
	}
}
