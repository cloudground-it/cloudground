// Package agentrpc is the protocol between panel-api and panel-agent: HTTP/1.1
// over the agent's Unix socket, peers authenticated by uid and token, one
// method table for client and server, a context per request with a per-method
// deadline, and typed errors whose message is the only part a user sees.
// SPEC.md is the specification.
package agentrpc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Path prefix of every method: POST /rpc/<Method>.
const pathPrefix = "/rpc/"

// maxBody bounds a request: a 16 MiB upload is about 22 MiB as base64.
const maxBody = 32 << 20

// Locker takes the agent's per-site locks for methods whose table row asks
// for one (SPEC.md §3.6).
type Locker interface {
	// LockSite takes the site's lock and returns its release.
	LockSite(domain string) func()
	// LockLiveSite takes the lock and fails, holding nothing, when the site
	// no longer exists.
	LockLiveSite(s state.Site) (func(), error)
}

// Config sets up a Server.
type Config struct {
	// Token is the shared secret every request carries.
	Token string
	// SitesRoot is where sites live; every site in a request is confined to
	// SitesRoot/<domain>.
	SitesRoot string
	// AllowedUIDs are the peers served (SPEC.md §2). Empty serves nobody.
	AllowedUIDs []uint32
	// PeerUID reads a connection's peer uid. Nil means the kernel's
	// (SO_PEERCRED); tests substitute it.
	PeerUID func(net.Conn) (uint32, error)
	// Locker is required when any registered method locks a site.
	Locker Locker
	// Registry, when set, ties every site of a request to the agent's own
	// record of the sites it provisioned (SPEC.md §3.5).
	Registry SiteRegistry
	// MaxInFlight bounds the requests served at once (SPEC.md §3.7);
	// 0 is DefaultMaxInFlight.
	MaxInFlight int
	Log         *slog.Logger
}

// SiteRegistry is the agent's record of its sites (SPEC.md §3.5).
type SiteRegistry interface {
	// Check is called for each validated site of a request, before the
	// lock; an error refuses the request.
	Check(method string, s state.Site) error
	// Done is called after the handler returned, with the request's
	// validated sites and the handler's error.
	Done(method string, sites []state.Site, err error)
}

// Server dispatches authenticated requests to the handlers of the method
// table.
type Server struct {
	cfg      Config
	log      *slog.Logger
	mu       sync.RWMutex
	handlers map[string]*entry
	hs       *http.Server // set by Serve, for Shutdown
	slots    chan struct{}
}

type entry struct {
	spec Spec
	run  func(ctx context.Context, raw []byte) (any, error)
}

// NewServer creates a Server.
func NewServer(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.PeerUID == nil {
		cfg.PeerUID = kernelPeerUID
	}
	return &Server{cfg: cfg, log: cfg.Log, handlers: map[string]*entry{}, slots: newSlots(cfg.MaxInFlight)}
}

// Handle registers fn for m. Sites in the parameters are validated and
// replaced by their validated value, and the site lock the table asks for is
// held, before fn runs.
func Handle[P, R any](s *Server, m Method[P, R], fn func(context.Context, P) (R, error)) {
	spec := m.Spec()
	if spec.Lock != LockNone && s.cfg.Locker == nil {
		panic("agentrpc: " + spec.Name + " locks a site but the server has no Locker")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.handlers[spec.Name]; dup {
		panic("agentrpc: handler registered twice: " + spec.Name)
	}
	s.handlers[spec.Name] = &entry{spec: spec, run: func(ctx context.Context, raw []byte) (any, error) {
		var p P
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, Wrap(CodeInvalid, "bad parameters", err)
			}
		}
		var first *state.Site
		var sites []state.Site
		if spec.Sites {
			for _, sp := range any(&p).(SiteCarrier).Sites() {
				if sp == nil {
					continue
				}
				v, err := site.Validate(s.cfg.SitesRoot, *sp)
				if err != nil {
					return nil, siteError(err)
				}
				*sp = v.Site()
				if first == nil {
					first = sp
				}
				sites = append(sites, *sp)
			}
			if s.cfg.Registry != nil {
				for _, st := range sites {
					if err := s.cfg.Registry.Check(spec.Name, st); err != nil {
						return nil, err
					}
				}
			}
		}
		if v, ok := any(&p).(Validator); ok {
			if err := v.Validate(); err != nil {
				return nil, &Error{Code: CodeInvalid, Message: err.Error()}
			}
		}
		unlock, err := s.lock(ctx, spec, first)
		if err != nil {
			return nil, err
		}
		defer unlock()
		r, err := fn(ctx, p)
		if s.cfg.Registry != nil && len(sites) > 0 {
			s.cfg.Registry.Done(spec.Name, sites, err)
		}
		return r, err
	}}
}

