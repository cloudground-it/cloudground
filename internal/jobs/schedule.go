package jobs

import (
	"context"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/state"
)

// TickMode says whether a schedule is part of "run one tick now"
// (SPEC.md §9).
type TickMode int

const (
	// NotInTick: only the scheduler enqueues it.
	NotInTick TickMode = iota
	// InTick: the tick enqueues it whether or not it is due.
	InTick
	// InTickWhenDue: the tick enqueues it only when it is due.
	InTickWhenDue
	// AfterTick: the tick enqueues it once the others have ended.
	AfterTick
)

// Schedule is recurring work: every Every, the job Spec returns.
type Schedule struct {
	Name  string
	Every time.Duration
	// PersistKey keeps the last run in schedule_state; empty keeps it in
	// memory, so the schedule is due at the first wake after a start.
	PersistKey string
	Tick       TickMode
	// Spec is the job to enqueue. Its DedupKey defaults to
	// "schedule:<Name>".
	Spec func() Spec
}

// Scheduler enqueues the jobs of its schedules when they are due.
type Scheduler struct {
	q         *Queue
	store     *state.Store
	schedules []Schedule
	// FirstWake and Every are the scheduler's own rhythm: the first look
	// after Start, then every Every (30 s and 30 s when 0).
	FirstWake, Every time.Duration
	now              func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	stop chan struct{}
	done chan struct{}
}

// NewScheduler returns a scheduler over q's store.
func NewScheduler(q *Queue, schedules []Schedule) *Scheduler {
	return &Scheduler{q: q, store: q.store, schedules: schedules, now: time.Now, last: map[string]time.Time{}}
}

// Start runs the scheduler until Stop.
func (s *Scheduler) Start() {
	first, every := s.FirstWake, s.Every
	if first <= 0 {
		first = 30 * time.Second
	}
	if every <= 0 {
		every = 30 * time.Second
	}
	s.mu.Lock()
	if s.stop != nil {
		s.mu.Unlock()
		return
	}
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	stop, done := s.stop, s.done
	s.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTimer(first)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.wake()
				t.Reset(every)
			}
		}
	}()
}

// Stop ends the scheduler; jobs it enqueued stay with the queue.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	stop, done := s.stop, s.done
	s.stop = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}

func (s *Scheduler) lastRun(sc Schedule) time.Time {
	if sc.PersistKey != "" {
		t, _ := s.store.LastScheduleRun(sc.PersistKey)
		return t
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last[sc.Name]
}

func (s *Scheduler) due(sc Schedule) bool {
	last := s.lastRun(sc)
	return last.IsZero() || s.now().Sub(last) >= sc.Every
}

// enqueue enqueues a schedule's job and records the run.
func (s *Scheduler) enqueue(sc Schedule) (state.Job, error) {
	spec := sc.Spec()
	if spec.DedupKey == "" {
		spec.DedupKey = "schedule:" + sc.Name
	}
	job, err := s.q.Enqueue(spec)
	if err != nil {
		return job, err
	}
	if sc.PersistKey != "" {
		s.q.logIfErr(s.store.MarkScheduleRun(sc.PersistKey), "MarkScheduleRun")
	} else {
		s.mu.Lock()
		s.last[sc.Name] = s.now()
		s.mu.Unlock()
	}
	return job, nil
}

// wake enqueues every schedule that is due.
func (s *Scheduler) wake() {
	for _, sc := range s.schedules {
		if !s.due(sc) {
			continue
		}
		if _, err := s.enqueue(sc); err != nil {
			s.q.log.Warn("scheduler: could not enqueue", "schedule", sc.Name, "err", err)
		}
	}
}

// RunTick runs one tick now (SPEC.md §9) and returns when its jobs have
// ended, or when ctx ends.
func (s *Scheduler) RunTick(ctx context.Context) error {
	_, done, err := s.StartTick(ctx)
	if err != nil {
		return err
	}
	return <-done
}

// StartTick enqueues the tick's first jobs and returns their IDs at once;
// done receives nil once they and the after-tick jobs have ended (or the
// error that stopped the tick, or ctx's).
func (s *Scheduler) StartTick(ctx context.Context) (ids []int64, done <-chan error, err error) {
	enqueue := func(pick func(Schedule) bool) ([]int64, error) {
		var out []int64
		for _, sc := range s.schedules {
			if !pick(sc) {
				continue
			}
			job, err := s.enqueue(sc)
			if err != nil {
				return out, err
			}
			out = append(out, job.ID)
		}
		return out, nil
	}
	wait := func(ids []int64) error {
		for _, id := range ids {
			if _, err := s.q.Wait(ctx, id); err != nil {
				return err
			}
		}
		return nil
	}
	first, err := enqueue(func(sc Schedule) bool {
		return sc.Tick == InTick || (sc.Tick == InTickWhenDue && s.due(sc))
	})
	if err != nil {
		return first, nil, err
	}
	ch := make(chan error, 1)
	go func() {
		if err := wait(first); err != nil {
			ch <- err
			return
		}
		after, err := enqueue(func(sc Schedule) bool { return sc.Tick == AfterTick })
		if err != nil {
			ch <- err
			return
		}
		ch <- wait(after)
	}()
	return first, ch, nil
}
