package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/backuprepo"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
	"github.com/cloudground-it/cloudground/internal/version"
)

// /api/settings (SPEC.md §12): a flat map of string keys, as the settings
// page and cgctl have always used it, over the typed state.PanelSettings
// and the secret store.

// secretMask stands for a secret that is set; sent back, it means "unchanged".
const secretMask = "••••••••"

// setting is one key of the map that is a field of PanelSettings.
type setting struct {
	show func(state.PanelSettings) string
	// parse sets the field from a submitted value; nil: read-only.
	parse func(*state.PanelSettings, string) error
}

// textSetting is a setting kept as it is typed.
func textSetting(field func(*state.PanelSettings) *string) setting {
	return setting{
		show:  func(p state.PanelSettings) string { return *field(&p) },
		parse: func(p *state.PanelSettings, v string) error { *field(p) = v; return nil },
	}
}

// flagSetting is a boolean setting written on as onText, off as offText;
// accepted lists what each value may be sent as.
func flagSetting(field func(*state.PanelSettings) *bool, onText, offText string, on, off []string, why string) setting {
	return setting{
		show: func(p state.PanelSettings) string {
			if *field(&p) {
				return onText
			}
			return offText
		},
		parse: func(p *state.PanelSettings, v string) error {
			switch {
			case slices.Contains(on, v):
				*field(p) = true
			case slices.Contains(off, v):
				*field(p) = false
			default:
				return errors.New(why)
			}
			return nil
		},
	}
}