func siteError(err error) error {
	var se *site.Error
	if errors.As(err, &se) {
		return &Error{Code: CodeInvalid, Message: se.Msg, Detail: se.Detail}
	}
	return Wrap(CodeInvalid, "invalid site", err)
}

func (s *Server) lock(ctx context.Context, spec Spec, on *state.Site) (func(), error) {
	if spec.Lock == LockNone {
		return func() {}, nil
	}
	if on == nil {
		return nil, &Error{Code: CodeInvalid, Message: "no site given", Detail: spec.Name + " locks a site but the request carries none"}
	}
	var unlock func()
	switch spec.Lock {
	case LockSite:
		unlock = s.cfg.Locker.LockSite(on.Domain)
	case LockLiveSite:
		var err error
		if unlock, err = s.cfg.Locker.LockLiveSite(*on); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown lock kind %d", spec.Lock)
	}
	// A mutex wait cannot be cancelled: a caller that gave up meanwhile must
	// not have its change run anyway.
	if err := ctx.Err(); err != nil {
		unlock()
		return nil, ctxError(spec, ctx)
	}
	return unlock, nil
}

func ctxError(spec Spec, ctx context.Context) *Error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &Error{Code: CodeTimeout, Message: spec.Fail + ": the agent's time limit ran out",
			Detail: fmt.Sprintf("%s exceeded its %s deadline", spec.Name, spec.Timeout)}
	}
	return &Error{Code: CodeCanceled, Message: spec.Fail + ": canceled", Detail: spec.Name + ": the caller went away"}
}

// Missing lists the table's methods that have no handler.
func (s *Server) Missing() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, spec := range Specs() {
		if _, ok := s.handlers[spec.Name]; !ok {
			out = append(out, spec.Name)
		}
	}
	return out
}

type outcome struct {
	v   any
	err error
}

// Invoke runs one method the way a request does, minus HTTP and
// authentication: decode, validate, lock, handler, under the method's
// deadline. Tests in other packages use it to go through the same pipeline.
func (s *Server) Invoke(ctx context.Context, name string, raw []byte) (any, *Error) {
	s.mu.RLock()
	e, ok := s.handlers[name]
	s.mu.RUnlock()
	if !ok {
		return nil, &Error{Code: CodeNotFound, Message: "unknown agent method", Detail: name}
	}
	ctx, cancel := context.WithTimeout(ctx, e.spec.Timeout)
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("agentrpc: handler panicked", "method", name, "panic", r, "stack", string(debug.Stack()))
				done <- outcome{err: fmt.Errorf("panic: %v", r)}
			}
		}()
		v, err := e.run(ctx, raw)
		done <- outcome{v, err}
	}()
	var o outcome
	select {
	case o = <-done:
	case <-ctx.Done():
		select {
		case o = <-done: // finished at the deadline: its answer counts
		default:
			go func() {
				late := <-done
				s.log.Warn("agentrpc: handler finished after its caller stopped waiting", "method", name, "err", RedactCredentials(fmt.Sprint(late.err)))
			}()
			ee := ctxError(e.spec, ctx)
			s.log.Error("agentrpc: call abandoned", "method", name, "code", ee.Code, "detail", ee.Detail)
			return nil, ee
		}
	}
	if o.err != nil {
		var ee *Error
		if AsError(o.err) == nil && ctx.Err() != nil {
			ee = ctxError(e.spec, ctx)
			ee.Detail += ": " + RedactCredentials(o.err.Error())
		} else {
			ee = toError(e.spec, o.err)
		}
		s.log.Error("agentrpc: handler failed", "method", name, "code", ee.Code, "message", ee.Message, "detail", ee.Detail)
		return nil, ee
	}
	return o.v, nil
}

