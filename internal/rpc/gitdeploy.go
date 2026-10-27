package rpc

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// GitSyncParams fetches a branch of a site's repository as the site user,
// exports its tree and runs the build command there (ops/gitdeploy SPEC).
type GitSyncParams struct {
	Site   state.Site `json:"site"`
	Repo   string     `json:"repo"`
	Branch string     `json:"branch"`
	// DeployKey is the OpenSSH private key the fetch uses; "" for a
	// repository that needs none. It is a secret: never logged.
	DeployKey string `json:"deploy_key,omitempty"`
	// BuildCommand runs in the export, as the site user; "" runs none.
	BuildCommand string `json:"build_command,omitempty"`
	// ReleaseID names the export directory the release is built from.
	ReleaseID string `json:"release_id"`
}

// Sites lists the site fetched into.
func (p *GitSyncParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// MaxDeployKey bounds a deploy key's text (an ed25519 key is under 500
// bytes).
const MaxDeployKey = 4096

// Validate holds the rules about the values themselves.
func (p *GitSyncParams) Validate() error {
	if !site.ValidReleaseID(p.ReleaseID) || p.ReleaseID == site.InitialRelease {
		return fmt.Errorf("invalid release id %q", p.ReleaseID)
	}
	if err := site.CheckGitRepo(p.Repo, p.Site.RootPath); err != nil {
		return err
	}
	if err := site.CheckGitBranch(p.Branch); err != nil {
		return err
	}
	if err := site.CheckGitBuildCommand(p.BuildCommand); err != nil {
		return err
	}
	if p.DeployKey != "" {
		k := strings.TrimSpace(p.DeployKey)
		if len(k) > MaxDeployKey || !strings.HasPrefix(k, "-----BEGIN OPENSSH PRIVATE KEY-----\n") ||
			!strings.HasSuffix(k, "\n-----END OPENSSH PRIVATE KEY-----") || strings.Count(k, "-----BEGIN") != 1 {
			return errors.New("the deploy key is not one OpenSSH private key")
		}
	}
	return nil
}

// GitSyncResult is what a sync produced. A failed fetch or build is a
// result (Failed set, Output the tail of what it printed), not an error, so
// the panel can show the output.
type GitSyncResult struct {
	// Commit is the fetched commit the export is of.
	Commit string `json:"commit"`
	// SourceDir is the export, the tree DeployRelease builds from.
	SourceDir string `json:"source_dir"`
	// Output is the last part of the fetch's or the build's output.
	Output string `json:"output,omitempty"`
	// Failed is "fetch" or "build" when that step did not succeed.
	Failed string `json:"failed,omitempty"`
}
