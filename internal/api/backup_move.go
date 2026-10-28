package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/backuprepo"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Moving the backup repository (SPEC.md §5.9, #192): the records and the
// setting point at the new place only once it holds every recorded
// snapshot.

// moveRequest is the body of POST /api/backups/repository/move. The
// password and the credentials are the new repository's; empty keeps the
// current setting's.
type moveRequest struct {
	Mode         string `json:"mode"`
	Repository   string `json:"repository"`
	Password     string `json:"password"`
	S3AccessKey  string `json:"s3_access_key"`
	S3SecretKey  string `json:"s3_secret_key"`
	S3Region     string `json:"s3_region"`
	B2AccountID  string `json:"b2_account_id"`
	B2AccountKey string `json:"b2_account_key"`
}

// The two modes: the panel copies, or the files are already there.
const (
	moveCopy  = "copy"
	moveMoved = "moved"
)

// movePayload is backup.move's record: no secret in it.
type movePayload struct {
	Mode string `json:"mode"`
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) handleMoveBackupRepository(w http.ResponseWriter, r *http.Request, c *call) {
	var req moveRequest
	if !decodeOr400(w, r, &req) {
		return
	}
	if req.Mode != moveCopy && req.Mode != moveMoved {
		respondErr(w, http.StatusBadRequest, "mode must be copy or moved")
		return
	}
	from, _, ok := s.backupConfig(w)
	if !ok {
		return
	}
	req.Repository = strings.TrimSpace(req.Repository)
	if req.Repository == "" {
		respondErr(w, http.StatusBadRequest, "give the new repository")
		return
	}
	if err := backuprepo.Validate(r.Context(), req.Repository, backuprepo.Options{
		AllowPrivate: s.BackupPrivateEndpoint,
		Rclone:       func() bool { return true }, // the agent's to say
	}); err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if state.RepoKey(req.Repository) == state.RepoKey(from) {
		respondErr(w, http.StatusBadRequest, "this is already the backup repository")
		return
	}
	to := maskRepositoryCredentials(req.Repository)
	s.audit(c.Actor(), "backup.move", to, req.Mode+" to "+to)
	task, err := s.startJob(jobReq{kind: "backup.move",
		locks:   []string{jobs.RepoLock(from), jobs.RepoLock(req.Repository)},
		payload: movePayload{Mode: req.Mode, From: maskRepositoryCredentials(from), To: to},
	}, func(ctx context.Context, _ state.Site, progress func(int, string)) error {
		return s.moveRepository(ctx, from, req, progress)
	})
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

// moveRepository is the backup.move job (§5.9). Until the transaction of
// its last step, nothing of the panel names the new repository.
func (s *Server) moveRepository(ctx context.Context, from string, req moveRequest, progress func(int, string)) error {
	cur, password, err := s.backupRepository()
	if err != nil {
		return err
	}
	if cur == "" || password == "" {
		return errBackupsOff
	}
	if state.RepoKey(cur) != state.RepoKey(from) {
		return errors.New("the backup repository changed while the move waited; ask again")
	}
	records, err := s.Store.BackupsInRepository(from)
	if err != nil {
		return err
	}
	expect := make([]string, 0, len(records))
	for _, b := range records {
		if rpc.ValidSnapshotID(b.SnapshotID) {
			expect = append(expect, b.SnapshotID)
		}
	}
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return err
	}
	fromEnv := s.backupEnvFor(ps, nil)
	next, secrets := s.movedSettings(ps, req)
	toPassword := password
	if req.Password != "" {
		toPassword = req.Password
	}
	toEnv := s.backupEnvFor(next, secrets)
	src := rpc.TestBackupParams{Repository: from, Password: password, Env: fromEnv}
	dst := rpc.TestBackupParams{Repository: req.Repository, Password: toPassword, Env: toEnv}

	progress(5, "Reading the current repository")
	var old rpc.RepositoryInfoResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.RepositoryInfo, rpc.RepositoryInfoParams{
		Repository: src.Repository, Password: src.Password, Env: src.Env}, &old); err != nil {
		return fmt.Errorf("the current repository cannot be read: %w", err)
	}

	if req.Mode == moveCopy {
		progress(15, fmt.Sprintf("Copying %d snapshots to the new repository; this can take a while", old.Snapshots))
		var res rpc.CopyRepositoryResult
		if err := agentrpc.Into(ctx, s.Agent, agentrpc.CopyRepository, rpc.CopyRepositoryParams{From: src, To: dst}, &res); err != nil {
			return fmt.Errorf("the copy failed; the backups stay where they are: %w", err)
		}
	}

	progress(80, fmt.Sprintf("Checking the new repository holds the %d recorded backups", len(expect)))
	var info rpc.RepositoryInfoResult
	if err := agentrpc.Into(ctx, s.Agent, agentrpc.RepositoryInfo, rpc.RepositoryInfoParams{
		Repository: dst.Repository, Password: dst.Password, Env: dst.Env, Expect: expect}, &info); err != nil {
		if req.Mode == moveMoved && agentrpc.CodeOf(err) == agentrpc.CodeNotFound {
			return errors.New("the new repository does not exist: copy the backups there first, or let the panel copy them")
		}
		return fmt.Errorf("the new repository cannot be read: %w", err)
	}
	if req.Mode == moveMoved && info.ID != old.ID {
		return fmt.Errorf("this is another repository (ID %s), not a copy of the current one (ID %s)", shortID(info.ID), shortID(old.ID))
	}
	if len(info.Missing) > 0 {
		return fmt.Errorf("%d recorded backups are not in the new repository (%s); the backups stay where they are",
			len(info.Missing), strings.Join(info.Missing[:min(3, len(info.Missing))], ", "))
	}
	if err := checkpoint(ctx, progress, "pointing the backups at the new repository"); err != nil {
		return err
	}

	moved, err := s.Store.MoveBackupRepository(from, func(p *state.PanelSettings) error {
		p.BackupRepository = next.BackupRepository
		p.BackupS3AccessKey = next.BackupS3AccessKey
		p.BackupS3Region = next.BackupS3Region
		return nil
	}, secrets)
	if errors.Is(err, state.ErrRepositoryChanged) {
		return errors.New("the backup repository changed while the move ran; nothing was moved")
	}
	if err != nil {
		return err
	}
	verified := s.verifyLatest(req.Repository)
	detail := fmt.Sprintf("%d backups moved from %s to %s (%s)", moved, maskRepositoryCredentials(from), maskRepositoryCredentials(req.Repository), req.Mode)
	s.audit("system", "backup.moved", maskRepositoryCredentials(req.Repository), detail)
	progress(100, fmt.Sprintf("%s; the newest backup of %d sites is being test-restored from the new repository", detail, verified))
	return nil
}

