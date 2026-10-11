package phpfpm

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// header is the first line of every file this package renders.
const header = "Managed by CloudGround — do not edit. Changes are detected as drift."

// lines collects a rendered file one line at a time.
type lines struct{ strings.Builder }

func (l *lines) add(format string, a ...any) {
	fmt.Fprintf(&l.Builder, format, a...)
	l.WriteByte('\n')
}

// addAll writes each of text's lines verbatim (a block of comments).
func (l *lines) addAll(text ...string) {
	for _, t := range text {
		l.WriteString(t)
		l.WriteByte('\n')
	}
}

// RenderUnit renders the systemd unit of site's master (SPEC §4).
func RenderUnit(site state.Site, phpBinDir string) (name, content string, err error) {
	if site.SystemUser == "" || site.PHPVersion == "" || site.RootPath == "" {
		return "", "", fmt.Errorf("site %q missing system user, php version or root", site.Domain)
	}
	u := site.SystemUser
	c := SizeSiteCaches(site.Type)
	cmd := []string{
		filepath.Join(phpBinDir, "php-fpm"+site.PHPVersion), "--nodaemonize", "--fpm-config", confPath(u),
		fmt.Sprintf("-d opcache.memory_consumption=%d", c.OPcacheMB),
		fmt.Sprintf("-d opcache.interned_strings_buffer=%d", c.InternedMB),
		fmt.Sprintf("-d opcache.max_accelerated_files=%d", c.MaxFiles),
		fmt.Sprintf("-d opcache.jit_buffer_size=%dM", c.JITMB),
		fmt.Sprintf("-d apc.shm_size=%dM", c.APCuMB),
	}
	var f lines
	f.add("# %s", header)
	f.add("[Unit]")
	f.add("Description=PHP-FPM for %s", site.Domain)
	f.add("After=network.target mariadb.service")
	f.add("")
	f.add("[Service]")
	f.add("Type=notify")
	f.add("User=%s", u)
	f.add("Group=%s", u)
	f.add("RuntimeDirectory=cloudground/php/%s", u)
	f.add("RuntimeDirectoryMode=0750")
	f.add("ExecStart=%s", strings.Join(cmd, " "))
	f.add("ExecReload=/bin/kill -USR2 $MAINPID")
	f.add("Restart=on-failure")
	f.add("RestartSec=2")
	f.add("LimitNOFILE=65536")
	f.add("")
	f.addAll(
		"# The site sees its own tree and nothing of any other site: /srv/sites is an",
		"# empty tmpfs with only this site bound into it, and the same for its logs.",
		"# This, not open_basedir, is the tenant boundary.",
	)
	f.addAll(Sandbox(site)...)
	f.add("")
	f.add("[Install]")
	f.add("WantedBy=multi-user.target")
	return UnitName(u), f.String(), nil
}
