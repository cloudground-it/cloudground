package api

import (
	"net/http"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/cloudground-it/cloudground/internal/accounts"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Setup, sign-in, sessions and the caller's own account (SPEC.md §2). The
// rules are accounts.Service's; these handlers decode, call it and answer.

// Session cookie names. The __Host- prefix makes a browser take the cookie
// only with Secure, Path=/ and no Domain, so a sibling subdomain (a site
// hosted under the panel's own domain) can neither set nor replace it.
// Plain-HTTP development cannot meet that and uses the bare name.
const (
	sessionCookie       = "__Host-cloudground_session"
	insecureSessionName = "cloudground_session"
)

// sessionTTL is a session's absolute lifetime (the store also ends one
// left idle: state.SessionIdleTimeout).
const sessionTTL = 24 * time.Hour

func (s *Server) sessionCookieName() string {
	if s.InsecureCookies {
		return insecureSessionName
	}
	return sessionCookie
}

// sessionToken is the request's session cookie value, "" without one.
func (s *Server) sessionToken(r *http.Request) string {
	if c, err := r.Cookie(s.sessionCookieName()); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.sessionCookieName(), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: !s.InsecureCookies, SameSite: http.SameSiteStrictMode,
	})
}

// startSession signs u in on this response. When the session cannot be
// stored it answers 500 itself and returns false: the caller must not then
// report success.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u state.User) bool {
	token := randomToken(32)
	if err := s.Store.CreateSession(token, u.ID, sessionTTL); err != nil {
		internalError(w, "CreateSession", err, "could not start a session")
		return false
	}
	s.logIfErr(s.Store.SetSessionClient(token, r.UserAgent(), clientIP(r)), "SetSessionClient")
	s.setSessionCookie(w, token, int(sessionTTL/time.Second))
	return true
}

// refusalStatus maps an accounts refusal to its status (SPEC.md §2).
var refusalStatus = map[accounts.Kind]int{
	accounts.Invalid:      http.StatusBadRequest,
	accounts.Unauthorized: http.StatusUnauthorized,
	accounts.Forbidden:    http.StatusForbidden,
	accounts.NotFound:     http.StatusNotFound,
	accounts.Conflict:     http.StatusConflict,
	accounts.Throttled:    http.StatusTooManyRequests,
	accounts.Unavailable:  http.StatusServiceUnavailable,
}

// busyRetryAfter is the Retry-After of a 503 for a full password queue.
const busyRetryAfter = "5"

// respondAccountErr answers an accounts error: a refusal with its status
// and message, anything else (the store failed) 500 with a fixed message,
// its text only in the journal.
func respondAccountErr(w http.ResponseWriter, err error) {
	ref, ok := accounts.IsRefusal(err)
	if !ok {
		internalError(w, "accounts", err, "")
		return
	}
	if ref.Kind == accounts.Unavailable {
		w.Header().Set("Retry-After", busyRetryAfter)
	}
	body := map[string]any{"error": ref.Msg}
	if ref.SecondFactor {
		body["totp_required"] = true
	}
	respond(w, refusalStatus[ref.Kind], body)
}

// decodeOr400 decodes the body into v, answering 400 when it cannot.
func decodeOr400[T any](w http.ResponseWriter, r *http.Request, v *T) bool {
	if err := decode(r, v); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return false
	}
	return true
}

// NewSetupToken mints and stores the first-boot setup token panel-api
// prints as a setup URL.
func NewSetupToken(store *state.Store, ttl time.Duration) (string, error) {
	token := randomToken(24)
	return token, store.CreateSetupToken(token, ttl)
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.CountUsers()
	if err != nil {
		internalError(w, "CountUsers", err, "")
		return
	}
	respond(w, http.StatusOK, map[string]bool{"setup_complete": n > 0})
}

