package agentrpc

import (
	"context"
	"strings"
	"testing"
)

// Past MaxInFlight requests at once, the next is refused at once as
// unavailable (503), without running; once one ends, calls are served again
// (SPEC.md §3.7).
func TestInFlightRequestsAreBounded(t *testing.T) {
	r := newRig(t, Config{MaxInFlight: 2})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := Call(context.Background(), r.client, tWaitRW, None{})
			done <- err
		}()
	}
	waitRan(t, r, 2)
	_, err := Call(context.Background(), r.client, tEcho, echoParams{Text: "x"})
	e := wantCode(t, err, CodeUnavailable)
	if !strings.Contains(e.Message, "busy") {
		t.Errorf("message %q", e.Message)
	}
	if n := r.ran.Load(); n != 2 {
		t.Errorf("the refused call ran (%d)", n)
	}
	close(r.gate)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Call(context.Background(), r.client, tEcho, echoParams{Text: "x"}); err != nil {
		t.Fatalf("after the requests ended: %v", err)
	}
}
