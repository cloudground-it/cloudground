package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/state"
)

// What every handler shares (SPEC.md §13).

// respond writes v as the JSON answer with status. The status is sent
// before the body, so a failed write (the client went away) has nobody
// left to tell.
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// respondErr answers {"error": msg}.
func respondErr(w http.ResponseWriter, status int, msg string) {
	respond(w, status, struct {
		Error string `json:"error"`
	}{msg})
}

// declined is a request a service turns down, with what the caller is told.
type declined struct {
	status int
	msg    string
}

func (r *declined) Error() string { return r.msg }

// decline refuses with status and a formatted message.
func decline(status int, format string, args ...any) error {
	return &declined{status: status, msg: fmt.Sprintf(format, args...)}
}

// queueError is a job that could not be enqueued.
type queueError struct{ err error }

func (e queueError) Error() string { return e.err.Error() }
func (e queueError) Unwrap() error { return e.err }

// storeFailure marks an error of the panel's own records, for fail to
// answer 500 with a fixed message.
type storeFailure struct{ err error }

func (e storeFailure) Error() string { return e.err.Error() }
func (e storeFailure) Unwrap() error { return e.err }

// stored marks err (nil stays nil) as a failure of the store.
func stored(err error) error {
	if err == nil {
		return nil
	}
	return storeFailure{err}
}

// fail answers a service's error: a refusal as it says; a job that could
// not be queued through respondJobErr; a failure of the store (marked with
// stored, or one of its sentinels) 500 with a fixed message, its text only
// in the journal; anything else is the agent call's, through errorReply,
// which never trusts the text of an error that is not the agent's own.
func fail(w http.ResponseWriter, err error) {
	var no *declined
	var q queueError
	var st storeFailure
	switch {
	case errors.As(err, &no):
		respondErr(w, no.status, no.msg)
	case errors.As(err, &q):
		respondJobErr(w, q.err)
	case errors.As(err, &st), errors.Is(err, state.ErrNotFound), errors.Is(err, state.ErrNoSecretKey):
		slog.Error("request failed in the panel's store", "err", err)
		respondErr(w, http.StatusInternalServerError, msgInternal)
	default:
		respondAgentErr(w, 0, "", err)
	}
}

// msgInternal answers a failure of the panel's own (its database).
const msgInternal = "the panel could not complete the request"

// internalError answers 500 for a failure of the panel's own with a fixed
// message (msgInternal when msg is ""); the error's text, which can be the
// database's (a table, a column, a constraint), goes to the journal only
// (SPEC.md §1.7).
func internalError(w http.ResponseWriter, what string, err error, msg string) {
	slog.Error("request failed in the panel", "op", what, "err", err)
	if msg == "" {
		msg = msgInternal
	}
	respondErr(w, http.StatusInternalServerError, msg)
}

// runJob is startJob for a service: a job that cannot be enqueued comes
// back as a queueError, which fail answers 503 or 500.
func (s *Server) runJob(req jobReq, fn jobWork) (jobs.Task, error) {
	task, err := s.startJob(req, fn)
	if err != nil {
		return jobs.Task{}, queueError{err}
	}
	return task, nil
}

// answer responds with v, or with err through fail.
func answer[T any](w http.ResponseWriter, status int, v T, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, status, v)
}

// accepted answers a job that was started: 202 and its task.
func accepted(w http.ResponseWriter, task any, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

// logIfErr reports to the journal a bookkeeping write the request survives
// (a status, a record put back).
func (s *Server) logIfErr(err error, what string) {
	if err == nil || s.Log == nil {
		return
	}
	s.Log.Error("state write failed", "op", what, "err", err)
}

// audit appends a row to the audit log. A row that cannot be written is
// reported to the journal, never dropped silently.
func (s *Server) audit(actor, action, subject, detail string) {
	err := s.Store.Audit(actor, action, subject, detail)
	if err != nil && s.Log != nil {
		s.Log.Error("audit log write failed", "actor", actor, "action", action, "subject", subject, "err", err)
	}
}

// redactForAudit stands for a command or a statement in the audit log,
// which read-only accounts can read: its first word, how many words, and a
// short fingerprint of the whole, never the arguments (keys, passwords).
func redactForAudit(text string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%s … (%d words, sha256:%x)", words[0], len(words), sum[:4])
}

// randomToken is n random bytes in URL-safe base64.
func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand: " + err.Error()) // nothing safe to fall back on
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// lockSite takes the site's lock, which serialises every read → agent call
// → write sequence on one site: the agent renders what it is sent, so a
// copy read before another request saved its change would render the old
// configuration. Under the lock a sequence reads the site again
// (freshSite). Never hold two sites' locks at once.
func (s *Server) lockSite(id int64) (unlock func()) {
	v, _ := s.siteLocks.LoadOrStore(id, new(sync.Mutex))
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// freshSite takes the site's lock and reads the site as it is stored now.
// unlock must be called whatever err is.
func (s *Server) freshSite(id int64) (site state.Site, unlock func(), err error) {
	unlock = s.lockSite(id)
	site, err = s.Store.GetSite(id)
	return site, unlock, err
}

// revertField puts previous back into field, unless field no longer holds
// ours: then someone changed it since, and theirs is the newer value.
func revertField[T comparable](field *T, ours, previous T) {
	if *field != ours {
		return
	}
	*field = previous
}
