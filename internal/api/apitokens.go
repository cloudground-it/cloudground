package api

// API tokens (R-4.6): bearer credentials for cgctl and scripts, so they do
// not sign in with a password on every call (argon2 each time, and the login
// rate limit in any loop). The rules:
//
//   - only an admin creates one, from a signed-in session. A token never
//     reaches the SessionOnly rows of the route table (accounts, sessions,
//     2FA, users, tokens, a site's SSH shell; the router answers 403): it
//     cannot mint a token, create an admin, reset a password or open a
//     shell, so a leaked token cannot make itself permanent. Nor can it add
//     a "shell" SSH key (sitessh.go);
//   - changing or resetting the owner's password, turning their 2FA off,
//     disabling, demoting or deleting them, and host recovery revoke their
//     tokens, as they end sessions (state);
//   - the secret is shown once, in the creation response; the store keeps
//     only its SHA-256;
//   - a token acts as its owner and only while the owner is an enabled
//     admin (state.UserForAPIToken), under the same two-factor policy;
//   - any admin can revoke any token; creation, revocation and the actor of
//     every audited action taken with a token are in the audit log.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/cloudground-it/cloudground/internal/state"
)

const (
	apiTokenPrefix     = "cgt_"
	maxTokenNameLen    = 64
	maxTokenExpiryDays = 365
)

var errBadToken = errors.New("invalid or revoked API token")

// bearerToken returns the request's API token: an Authorization header of
// the Bearer scheme whose credential is a cgt_ token. Any other header
// (Basic auth of a proxy in front of the panel, another system's bearer
// token) is not the panel's and is ignored, so it cannot lock the UI out.
func bearerToken(r *http.Request) (string, bool) {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	value = strings.TrimSpace(value)
	if !ok || !strings.EqualFold(scheme, "Bearer") || !strings.HasPrefix(value, apiTokenPrefix) {
		return "", false
	}
	return value, true
}

// msgSessionOnly is the router's refusal of a SessionOnly route to a token.
const msgSessionOnly = "this needs a signed-in session: an API token cannot manage accounts, users, tokens, site shells or the panel itself"

type createTokenRequest struct {
	Name          string `json:"name"`
	ExpiresInDays int    `json:"expires_in_days"`
}

func validTokenName(name string) bool {
	if name == "" || len(name) > maxTokenNameLen {
		return false
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func (s *Server) handleCreateAPIToken(w http.ResponseWriter, r *http.Request, c *call) {
	user := c.User
	var req createTokenRequest
	if err := decode(r, &req); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !validTokenName(req.Name) {
		respondErr(w, http.StatusBadRequest, fmt.Sprintf("give the token a name of 1 to %d printable characters", maxTokenNameLen))
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > maxTokenExpiryDays {
		respondErr(w, http.StatusBadRequest, fmt.Sprintf("expires_in_days must be 0 (never) to %d", maxTokenExpiryDays))
		return
	}
	var expires time.Time
	if req.ExpiresInDays > 0 {
		expires = time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
	}
	secret := apiTokenPrefix + randomToken(32)
	tok, err := s.Store.CreateAPIToken(user.ID, req.Name, secret, secret[:len(apiTokenPrefix)+6], expires)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "could not store the token")
		return
	}
	s.audit(c.Actor(), "token.create", req.Name, fmt.Sprintf("#%d %s…", tok.ID, tok.Hint))
	respond(w, http.StatusCreated, map[string]any{
		"token":     secret,
		"api_token": tok,
		"note":      "store the token now: it is not shown again",
	})
}

func (s *Server) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.Store.ListAPITokens()
	if err != nil {
		internalError(w, "ListAPITokens", err, "")
		return
	}
	respond(w, http.StatusOK, toks)
}

func (s *Server) handleRevokeAPIToken(w http.ResponseWriter, r *http.Request, c *call) {
	id, err := pathID(r)
	if err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	tok, err := s.Store.DeleteAPIToken(id)
	if errors.Is(err, state.ErrNotFound) {
		respondErr(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		internalError(w, "DeleteAPIToken", err, "")
		return
	}
	s.audit(c.Actor(), "token.revoke", tok.Name, fmt.Sprintf("#%d %s… of %s", tok.ID, tok.Hint, tok.Email))
	respond(w, http.StatusOK, map[string]string{"status": "revoked"})
}
