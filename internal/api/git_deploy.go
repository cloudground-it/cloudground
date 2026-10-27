package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	siteval "github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Git deploy (SPEC.md §15, issue #24): a site's release built from a branch
// of a repository when the forge says it moved. This file is the settings,
// the queue and the job; the webhook is git_hook.go.

// gitQueue holds, per site, the deployment of the deploy.git job that is
// queued and has not started (§15.4): a push that finds one joins it.
type gitQueue struct {
	mu      sync.Mutex
	waiting map[int64]int64
}

// clear forgets the waiting deployment of a site when it is dep (0: any).
func (q *gitQueue) clear(siteID, dep int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if dep == 0 || q.waiting[siteID] == dep {
		delete(q.waiting, siteID)
	}
}

// gitQueued is what queueGitDeploy did.
type gitQueued struct {
	Deployment state.Deployment
	Task       jobs.Task
	// Coalesced: a deploy was already waiting, and this push joined it.
	Coalesced bool
}

// queueGitDeploy records a deployment of the site's branch and queues the
// job, unless one is already waiting for its turn, which it joins (§15.4).
func (s *Server) queueGitDeploy(site state.Site, branch string) (gitQueued, error) {
	s.gitq.mu.Lock()
	defer s.gitq.mu.Unlock()
	if id, ok := s.gitq.waiting[site.ID]; ok {
		if d, err := s.Store.GetDeployment(id); err == nil && d.Status == state.DeployPending {
			return gitQueued{Deployment: d, Coalesced: true}, nil
		}
		delete(s.gitq.waiting, site.ID)
	}
	dep, err := s.Store.CreateDeployment(state.Deployment{
		SiteID: site.ID, ReleaseID: siteval.NewReleaseID(time.Now()), Status: state.DeployPending,
	})
	if err != nil {
		return gitQueued{}, fmt.Errorf("could not record the deployment: %w", err)
	}
	task, err := s.enqueueDeploy("deploy.git", dep, deployPayload{DeploymentID: dep.ID, Branch: branch}, siteLocks(site.ID))
	if err != nil {
		return gitQueued{}, queueError{err}
	}
	if s.gitq.waiting == nil {
		s.gitq.waiting = map[int64]int64{}
	}
	s.gitq.waiting[site.ID] = dep.ID
	return gitQueued{Deployment: dep, Task: task}, nil
}

// settleGitDeploy is the Canceled and Interrupted hook of deploy.git: the
// deploy no longer waits, and the deployment is settled as any deploy's.
func (s *Server) settleGitDeploy(j state.Job) {
	var p deployPayload
	if json.Unmarshal([]byte(j.Payload), &p) == nil {
		s.gitq.clear(j.SiteID, p.DeploymentID)
	}
	s.settleDeployment(j)
}

