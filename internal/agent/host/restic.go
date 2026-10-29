package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Typed restic failures, from its documented exit codes (restic >= 0.17).
var (
	ErrIncomplete    = errors.New("restic: some source files could not be read")
	ErrRepoNotFound  = errors.New("restic: repository does not exist")
	ErrRepoLocked    = errors.New("restic: repository is locked")
	ErrWrongPassword = errors.New("restic: wrong repository password")
	ErrInterrupted   = errors.New("restic: interrupted")
)

var resticCodes = map[int]error{3: ErrIncomplete, 10: ErrRepoNotFound, 11: ErrRepoLocked, 12: ErrWrongPassword, 130: ErrInterrupted}

// ResticGrace is how long a cancelled restic has, after SIGINT, to remove its
// lock from the repository and exit before its process group is killed. A
// remote backend's delete takes a round trip or two.
const ResticGrace = 20 * time.Second

// ResticEnvKeys are the only variables a caller may hand restic: object
// storage credentials and region.
var ResticEnvKeys = map[string]bool{
	"AWS_ACCESS_KEY_ID": true, "AWS_SECRET_ACCESS_KEY": true, "AWS_DEFAULT_REGION": true,
	"B2_ACCOUNT_ID": true, "B2_ACCOUNT_KEY": true,
}

// Repo is a restic repository and its secrets.
type Repo struct {
	URL      string
	Password string
	Env      map[string]string
	// SSHKey is the panel's private key for an sftp: repository (#23);
	// empty for any other, or to use root's own ssh configuration.
	SSHKey string
	// From is the source of a `restic copy` (#192): it reaches restic as
	// RESTIC_FROM_REPOSITORY and RESTIC_FROM_PASSWORD, its Env and SSHKey
	// with this repository's (the caller has checked they agree). Nil for
	// every other command.
	From *Repo
}

// Restic runs restic against one repository.
type Restic interface {
	Run(ctx context.Context, repo Repo, args ...string) ([]byte, error)
	RunIO(ctx context.Context, repo Repo, stdin io.Reader, stdout io.Writer, args ...string) error
	// RunSandboxed runs a restic command that walks a tenant's tree (a files
	// backup) where only that tree is visible (TreeSandbox).
	RunSandboxed(ctx context.Context, repo Repo, sb TreeSandbox, args ...string) ([]byte, error)
}

// ResticCLI is the real Restic.
type ResticCLI struct {
	Exec Exec
	// BaseEnv is the environment the secrets are added to; nil means the
	// agent's own, with every RESTIC_*, AWS_* and B2_* variable removed.
	BaseEnv []string
	// Transient runs RunSandboxed in a transient service with its own mount
	// namespace (the agent's wiring); false runs restic directly (tests,
	// which have no systemd).
	Transient bool
	// AgentBinary is the panel-agent the sandboxed run starts restic-exec
	// from; "" is /usr/local/bin/panel-agent.
	AgentBinary string
	// SSHDir holds an SFTP repository's key and known hosts; "" is
	// DefaultSSHDir (restic_sftp.go).
	SSHDir string
}

// NewRestic returns the real adapter.
func NewRestic(e Exec) *ResticCLI { return &ResticCLI{Exec: e} }

// ResticError is a restic exit, with its typed meaning when it has one.
type ResticError struct {
	*ExitError
	Kind error // one of the Err* above, or nil
}

func (e *ResticError) Unwrap() []error {
	if e.Kind == nil {
		return []error{e.ExitError}
	}
	return []error{e.Kind, e.ExitError}
}

func (r *ResticCLI) env(repo Repo) []string {
	base := r.BaseEnv
	if base == nil {
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(k, "RESTIC_") && k != "RESTIC_CACHE_DIR" || strings.HasPrefix(k, "AWS_") || strings.HasPrefix(k, "B2_") {
				continue
			}
			base = append(base, kv)
		}
	}
	env := append(append([]string(nil), base...), "RESTIC_REPOSITORY="+repo.URL, "RESTIC_PASSWORD="+repo.Password)
	seen := map[string]bool{}
	add := func(vars map[string]string) {
		for k, v := range vars {
			if ResticEnvKeys[k] && v != "" && !strings.ContainsAny(v, "\x00\n") && !seen[k] {
				seen[k] = true
				env = append(env, k+"="+v)
			}
		}
	}
	add(repo.Env)
	if repo.From != nil {
		env = append(env, "RESTIC_FROM_REPOSITORY="+repo.From.URL, "RESTIC_FROM_PASSWORD="+repo.From.Password)
		add(repo.From.Env)
	}
	return env
}

func (r *ResticCLI) Run(ctx context.Context, repo Repo, args ...string) ([]byte, error) {
	out := NewResticOutput(args)
	err := r.RunIO(ctx, repo, nil, out, args...)
	return out.Bytes(), withOutputErr(err, out)
}

