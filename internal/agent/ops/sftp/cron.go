package sftp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agent/ops/rootfile"
)

// The block an earlier build of the agent wrote into cron.deny and at.deny
// (SPEC §5.7). A user listed in cron.deny makes root's `crontab -u <user>`
// fail too ("cannot use this program", measured on Ubuntu 24.04's cron), so
// every site's crontab stopped installing. Nothing writes it any more; the
// agent removes it when it starts.
const (
	denyBegin = "# BEGIN CloudGround: site users may not edit their own crontab or at jobs"
	denyEnd   = "# END CloudGround"
)

// RemoveDenyBlocks takes the old block out of cron.deny and at.deny. Lines
// outside it stay; a file left empty is removed (an empty cron.deny and none
// mean the same to cron).
func (o *Ops) RemoveDenyBlocks() error {
	var errs []error
	for _, p := range []string{o.ShellPaths.CronDeny, o.ShellPaths.AtDeny} {
		if p != "" {
			errs = append(errs, removeDenyBlock(p))
		}
	}
	return errors.Join(errs...)
}

func removeDenyBlock(path string) error {
	old, err := rootfile.ReadNoFollow(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !strings.Contains(string(old), denyBegin) {
		return nil
	}
	var keep []string
	in := false
	for _, l := range strings.Split(strings.TrimRight(string(old), "\n"), "\n") {
		switch {
		case l == denyBegin:
			in = true
		case in && l == denyEnd:
			in = false
		case !in:
			keep = append(keep, l)
		}
	}
	if strings.TrimSpace(strings.Join(keep, "")) == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	if err := rootfile.Replace(path, []byte(strings.Join(keep, "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