// ServeHTTP answers POST /rpc/<Method>.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.cfg.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
		s.log.Warn("agentrpc: rejected a request with a bad token")
		writeJSON(w, http.StatusUnauthorized, &Error{Code: CodeUnauthorized, Message: "unauthorized"})
		return
	}
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, pathPrefix) {
		writeJSON(w, http.StatusNotFound, &Error{Code: CodeNotFound, Message: "unknown agent method", Detail: r.Method + " " + r.URL.Path})
		return
	}
	release, ok := s.take(w, r)
	if !ok {
		return
	}
	defer release()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, &Error{Code: CodeInvalid, Message: "request too large", Detail: err.Error()})
		return
	}
	v, e := s.Invoke(r.Context(), strings.TrimPrefix(r.URL.Path, pathPrefix), raw)
	if e != nil {
		writeJSON(w, socketStatus(e.Code), e)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body, _ = json.Marshal(&Error{Code: CodeInternal, Message: "The agent could not encode its answer", Detail: err.Error()})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// ListenAndServe binds the Unix socket at path (replacing a stale one),
// gives it mode 0660 and, when group is set, that group, and serves.
func (s *Server) ListenAndServe(path, group string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	if group != "" {
		if grp, err := user.LookupGroup(group); err == nil {
			if gid, err := strconv.Atoi(grp.Gid); err == nil {
				if err := os.Chown(path, -1, gid); err != nil {
					s.log.Warn("agentrpc: cannot chgrp socket", "group", group, "err", err)
				}
			}
		} else {
			s.log.Warn("agentrpc: socket group not found", "group", group)
		}
	}
	return s.Serve(ln)
}

// Serve serves HTTP on ln, admitting only connections from allowed uids.
func (s *Server) Serve(ln net.Listener) error {
	hs := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    8 << 10,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	s.mu.Lock()
	s.hs = hs
	s.mu.Unlock()
	return hs.Serve(&credListener{Listener: ln, s: s})
}

// Shutdown stops accepting connections and waits, until ctx ends, for the
// requests in flight to be answered. A restart (systemd's SIGTERM) used to
// kill the agent mid-reply: the self-update's own answer could be lost to
// the restart it had just scheduled. Serve then returns http.ErrServerClosed.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.RLock()
	hs := s.hs
	s.mu.RUnlock()
	if hs == nil {
		return nil
	}
	return hs.Shutdown(ctx)
}

// credListener closes, before a byte is read, every connection whose peer
// is not an allowed uid.
type credListener struct {
	net.Listener
	s *Server
}

func (l *credListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := l.s.cfg.PeerUID(c)
		if err == nil && l.s.allowed(uid) {
			return c, nil
		}
		l.s.log.Warn("agentrpc: refused a connection from a peer that is not the API or root", "uid", uid, "err", err)
		c.Close()
	}
}

func (s *Server) allowed(uid uint32) bool {
	for _, u := range s.cfg.AllowedUIDs {
		if u == uid {
			return true
		}
	}
	return false
}

// UIDOf resolves a user name to its uid.
func UIDOf(name string) (uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(n), nil
}

// PeerUID is the uid of the process at the other end of a Unix socket
// connection, as the kernel recorded it when it connected: what the API's
// mail gate knows a site by (internal/api SPEC §14.2).
func PeerUID(c net.Conn) (uint32, error) { return kernelPeerUID(c) }