// movedSettings is the settings as the move leaves them, and the secrets it
// writes: the request's credentials, where it gives them.
func (s *Server) movedSettings(ps state.PanelSettings, req moveRequest) (state.PanelSettings, map[string]string) {
	next := ps
	next.BackupRepository = req.Repository
	if req.S3AccessKey != "" {
		next.BackupS3AccessKey = req.S3AccessKey
	}
	if req.S3Region != "" {
		next.BackupS3Region = req.S3Region
	}
	secrets := map[string]string{}
	for name, v := range map[string]string{
		state.SecretBackupPassword:     req.Password,
		state.SecretBackupS3SecretKey:  req.S3SecretKey,
		state.SecretBackupB2AccountID:  req.B2AccountID,
		state.SecretBackupB2AccountKey: req.B2AccountKey,
	} {
		if v != "" {
			secrets[name] = v
		}
	}
	return next, secrets
}

// verifyLatest queues the restore test of each site's newest record in
// repo and says how many it queued.
func (s *Server) verifyLatest(repo string) int {
	records, err := s.Store.BackupsInRepository(repo)
	if err != nil {
		s.logIfErr(err, "BackupsInRepository")
		return 0
	}
	newest := map[int64]state.Backup{}
	for _, b := range records {
		newest[b.SiteID] = b // oldest first: the last one stays
	}
	n := 0
	for _, b := range newest {
		if _, err := s.enqueueVerify(b, false); err != nil {
			s.logIfErr(err, "enqueueVerify")
			continue
		}
		n++
	}
	return n
}

func shortID(id string) string { return id[:min(8, len(id))] }
