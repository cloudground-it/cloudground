package state

import (
	"errors"
	"testing"
)

// Settings are found by site and by hook id; a hook id is one site's; a
// site's deletion takes its rows and its sealed secrets.
func TestGitDeploySettings(t *testing.T) {
	s := testStore(t)
	a := seedSite(t, s, "a.example.com", SiteStatic, SiteActive, 0)
	b := seedSite(t, s, "b.example.com", SiteStatic, SiteActive, 0)
	if _, err := s.GitDeploy(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before it was saved: %v", err)
	}
	if _, err := s.GitDeployByHook(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty hook id: %v", err)
	}
	g := GitDeploy{SiteID: a.ID, Enabled: true, Repo: "https://example.com/a.git", Branch: "main", BuildCommand: "make", PublicKey: "ssh-ed25519 AAAA", HookID: "hookaaaa"}
	if err := s.SaveGitDeploy(g); err != nil {
		t.Fatal(err)
	}
	got, err := s.GitDeployByHook("hookaaaa")
	if err != nil || got != g {
		t.Fatalf("by hook: %+v %v", got, err)
	}
	g.Enabled, g.HookID = false, "hookbbbb"
	if err := s.SaveGitDeploy(g); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GitDeployByHook("hookaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old hook id still answers: %v", err)
	}
	if got, _ := s.GitDeploy(a.ID); got.Enabled || got.HookID != "hookbbbb" {
		t.Fatalf("saved %+v", got)
	}
	other := GitDeploy{SiteID: b.ID, Repo: "https://example.com/b.git", Branch: "main", HookID: "hookbbbb"}
	if err := s.SaveGitDeploy(other); !errors.Is(err, ErrGitHookTaken) {
		t.Fatalf("a hook id of another site: %v", err)
	}

	if err := s.SetSecret(SiteSecret(a.ID, SiteSecretGitWebhook), "s3cret"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddGitDelivery(GitDelivery{SiteID: a.ID, Forge: "github", Outcome: GitPing}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSite(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GitDeploy(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("settings survive their site: %v", err)
	}
	if v, _ := s.Secret(SiteSecret(a.ID, SiteSecretGitWebhook)); v != "" {
		t.Fatal("the secret survives its site")
	}
	if d, _ := s.GitDeliveries(a.ID); len(d) != 0 {
		t.Fatalf("deliveries survive their site: %+v", d)
	}
}

// A site keeps its newest deliveries; one that joined a deployment gets
// the commit it was built from; the deployment's deletion leaves the row.
func TestGitDeliveries(t *testing.T) {
	s := testStore(t)
	a := seedSite(t, s, "a.example.com", SiteStatic, SiteActive, 0)
	b := seedSite(t, s, "b.example.com", SiteStatic, SiteActive, 0)
	dep, err := s.CreateDeployment(Deployment{SiteID: a.ID, ReleaseID: "01JA2B3C4D5E6F7G8H9JKMNPQR", Status: DeployPending})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < GitDeliveriesKept+5; i++ {
		if _, err := s.AddGitDelivery(GitDelivery{SiteID: a.ID, Forge: "gitea", Event: "push", Ref: "refs/heads/main", Outcome: GitIgnored, Detail: "n"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddGitDelivery(GitDelivery{SiteID: b.ID, Forge: "manual", Outcome: GitQueued}); err != nil {
		t.Fatal(err)
	}
	d, err := s.AddGitDelivery(GitDelivery{SiteID: a.ID, Forge: "manual", Outcome: GitQueued, DeploymentID: dep.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGitDeliveryTask(d.ID, 42); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGitDeliveryCommit(dep.ID, "abc123"); err != nil {
		t.Fatal(err)
	}
	list, err := s.GitDeliveries(a.ID)
	if err != nil || len(list) != GitDeliveriesKept {
		t.Fatalf("%d deliveries kept (%v), want %d", len(list), err, GitDeliveriesKept)
	}
	if list[0].ID != d.ID || list[0].Commit != "abc123" || list[0].TaskID != 42 || list[0].DeploymentID != dep.ID {
		t.Fatalf("newest first: %+v", list[0])
	}
	if other, _ := s.GitDeliveries(b.ID); len(other) != 1 {
		t.Fatalf("another site's deliveries were pruned: %d", len(other))
	}
}
