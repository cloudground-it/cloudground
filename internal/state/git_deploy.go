package state

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Git deploy (#24, api SPEC §15): a site's repository settings and the
// webhook deliveries it received. The webhook secret and the deploy key's
// private half are sealed secrets (SiteSecretGitWebhook, SiteSecretGitKey).

// GitDeploy is a site's git deploy settings.
type GitDeploy struct {
	SiteID       int64  `json:"site_id"`
	Enabled      bool   `json:"enabled"`
	Repo         string `json:"repo"`
	Branch       string `json:"branch"`
	BuildCommand string `json:"build_command"`
	// PublicKey is the deploy key's public half, OpenSSH format.
	PublicKey string `json:"public_key"`
	// HookID is what the webhook's URL carries: unguessable, unique.
	HookID string `json:"hook_id"`
}

// GitDelivery is one authenticated webhook delivery (or manual deploy).
type GitDelivery struct {
	ID     int64     `json:"id"`
	SiteID int64     `json:"-"`
	At     time.Time `json:"at"`
	// Forge is github, gitlab, gitea or forgejo ("manual" for a deploy
	// asked for in the panel).
	Forge   string `json:"forge"`
	Event   string `json:"event"`
	Ref     string `json:"ref"`
	Commit  string `json:"commit"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
	// DeploymentID is the deployment the delivery queued, or joined; 0 for
	// none (or one since deleted).
	DeploymentID int64 `json:"deployment_id"`
	TaskID       int64 `json:"task_id"`
}

// Delivery outcomes.
const (
	GitPing      = "ping"
	GitIgnored   = "ignored"
	GitQueued    = "queued"
	GitCoalesced = "coalesced"
)

// GitDeliveriesKept is how many deliveries a site keeps.
const GitDeliveriesKept = 30

// ErrGitHookTaken is a hook id another site holds.
var ErrGitHookTaken = errors.New("that hook id belongs to another site")

// GitDeploy returns a site's settings, ErrNotFound before they were saved.
func (s *Store) GitDeploy(siteID int64) (GitDeploy, error) {
	return s.scanGitDeploy(s.db.QueryRow(gitDeployCols+` WHERE site_id = ?`, siteID))
}

// GitDeployByHook finds the site a webhook URL names: one indexed read.
func (s *Store) GitDeployByHook(hookID string) (GitDeploy, error) {
	if hookID == "" {
		return GitDeploy{}, ErrNotFound
	}
	return s.scanGitDeploy(s.db.QueryRow(gitDeployCols+` WHERE hook_id = ?`, hookID))
}

const gitDeployCols = `SELECT site_id, enabled, repo, branch, build_command, public_key, hook_id FROM site_git_deploys`

func (s *Store) scanGitDeploy(row *sql.Row) (GitDeploy, error) {
	var g GitDeploy
	var on int
	err := row.Scan(&g.SiteID, &on, &g.Repo, &g.Branch, &g.BuildCommand, &g.PublicKey, &g.HookID)
	if errors.Is(err, sql.ErrNoRows) {
		return GitDeploy{}, ErrNotFound
	}
	g.Enabled = on != 0
	return g, err
}

// SaveGitDeploy stores a site's settings (an upsert). A hook id another
// site holds is refused.
func (s *Store) SaveGitDeploy(g GitDeploy) error {
	on := 0
	if g.Enabled {
		on = 1
	}
	_, err := s.db.Exec(`INSERT INTO site_git_deploys (site_id, enabled, repo, branch, build_command, public_key, hook_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (site_id) DO UPDATE SET enabled = excluded.enabled, repo = excluded.repo, branch = excluded.branch,
			build_command = excluded.build_command, public_key = excluded.public_key, hook_id = excluded.hook_id`,
		g.SiteID, on, g.Repo, g.Branch, g.BuildCommand, g.PublicKey, g.HookID)
	if err != nil && isUnique(err) {
		return ErrGitHookTaken
	}
	return err
}

func isUnique(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// AddGitDelivery records a delivery and keeps the site's newest
// GitDeliveriesKept, in one transaction.
func (s *Store) AddGitDelivery(d GitDelivery) (GitDelivery, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	var dep any
	if d.DeploymentID != 0 {
		dep = d.DeploymentID
	}
	at := now()
	res, err := tx.Exec(`INSERT INTO site_git_deliveries (site_id, at, forge, event, ref, commit_sha, outcome, detail, deployment_id, task_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.SiteID, at, d.Forge, d.Event, d.Ref, d.Commit, d.Outcome, d.Detail, dep, d.TaskID)
	if err != nil {
		return d, err
	}
	if d.ID, err = res.LastInsertId(); err != nil {
		return d, err
	}
	if _, err := tx.Exec(`DELETE FROM site_git_deliveries WHERE site_id = ? AND id NOT IN
		(SELECT id FROM site_git_deliveries WHERE site_id = ? ORDER BY id DESC LIMIT ?)`,
		d.SiteID, d.SiteID, GitDeliveriesKept); err != nil {
		return d, err
	}
	d.At = parseTime(at)
	return d, tx.Commit()
}

// GitDeliveries returns a site's deliveries, newest first.
func (s *Store) GitDeliveries(siteID int64) ([]GitDelivery, error) {
	rows, err := s.db.Query(`SELECT id, at, forge, event, ref, commit_sha, outcome, detail, IFNULL(deployment_id, 0), task_id
		FROM site_git_deliveries WHERE site_id = ? ORDER BY id DESC LIMIT ?`, siteID, GitDeliveriesKept)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GitDelivery
	for rows.Next() {
		d := GitDelivery{SiteID: siteID}
		var at string
		if err := rows.Scan(&d.ID, &at, &d.Forge, &d.Event, &d.Ref, &d.Commit, &d.Outcome, &d.Detail, &d.DeploymentID, &d.TaskID); err != nil {
			return nil, err
		}
		d.At = parseTime(at)
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetGitDeliveryCommit fills in the commit a deployment was built from on
// the deliveries that joined it and did not announce one (a manual deploy).
func (s *Store) SetGitDeliveryCommit(deploymentID int64, commit string) error {
	_, err := s.db.Exec(`UPDATE site_git_deliveries SET commit_sha = ? WHERE deployment_id = ? AND commit_sha = ''`, commit, deploymentID)
	return err
}

// SetGitDeliveryTask records the task of the deployment a delivery queued.
func (s *Store) SetGitDeliveryTask(id, taskID int64) error {
	if id == 0 {
		return fmt.Errorf("no delivery")
	}
	_, err := s.db.Exec(`UPDATE site_git_deliveries SET task_id = ? WHERE id = ?`, taskID, id)
	return err
}
