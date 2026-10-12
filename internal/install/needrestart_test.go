package install

import (
	"regexp"
	"strings"
	"testing"

	"github.com/cloudground-it/cloudground/internal/install/managed"
)

// The needrestart override (#125) names the agent's transient units and
// nothing else: not the panel's services, not the sites' PHP masters.
func TestNeedrestartOverride(t *testing.T) {
	conf := render(t, managed.Needrestart, Vars{})
	var keys []*regexp.Regexp
	for _, l := range strings.Split(conf, "\n") {
		if strings.HasPrefix(l, "#") || strings.TrimSpace(l) == "" {
			continue
		}
		m := regexp.MustCompile(`^\$nrconf\{override_rc\}->\{qr\((\^cg-[a-z-]+-)\)\} = 0;$`).FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("unexpected line: %q", l)
		}
		keys = append(keys, regexp.MustCompile(m[1]))
	}
	match := func(unit string) bool {
		for _, k := range keys {
			if k.MatchString(unit) {
				return true
			}
		}
		return false
	}
	for _, u := range []string{"cg-helper-restic-1-2.service", "cg-restic-1-2.service", "cg-scan-cg-site-1-1-2.service",
		"cg-wp-cg-site-1-1-2.service", "cg-malware-signatures-1-2.service", "cg-clamav-install-1-2.service",
		"cg-apt-1-2.service", "cg-helper-test-1.service"} {
		if !match(u) {
			t.Errorf("%s is not kept from needrestart", u)
		}
	}
	for _, u := range []string{"cloudground-agent.service", "cloudground-api.service", "cloudground-php-cg-site-1.service",
		"nginx.service", "cg-site-1.slice", "cg-fpm-test-x-1-2.service"} {
		if match(u) {
			t.Errorf("%s must keep needrestart's normal handling", u)
		}
	}
}
