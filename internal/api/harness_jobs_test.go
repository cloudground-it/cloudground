package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/state"
)

// jobWait bounds every wait on the queue in these tests.
const jobWait = 5 * time.Second

// waitForTasks returns once no job is queued or running, maintenance jobs
// included, and fails the test after jobWait.
func waitForTasks(t *testing.T, s *Server) {
	t.Helper()
	for end := time.Now().Add(jobWait); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		active, err := s.Store.ActiveJobs()
		if err != nil {
			t.Fatal(err)
		}
		if len(active) == 0 {
			return
		}
	}
	t.Fatal("tasks did not finish")
}

// runJobNow queues a job of a resumable kind, waits for it to end and for
// every job it queued in turn, and returns it as it ended.
func runJobNow(t *testing.T, s *Server, spec jobs.Spec) state.Job {
	t.Helper()
	queued, err := s.Jobs.Enqueue(spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), jobWait)
	defer cancel()
	done, err := s.Jobs.Wait(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitForTasks(t, s)
	return done
}

// holdLocks runs a job that takes keys and keeps them until release is
// called (more than once is fine). It returns once that job is running.
func holdLocks(t *testing.T, s *Server, keys ...string) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	task, err := s.startJob(jobReq{kind: "cache.warm", locks: keys}, func(context.Context, state.Site, func(int, string)) error {
		<-gate
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(jobWait); ; time.Sleep(5 * time.Millisecond) {
		if j, _ := s.Store.GetJob(task.ID); j.Status == state.JobRunning {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the lock-holding job did not start")
		}
	}
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}
