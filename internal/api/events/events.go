// Package events is the panel's in-process publish/subscribe bus: typed
// events with IDs, a short history for resuming a stream, and subscribers
// that are dropped, never waited for, when they fall behind. See SPEC.md.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Type names an event. The catalogue is in SPEC.md §2.
type Type string

const (
	TaskCreated       Type = "task.created"
	TaskProgress      Type = "task.progress"
	TaskFinished      Type = "task.finished"
	SiteChanged       Type = "site.changed"
	SiteDeleted       Type = "site.deleted"
	DeploymentChanged Type = "deployment.changed"
	BackupFinished    Type = "backup.finished"
	BackupVerified    Type = "backup.verified"
	BackupRemoved     Type = "backup.removed"
	CronChanged       Type = "cron.changed"
	DatabasesChanged  Type = "databases.changed"
	CertChanged       Type = "cert.changed"
	AlertRaised       Type = "alert.raised"
	AlertCleared      Type = "alert.cleared"
	DriftChanged      Type = "drift.changed"
	ServiceChanged    Type = "service.changed"
	SystemStatus      Type = "system.status"
	MetricsSample     Type = "metrics.sample"
	SettingsChanged   Type = "settings.changed"
	UserChanged       Type = "user.changed"

	// Resync and Ping are written by the stream itself, never published.
	Resync Type = "resync"
	Ping   Type = "ping"

	// Internal events steer the API and never reach a browser.
	SessionRevoked Type = "session.revoked"
)

// internalTypes are never delivered to a stream.
var internalTypes = map[Type]bool{SessionRevoked: true}

// Event is one published change.
type Event struct {
	// ID is "<epoch>.<seq>"; seq orders events within one bus.
	ID     string
	Seq    uint64
	Type   Type
	SiteID int64
	At     time.Time
	// Data is the payload as published (for in-process subscribers); JSON
	// is the wire form of the whole event, marshalled once.
	Data any
	JSON []byte
}

// Internal reports whether the event is for the API only.
func (e Event) Internal() bool { return internalTypes[e.Type] }

// wire is the JSON a browser receives.
type wire struct {
	Type   Type      `json:"type"`
	SiteID int64     `json:"site_id,omitempty"`
	At     time.Time `json:"at"`
	Data   any       `json:"data,omitempty"`
}

// Marshal renders an event that is not published (resync, ping) in the
// same wire form as published ones.
func Marshal(typ Type, data any, at time.Time) []byte {
	b, err := json.Marshal(wire{Type: typ, At: at.UTC(), Data: data})
	if err != nil {
		b, _ = json.Marshal(wire{Type: typ, At: at.UTC()})
	}
	return b
}

// DefaultRing is the number of events kept for resuming a stream.
const DefaultRing = 1024

// Bus fans events out to subscribers.
type Bus struct {
	mu    sync.Mutex
	epoch string
	seq   uint64
	ring  []Event // circular, len(ring) == capacity once full
	next  int     // where the next event goes in ring
	full  bool
	subs  map[*Subscription]struct{}
	now   func() time.Time
}

// New returns a bus keeping the last ring events (DefaultRing when <= 0).
func New(ring int) *Bus {
	if ring <= 0 {
		ring = DefaultRing
	}
	var e [4]byte
	_, _ = rand.Read(e[:])
	return &Bus{
		epoch: hex.EncodeToString(e[:]),
		ring:  make([]Event, ring),
		subs:  map[*Subscription]struct{}{},
		now:   time.Now,
	}
}

// Publish sends an event to every subscriber and keeps it in the history.
// It never blocks: a subscriber with a full buffer is dropped (SPEC.md §3.3).
// On a nil bus it does nothing.
func (b *Bus) Publish(typ Type, siteID int64, data any) {
	if b == nil {
		return
	}
	at := b.now().UTC()
	payload, err := json.Marshal(wire{Type: typ, SiteID: siteID, At: at, Data: data})
	if err != nil {
		data = nil
		payload, _ = json.Marshal(wire{Type: typ, SiteID: siteID, At: at})
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	ev := Event{
		ID: b.epoch + "." + strconv.FormatUint(b.seq, 10), Seq: b.seq,
		Type: typ, SiteID: siteID, At: at, Data: data, JSON: payload,
	}
	b.ring[b.next] = ev
	b.next = (b.next + 1) % len(b.ring)
	if b.next == 0 {
		b.full = true
	}
	for sub := range b.subs {
		select {
		case sub.ch <- ev:
		default:
			b.dropLocked(sub)
		}
	}
}

func (b *Bus) dropLocked(sub *Subscription) {
	delete(b.subs, sub)
	sub.dropped.Store(true)
	close(sub.ch)
}

// Subscription receives events until it is closed or dropped.
type Subscription struct {
	bus     *Bus
	ch      chan Event
	dropped atomic.Bool
	once    sync.Once
}

// C delivers the events. It is closed by Close or when the subscriber is
// dropped for falling behind (Dropped then reports true).
func (s *Subscription) C() <-chan Event { return s.ch }

// Dropped reports whether the bus dropped this subscriber because its
// buffer was full.
func (s *Subscription) Dropped() bool { return s.dropped.Load() }

// Close unregisters the subscriber. Idempotent; safe after a drop.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.bus.mu.Lock()
		defer s.bus.mu.Unlock()
		if _, ok := s.bus.subs[s]; ok {
			delete(s.bus.subs, s)
			close(s.ch)
		}
	})
}

// Subscribe registers a subscriber with a buffer of buffer events and
// returns, atomically with the registration, the history after lastID.
// resumed is false when lastID is set but cannot be resumed from (another
// epoch, too old, malformed): the caller must have the client resync.
func (b *Bus) Subscribe(lastID string, buffer int) (sub *Subscription, replay []Event, resumed bool) {
	if buffer <= 0 {
		buffer = 64
	}
	sub = &Subscription{bus: b, ch: make(chan Event, buffer)}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[sub] = struct{}{}
	if lastID == "" {
		return sub, nil, true
	}
	replay, resumed = b.sinceLocked(lastID)
	return sub, replay, resumed
}

// sinceLocked returns the events after lastID, if the ring still holds
// everything after it.
func (b *Bus) sinceLocked(lastID string) ([]Event, bool) {
	epoch, seqStr, ok := strings.Cut(lastID, ".")
	if !ok || epoch != b.epoch {
		return nil, false
	}
	last, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil || last > b.seq {
		return nil, false
	}
	if last == b.seq {
		return nil, true
	}
	held := uint64(b.next)
	if b.full {
		held = uint64(len(b.ring))
	}
	oldest := b.seq - held + 1 // seq of the oldest event in the ring
	if last+1 < oldest {
		return nil, false
	}
	out := make([]Event, 0, b.seq-last)
	for seq := last + 1; seq <= b.seq; seq++ {
		// The newest event sits just before next; walk back from it.
		back := int(b.seq - seq)
		idx := (b.next - 1 - back + 2*len(b.ring)) % len(b.ring)
		out = append(out, b.ring[idx])
	}
	return out, true
}

// Subscribers is the number of registered subscribers.
func (b *Bus) Subscribers() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// LastID is the ID of the newest event ("" before the first).
func (b *Bus) LastID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seq == 0 {
		return ""
	}
	return b.epoch + "." + strconv.FormatUint(b.seq, 10)
}
