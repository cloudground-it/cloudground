//go:build e2e

package e2e

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// gitDeployWebhook (#24): a static site deploys a branch of a repository
// that lives in its own tree (a bare repository made on the box, read over
// file://: no network beyond the box). Deliveries are signed as GitHub and
// GitLab sign them and sent to the panel's real route: a push to the branch
// fetches and builds as the site user, becomes a release and is promoted
// (and rolled back with the usual rollback); a wrong signature, an unknown
// hook, another branch and a ping change nothing; a failing build leaves the
// live release live.
func (h *harness) gitDeployWebhook(t *testing.T) {
	dom := h.dom("gitdeploy")
	id := h.api.createSite(fmt.Sprintf(`{"domain":%q,"type":"static"}`, dom))
	ok := h.api.waitActive(id, 4*time.Minute) == "active"
	h.assert(t, "#24: the static site provisions", ok, "%s (id %d) did not become active", dom, id)
	if !ok {
		return
	}
	defer func() {
		h.api.deleteSite(id)
		h.api.teardownGone(id)
	}()
	user := h.api.site(id).SystemUser
	root := "/srv/sites/" + dom
	base := fmt.Sprintf("/api/sites/%d", id)

	// The repository, made by the site user: a bare one to fetch from and a
	// working tree to push from. build.sh is the build command's work: it
	// writes the name of the user that ran it.
	commit := func(content string) {
		script := fmt.Sprintf(`set -e; cd %s/tmp
[ -d cg24-repo.git ] || git init -q --bare -b main cg24-repo.git
[ -d cg24-work ] || { git init -q -b main cg24-work; git -C cg24-work config user.email e2e@example.com; git -C cg24-work config user.name e2e; }
cd cg24-work
printf '%%s\n' %s > index.html
printf '#!/bin/sh\necho built-by-$(id -un) > built.txt\n' > build.sh
git add -A; git commit -q -m %s
git push -q ../cg24-repo.git main
git rev-parse HEAD`, root, content, content)
		r := h.as(user, "env", "HOME=/tmp", "sh", "-c", script)
		if !r.OK() {
			h.fail(t, "#24: the repository is made on the box", "%s", r)
		}
	}
	head := func() string {
		r := h.as(user, "env", "HOME=/tmp", "git", "-C", root+"/tmp/cg24-work", "rev-parse", "HEAD")
		return strings.TrimSpace(r.Out)
	}
	defer h.remove(root+"/tmp/cg24-repo.git", root+"/tmp/cg24-work")
	commit("cg24-v1")
	c1 := head()

	// Turn it on.
	c, b := h.api.put(base+"/git-deploy", map[string]any{"enabled": true, "repo": "file://" + root + "/tmp/cg24-repo.git", "branch": "main", "build_command": "sh build.sh"})
	h.check(t, "#24: turning git deploy on is accepted", fmt.Sprint(c), "200")
	var view struct {
		WebhookPath string `json:"webhook_path"`
		Secret      string `json:"secret"`
		PublicKey   string `json:"public_key"`
		Deliveries  []struct {
			Forge, Outcome, Detail string
		} `json:"deliveries"`
	}
	_ = json.Unmarshal(b, &view)
	h.assert(t, "#24: the answer has the webhook path, a 64-character secret and an ed25519 public key",
		strings.HasPrefix(view.WebhookPath, "/hooks/git/") && len(view.Secret) == 64 && strings.HasPrefix(view.PublicKey, "ssh-ed25519 "), "%s", head2(b))
	h.assert(t, "#24: the private key is not in the answer", !strings.Contains(string(b), "PRIVATE KEY"), "%s", head2(b))
	sign := func(secret string, body []byte) string {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(body)
		return hex.EncodeToString(m.Sum(nil))
	}
	push := func(ref, after string) []byte {
		out, _ := json.Marshal(map[string]any{"ref": ref, "after": after})
		return out
	}
	github := func(event string, body []byte) (int, []byte) {
		return h.api.do("POST", view.WebhookPath, body, "X-GitHub-Event: "+event, "X-Hub-Signature-256: sha256="+sign(view.Secret, body))
	}
	type dep struct {
		ID        int64  `json:"id"`
		ReleaseID string `json:"release_id"`
		Status    string `json:"status"`
	}
	deployments := func() []dep {
		_, b := h.api.get(base + "/deployments")
		var out []dep
		_ = json.Unmarshal(b, &out)
		return out
	}
	page := func() string { return strings.TrimSpace(string(h.get(dom, "/").Body)) }
	taskOf := func(b []byte) int64 {
		var out struct {
			Task json.RawMessage `json:"task"`
		}
		_ = json.Unmarshal(b, &out)
		var n int64
		if json.Unmarshal(out.Task, &n) != nil {
			var obj struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(out.Task, &obj)
			n = obj.ID
		}
		return n
	}

	// Nothing the forge could get wrong, or an attacker send, builds anything.
	before := len(deployments())
	body := push("refs/heads/main", c1)
	for name, hdr := range map[string][]string{
		"a signature made with another secret": {"X-GitHub-Event: push", "X-Hub-Signature-256: sha256=" + sign("not the secret", body)},
		"no signature at all":                  {"X-GitHub-Event: push"},
		"GitLab's token, wrong":                {"X-Gitlab-Event: Push Hook", "X-Gitlab-Token: nope"},
		"the SHA-1 header alone":               {"X-GitHub-Event: push", "X-Hub-Signature: sha1=" + sign(view.Secret, body)},
	} {
		code, out := h.api.do("POST", view.WebhookPath, body, hdr...)
		h.check(t, "#24: "+name+" is refused with 401", fmt.Sprint(code), "401")
		h.assert(t, "#24: the refusal for "+name+" says nothing about the site", strings.Contains(string(out), `"unauthorized"`) && !strings.Contains(string(out), dom), "%s", head2(out))
	}
	code, out := h.api.do("POST", "/hooks/git/"+strings.Repeat("a", 24), body, "X-GitHub-Event: push", "X-Hub-Signature-256: sha256="+sign(view.Secret, body))
	h.check(t, "#24: a valid signature for a hook that does not exist is the same 401", fmt.Sprint(code), "401")
	h.assert(t, "#24: with the same answer", strings.Contains(string(out), `"unauthorized"`), "%s", head2(out))
	h.assert(t, "#24: control: nothing was fetched or built by a refused request",
		len(deployments()) == before && h.run("test", "-e", root+"/tmp/cg-git").Code != 0, "%d deployments (had %d)", len(deployments()), before)

	// A ping answers; other branches, tags and events are ignored.
	code, out = github("ping", []byte(`{"zen":"cg24"}`))
	h.assert(t, "#24: a ping answers pong", code == 200 && strings.Contains(string(out), "pong"), "%d %s", code, head2(out))
	code, out = github("push", push("refs/heads/develop", c1))
	h.assert(t, "#24: a push to another branch is ignored", code == 200 && strings.Contains(string(out), `"ignored"`), "%d %s", code, head2(out))
	code, out = github("push", push("refs/tags/v1", c1))
	h.assert(t, "#24: a tag is ignored", code == 200 && strings.Contains(string(out), `"ignored"`), "%d %s", code, head2(out))
	h.check(t, "#24: and none of them built a release", fmt.Sprint(len(deployments())), fmt.Sprint(before))

	// A push to the branch: fetched, built as the site user, promoted.
	code, out = github("push", push("refs/heads/main", c1))
	h.check(t, "#24: a signed push to the branch is accepted", fmt.Sprint(code), "202")
	h.check(t, "#24: its task ends", h.api.waitTask(taskOf(out), 5*time.Minute), "succeeded")
	ds := deployments()
	h.assert(t, "#24: a release was made and is the live one", len(ds) == before+1 && ds[0].Status == "promoted", "%+v", ds)
	if len(ds) == 0 {
		return
	}
	first := ds[0]
	h.check(t, "#24: the site serves the pushed commit", page(), "cg24-v1")
	h.check(t, "#24: the build ran as the site user, not root", strings.TrimSpace(string(h.get(dom, "/built.txt").Body)), "built-by-"+user)
	h.check(t, "#24: the repository was fetched as the site user", strings.TrimSpace(h.run("stat", "-c", "%U", root+"/tmp/cg-git/repo/.git").Out), user)
	h.assert(t, "#24: the release holds no .git", h.run("test", "-e", root+"/releases/"+first.ReleaseID+"/.git").Code != 0, "%s", root+"/releases/"+first.ReleaseID)
	h.assert(t, "#24: and nginx does not serve one", h.get(dom, "/.git/config").Code >= 400, "%s", h.get(dom, "/.git/config").CodeStr())
	h.check(t, "#24: the release is in the deliveries as queued", fmt.Sprint(strings.Contains(string(mustGet(h, base+"/git-deploy")), `"outcome":"queued"`)), "true")

	// A second commit through GitLab's header, then the existing rollback.
	commit("cg24-v2")
	c2 := head()
	body = push("refs/heads/main", c2)
	code, out = h.api.do("POST", view.WebhookPath, body, "X-Gitlab-Event: Push Hook", "X-Gitlab-Token: "+view.Secret)
	h.check(t, "#24: GitLab's token is accepted", fmt.Sprint(code), "202")
	h.check(t, "#24: the second task ends", h.api.waitTask(taskOf(out), 5*time.Minute), "succeeded")
	h.check(t, "#24: the site serves the second commit", page(), "cg24-v2")
	ds = deployments()
	h.assert(t, "#24: the first release is superseded", len(ds) == before+2 && ds[0].Status == "promoted" && ds[1].Status == "superseded", "%+v", ds)
	c, b = h.api.post(base+"/rollback", map[string]any{})
	h.check(t, "#24: the usual rollback is accepted", fmt.Sprint(c), "202")
	h.check(t, "#24: it ends", h.api.waitTask(taskOf(b), 2*time.Minute), "succeeded")
	h.check(t, "#24: the site serves the first release again", page(), "cg24-v1")

	// A build that fails leaves the live release live.
	c, _ = h.api.put(base+"/git-deploy", map[string]any{"build_command": "echo cg24-build-broke >&2; exit 3"})
	h.check(t, "#24: a failing build command is accepted", fmt.Sprint(c), "200")
	code, out = github("push", push("refs/heads/main", c2))
	h.check(t, "#24: the push is accepted", fmt.Sprint(code), "202")
	tid := taskOf(out)
	h.check(t, "#24: its task fails", h.api.waitTask(tid, 5*time.Minute), "failed")
	_, tb := h.api.get(fmt.Sprintf("/api/tasks/%d", tid))
	h.assert(t, "#24: the task's log has the build's output", strings.Contains(string(tb), "cg24-build-broke"), "%s", head2(tb))
	ds = deployments()
	h.assert(t, "#24: the deployment is failed and the rolled-back release is still the live one",
		len(ds) > 0 && ds[0].Status == "failed" && h.api.site(id).Status == "active", "%+v", ds)
	h.check(t, "#24: the site still serves the live release", page(), "cg24-v1")

	// The deploy key is the panel's: regenerating it gives another one.
	c, b = h.api.post(base+"/git-deploy/key", map[string]any{})
	var k struct {
		PublicKey string `json:"public_key"`
	}
	_ = json.Unmarshal(b, &k)
	h.assert(t, "#24: a new deploy key is another key", c == 200 && strings.HasPrefix(k.PublicKey, "ssh-ed25519 ") && k.PublicKey != view.PublicKey, "%d %s", c, head2(b))

	// A new secret changes the URL: the old one no longer answers.
	old := view.WebhookPath
	c, b = h.api.post(base+"/git-deploy/secret", map[string]any{})
	_ = json.Unmarshal(b, &view)
	h.assert(t, "#24: a new secret makes a new URL", c == 200 && view.WebhookPath != old && len(view.Secret) == 64, "%d %s", c, head2(b))
	code, _ = h.api.do("POST", old, body, "X-GitHub-Event: push", "X-Hub-Signature-256: sha256="+sign(view.Secret, body))
	h.check(t, "#24: the old URL is gone", fmt.Sprint(code), "401")
	code, _ = github("ping", []byte(`{}`))
	h.check(t, "#24: the new URL answers", fmt.Sprint(code), "200")

	// Turned off, the hook answers as if it did not exist.
	h.api.put(base+"/git-deploy", map[string]any{"enabled": false})
	code, _ = github("ping", []byte(`{}`))
	h.check(t, "#24: a hook that is off is a 401", fmt.Sprint(code), "401")
}

func mustGet(h *harness, path string) []byte {
	_, b := h.api.get(path)
	return b
}

func head2(b []byte) string { return head(b, 300) }