// runDeployGit is the job (§15.4): fetch and build as the site user, build
// the release, promote it. Anything before the promotion that fails leaves
// the live release alone.
func (s *Server) runDeployGit(ctx context.Context, r *jobs.Run) error {
	var first deployPayload
	if err := r.Payload(&first); err != nil {
		return err
	}
	s.gitq.clear(r.Job().SiteID, first.DeploymentID) // it has started: the next push queues one more
	p, site, d, err := s.deployJob(ctx, r)
	if err != nil {
		return err
	}
	if d.Status != state.DeployPending {
		return fmt.Errorf("the deployment is %s, not waiting to be built", d.Status)
	}
	g, err := s.Store.GitDeploy(site.ID)
	if err != nil || !g.Enabled {
		return s.fail(&d, errors.New("git deploy is off for this site"))
	}
	key, err := s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretGitKey))
	if err != nil {
		return s.fail(&d, errors.New("could not read the site's deploy key"))
	}
	branch := g.Branch
	if p.Branch != "" {
		branch = p.Branch
	}
	if err := s.stop(ctx, &d, r.Progress, "fetching the repository"); err != nil {
		return err
	}
	r.Progress(10, "Fetching branch "+branch+" and building it as the site user")
	var res rpc.GitSyncResult
	err = agentrpc.Into(ctx, s.Agent, agentrpc.GitSync, rpc.GitSyncParams{
		Site: site, Repo: g.Repo, Branch: branch, DeployKey: key, BuildCommand: g.BuildCommand, ReleaseID: d.ReleaseID,
	}, &res)
	if err != nil {
		if serr := s.stop(ctx, &d, r.Progress, "the release"); serr != nil {
			return serr
		}
		return s.settleFailed(&d, err)
	}
	if out := strings.TrimSpace(res.Output); out != "" {
		r.Progress(30, "Output:\n"+out)
	}
	if res.Failed != "" {
		what := map[string]string{"fetch": "fetching " + g.Repo, "build": "the build command"}[res.Failed]
		return s.fail(&d, fmt.Errorf("%s failed: the live release was not changed", what))
	}
	s.logIfErr(s.Store.SetGitDeliveryCommit(d.ID, res.Commit), "SetGitDeliveryCommit")
	r.Progress(40, fmt.Sprintf("Creating release %s from %s at %s", d.ReleaseID, branch, short(res.Commit)))
	if err := s.build(context.WithoutCancel(ctx), &d, rpc.DeployParams{Site: site, SourceDir: res.SourceDir}, "", state.DeployCreated); err != nil {
		return err
	}
	r.Progress(60, "Release "+d.ReleaseID+" staged")
	if err := s.move(&d, state.DeployPromoting, state.DeploymentUpdate{}); err != nil {
		return err
	}
	return s.finishPromote(ctx, r, site, &d, state.DeployCreated)
}

// short is the first 7 characters of a commit.
func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

// ---- settings ----

// gitDeliveryView is a delivery with its deployment's release and status.
type gitDeliveryView struct {
	state.GitDelivery
	ReleaseID string `json:"release_id,omitempty"`
	Status    string `json:"status,omitempty"`
}

// gitDeployView is what the routes answer (§15.2). The private key never
// leaves the store.
type gitDeployView struct {
	Enabled      bool              `json:"enabled"`
	Repo         string            `json:"repo"`
	Branch       string            `json:"branch"`
	BuildCommand string            `json:"build_command"`
	WebhookPath  string            `json:"webhook_path"`
	Secret       string            `json:"secret"`
	PublicKey    string            `json:"public_key"`
	Deliveries   []gitDeliveryView `json:"deliveries"`
}

func (s *Server) gitDeployView(site state.Site) (gitDeployView, error) {
	g, err := s.Store.GitDeploy(site.ID)
	if errors.Is(err, state.ErrNotFound) {
		g, err = state.GitDeploy{SiteID: site.ID, Branch: siteval.DefaultGitBranch}, nil
	}
	if err != nil {
		return gitDeployView{}, err
	}
	v := gitDeployView{Enabled: g.Enabled, Repo: g.Repo, Branch: g.Branch, BuildCommand: g.BuildCommand,
		PublicKey: g.PublicKey, Deliveries: []gitDeliveryView{}}
	if g.HookID != "" {
		v.WebhookPath = "/hooks/git/" + g.HookID
		if v.Secret, err = s.Store.Secret(state.SiteSecret(site.ID, state.SiteSecretGitWebhook)); err != nil {
			return v, err
		}
	}
	ds, err := s.Store.GitDeliveries(site.ID)
	if err != nil {
		return v, err
	}
	for _, d := range ds {
		dv := gitDeliveryView{GitDelivery: d}
		if d.DeploymentID != 0 {
			if dep, err := s.Store.GetDeployment(d.DeploymentID); err == nil {
				dv.ReleaseID, dv.Status = dep.ReleaseID, string(dep.Status)
			}
		}
		v.Deliveries = append(v.Deliveries, dv)
	}
	return v, nil
}

