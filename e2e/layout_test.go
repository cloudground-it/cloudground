//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// R-2.3: a site's links are relative, so they resolve inside the site, a
// staging clone's point at the clone's own shared/, and the agent (root)
// follows nothing that leaves a site. These checks run around the staging
// clone of the WordPress feature site.

// layoutProbe is what layoutBeforeClone saw, for layoutAfterClone.
type layoutProbe struct {
	cfgSum  string // sha256 of production's shared/wp-config.php
	outside string // a file of the site user's, outside its tree
	link    string // the hard link to it inside the live release
	content string
	linked  bool // the fixture is in place
}

func nlink(p string) uint64 {
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return 0
	}
	return uint64(st.Nlink)
}

// layoutBeforeClone checks the production site's links and plants the
// hard-link fixture: the site user links a file it owns outside its tree
// (fs.protected_hardlinks allows exactly that) into its live release.
func (f *feat) layoutBeforeClone(t *testing.T) *layoutProbe {
	h := f.h
	root := "/srv/sites/" + f.fs
	cur, _ := os.Readlink(root + "/current")
	h.assert(t, "R-2.3: a site's current link is relative", strings.HasPrefix(cur, "releases/") && !strings.Contains(cur[len("releases/"):], "/"),
		"current -> %q", cur)
	rel := filepath.Join(root, cur)
	cfg, _ := os.Readlink(rel + "/wp-config.php")
	h.check(t, "R-2.3: a release's wp-config.php link is relative", cfg, "../../shared/wp-config.php")
	up, _ := os.Readlink(rel + "/wp-content/uploads")
	h.check(t, "R-2.3: a release's uploads link is relative", up, "../../../shared/uploads")

	p := &layoutProbe{
		cfgSum:  sha256File(root + "/shared/wp-config.php"),
		outside: fmt.Sprintf("/var/tmp/cg-e2e-hardlink-%d", h.pid),
		link:    f.fp + "/cg-e2e-hardlink.txt",
		content: fmt.Sprintf("outside-%d", h.pid),
	}
	h.undo("remove the hard-link fixture", func() { h.remove(p.outside, p.link) })
	if err := h.writeAs(p.outside, p.content+"\n", f.fsu, 0o644); err != nil {
		t.Logf("fixture: %v", err)
	}
	r := h.as(f.fsu, "ln", p.outside, p.link)
	p.linked = r.OK() && inode(p.outside) != "" && inode(p.outside) == inode(p.link)
	h.assert(t, "R-2.3 control: the site user hard-linked a file of its own into its release", p.linked,
		"ln as %s: %s (the hard-link checks below cannot run)", f.fsu, r)
	return p
}

// layoutAfterClone checks the staging clone: its links lead into its own
// tree, production's config is untouched, and the hard link became an
// independent copy. Then the file manager's write over the hard link.
func (f *feat) layoutAfterClone(t *testing.T, p *layoutProbe, stg, stgUser string) {
	h := f.h
	sroot := "/srv/sites/" + stg
	scur, _ := os.Readlink(sroot + "/current")
	h.assert(t, "R-2.3: the staging clone's current link is relative", strings.HasPrefix(scur, "releases/"), "staging current -> %q", scur)
	real, err := filepath.EvalSymlinks(sroot + "/current/wp-config.php")
	h.assert(t, "R-2.3: staging's wp-config.php resolves to staging's own shared/",
		err == nil && real == sroot+"/shared/wp-config.php", "staging wp-config resolves to %q (%v)", real, err)
	ureal, err := filepath.EvalSymlinks(sroot + "/current/wp-content/uploads")
	h.assert(t, "R-2.3: staging's uploads resolve to staging's own shared/",
		err == nil && ureal == sroot+"/shared/uploads", "staging uploads resolve to %q (%v)", ureal, err)
	// The clone rewrote staging's credentials through its wp-config link.
	h.check(t, "R-2.3: production's wp-config.php is unchanged by the clone",
		sha256File("/srv/sites/"+f.fs+"/shared/wp-config.php"), p.cfgSum)
	if !p.linked {
		return
	}

	cp := sroot + "/current/cg-e2e-hardlink.txt"
	owner, _ := statOwnerMode(cp)
	h.assert(t, "R-2.3: the clone copied a hard-linked file as an independent file owned by staging",
		readTrim(cp) == p.content && inode(cp) != inode(p.outside) && nlink(cp) == 1 && owner == stgUser,
		"staging copy: content %q, inode %s (outside %s), links %d, owner %s (want %s)",
		readTrim(cp), inode(cp), inode(p.outside), nlink(cp), owner, stgUser)
	oowner, _ := statOwnerMode(p.outside)
	h.assert(t, "R-2.3: the clone neither linked nor re-owned the file outside the tree",
		nlink(p.outside) == 2 && oowner == f.fsu, "outside file: %d links, owner %s (want 2, %s)", nlink(p.outside), oowner, f.fsu)

	// The editor saves by renaming a new file over the name: the site
	// user's outside file keeps its content, the release gets a new inode.
	c, b := h.api.post(f.api("/files/write"), `{"path":"current/cg-e2e-hardlink.txt","content":"edited\n"}`)
	h.assert(t, "R-2.3: the file manager does not write through a hard link",
		c == 200 && readTrim(p.link) == "edited" && readTrim(p.outside) == p.content && inode(p.link) != inode(p.outside),
		"write: %d %s; release file %q, outside file %q, inodes %s/%s",
		c, head(b, 200), readTrim(p.link), readTrim(p.outside), inode(p.link), inode(p.outside))
}
