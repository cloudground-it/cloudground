// Package mailgate is the client of the panel's mail gate (internal/api
// SPEC §14.2). PHP's mail() runs it as `panel-agent sendmail -t -i`, as
// the site's user inside the site's sandbox: it holds no secret and no
// privilege. It reads the message on its standard input and posts it to
// the gate's Unix socket, where the panel knows the site by the kernel's
// account of the connecting process, not by anything said here.
package mailgate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Name is the panel-agent subcommand that runs the client.
const Name = "sendmail"

// Socket is the panel's mail gate.
const Socket = "/run/cloudground-api/mail.sock"

// MaxMessage bounds one message, attachments included.
const MaxMessage = 10 << 20

// timeout bounds the whole hand-over: the panel answers once the relay
// has, and a relay can be slow.
const timeout = 90 * time.Second

// Exit statuses: sendmail's success and EX_TEMPFAIL, which mail() reads
// as a failure.
const (
	exitOK       = 0
	exitTempFail = 75
	exitUsage    = 64
)

// Request is what the command line asks: the recipients it names, and
// whether the message's own headers name more (-t).
type Request struct {
	To          []string
	FromHeaders bool
}

// ParseArgs reads sendmail's command line as PHP and its libraries use
// it: -t, -i, -oi, -f <sender> (the relay's sender is used instead), -F
// <name>, -o<x> and -B<x> (ignored), and recipients.
func ParseArgs(args []string) (Request, error) {
	var r Request
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-t":
			r.FromHeaders = true
		case a == "-i" || a == "-oi" || a == "--":
		case a == "-f" || a == "-F" || a == "-r":
			i++ // its value
		case strings.HasPrefix(a, "-f") || strings.HasPrefix(a, "-F") || strings.HasPrefix(a, "-r") ||
			strings.HasPrefix(a, "-o") || strings.HasPrefix(a, "-B"):
		case strings.HasPrefix(a, "-"):
			return r, fmt.Errorf("unknown option %s", a)
		default:
			r.To = append(r.To, a)
		}
	}
	if !r.FromHeaders && len(r.To) == 0 {
		return r, fmt.Errorf("no recipient: name one, or use -t")
	}
	return r, nil
}

// Main is `panel-agent sendmail [options] [recipients]`: the message on
// stdin, an error on stderr. It returns the exit status.
func Main(args []string, stdin io.Reader, stderr io.Writer) int {
	req, err := ParseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, "sendmail:", err)
		return exitUsage
	}
	msg, err := io.ReadAll(io.LimitReader(stdin, MaxMessage+1))
	if err != nil {
		fmt.Fprintln(stderr, "sendmail: reading the message:", err)
		return exitTempFail
	}
	if len(msg) > MaxMessage {
		fmt.Fprintf(stderr, "sendmail: the message is larger than %d MB\n", MaxMessage>>20)
		return exitTempFail
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := Send(ctx, Socket, req, msg); err != nil {
		fmt.Fprintln(stderr, "sendmail:", err)
		return exitTempFail
	}
	return exitOK
}

// Send posts msg to the gate at socket.
func Send(ctx context.Context, socket string, req Request, msg []byte) error {
	q := url.Values{}
	for _, to := range req.To {
		q.Add("to", to)
	}
	if req.FromHeaders {
		q.Set("t", "1")
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://mail-gate/send?"+q.Encode(), bytes.NewReader(msg))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "message/rfc822")
	resp, err := client.Do(hr)
	if err != nil {
		return fmt.Errorf("the panel's mail gate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var e struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		e.Error = strings.TrimSpace(string(body))
	}
	return fmt.Errorf("refused (HTTP %d): %s", resp.StatusCode, e.Error)
}