// newHookID is 120 random bits as 24 lower-case base32 characters.
func newHookID() (string, error) {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

// newWebhookSecret is 256 random bits in hex.
func newWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// newDeployKey makes an ed25519 key: the OpenSSH private block, and the
// public half as an authorized_keys line commented with the site.
func newDeployKey(siteID int64) (private, public string, err error) {
	comment := fmt.Sprintf("cloudground-site-%d", siteID)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment, nil
}

func (s *Server) handleGetGitDeploy(w http.ResponseWriter, r *http.Request, c *call) {
	v, err := s.gitDeployView(c.Site)
	if err != nil {
		internalError(w, "gitDeployView", err, "")
		return
	}
	respond(w, http.StatusOK, v)
}

// handleSetGitDeploy changes the settings; the first time it is turned on
// it makes the hook id, the secret and the deploy key (§15.2).
func (s *Server) handleSetGitDeploy(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Enabled      *bool   `json:"enabled"`
		Repo         *string `json:"repo"`
		Branch       *string `json:"branch"`
		BuildCommand *string `json:"build_command"`
	}
	if !decodeOr400(w, r, &req) {
		return
	}
	if req.Enabled == nil && req.Repo == nil && req.Branch == nil && req.BuildCommand == nil {
		respondErr(w, http.StatusBadRequest, "name at least one setting to change")
		return
	}
	site, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	g, err := s.Store.GitDeploy(site.ID)
	if errors.Is(err, state.ErrNotFound) {
		g, err = state.GitDeploy{SiteID: site.ID, Branch: siteval.DefaultGitBranch}, nil
	}
	if err != nil {
		internalError(w, "GitDeploy", err, "")
		return
	}
	if req.Enabled != nil {
		g.Enabled = *req.Enabled
	}
	if req.Repo != nil {
		g.Repo = strings.TrimSpace(*req.Repo)
	}
	if req.Branch != nil {
		g.Branch = strings.TrimSpace(*req.Branch)
		if g.Branch == "" {
			g.Branch = siteval.DefaultGitBranch
		}
	}
	if req.BuildCommand != nil {
		g.BuildCommand = strings.TrimSpace(*req.BuildCommand)
	}
	if g.Repo == "" && g.Enabled {
		respondErr(w, http.StatusBadRequest, "give the repository the site is deployed from")
		return
	}
	if g.Repo != "" {
		if err := siteval.CheckGitRepo(g.Repo, site.RootPath); err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	for _, err := range []error{siteval.CheckGitBranch(g.Branch), siteval.CheckGitBuildCommand(g.BuildCommand)} {
		if err != nil {
			respondErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if g.HookID == "" {
		if err := s.makeGitCredentials(&g); err != nil {
			internalError(w, "makeGitCredentials", err, "")
			return
		}
	}
	if err := s.Store.SaveGitDeploy(g); err != nil {
		internalError(w, "SaveGitDeploy", err, "")
		return
	}
	s.audit(c.Actor(), "site.gitdeploy", site.Domain, fmt.Sprintf("enabled=%t branch=%s", g.Enabled, g.Branch))
	s.respondGitDeploy(w, site)
}

// makeGitCredentials gives a site that never had them a hook id, a webhook
// secret and a deploy key: the secrets are stored, the rest set on g.
func (s *Server) makeGitCredentials(g *state.GitDeploy) error {
	id, err := newHookID()
	if err != nil {
		return err
	}
	secret, err := newWebhookSecret()
	if err != nil {
		return err
	}
	priv, pub, err := newDeployKey(g.SiteID)
	if err != nil {
		return err
	}
	if err := s.Store.SetSecret(state.SiteSecret(g.SiteID, state.SiteSecretGitWebhook), secret); err != nil {
		return err
	}
	if err := s.Store.SetSecret(state.SiteSecret(g.SiteID, state.SiteSecretGitKey), priv); err != nil {
		return err
	}
	g.HookID, g.PublicKey = id, pub
	return nil
}

func (s *Server) respondGitDeploy(w http.ResponseWriter, site state.Site) {
	v, err := s.gitDeployView(site)
	if err != nil {
		internalError(w, "gitDeployView", err, "")
		return
	}
	respond(w, http.StatusOK, v)
}

// handleGitDeploySecret makes a new webhook secret and a new hook id: the
// old URL stops answering (§15.2).
func (s *Server) handleGitDeploySecret(w http.ResponseWriter, r *http.Request, c *call) {
	site, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	g, err := s.Store.GitDeploy(site.ID)
	if errors.Is(err, state.ErrNotFound) {
		respondErr(w, http.StatusConflict, "git deploy was never turned on for this site")
		return
	}
	if err != nil {
		internalError(w, "GitDeploy", err, "")
		return
	}
	id, err := newHookID()
	if err == nil {
		var secret string
		if secret, err = newWebhookSecret(); err == nil {
			// The secret first: a request for the new URL cannot find the
			// new id with the old secret.
			if err = s.Store.SetSecret(state.SiteSecret(site.ID, state.SiteSecretGitWebhook), secret); err == nil {
				g.HookID = id
				err = s.Store.SaveGitDeploy(g)
			}
		}
	}
	if err != nil {
		internalError(w, "rotate the webhook secret", err, "")
		return
	}
	s.audit(c.Actor(), "site.gitdeploy.secret", site.Domain, "")
	s.respondGitDeploy(w, site)
}

// handleGitDeployKey makes a new deploy key.
func (s *Server) handleGitDeployKey(w http.ResponseWriter, r *http.Request, c *call) {
	site, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	g, err := s.Store.GitDeploy(site.ID)
	if errors.Is(err, state.ErrNotFound) {
		respondErr(w, http.StatusConflict, "git deploy was never turned on for this site")
		return
	}
	if err != nil {
		internalError(w, "GitDeploy", err, "")
		return
	}
	priv, pub, err := newDeployKey(site.ID)
	if err == nil {
		if err = s.Store.SetSecret(state.SiteSecret(site.ID, state.SiteSecretGitKey), priv); err == nil {
			g.PublicKey = pub
			err = s.Store.SaveGitDeploy(g)
		}
	}
	if err != nil {
		internalError(w, "rotate the deploy key", err, "")
		return
	}
	s.audit(c.Actor(), "site.gitdeploy.key", site.Domain, "")
	s.respondGitDeploy(w, site)
}

// handleGitDeployRun deploys the branch's tip now (§15.2).
func (s *Server) handleGitDeployRun(w http.ResponseWriter, r *http.Request, c *call) {
	g, err := s.Store.GitDeploy(c.Site.ID)
	if errors.Is(err, state.ErrNotFound) || (err == nil && !g.Enabled) {
		respondErr(w, http.StatusConflict, "git deploy is off")
		return
	}
	if err != nil {
		internalError(w, "GitDeploy", err, "")
		return
	}
	q, err := s.queueGitDeploy(c.Site, g.Branch)
	if err != nil {
		fail(w, err)
		return
	}
	outcome := state.GitQueued
	if q.Coalesced {
		outcome = state.GitCoalesced
	}
	_, err = s.Store.AddGitDelivery(state.GitDelivery{SiteID: c.Site.ID, Forge: "manual", Event: "manual",
		Ref: "refs/heads/" + g.Branch, Outcome: outcome, DeploymentID: q.Deployment.ID, TaskID: q.Task.ID})
	s.logIfErr(err, "AddGitDelivery")
	s.audit(c.Actor(), "site.gitdeploy.run", c.Site.Domain, g.Branch)
	respond(w, http.StatusAccepted, map[string]any{"task": q.Task, "release_id": q.Deployment.ReleaseID,
		"deployment_id": q.Deployment.ID, "coalesced": q.Coalesced})
}
