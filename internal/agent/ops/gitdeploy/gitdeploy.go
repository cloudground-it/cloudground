// Package gitdeploy fetches a branch of a site's repository as the site
// user, exports its tree and runs the site's build command there. It makes
// no release: DeployRelease builds one from the export. SPEC.md in this
// directory is the contract.
package gitdeploy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/host"
	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/phpfpm"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Where the work happens, inside the site's own tmp/ (SPEC §2).
const (
	WorkDir    = "tmp/cg-git"
	repoDir    = "repo"
	exportDir  = "export"
	knownHosts = "known_hosts"
)

// Limits of the two steps, and how much of their output is kept.
const (
	FetchLimit  = 20 * time.Minute
	BuildLimit  = 30 * time.Minute
	OutputLimit = 4 << 10
)

// commitMark starts the last line the fetch script prints.
const commitMark = "CG-COMMIT "

// fetchScript fetches and exports. Arguments: $1 the work directory, $2 the
// repository, $3 the branch, $4 the release ID, $5 "key" when a deploy key
// is on standard input. Everything it prints, both streams, is the output;
// the last line is the commit.
const fetchScript = `exec 2>&1
set -eu
umask 022
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export HOME="$tmp" GIT_TERMINAL_PROMPT=0 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_ALLOW_PROTOCOL=https:ssh:file
ssh_cmd="ssh -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$1/known_hosts"
if [ "$5" = key ]; then
	( umask 077; cat > "$tmp/key" )
	ssh_cmd="$ssh_cmd -i $tmp/key"
fi
export GIT_SSH_COMMAND="$ssh_cmd"
cd "$1"
[ -d repo/.git ] || { rm -rf repo; git init -q repo; }
git -C repo remote remove origin 2>/dev/null || true
git -C repo remote add origin "$2"
git -C repo fetch --quiet --depth 1 --no-tags origin "+refs/heads/$3:refs/remotes/origin/$3"
commit=$(git -C repo rev-parse "refs/remotes/origin/$3^{commit}")
rm -rf export
mkdir -p "export/$4"
git -C repo archive --format=tar "$commit" | tar -x -f - -C "export/$4"
[ -n "$(ls -A "export/$4")" ] || { echo "the branch has no files"; exit 1; }
echo "` + commitMark + `$commit"
`

// buildScript runs the build command: $1 the command.
const buildScript = `exec 2>&1; exec /bin/sh -c "$1"`

// Ops holds what the handler uses.
type Ops struct {
	// Open opens a site's tree; what it creates belongs to the site user.
	Open func(state.Site) (*sitefs.Root, error)
	// Exec runs systemd-run.
	Exec host.Exec
}

// Register wires this package's method into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.GitSync, o.Sync)
}

var runs atomic.Uint64

// commandEnv is the environment of systemd-run itself; the service's own is
// serviceEnv.
var commandEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}

func serviceEnv() []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "SHELL=/bin/sh", "TMPDIR=/tmp"}
}

// service is the transient service of one step: the site user, the site's
// slice and run sandbox.
func service(s state.Site, kind, what, dir string, limit time.Duration, argv []string) host.Service {
	return host.Service{
		Unit:        fmt.Sprintf("cg-git-%s%s-%d-%d.service", kind, s.SystemUser, time.Now().UnixNano(), runs.Add(1)),
		Description: "CloudGround git deploy " + what + " for " + s.Domain,
		User:        s.SystemUser, Slice: site.Slice(s.SystemUser), Dir: dir,
		Env: serviceEnv(), Props: phpfpm.RunSandbox(s), MaxRuntime: limit, Pipe: true,
		Argv: argv,
	}
}

// FetchService is the fetch's service (tests read its argv).
func FetchService(s state.Site, p rpc.GitSyncParams) host.Service {
	root := s.RootPath
	hasKey := "nokey"
	if p.DeployKey != "" {
		hasKey = "key"
	}
	return service(s, "", "fetch", filepath.Join(root, WorkDir), FetchLimit,
		[]string{"/bin/sh", "-c", fetchScript, "cg-git", filepath.Join(root, WorkDir), p.Repo, p.Branch, p.ReleaseID, hasKey})
}

// BuildService is the build's service.
func BuildService(s state.Site, p rpc.GitSyncParams, dir string) host.Service {
	return service(s, "build-", "build", dir, BuildLimit, []string{"/bin/sh", "-c", buildScript, "cg-git-build", p.BuildCommand})
}

