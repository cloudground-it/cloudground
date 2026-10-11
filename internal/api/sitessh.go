package api

import (
	"net/http"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's SSH access (ops/sftp SPEC): the SFTP login and its password, the
// SSH keys with their type, and the shell an administrator can turn on
// (R-4.5). The agent makes every change on the server; the API stores the
// switches once it has answered.
//
// A shell is a shell for whoever holds its key, so both ways to one are an
// administrator's, from a signed-in session (SPEC.md §1.3):
//   - POST /api/sites/{id}/ssh-shell is an admin, SessionOnly row;
//   - POST /api/sites/{id}/ssh-keys stays a manage row, as an operator adds
//     SFTP keys to its sites, and the handler refuses "access": "shell" to
//     anyone but an admin signed in with a session (an API token, an
//     admin's included, gets 403: a leaked token cannot plant a key that
//     outlives its revocation).

// minSFTPPassword is the shortest SFTP password accepted (the agent checks
// too).
const minSFTPPassword = 12

// msgShellKeySession is the refusal of a shell key to anyone but an admin's
// session.
const msgShellKeySession = "only an administrator, signed in with a session, can add a shell key"

// handleSetSFTP turns the SFTP login on, with a password or for keys only,
// or off (#47). Body {"enable": bool, "password": string, "keys_only": bool,
// "public_key": string}. keys_only left out is true when a public_key is
// given: a login that comes with a key takes keys only unless asked. The
// key is added (as an SFTP key) before the login is turned on, and one
// already there is fine.
func (s *Server) handleSetSFTP(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Enable    bool   `json:"enable"`
		Password  string `json:"password"`
		KeysOnly  *bool  `json:"keys_only"`
		PublicKey string `json:"public_key"`
	}
	if err := decode(r, &req); err != nil {
		respondErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	keysOnly := req.PublicKey != ""
	if req.KeysOnly != nil {
		keysOnly = *req.KeysOnly
	}
	switch {
	case !req.Enable:
		keysOnly = false
	case keysOnly && req.Password != "":
		respondErr(w, http.StatusBadRequest, "a keys-only SFTP login takes no password")
		return
	case !keysOnly && len(req.Password) < minSFTPPassword:
		respondErr(w, http.StatusBadRequest, "SFTP password must be at least 12 characters")
		return
	}
	// Under the site's lock, on the site as stored now: the agent applies
	// the access to the shell login too, as the stored switches say.
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	if req.Enable && req.PublicKey != "" {
		var keys rpc.SSHKeysResult
		err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SSHKeys, rpc.SSHKeyParams{Site: cur, Action: "add", PublicKey: req.PublicKey, Access: "sftp"}, &keys)
		if err != nil && agentrpc.CodeOf(err) != agentrpc.CodeConflict {
			respondAgentErr(w, 0, "", err)
			return
		}
		if err == nil && len(keys.Keys) > 0 {
			s.audit(c.Actor(), "sftp.key.add", c.Site.Domain, keys.Keys[len(keys.Keys)-1].Fingerprint+" sftp")
		}
	}
	var out map[string]string
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SetSFTP, rpc.SFTPParams{Site: cur, Password: req.Password, Enable: req.Enable, KeysOnly: keysOnly}, &out); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	_, err = s.Store.MutateSiteApplied(c.Site.ID, func(st *state.Site) error {
		st.Config.SFTPEnabled = req.Enable
		if req.Enable {
			st.Config.SFTPKeysOnly = keysOnly
		}
		return nil
	})
	s.logIfErr(err, "MutateSite")
	detail := onOff(req.Enable)
	if keysOnly {
		detail += ", keys only"
	}
	s.audit(c.Actor(), "sftp.set", c.Site.Domain, detail)
	respond(w, http.StatusOK, map[string]any{
		"sftp": out, "host": r.Host, "username": c.Site.SystemUser, "port": 22, "keys_only": keysOnly,
	})
}

// handleSetShell turns the site's SSH shell on or off. Body {"enable": bool}.
func (s *Server) handleSetShell(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Enable *bool `json:"enable"`
	}
	if err := decode(r, &req); err != nil || req.Enable == nil {
		respondErr(w, http.StatusBadRequest, "enable required")
		return
	}
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	var out map[string]string
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SetShell, rpc.ShellParams{Site: cur, Enable: *req.Enable}, &out); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	_, err = s.Store.MutateSiteApplied(c.Site.ID, func(st *state.Site) error {
		st.Config.ShellEnabled = *req.Enable
		return nil
	})
	s.logIfErr(err, "MutateSite")
	s.audit(c.Actor(), "ssh.shell.set", c.Site.Domain, onOff(*req.Enable))
	respond(w, http.StatusOK, map[string]any{"shell": out["shell"], "user": out["user"]})
}

func (s *Server) handleListSSHKeys(w http.ResponseWriter, r *http.Request, c *call) {
	var res rpc.SSHKeysResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SSHKeys, rpc.SSHKeyParams{Site: c.Site, Action: "list"}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	respond(w, http.StatusOK, res)
}

// handleAddSSHKey adds one public key. Body {"public_key": string,
// "access": "sftp" | "shell"} (no access is "sftp").
func (s *Server) handleAddSSHKey(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		PublicKey string `json:"public_key"`
		Access    string `json:"access"`
	}
	if err := decode(r, &req); err != nil || req.PublicKey == "" {
		respondErr(w, http.StatusBadRequest, "public_key required")
		return
	}
	switch req.Access {
	case "":
		req.Access = "sftp"
	case "sftp":
	case "shell":
		if c.User.Role != roleAdmin || c.ViaToken() {
			respondErr(w, http.StatusForbidden, msgShellKeySession)
			return
		}
	default:
		respondErr(w, http.StatusBadRequest, `access must be "sftp" or "shell"`)
		return
	}
	var res rpc.SSHKeysResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SSHKeys, rpc.SSHKeyParams{Site: c.Site, Action: "add", PublicKey: req.PublicKey, Access: req.Access}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	fingerprint := ""
	if len(res.Keys) > 0 {
		fingerprint = res.Keys[len(res.Keys)-1].Fingerprint
	}
	s.audit(c.Actor(), "sftp.key.add", c.Site.Domain, fingerprint+" "+req.Access)
	respond(w, http.StatusCreated, res)
}

func (s *Server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request, c *call) {
	fingerprint := r.PathValue("fingerprint")
	var res rpc.SSHKeysResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.SSHKeys, rpc.SSHKeyParams{Site: c.Site, Action: "delete", Fingerprint: fingerprint}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	s.audit(c.Actor(), "sftp.key.delete", c.Site.Domain, fingerprint)
	respond(w, http.StatusOK, res)
}

func onOff(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}