var settingFields = map[string]setting{
	"acme_email":   textSetting(func(p *state.PanelSettings) *string { return &p.ACMEEmail }),
	"probe_target": textSetting(func(p *state.PanelSettings) *string { return &p.ProbeTarget }),
	// Object storage (S3, Cloudflare R2, Backblaze B2); its secret key is a
	// secret.
	"backup_s3_access_key": textSetting(func(p *state.PanelSettings) *string { return &p.BackupS3AccessKey }),
	"backup_s3_region":     textSetting(func(p *state.PanelSettings) *string { return &p.BackupS3Region }),
	// The repository may carry a password in its URL: never shown.
	"backup_repository": {
		show:  func(p state.PanelSettings) string { return maskRepositoryCredentials(p.BackupRepository) },
		parse: func(p *state.PanelSettings, v string) error { p.BackupRepository = v; return nil },
	},
	// Who must have two-factor (§1.2 step 7): off, admins or all.
	"require_2fa": {
		show: func(p state.PanelSettings) string { return p.TwoFactorRequirement() },
		parse: func(p *state.PanelSettings, v string) error {
			if !p.SetTwoFactorRequirement(v) {
				return errors.New("must be off, admins or all")
			}
			return nil
		},
	},
	// The older form of require_2fa, kept for scripts: on makes an off
	// requirement admins and leaves all alone; off makes it off.
	"require_2fa_admin": {
		show: func(p state.PanelSettings) string {
			if p.TwoFactorRequirement() == state.Require2FAOff {
				return ""
			}
			return "1"
		},
		parse: func(p *state.PanelSettings, v string) error {
			switch {
			case slices.Contains([]string{"1", "true"}, v):
				if !p.Require2FAAll {
					p.SetTwoFactorRequirement(state.Require2FAAdmins)
				}
			case slices.Contains([]string{"", "0", "false"}, v):
				p.SetTwoFactorRequirement(state.Require2FAOff)
			default:
				return errors.New("must be 1 or empty")
			}
			return nil
		},
	},
	"update_check": flagSetting(func(p *state.PanelSettings) *bool { return &p.UpdateCheck },
		"", "0", []string{"", "1", "true"}, []string{"0", "false"}, "must be 0 or empty"),
	// The daily lookup of the WordPress sites' versions in a public
	// vulnerability database (§11.6).
	"vulnerability_check": flagSetting(func(p *state.PanelSettings) *bool { return &p.VulnerabilityCheck },
		"", "0", []string{"", "1", "true"}, []string{"0", "false"}, "must be 0 or empty"),
	// The weekly malware scan of every site (§11.7); on installs ClamAV.
	"malware_scan": flagSetting(func(p *state.PanelSettings) *bool { return &p.MalwareScan },
		"1", "", []string{"1", "true"}, []string{"", "0", "false"}, "must be 1 or empty"),
	// The release channel the update check and the self-update follow.
	"update_channel": {
		show: func(p state.PanelSettings) string { return p.UpdateChannel },
		parse: func(p *state.PanelSettings, v string) error {
			ch, ok := version.ParseChannel(v)
			if !ok {
				return errors.New("must be stable or beta")
			}
			p.UpdateChannel = ch
			return nil
		},
	},
	// Written by the operation that issues the panel's certificate: letting
	// it be typed would let the panel claim a domain it has no certificate
	// for.
	"panel_domain": {show: func(p state.PanelSettings) string { return p.PanelDomain }},

	// The server-wide mail relay and who gets the alerts by email (§14).
	"mail_host": {
		show: func(p state.PanelSettings) string { return p.Mail.Host },
		parse: func(p *state.PanelSettings, v string) error {
			if err := checkMailHost(v); err != nil {
				return err
			}
			p.Mail.Host = v
			return nil
		},
	},
	"mail_port": {
		show: func(p state.PanelSettings) string {
			if p.Mail.Port == 0 {
				return ""
			}
			return strconv.Itoa(p.Mail.Port)
		},
		parse: func(p *state.PanelSettings, v string) error {
			n, err := parseMailPort(v)
			p.Mail.Port = n
			return err
		},
	},
	"mail_security": {
		show: func(p state.PanelSettings) string { return p.Mail.Security },
		parse: func(p *state.PanelSettings, v string) error {
			if err := checkMailSecurity(v); err != nil {
				return err
			}
			p.Mail.Security = v
			return nil
		},
	},
	"mail_username": textSetting(func(p *state.PanelSettings) *string { return &p.Mail.Username }),
	"mail_from": {
		show: func(p state.PanelSettings) string { return p.Mail.From },
		parse: func(p *state.PanelSettings, v string) error {
			if v != "" && !plainAddress(v) {
				return errors.New("must be one email address")
			}
			p.Mail.From = v
			return nil
		},
	},
	"alert_emails": {
		show: func(p state.PanelSettings) string { return p.AlertEmails },
		parse: func(p *state.PanelSettings, v string) error {
			list, err := parseAlertEmails(v)
			p.AlertEmails = strings.Join(list, ", ")
			return err
		},
	},
}

// secretKeys are the keys kept in the secret store. The webhook is a
// credential too (whoever has it posts as the panel), but admins, who set
// it, see it in full.
var secretKeys = []string{
	state.SecretBackupPassword, state.SecretBackupS3SecretKey, state.SecretCloudflareToken, state.SecretNotifyWebhook,
	state.SecretMailPassword,
}

// repoPasswordRe finds the password of a URL's userinfo, as in
// rest:https://user:secret@host/ or s3:https://key:secret@endpoint/bucket.
var repoPasswordRe = regexp.MustCompile(`(://[^/@:]*:)[^/@]+@`)

// maskRepositoryCredentials hides a password in a restic repository
// string: every global viewer reads the settings, and that password opens
// every backup.
func maskRepositoryCredentials(repo string) string {
	return repoPasswordRe.ReplaceAllString(repo, "${1}"+secretMask+"@")
}

// shownSecret is how a secret appears to role: "" when unset, the secretMask
// otherwise, the webhook in full to an admin and as its host to others.
func shownSecret(key, value, role string) string {
	switch {
	case value == "":
		return ""
	case key != state.SecretNotifyWebhook:
		return secretMask
	case role == roleAdmin:
		return value
	}
	if u, err := url.Parse(value); err == nil {
		return u.Scheme + "://" + u.Host + "/" + secretMask
	}
	return secretMask
}

