//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// migrationArchives imports hostile archives into the migration site (R-3.5).
// Each check fails on the build before R-3.5 unless noted.
func (h *harness) migrationArchives(t *testing.T, mid int64, ms, mroot string) {
	confined := mroot + "/shared/cg-e2e-confined.tgz"
	bomb := mroot + "/shared/cg-e2e-bomb.tgz"
	cleanup := h.undo("remove the R-3.5 migration archives", func() { h.remove(confined, bomb) })
	defer cleanup()
	migrate := func(archive string) string {
		_, b := h.api.post(fmt.Sprintf("/api/sites/%d/migration", mid),
			fmt.Sprintf(`{"archive":%q,"confirm":%q}`, archive, ms))
		return h.api.waitTask(taskID(b), 300*time.Second)
	}

	// One top-level directory is the site, and its links stay inside it:
	// "public_html/up -> .." used to survive and become releases/<id>/up,
	// the site's releases/. A FIFO is never created (that one also held
	// before).
	err := writeTGZ(confined, func(tw *tar.Writer) error {
		for _, hdr := range []tar.Header{
			{Name: "public_html/", Mode: 0o755, Typeflag: tar.TypeDir},
			{Name: "public_html/up", Linkname: "..", Typeflag: tar.TypeSymlink},
			{Name: "public_html/fifo", Mode: 0o644, Typeflag: tar.TypeFifo},
		} {
			if err := tw.WriteHeader(&hdr); err != nil {
				return err
			}
		}
		for name, body := range map[string]string{
			"public_html/index.php":           "<?php echo 'confined';\n",
			"public_html/cg-e2e-confined.txt": "confined\n",
		} {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			if _, err := io.WriteString(tw, body); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Logf("fixture: %v", err)
	}
	h.check(t, "R-3.5: a migration of one top-level directory succeeds", migrate("shared/cg-e2e-confined.tgz"), "succeeded")
	h.check(t, "R-3.5: the top-level directory is the site", readTrim(mroot+"/current/cg-e2e-confined.txt"), "confined")
	up, _ := os.Readlink(mroot + "/current/up")
	h.assert(t, "R-3.5: a link leading out of the archive's directory is dropped", !exists(mroot+"/current/up"),
		"current/up survived the migration (-> %s)", up)
	h.assert(t, "R-3.5: a FIFO in the archive is not created", !exists(mroot+"/current/fifo"), "current/fifo was created")

	// A decompression bomb: 1 GiB of zeros is about a megabyte of gzip.
	// Before, it was written out in full into root's work directory and
	// then into the release, and the migration went live.
	before, _ := os.Readlink(mroot + "/current")
	err = writeTGZ(bomb, func(tw *tar.Writer) error {
		if err := tw.WriteHeader(&tar.Header{Name: "index.php", Mode: 0o644, Size: 6, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, "<?php\n"); err != nil {
			return err
		}
		const size = 1 << 30
		if err := tw.WriteHeader(&tar.Header{Name: "debug.log", Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.CopyN(tw, zeroReader{}, size)
		return err
	})
	if err != nil {
		t.Logf("fixture: %v", err)
	}
	// R-4.4: the refusal says the limit and how to raise it, in the
	// language of whoever reads the task (Italian, then German, here). The
	// old message was English only and named no way out.
	_, bb := h.api.post(fmt.Sprintf("/api/sites/%d/migration", mid), fmt.Sprintf(`{"archive":"shared/cg-e2e-bomb.tgz","confirm":%q}`, ms))
	bombTask := taskID(bb)
	h.check(t, "R-3.5: a decompression bomb is refused", h.api.waitTask(bombTask, 300*time.Second), "failed")
	taskError := func(lang string) string {
		_, tb := h.api.do("GET", fmt.Sprintf("/api/tasks/%d", bombTask), nil, "Accept-Language: "+lang)
		return jsonField(tb, "error")
	}
	msg := taskError("it-IT,it;q=0.9")
	h.assert(t, "R-4.4: the refusal names the expansion limit and how to raise it, translated",
		strings.Contains(msg, "200 volte la sua dimensione compressa") && strings.Contains(msg, "limite di espansione") && strings.Contains(msg, "fino a 1000 volte"),
		"the bomb's task error read in Italian is %q", msg)
	msg = taskError("de")
	h.assert(t, "R-4.4: the same task error reads in the reader's language (German)",
		strings.Contains(msg, "Expansionslimit") && strings.Contains(msg, "200-Fache"), "the bomb's task error read in German is %q", msg)
	// An administrator may raise the limit, not past the ceiling.
	c, _ := h.api.post(fmt.Sprintf("/api/sites/%d/migration", mid),
		fmt.Sprintf(`{"archive":"shared/cg-e2e-bomb.tgz","confirm":%q,"max_ratio":1001}`, ms))
	h.check(t, "R-4.4: an expansion limit above 1000 is refused", strconv.Itoa(c), "400")
	after, _ := os.Readlink(mroot + "/current")
	h.check(t, "R-3.5: a refused migration leaves the live release", after, before)
	h.assert(t, "R-3.5: a refused migration keeps its input for a retry", exists(bomb), "the refused archive was removed")

	h.migrationLimitRaised(t, mid, ms, mroot)
}

// migrationLimitRaised (R-4.4): only an administrator moves the expansion
// limit, and a raised one lets through an archive refused at 200 times.
// The archive expands about 380 times: 256 MiB of zeros and 400 KiB of
// noise.
func (h *harness) migrationLimitRaised(t *testing.T, mid int64, ms, mroot string) {
	dense := mroot + "/shared/cg-e2e-dense.tgz"
	ope, opw := fmt.Sprintf("e2e-mig-op-%d@example.com", h.pid), fmt.Sprintf("e2e-mig-operator-pw-%d", h.pid)
	_, ub := h.api.post("/api/users", fmt.Sprintf(`{"email":%q,"password":%q,"role":"operator","site_ids":[%d]}`, ope, opw, mid))
	opid := jsonField(ub, "id")
	cleanup := h.undo("remove the R-4.4 dense archive and operator", func() {
		h.remove(dense)
		if opid != "" {
			h.api.relogin()
			h.api.del("/api/users/" + opid)
		} else {
			h.deleteUserByEmail(ope)
		}
	})
	defer cleanup()
	noise := make([]byte, 400<<10)
	_, _ = rand.Read(noise)
	err := writeTGZ(dense, func(tw *tar.Writer) error {
		for _, f := range []struct {
			name string
			size int64
			r    io.Reader
		}{
			{"index.php", 6, strings.NewReader("<?php\n")},
			{"noise.bin", int64(len(noise)), bytes.NewReader(noise)},
			{"zeros.bin", 256 << 20, io.LimitReader(zeroReader{}, 256<<20)},
		} {
			if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: f.size, Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			if _, err := io.Copy(tw, f.r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Logf("fixture: %v", err)
	}
	body := func(extra string) string {
		return fmt.Sprintf(`{"archive":"shared/cg-e2e-dense.tgz","confirm":%q%s}`, ms, extra)
	}
	op := newAPIClient(h, ope, opw)
	op.login()
	h.check(t, "R-4.4: an operator of the site cannot change the expansion limit", code(op.post(fmt.Sprintf("/api/sites/%d/migration", mid), body(`,"max_ratio":1000`))), "403")
	_, b := h.api.post(fmt.Sprintf("/api/sites/%d/migration", mid), body(""))
	h.check(t, "R-4.4 control: the dense archive is refused at the default 200 times", h.api.waitTask(taskID(b), 300*time.Second), "failed")
	_, b = h.api.post(fmt.Sprintf("/api/sites/%d/migration", mid), body(`,"max_ratio":1000`))
	h.check(t, "R-4.4: with the limit raised to 1000 times the same archive migrates", h.api.waitTask(taskID(b), 600*time.Second), "succeeded")
	h.assert(t, "R-4.4: the raised migration's files are live", exists(mroot+"/current/zeros.bin"), "current/zeros.bin is missing")
}

// migrationPrefix (R-4.4) migrates a site whose database uses the table
// prefix xyz_ into the migration site, a fresh install on wp_. The dump is
// the site's own export with its tables and prefixed keys renamed, holding a
// marker post that the live site no longer has. The old agent imported it
// beside the wp_ tables and the site went on serving those: the marker was
// not found and wp-config.php still said wp_.
func (h *harness) migrationPrefix(t *testing.T, mid int64, ms, mu, mroot string) {
	marker := fmt.Sprintf("cg-e2e-prefix-marker-%d", h.pid)
	dump := mroot + "/shared/cg-e2e-xyz.sql"
	archive := mroot + "/shared/cg-e2e-xyz.tar.gz"
	cleanup := h.undo("remove the R-4.4 prefix migration inputs", func() { h.remove(dump, archive) })
	defer cleanup()
	cur := mroot + "/current"
	id := h.wp(mu, cur, "post", "create", "--post_title="+marker, "--post_name="+marker, "--post_status=publish", "--porcelain").Trim()
	if _, err := strconv.Atoi(id); err != nil {
		h.fail(t, "R-4.4 fixture: a marker post", "R-4.4 fixture: wp post create: %q", id)
		return
	}
	exported := mroot + "/shared/cg-e2e-wp.sql"
	if r := h.wp(mu, cur, "db", "export", exported, "--quiet"); !r.OK() {
		h.fail(t, "R-4.4 fixture: the database export", "R-4.4 fixture: wp db export: %s", r)
		return
	}
	raw, err := os.ReadFile(exported)
	h.remove(exported)
	if err != nil {
		h.fail(t, "R-4.4 fixture: the database export", "R-4.4 fixture: %v", err)
		return
	}
	// What another host's export with prefix xyz_ holds: its tables, and the
	// option and user-meta keys WordPress names after the prefix.
	sql := strings.NewReplacer("`wp_", "`xyz_", "'wp_user_roles'", "'xyz_user_roles'",
		"'wp_capabilities'", "'xyz_capabilities'", "'wp_user_level'", "'xyz_user_level'").Replace(string(raw))
	if err := h.writeAs(dump, sql, mu, 0o600); err != nil {
		t.Logf("fixture: %v", err)
	}
	// The live site loses the marker: only the dump has it now.
	h.wp(mu, cur, "post", "delete", id, "--force")
	h.check(t, "R-4.4 control: the live site no longer has the marker post", h.wp(mu, cur, "post", "list", "--name="+marker, "--format=count").Trim(), "0")
	if r := h.run("tar", "czf", archive, "-C", cur+"/", "."); !r.OK() {
		t.Logf("fixture: tar: %s", r)
	}

	// The switch made slow: a wp-cli stand-in ahead of the real one on the
	// agent's PATH sleeps before the URL rewrite, the last step of the
	// switch, for this site's user only (other lanes' sites are not slowed).
	// Meanwhile the site is probed: every request must get WordPress's
	// maintenance answer (503) or the old or new site, never the installer,
	// which anyone could complete with a backdoor in wp-config.php.
	const standIn = "/usr/local/sbin/wp"
	realWP, _ := exec.LookPath("wp")
	removeStandIn := h.undo("remove the R-4.4 wp-cli stand-in", func() { _ = os.Remove(standIn) })
	defer removeStandIn()
	h.step("install " + standIn)
	_ = os.WriteFile(standIn, []byte(fmt.Sprintf("#!/bin/sh\nif [ \"$(id -un)\" = %q ]; then case \" $* \" in *\" search-replace \"*) sleep 25 ;; esac; fi\nexec %s \"$@\"\n", mu, realWP)), 0o755)
	_ = os.Chmod(standIn, 0o755)

	var mu2 sync.Mutex
	var installer []string
	maintenance := 0
	done := make(chan struct{})
	probed := make(chan struct{})
	go func() {
		defer close(probed)
		for {
			select {
			case <-done:
				return
			case <-time.After(300 * time.Millisecond):
			}
			// wp-login.php is never page-cached, so it shows what WordPress
			// itself answers; install.php skips the maintenance check
			// (WP_INSTALLING) and must find the site installed.
			for _, path := range []string{"/wp-login.php", "/", "/wp-admin/install.php"} {
				r := h.get(ms, path)
				mu2.Lock()
				if r.Code == 503 && path == "/wp-login.php" {
					maintenance++
				}
				if contains(r.Body, "install.php?step=") || contains(r.Body, `name="weblog_title"`) ||
					strings.Contains(r.Header.Get("Location"), "install.php") {
					installer = append(installer, fmt.Sprintf("%s: %d", path, r.Code))
				}
				mu2.Unlock()
			}
		}
	}()
	_, b := h.api.post(fmt.Sprintf("/api/sites/%d/migration", mid),
		fmt.Sprintf(`{"archive":"shared/cg-e2e-xyz.tar.gz","sql":"shared/cg-e2e-xyz.sql","old_domain":"old-host.example.org","confirm":%q}`, ms))
	st := h.api.waitTask(taskID(b), 300*time.Second)
	close(done)
	<-probed
	removeStandIn()
	h.check(t, "R-4.4: a migration whose dump uses the prefix xyz_ succeeds", st, "succeeded")
	h.assert(t, "R-4.4: the site answers 503 (WordPress maintenance) while the migration switches it", maintenance > 0,
		"no request got 503 during the switch (wp-cli stand-in at %s)", realWP)
	h.assert(t, "R-4.4: no request reaches the WordPress installer during the migration", len(installer) == 0,
		"the installer was served: %v", installer)
	h.assert(t, "R-4.4: the site is out of maintenance after the migration", !exists(mroot+"/current/.maintenance"),
		"current/.maintenance is still there")
	h.check(t, "R-4.4: wp-config.php has the dump's table prefix", h.wp(mu, mroot+"/current", "config", "get", "table_prefix").Trim(), "xyz_")
	// Its permalink (/%postname%/, from the dump's options), over HTTP.
	r := h.get(ms, "/"+marker+"/")
	h.assert(t, "R-4.4: the migrated site serves the dump's marker post", r.Code == 200 && contains(r.Body, marker),
		"GET /%s/: %d, marker in the body: %v", marker, r.Code, contains(r.Body, marker))
	db := h.api.site(mid).Config.Database.Name
	left := h.run("mariadb", "-N", "-e", "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='"+db+"' AND TABLE_NAME LIKE 'wp\\_%'").Trim()
	h.check(t, "R-4.4: the fresh install's wp_ tables are gone (the database is the dump)", left, "0")
}

// writeTGZ writes a .tar.gz whose entries fill writes.
func writeTGZ(path string, fill func(*tar.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	err = fill(tw)
	for _, c := range []io.Closer{tw, gz, f} {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
