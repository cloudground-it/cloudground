package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/jobs"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Downloads of a backup and the destinations' credentials (SPEC.md §5.7,
// §5.8, #23). Restoring one file or folder is backup.restore with paths
// (backup_routes.go, §5.6).

// backupEnvFor is restic's credentials for repo: the object stores' keys
// (S3, B2) and, for an sftp: repository, the panel's SSH key. override
// holds secrets a settings request is about to save (sent masked: absent).
func (s *Server) backupEnvFor(ps state.PanelSettings, override map[string]string) map[string]string {
	secret := func(name string) string {
		if v, ok := override[name]; ok && v != secretMask {
			return v
		}
		v, err := s.Store.Secret(name)
		s.logIfErr(err, "Secret")
		return v
	}
	env := map[string]string{}
	for k, v := range map[string]string{
		"AWS_ACCESS_KEY_ID":     ps.BackupS3AccessKey,
		"AWS_SECRET_ACCESS_KEY": secret(state.SecretBackupS3SecretKey),
		"AWS_DEFAULT_REGION":    ps.BackupS3Region,
		"B2_ACCOUNT_ID":         secret(state.SecretBackupB2AccountID),
		"B2_ACCOUNT_KEY":        secret(state.SecretBackupB2AccountKey),
	} {
		if v != "" {
			env[k] = v
		}
	}
	if strings.HasPrefix(ps.BackupRepository, "sftp:") {
		if key := secret(state.SecretBackupSFTPKey); key != "" {
			env[envSFTPKey] = key
		}
	}
	return env
}

// envSFTPKey is the credential the agent takes an SFTP repository's key
// from (ops/backup SPEC §2).
const envSFTPKey = "CG_SFTP_KEY"

func (s *Server) handleArchiveBackup(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Part string `json:"part"`
	}
	if err := decode(r, &req); err != nil && !errors.Is(err, io.EOF) {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	part, err := rpc.ArchivePart(req.Part)
	if err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_, password, ok := s.backupConfig(w)
	if !ok {
		return
	}
	b := c.Backup
	task, err := s.startJob(jobReq{kind: "backup.archive", siteID: c.Site.ID,
		locks:    []string{jobs.RepoLock(b.Repository)},
		dedupKey: "backup.archive:" + strconv.FormatInt(b.ID, 10) + ":" + part,
		payload:  map[string]any{"backup_id": b.ID, "part": part},
	}, func(ctx context.Context, site state.Site, progress func(int, string)) error {
		progress(10, "Preparing "+b.SnapshotID+" ("+part+") for download")
		var res rpc.ArchiveResult
		if err := agentrpc.Into(ctx, s.Agent, agentrpc.ArchiveSnapshot, rpc.ArchiveParams{
			Site: site, Repository: b.Repository, Password: password, SnapshotID: b.SnapshotID, Part: part, Env: s.backupEnv(),
		}, &res); err != nil {
			return err
		}
		progress(100, fmt.Sprintf("Ready to download: %s (%d bytes); kept for a day", res.Name, res.Size))
		return nil
	})
	if err != nil {
		respondJobErr(w, err)
		return
	}
	respond(w, http.StatusAccepted, map[string]any{"task": task})
}

func (s *Server) handleDownloadArchive(w http.ResponseWriter, r *http.Request, c *call) {
	part, err := rpc.ArchivePart(r.URL.Query().Get("part"))
	if err != nil {
		respondErr(w, http.StatusBadRequest, err.Error())
		return
	}
	b, site := c.Backup, c.Site
	name := rpc.ArchiveName(b.SnapshotID, part)
	read := func(off int64) (rpc.ArchiveChunkResult, error) {
		var res rpc.ArchiveChunkResult
		err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ReadArchiveChunk, rpc.ArchiveChunkParams{
			Site: site, Name: name, Offset: off, Length: downloadChunk,
		}, &res)
		return res, err
	}
	first, err := read(0)
	if err != nil {
		if agentrpc.CodeOf(err) == agentrpc.CodeNotFound {
			respondErr(w, http.StatusNotFound, "prepare the download first")
			return
		}
		replyFailure(w, err)
		return
	}
	s.audit(c.Actor(), "backup.download", site.Domain, b.SnapshotID+" "+part)
	short := b.SnapshotID
	if len(short) > 12 {
		short = short[:12]
	}
	h := w.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", site.Domain+"-"+short+"-"+part+".tar.gz"))
	h.Set("Content-Length", strconv.FormatInt(first.Size, 10))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	size, off, chunk := first.Size, int64(0), first
	for {
		if _, err := w.Write(chunk.Data); err != nil {
			return
		}
		off += int64(len(chunk.Data))
		if off >= size {
			// Sent whole: the archive goes (SPEC.md §5.7).
			_ = agentrpc.Into(context.WithoutCancel(r.Context()), s.Agent, agentrpc.DeleteArchive, rpc.ArchiveChunkParams{Site: site, Name: name}, nil)
			return
		}
		if len(chunk.Data) == 0 {
			return
		}
		if chunk, err = read(off); err != nil {
			// The headers are gone: a short body is how the browser learns
			// the download failed.
			if s.Log != nil {
				s.Log.Warn("a backup download stopped", "site", site.Domain, "archive", name, "offset", off, "err", err)
			}
			return
		}
	}
}

// backupKeyComment names the panel's key in a destination's
// authorized_keys.
func backupKeyComment() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "panel"
	}
	return "cloudground-backup@" + host
}

// backupPublicKey is the authorized_keys line of a private key, "" for
// none.
func backupPublicKey(private string) (string, error) {
	if private == "" {
		return "", nil
	}
	signer, err := ssh.ParsePrivateKey([]byte(private))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + backupKeyComment(), nil
}

func (s *Server) handleGetBackupSSHKey(w http.ResponseWriter, r *http.Request, c *call) {
	private, err := s.Store.Secret(state.SecretBackupSFTPKey)
	if err != nil {
		internalError(w, "Secret", err, "could not read the backup key")
		return
	}
	pub, err := backupPublicKey(private)
	if err != nil {
		internalError(w, "backupPublicKey", err, "could not read the backup key")
		return
	}
	respond(w, http.StatusOK, map[string]string{"public_key": pub})
}

func (s *Server) handleNewBackupSSHKey(w http.ResponseWriter, r *http.Request, c *call) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		internalError(w, "GenerateKey", err, "")
		return
	}
	block, err := ssh.MarshalPrivateKey(priv, backupKeyComment())
	if err != nil {
		internalError(w, "MarshalPrivateKey", err, "")
		return
	}
	private := string(pem.EncodeToMemory(block))
	if err := s.Store.SetSecret(state.SecretBackupSFTPKey, private); err != nil {
		internalError(w, "SetSecret", err, "could not store the backup key")
		return
	}
	pub, err := backupPublicKey(private)
	if err != nil {
		internalError(w, "backupPublicKey", err, "")
		return
	}
	s.audit(c.Actor(), "backup.ssh_key", "panel", strings.Fields(pub)[0]+" key made")
	respond(w, http.StatusOK, map[string]string{"public_key": pub})
}
