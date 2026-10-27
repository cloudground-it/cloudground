package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The git webhook (SPEC.md §15.3): GitHub, GitLab and Gitea tell the panel
// that a branch moved. Every request is authenticated by its signature;
// nothing a refusal says tells a site that exists from one that does not.

// Per-address limits a minute (§15.3 step 1).
const (
	gitHookDeliveries = 120
	gitHookFailures   = 20
	// gitHookBody is the most a delivery may weigh.
	gitHookBody = 1 << 20
)

// gitDummySecret stands in for the secret of a hook that does not exist or
// is off, so the check costs the same.
const gitDummySecret = "0000000000000000000000000000000000000000000000000000000000000000"

// gitSignature is how a request proves it knows the secret, and the family
// of forge that sends it.
type gitSignature int

const (
	gitSigNone    gitSignature = iota
	gitSigHub                  // X-Hub-Signature-256: sha256=<hex>
	gitSigGitea                // X-Gitea-Signature: <hex>
	gitSigForgejo              // X-Forgejo-Signature: <hex>
	gitSigGitLab               // X-Gitlab-Token: <secret>
)

// verifyGitSignature judges a delivery by the first of the headers it
// carries (§15.3 step 3), in constant time. A request with none, or a
// malformed value, fails.
func verifyGitSignature(h http.Header, body []byte, secret string) (gitSignature, bool) {
	hmacHex := func(v string) bool {
		got, err := hex.DecodeString(v)
		if err != nil {
			return false
		}
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(body)
		return hmac.Equal(m.Sum(nil), got)
	}
	if v := h.Get("X-Hub-Signature-256"); v != "" {
		hexv, ok := strings.CutPrefix(v, "sha256=")
		return gitSigHub, ok && hmacHex(hexv)
	}
	if v := h.Get("X-Gitea-Signature"); v != "" {
		return gitSigGitea, hmacHex(v)
	}
	if v := h.Get("X-Forgejo-Signature"); v != "" {
		return gitSigForgejo, hmacHex(v)
	}
	if v := h.Get("X-Gitlab-Token"); v != "" {
		a, b := sha256.Sum256([]byte(v)), sha256.Sum256([]byte(secret))
		return gitSigGitLab, subtle.ConstantTimeCompare(a[:], b[:]) == 1
	}
	return gitSigNone, false
}

// gitEvent names the forge that sent an authenticated delivery and what it
// says happened: "push", "ping" or something else.
func gitEvent(h http.Header, sig gitSignature) (forge, event string) {
	if sig == gitSigGitLab {
		e := h.Get("X-Gitlab-Event")
		if e == "Push Hook" {
			return "gitlab", "push"
		}
		return "gitlab", strings.ToLower(e)
	}
	// Gitea and Forgejo also send GitHub's header: theirs comes first.
	for _, c := range []struct{ header, forge string }{
		{"X-Forgejo-Event", "forgejo"}, {"X-Gitea-Event", "gitea"}, {"X-GitHub-Event", "github"},
	} {
		if e := h.Get(c.header); e != "" {
			return c.forge, strings.ToLower(e)
		}
	}
	return "unknown", ""
}

// gitPush is the part of a push event the webhook reads (the three forges
// agree on it).
type gitPush struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
}

const gitHookIDMax = 64

var (
	zeroCommit  = regexp.MustCompile(`^0+$`)
	commitHexRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

// gitAnswer is one webhook reply.
func gitAnswer(w http.ResponseWriter, status int, v map[string]any) { respond(w, status, v) }

func gitUnauthorized(w http.ResponseWriter) {
	respondErr(w, http.StatusUnauthorized, "unauthorized")
}

func (s *Server) handleGitHook(w http.ResponseWriter, r *http.Request) {
	ip := clientNetwork(clientIP(r))
	if s.gitHookFailLimiter.Blocked(ip) || !s.gitHookLimiter.Allow(ip) {
		respondErr(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			respondErr(w, http.StatusRequestEntityTooLarge, "the delivery is too large")
			return
		}
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	hook := r.PathValue("hook")
	if len(hook) > gitHookIDMax {
		hook = ""
	}
	g, err := s.Store.GitDeployByHook(hook)
	secret, known := gitDummySecret, err == nil && g.Enabled
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		s.logIfErr(err, "GitDeployByHook")
	}
	if known {
		sec, err := s.Store.Secret(state.SiteSecret(g.SiteID, state.SiteSecretGitWebhook))
		if err != nil || sec == "" {
			s.logIfErr(err, "git webhook secret")
			known = false
		} else {
			secret = sec
		}
	}
	sig, ok := verifyGitSignature(r.Header, body, secret)
	if !ok || !known {
		s.gitHookFailLimiter.Allow(ip)
		slog.Info("git webhook refused", "address", clientIP(r))
		gitUnauthorized(w)
		return
	}

	forge, event := gitEvent(r.Header, sig)
	deliver := func(d state.GitDelivery) {
		d.SiteID, d.Forge, d.Event = g.SiteID, forge, event
		_, err := s.Store.AddGitDelivery(d)
		s.logIfErr(err, "AddGitDelivery")
	}
	if event == "ping" {
		deliver(state.GitDelivery{Outcome: state.GitPing})
		gitAnswer(w, http.StatusOK, map[string]any{"status": "pong"})
		return
	}
	ignore := func(reason, ref, commit string) {
		deliver(state.GitDelivery{Outcome: state.GitIgnored, Detail: reason, Ref: ref, Commit: commit})
		gitAnswer(w, http.StatusOK, map[string]any{"status": "ignored", "reason": reason})
	}
	if event != "push" {
		ignore("event", "", "")
		return
	}
	var push gitPush
	if json.Unmarshal(body, &push) != nil || push.Ref == "" {
		ignore("payload", "", "")
		return
	}
	commit := ""
	if commitHexRe.MatchString(push.After) && !zeroCommit.MatchString(push.After) {
		commit = push.After
	}
	switch {
	case push.Deleted || (push.After != "" && zeroCommit.MatchString(push.After)):
		ignore("deleted", push.Ref, "")
		return
	case push.Ref != "refs/heads/"+g.Branch:
		ignore("branch", push.Ref, commit)
		return
	}
	site, err := s.Store.GetSite(g.SiteID)
	if err != nil {
		s.logIfErr(err, "GetSite")
		respondErr(w, http.StatusInternalServerError, msgInternal)
		return
	}
	q, err := s.queueGitDeploy(site, g.Branch)
	if err != nil {
		fail(w, err)
		return
	}
	outcome := state.GitQueued
	if q.Coalesced {
		outcome = state.GitCoalesced
	}
	deliver(state.GitDelivery{Outcome: outcome, Ref: push.Ref, Commit: commit, DeploymentID: q.Deployment.ID, TaskID: q.Task.ID})
	if !q.Coalesced {
		s.audit("git webhook ("+forge+")", "git.deploy", site.Domain, g.Branch+" "+short(commit))
	}
	reply := map[string]any{"status": "queued", "deployment_id": q.Deployment.ID, "release_id": q.Deployment.ReleaseID}
	if q.Coalesced {
		reply["coalesced"] = true
	} else {
		reply["task"] = q.Task.ID
	}
	gitAnswer(w, http.StatusAccepted, reply)
}