// stepResult is how one service ended.
type stepResult struct {
	output string
	ok     bool
}

var resultRe = regexp.MustCompile(`(?m)^[ \t]*Finished with result: (\S+)`)

// runStep runs a service to its end and keeps the tail of its output. A
// command that ran and failed (non-zero exit, a timeout) is not an error:
// ok is false. One that could not be run is.
func (o *Ops) runStep(ctx context.Context, svc host.Service, stdin string) (stepResult, error) {
	// Not --quiet: systemd-run's "Finished with result" line tells a
	// timeout from an exit status.
	args := slices.DeleteFunc(host.ServiceArgs(svc), func(a string) bool { return a == "--quiet" })
	out := host.NewTailBuffer(OutputLimit)
	c := host.Cmd{Name: "systemd-run", Args: args, Env: commandEnv, Stdout: out, Timeout: svc.MaxRuntime + time.Minute}
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	res, err := o.Exec.Run(ctx, c)
	if err != nil && ctx.Err() != nil {
		// systemd-run was killed with the request; the service is not its
		// child and would run on.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_, _ = o.Exec.Run(sctx, host.Cmd{Name: "systemctl", Args: []string{"stop", svc.Unit}, Env: commandEnv})
		cancel()
		return stepResult{}, ctx.Err()
	}
	text := string(out.Bytes())
	var ee *host.ExitError
	switch {
	case err == nil:
		return stepResult{output: text, ok: true}, nil
	case errors.As(err, &ee):
		// systemd-run says why on its own stderr; the job's output is on
		// stdout.
		if m := resultRe.FindStringSubmatch(string(res.Stderr)); m != nil && m[1] == "timeout" {
			text += "\n(stopped: it took longer than " + svc.MaxRuntime.String() + ")"
		}
		return stepResult{output: text}, nil
	}
	return stepResult{}, fmt.Errorf("run the git deploy service: %w", err)
}

// Sync is GitSync (SPEC §1). The server holds the site's lock.
func (o *Ops) Sync(ctx context.Context, p rpc.GitSyncParams) (rpc.GitSyncResult, error) {
	s := p.Site
	if !site.ValidSystemUser(s.SystemUser) {
		return rpc.GitSyncResult{}, agentrpc.Invalid("invalid system user %q", s.SystemUser)
	}
	root, err := o.Open(s)
	if err != nil {
		return rpc.GitSyncResult{}, err
	}
	defer root.Close()
	// tmp/ is the site user's: the folder and what is in it are made as
	// the site user, 0700, so no other account can read the repository.
	if err := root.MkdirAll(WorkDir, 0o700); err != nil {
		return rpc.GitSyncResult{}, fmt.Errorf("make %s: %w", WorkDir, err)
	}
	work := filepath.Join(s.RootPath, WorkDir)

	key := ""
	if p.DeployKey != "" {
		key = strings.TrimSpace(p.DeployKey) + "\n"
	}
	r, err := o.runStep(ctx, FetchService(s, p), key)
	if err != nil {
		return rpc.GitSyncResult{}, err
	}
	commit := ""
	if r.ok {
		lines := strings.Split(strings.TrimSpace(r.output), "\n")
		if last := lines[len(lines)-1]; strings.HasPrefix(last, commitMark) {
			commit = strings.TrimPrefix(last, commitMark)
			r.output = strings.Join(lines[:len(lines)-1], "\n")
		}
		if !validCommit(commit) {
			r.ok = false
		}
	}
	if !r.ok {
		return rpc.GitSyncResult{Output: r.output, Failed: "fetch"}, nil
	}
	dir := filepath.Join(work, exportDir, p.ReleaseID)
	res := rpc.GitSyncResult{Commit: commit, SourceDir: dir}
	if p.BuildCommand == "" {
		return res, nil
	}
	b, err := o.runStep(ctx, BuildService(s, p, dir), "")
	if err != nil {
		return rpc.GitSyncResult{}, err
	}
	res.Output = b.output
	if !b.ok {
		res.Failed, res.SourceDir = "build", ""
		_ = root.RemoveAll(filepath.Join(WorkDir, exportDir, p.ReleaseID))
	}
	return res, nil
}

var commitRe = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

func validCommit(c string) bool { return commitRe.MatchString(c) }