// settingsView is the map as role sees it.
func (s *Server) settingsView(role string) (map[string]string, error) {
	ps, err := s.Store.PanelSettings()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(settingFields)+len(secretKeys))
	for key, f := range settingFields {
		out[key] = f.show(ps)
	}
	for _, key := range secretKeys {
		v, err := s.Store.Secret(key)
		if err != nil {
			return nil, err
		}
		out[key] = shownSecret(key, v, role)
	}
	return out, nil
}

// checkProbeTarget: Performance Guard sends its requests from the server
// to the server, so the target may only be this machine over HTTPS.
func checkProbeTarget(v string) error {
	u, err := url.Parse(v)
	if err == nil && u.Scheme == "https" && (u.Path == "" || u.Path == "/") {
		switch u.Hostname() {
		case "127.0.0.1", "::1", "localhost":
			return nil
		}
	}
	return decline(http.StatusBadRequest, "probe_target must be https://127.0.0.1 (optionally with a port)")
}

// changeSettings applies a partial map (SPEC.md §12). It returns the keys
// it changed or set.
func (s *Server) changeSettings(ctx context.Context, req map[string]string) ([]string, error) {
	internal := func(err error, op string) error {
		s.logIfErr(err, op)
		return decline(http.StatusInternalServerError, "could not save the settings")
	}
	cur, err := s.Store.PanelSettings()
	if err != nil {
		return nil, internal(err, "PanelSettings")
	}
	// What GET returned, sent back, changes nothing: the masked
	// repository, a masked secret, a read-only value as it is.
	if v, ok := req["backup_repository"]; ok && cur.BackupRepository != "" && v == maskRepositoryCredentials(cur.BackupRepository) {
		delete(req, "backup_repository")
	}
	for _, key := range secretKeys {
		if req[key] == secretMask {
			delete(req, key)
		}
	}
	if v := req[state.SecretNotifyWebhook]; v != "" {
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, decline(http.StatusBadRequest, "notify_webhook must be an https URL")
		}
		if err := s.webhookTargetProblem(ctx, u.Hostname()); err != nil {
			return nil, err
		}
	}
	if v := req["probe_target"]; v != "" {
		if err := checkProbeTarget(v); err != nil {
			return nil, err
		}
	}
	// The two forms of the requirement together (a page sending back the
	// whole map) must agree; then the older one is redundant.
	if legacy, both := req["require_2fa_admin"]; both {
		if mode, also := req["require_2fa"]; also {
			var p state.PanelSettings
			if !p.SetTwoFactorRequirement(mode) {
				return nil, decline(http.StatusBadRequest, "require_2fa: must be off, admins or all")
			}
			on := slices.Contains([]string{"1", "true"}, legacy)
			if on != (p.TwoFactorRequirement() != state.Require2FAOff) {
				return nil, decline(http.StatusBadRequest, "require_2fa: send require_2fa or require_2fa_admin, not both")
			}
			delete(req, "require_2fa_admin")
		}
	}
	next := cur
	for key, v := range req {
		if slices.Contains(secretKeys, key) {
			continue
		}
		f, ok := settingFields[key]
		switch {
		case !ok:
			return nil, decline(http.StatusBadRequest, "unknown setting %s", key)
		case f.parse == nil && f.show(cur) == v:
			delete(req, key)
		case f.parse == nil:
			return nil, decline(http.StatusBadRequest, "%s is set by the operation that owns it, not here", key)
		default:
			if err := f.parse(&next, v); err != nil {
				return nil, decline(http.StatusBadRequest, "%s: %s", key, err.Error())
			}
		}
	}
	if err := s.checkBackupPairing(ctx, req, next); err != nil {
		return nil, err
	}
	for _, key := range secretKeys {
		if v, ok := req[key]; ok {
			if err := s.Store.SetSecret(key, v); err != nil {
				return nil, internal(err, "SetSecret")
			}
		}
	}
	if next != cur {
		// The fields this request names, on the settings as stored now.
		if _, err := s.Store.UpdatePanelSettings(func(p *state.PanelSettings) error {
			for key, v := range req {
				if f, ok := settingFields[key]; ok && f.parse != nil {
					if err := f.parse(p, v); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return nil, internal(err, "UpdatePanelSettings")
		}
	}
	return slices.Sorted(maps.Keys(req)), nil
}

// checkBackupPairing tests the repository and password the request leaves
// in place, when it changes either and both are set. A restic repository is
// keyed by the password it was created with: saving another one does not
// re-key it, it locks the panel out of every backup, which is what saving
// an unchecked "new" password once did on a real server.
func (s *Server) checkBackupPairing(ctx context.Context, req map[string]string, next state.PanelSettings) error {
	_, repoChanged := req["backup_repository"]
	password, passwordChanged := req[state.SecretBackupPassword]
	if !repoChanged && !passwordChanged {
		return nil
	}
	if repoChanged && next.BackupRepository != "" {
		// The agent checks again before every restic call; this is the
		// same rule, so a refused repository is never saved.
		if err := backuprepo.Validate(ctx, next.BackupRepository, backuprepo.Options{
			AllowPrivate: s.BackupPrivateEndpoint,
			// Whether rclone is installed is the agent's to say.
			Rclone: func() bool { return true },
		}); err != nil {
			return decline(http.StatusBadRequest, "%s", err.Error())
		}
	}
	if !passwordChanged {
		var err error
		if password, err = s.Store.Secret(state.SecretBackupPassword); err != nil {
			s.logIfErr(err, "Secret")
			return decline(http.StatusInternalServerError, "could not save the settings")
		}
	}
	if next.BackupRepository == "" || password == "" {
		return nil
	}
	err := agentrpc.Into(ctx, s.Agent, agentrpc.TestBackup, rpc.TestBackupParams{
		Repository: next.BackupRepository, Password: password, Env: s.backupEnv(),
	}, nil)
	if err == nil {
		return nil
	}
	status, msg := errorReply(err, http.StatusBadRequest, "The agent could not complete the operation")
	return decline(status, "this repository/password pairing does not work, refusing to save (existing backups would become unreachable): %s", msg)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request, c *call) {
	view, err := s.settingsView(c.User.Role)
	if err != nil {
		s.logIfErr(err, "settingsView")
		respondErr(w, http.StatusInternalServerError, "could not read the settings")
		return
	}
	respond(w, http.StatusOK, view)
}

// tokenSettings are the settings an API token may change. Every other key
// governs the panel itself: the 2FA requirement, where backups go and the
// key that opens them, where alerts are posted, the DNS credential and the
// release channel. A token is an admin without a second factor, kept for the
// automation of sites; changing those takes a signed-in session.
var tokenSettings = map[string]bool{"acme_email": true, "probe_target": true}

// governanceRefusal is the first key of req, in key order, that a token
// may not change, or "": a key sent back as GET showed it changes nothing
// and passes, so a script may PUT back the map it read; an unknown key is
// left to the 400 that names it.
func (s *Server) governanceRefusal(req map[string]string) (string, error) {
	view, err := s.settingsView(roleAdmin)
	if err != nil {
		return "", err
	}
	for _, key := range slices.Sorted(maps.Keys(req)) {
		if tokenSettings[key] {
			continue
		}
		if shown, known := view[key]; known && req[key] != shown {
			return key, nil
		}
	}
	return "", nil
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request, c *call) {
	var req map[string]string
	if !decodeOr400(w, r, &req) {
		return
	}
	if c.ViaToken() {
		key, err := s.governanceRefusal(req)
		if err != nil {
			internalError(w, "settingsView", err, "could not read the settings")
			return
		}
		if key != "" {
			respondErr(w, http.StatusForbidden, "an API token cannot change "+key+": the panel's own settings need a signed-in session (a token may change acme_email and probe_target)")
			return
		}
	}
	keys, err := s.changeSettings(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(c.Actor(), "settings.update", strings.Join(keys, ","), "")
	s.handleGetSettings(w, r, c)
}