func (r *ResticCLI) RunIO(ctx context.Context, repo Repo, stdin io.Reader, stdout io.Writer, args ...string) error {
	if repo.URL == "" || repo.Password == "" || repo.From != nil && (repo.From.URL == "" || repo.From.Password == "") {
		return errors.New("restic: repository and password are required")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	keyed := repo
	if repo.SSHKey == "" && repo.From != nil {
		// One sftp.command serves both repositories of a copy: the
		// source's, when only the source is reached over SFTP.
		keyed = *repo.From
	}
	sftp, err := r.sftpArgs(keyed)
	if err != nil {
		return err
	}
	args = append(sftp, args...)
	// The caller's context carries the deadline: data-moving commands take
	// hours, metadata ones should not. The Exec default must not cut them.
	// A restic that is stopped removes its lock from the repository on
	// SIGINT; killed outright, it leaves the lock behind (SPEC: restic).
	_, err = r.Exec.Run(ctx, Cmd{Name: "restic", Args: args, Env: r.env(repo), Stdin: stdin, Stdout: stdout,
		Timeout: 24 * time.Hour, Interrupt: ResticGrace})
	var ee *ExitError
	if errors.As(err, &ee) {
		return &ResticError{ExitError: ee, Kind: resticCodes[ee.Code]}
	}
	return err
}

// ResticVer is the restic the agent runs, from `restic version`.
type ResticVer struct {
	Major, Minor int
	Line         string // restic's own line, e.g. "restic 0.19.1 compiled with go1.24.4 on linux/amd64"
}

// TypedExitCodes reports whether this restic documents its exit codes
// (0.17 and later): before, every failure is exit 1.
func (v ResticVer) TypedExitCodes() bool { return v.Major > 0 || v.Minor >= 17 }

// ResticVersion asks the first restic on PATH for its version (SPEC: restic,
// Version). The agent logs a warning at start for one without typed exit
// codes; Capabilities can report it.
func ResticVersion(ctx context.Context, e Exec) (ResticVer, error) {
	res, err := e.Run(ctx, Cmd{Name: "restic", Args: []string{"version"}, Timeout: 30 * time.Second})
	if err != nil {
		return ResticVer{}, err
	}
	return ParseResticVersion(string(res.Stdout))
}

// ParseResticVersion reads the output of `restic version`.
func ParseResticVersion(out string) (ResticVer, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != "restic" {
		return ResticVer{}, fmt.Errorf("restic version: unexpected output %q", line)
	}
	var v ResticVer
	if _, err := fmt.Sscanf(f[1], "%d.%d", &v.Major, &v.Minor); err != nil {
		return ResticVer{}, fmt.Errorf("restic version: unexpected version %q", f[1])
	}
	v.Line = line
	return v, nil
}

// BackupSummary is the summary message of `restic backup --json`.
type BackupSummary struct {
	SnapshotID          string  `json:"snapshot_id"`
	FilesNew            int64   `json:"files_new"`
	FilesChanged        int64   `json:"files_changed"`
	FilesUnmodified     int64   `json:"files_unmodified"`
	DataAdded           int64   `json:"data_added"`
	TotalFilesProcessed int64   `json:"total_files_processed"`
	TotalBytesProcessed int64   `json:"total_bytes_processed"`
	TotalDuration       float64 `json:"total_duration"`
}

// DecodeBackupSummary finds the summary among backup's JSON lines.
func DecodeBackupSummary(out []byte) (BackupSummary, error) {
	var found *BackupSummary
	for _, line := range bytes.Split(out, []byte("\n")) {
		var msg struct {
			MessageType string `json:"message_type"`
			BackupSummary
		}
		if json.Unmarshal(line, &msg) == nil && msg.MessageType == "summary" {
			s := msg.BackupSummary
			found = &s
		}
	}
	if found == nil || found.SnapshotID == "" {
		return BackupSummary{}, errors.New("restic backup: no summary in its output")
	}
	return *found, nil
}

// Snapshot is one entry of `restic snapshots --json`.
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags"`
	// Original is the ID the snapshot had before `restic copy` (or a
	// rewrite) gave it a new one; "" for a snapshot never copied.
	Original string `json:"original,omitempty"`
}

// RepoConfig is what `restic cat config` prints that the agent reads.
type RepoConfig struct {
	ID string `json:"id"`
}

// DecodeRepoConfig decodes `restic cat config`.
func DecodeRepoConfig(out []byte) (RepoConfig, error) {
	var c RepoConfig
	if err := json.Unmarshal(bytes.TrimSpace(out), &c); err != nil {
		return RepoConfig{}, fmt.Errorf("restic cat config: %w", err)
	}
	if len(c.ID) != 64 || strings.Trim(c.ID, "0123456789abcdef") != "" {
		return RepoConfig{}, errors.New("restic cat config: no repository id")
	}
	return c, nil
}

// DecodeSnapshots decodes `restic snapshots --json`.
func DecodeSnapshots(out []byte) ([]Snapshot, error) {
	var snaps []Snapshot
	if err := json.Unmarshal(bytes.TrimSpace(out), &snaps); err != nil {
		return nil, fmt.Errorf("restic snapshots: %w", err)
	}
	return snaps, nil
}

// ForgetGroup is one group of `restic forget --dry-run --json`.
type ForgetGroup struct {
	Tags   []string   `json:"tags"`
	Paths  []string   `json:"paths"`
	Keep   []Snapshot `json:"keep"`
	Remove []Snapshot `json:"remove"`
}

// DecodeForgetPlan decodes forget's JSON plan. restic may print other lines
// first; the plan is the line that is a JSON array.
func DecodeForgetPlan(out []byte) ([]ForgetGroup, error) {
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("[")) {
			continue
		}
		var groups []ForgetGroup
		if json.Unmarshal(line, &groups) == nil {
			return groups, nil
		}
	}
	return nil, errors.New("restic forget: no plan in its output")
}

var _ Restic = (*ResticCLI)(nil)
