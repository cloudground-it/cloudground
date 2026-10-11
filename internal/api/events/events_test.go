package events

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func recv(t *testing.T, sub *Subscription) Event {
	t.Helper()
	select {
	case ev, ok := <-sub.C():
		if !ok {
			t.Fatal("channel closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

// Every subscriber gets every event, in order, with the wire form marshalled
// once and the same for all.
func TestFanOut(t *testing.T) {
	b := New(16)
	subs := make([]*Subscription, 3)
	for i := range subs {
		subs[i], _, _ = b.Subscribe("", 8)
	}
	b.Publish(SiteChanged, 7, map[string]string{"reason": "updated"})
	b.Publish(SettingsChanged, 0, nil)
	for _, s := range subs {
		a, c := recv(t, s), recv(t, s)
		if a.Type != SiteChanged || a.SiteID != 7 || c.Type != SettingsChanged || c.Seq != a.Seq+1 {
			t.Fatalf("got %+v then %+v", a, c)
		}
		var w map[string]any
		if err := json.Unmarshal(a.JSON, &w); err != nil {
			t.Fatal(err)
		}
		if w["type"] != "site.changed" || w["site_id"] != float64(7) || w["data"].(map[string]any)["reason"] != "updated" {
			t.Fatalf("wire %s", a.JSON)
		}
		var w2 map[string]any
		_ = json.Unmarshal(c.JSON, &w2)
		if _, has := w2["site_id"]; has {
			t.Fatalf("a server-wide event carries a site_id: %s", c.JSON)
		}
	}
}

// A subscriber that does not read is dropped, with its channel closed, and
// the publisher is never held up by it; the others keep receiving.
func TestSlowSubscriberIsDroppedNotWaitedFor(t *testing.T) {
	b := New(64)
	slow, _, _ := b.Subscribe("", 2)
	fast, _, _ := b.Subscribe("", 64)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			b.Publish(TaskProgress, 1, i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a slow subscriber blocked the publisher")
	}
	n := 0
	for range slow.C() {
		n++
	}
	if !slow.Dropped() || n != 2 {
		t.Fatalf("slow: dropped=%v after %d events, want dropped after 2", slow.Dropped(), n)
	}
	for i := 0; i < 10; i++ {
		if ev := recv(t, fast); ev.Data != i {
			t.Fatalf("fast got %v at %d", ev.Data, i)
		}
	}
	if fast.Dropped() {
		t.Fatal("fast subscriber dropped")
	}
	if got := b.Subscribers(); got != 1 {
		t.Fatalf("%d subscribers, want 1", got)
	}
	slow.Close() // after a drop: no panic
	fast.Close()
	fast.Close()
	if got := b.Subscribers(); got != 0 {
		t.Fatalf("%d subscribers after close", got)
	}
}

func TestResume(t *testing.T) {
	b := New(4)
	if _, _, ok := b.Subscribe("bogus", 1); ok {
		t.Fatal("malformed ID resumed")
	}
	for i := 1; i <= 3; i++ {
		b.Publish(TaskProgress, 0, i)
	}
	first := b.LastID()
	b.Publish(TaskProgress, 0, 4)
	b.Publish(TaskProgress, 0, 5)

	// Within the ring: exactly the events after the ID.
	_, replay, ok := b.Subscribe(first, 8)
	if !ok || len(replay) != 2 || replay[0].Data != 4 || replay[1].Data != 5 {
		t.Fatalf("resume from %s: ok=%v %+v", first, ok, replay)
	}
	// The newest ID: nothing to replay, but resumed.
	_, replay, ok = b.Subscribe(b.LastID(), 8)
	if !ok || len(replay) != 0 {
		t.Fatalf("resume from newest: ok=%v %d", ok, len(replay))
	}
	// Fell out of the ring (5 published, ring of 4: seq 1 is gone, so
	// resuming from 0 would miss it).
	if _, _, ok := b.Subscribe(b.epoch+".0", 8); ok {
		t.Fatal("resumed past the ring's start")
	}
	_, replay, ok = b.Subscribe(b.epoch+".1", 8)
	if !ok || len(replay) != 4 || replay[0].Data != 2 {
		t.Fatalf("resume from the ring's start: ok=%v %+v", ok, replay)
	}
	// Another process's ID, or one from the future.
	if _, _, ok := b.Subscribe("deadbeef.3", 8); ok {
		t.Fatal("foreign epoch resumed")
	}
	if _, _, ok := b.Subscribe(fmt.Sprintf("%s.%d", b.epoch, 99), 8); ok {
		t.Fatal("future ID resumed")
	}
}

// Subscribing and publishing at once neither loses nor repeats an event:
// replay and live delivery meet exactly.
func TestSubscribeIsAtomicWithReplay(t *testing.T) {
	b := New(DefaultRing)
	b.Publish(TaskProgress, 0, 0)
	start := b.LastID()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 500; i++ {
			b.Publish(TaskProgress, 0, i)
		}
	}()
	time.Sleep(time.Millisecond)
	sub, replay, ok := b.Subscribe(start, 1000)
	wg.Wait()
	if !ok {
		t.Fatal("not resumed")
	}
	seen := map[uint64]bool{}
	var last uint64 = 1
	for _, ev := range replay {
		if ev.Seq != last+1 {
			t.Fatalf("replay gap at %d", ev.Seq)
		}
		last = ev.Seq
		seen[ev.Seq] = true
	}
	for len(seen) < 500 {
		ev := recv(t, sub)
		if seen[ev.Seq] || ev.Seq != last+1 {
			t.Fatalf("live event %d after %d", ev.Seq, last)
		}
		last = ev.Seq
		seen[ev.Seq] = true
	}
}

func TestNilBusPublishIsANoOp(t *testing.T) {
	var b *Bus
	b.Publish(SiteChanged, 1, nil)
	if b.Subscribers() != 0 {
		t.Fatal("nil bus has subscribers")
	}
}