// signInBody is what setup and sign-in take.
type signInBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Token    string `json:"token,omitempty"`
	TOTP     string `json:"totp,omitempty"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var in signInBody
	if !decodeOr400(w, r, &in) {
		return
	}
	u, err := s.accounts.Setup(r.Context(), in.Email, in.Password, in.Token)
	if err != nil {
		respondAccountErr(w, err)
		return
	}
	if s.startSession(w, r, u) {
		respond(w, http.StatusCreated, u)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in signInBody
	if !decodeOr400(w, r, &in) {
		return
	}
	o := clientOrigin(r)
	u, err := s.accounts.Login(r.Context(), accounts.SignIn{
		Email: in.Email, Password: in.Password, Code: in.TOTP, Address: o.Address, Network: o.Network, Wide: o.Wide,
	})
	if err != nil {
		respondAccountErr(w, err)
		return
	}
	if s.startSession(w, r, u) {
		respond(w, http.StatusOK, u)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := s.sessionToken(r); token != "" {
		// "Logged out" while the session still works would be worse than
		// an error.
		if err := s.Store.DeleteSession(token); err != nil {
			internalError(w, "DeleteSession", err, "could not end the session")
			return
		}
	}
	s.setSessionCookie(w, "", -1)
	respond(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// handleSession answers the signed-in account or {"user": null}, never 401
// (SPEC.md §2.3).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	var user *state.User
	if token := s.sessionToken(r); token != "" {
		if u, err := s.sessionUser(r, token); err == nil {
			user = &u
		}
	}
	out := map[string]any{"user": user}
	if user != nil {
		// The UI keeps an admin the 2FA requirement holds back on the
		// enrolment page, with only what the router lets it use there.
		out["enrol_required"] = s.secondFactorPending(*user)
	}
	respond(w, http.StatusOK, out)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, c *call) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeOr400(w, r, &in) {
		return
	}
	if err := s.accounts.ChangePassword(r.Context(), clientOrigin(r), c.User, c.Actor(), in.CurrentPassword, in.NewPassword); err != nil {
		respondAccountErr(w, err)
		return
	}
	// The change ended every session, this one too: keep this device in.
	if s.startSession(w, r, c.User) {
		respond(w, http.StatusOK, map[string]string{"status": "password changed"})
	}
}

// secondFactorBody is what the enrolment and disable routes take.
type secondFactorBody struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (s *Server) handleTOTPBegin(w http.ResponseWriter, r *http.Request, c *call) {
	var in secondFactorBody
	if !decodeOr400(w, r, &in) {
		return
	}
	en, err := s.accounts.BeginTOTP(r.Context(), clientOrigin(r), c.User, in.Password, in.Code)
	if err != nil {
		respondAccountErr(w, err)
		return
	}
	png, err := qrcode.Encode(en.URI, qrcode.Medium, 256)
	if err != nil {
		internalError(w, "qrcode", err, "")
		return
	}
	respond(w, http.StatusOK, map[string]string{
		"secret":  en.Secret,
		"uri":     en.URI,
		"qr_data": "data:image/png;base64," + base64Std(png),
	})
}

func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request, c *call) {
	var in struct {
		Code string `json:"code"`
	}
	if !decodeOr400(w, r, &in) {
		return
	}
	if err := s.accounts.ConfirmTOTP(c.User, c.Actor(), in.Code, s.sessionToken(r)); err != nil {
		respondAccountErr(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "two-factor enabled"})
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request, c *call) {
	var in secondFactorBody
	if !decodeOr400(w, r, &in) {
		return
	}
	if err := s.accounts.DisableTOTP(r.Context(), clientOrigin(r), c.User, c.Actor(), in.Password, in.Code, s.sessionToken(r)); err != nil {
		respondAccountErr(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "two-factor disabled"})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request, c *call) {
	list, err := s.accounts.Sessions(c.User, s.sessionToken(r))
	if err != nil {
		internalError(w, "ListSessionsForUser", err, "")
		return
	}
	respond(w, http.StatusOK, list)
}

func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request, c *call) {
	err := s.accounts.RevokeSession(c.User, c.Actor(), r.PathValue("id"))
	if _, refused := accounts.IsRefusal(err); err != nil && !refused {
		internalError(w, "RevokeSession", err, "could not revoke the session")
		return
	}
	if err != nil {
		respondAccountErr(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request, c *call) {
	var in struct {
		FirstName *string `json:"first_name"`
		LastName  *string `json:"last_name"`
		Locale    *string `json:"locale"`
	}
	if !decodeOr400(w, r, &in) {
		return
	}
	u, err := s.accounts.UpdateProfile(c.User, accounts.ProfileChange{FirstName: in.FirstName, LastName: in.LastName, Locale: in.Locale})
	if err != nil {
		respondAccountErr(w, err)
		return
	}
	respond(w, http.StatusOK, u)
}
