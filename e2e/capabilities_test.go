//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type capabilitiesView struct {
	PHPVersions []string `json:"php_versions"`
	DefaultPHP  string   `json:"default_php"`
	Services    []struct {
		Name        string `json:"name"`
		Restartable bool   `json:"restartable"`
	} `json:"services"`
	SitesRoot string `json:"sites_root"`
}

// fpmVersionsOnBox lists the PHP versions whose FPM binary is an executable
// regular file in /usr/sbin, newest first: what the box can run, read here
// without asking the product.
func fpmVersionsOnBox() []string {
	matches, _ := filepath.Glob("/usr/sbin/php-fpm8.*")
	var out []string
	for _, m := range matches {
		v := strings.TrimPrefix(filepath.Base(m), "php-fpm")
		fi, err := os.Stat(m)
		if err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 && len(v) == 3 && v[1] == '.' {
			out = append(out, v)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// capabilities (R-4.7): the panel offers the PHP versions the agent finds
// on the box, not a list of its own, and refuses one the box does not have
// before anything is created.
func (h *harness) capabilities(t *testing.T) {
	c, b := h.api.get("/api/system/capabilities")
	h.check(t, "R-4.7: the capabilities answer", fmt.Sprint(c), "200")
	var v capabilitiesView
	_ = json.Unmarshal(b, &v)
	box := fpmVersionsOnBox()
	h.check(t, "R-4.7: the PHP versions offered are the FPM binaries on the box",
		strings.Join(v.PHPVersions, ","), strings.Join(box, ","))
	h.assert(t, "R-4.7: the default PHP of a new site is installed",
		v.DefaultPHP != "" && strings.Contains(","+strings.Join(box, ",")+",", ","+v.DefaultPHP+","),
		"default %q, installed %v", v.DefaultPHP, box)
	h.check(t, "R-4.7: sites live where the agent keeps them", v.SitesRoot, "/srv/sites")
	restartable := map[string]bool{}
	for _, s := range v.Services {
		restartable[s.Name] = s.Restartable
	}
	h.assert(t, "R-4.7: nginx, MariaDB and Redis can be restarted from the panel",
		restartable["nginx"] && restartable["mariadb"] && restartable["redis"], "services %+v", v.Services)

	// A version the box lacks (the oldest 8.x missing from it) is refused
	// at the API, with the reason.
	missing := ""
	for d := 0; d <= 9 && missing == ""; d++ {
		if cand := fmt.Sprintf("8.%d", d); !strings.Contains(","+strings.Join(box, ",")+",", ","+cand+",") {
			missing = cand
		}
	}
	dom := h.dom(fmt.Sprintf("capsphp-%d", h.pid))
	c, b = h.api.post("/api/sites", fmt.Sprintf(`{"domain":%q,"type":"php","php_version":%q}`, dom, missing))
	h.check(t, "R-4.7: a PHP version not on the box is refused", fmt.Sprint(c), "400")
	h.assert(t, "R-4.7: the refusal names the missing version",
		strings.Contains(string(b), "PHP "+missing+" is not installed on this server"), "%s", b)
	if c/100 == 2 {
		// The product created it anyway: do not leave it behind.
		var created struct {
			Site struct {
				ID int64 `json:"id"`
			} `json:"site"`
		}
		if json.Unmarshal(b, &created) == nil && created.Site.ID != 0 {
			h.api.waitActive(created.Site.ID, 5*time.Minute)
			h.api.deleteSite(created.Site.ID)
			h.api.teardownGone(created.Site.ID)
		}
	}
	h.check(t, "R-4.7: the panel does not restart its own API from the services page",
		code(h.api.post("/api/services/cloudground-api/restart", `{}`)), "400")
}
