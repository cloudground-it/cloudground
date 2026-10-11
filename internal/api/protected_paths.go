package api

import (
	"fmt"
	"net/http"
	"slices"

	"golang.org/x/crypto/bcrypt"

	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/site"
	"github.com/cloudground-it/cloudground/internal/state"
)

// A site's protected paths (SPEC.md §3.12, issue #26): paths nginx asks a
// username and password for, such as /wp-admin or a private folder. Each
// change renders the site at once, as the routing block does (routing.go):
// it is stored only once nginx has taken it, and a refusal leaves the site
// as it was.

// Bounds of a protected path's password: bcrypt reads 72 bytes at most.
const (
	minAuthPassword = 8
	maxAuthPassword = 72
)

// protectedPathReq is the body of both routes; DELETE reads only Path.
type protectedPathReq struct {
	Path     string `json:"path"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// protectedPathsOf is the site's paths as the API shows them: no hashes.
func protectedPathsOf(st state.Site) []state.ProtectedPath {
	out := []state.ProtectedPath{}
	for _, p := range st.Public().Config.ProtectedPaths {
		out = append(out, p)
	}
	return out
}

// checkProtectable refuses a site whose paths cannot be protected: a
// staging copy asks for its own password everywhere already.
func checkProtectable(st state.Site) error {
	if st.StagingOf != 0 {
		return decline(http.StatusBadRequest, "a staging copy is protected as a whole: it takes no protected paths")
	}
	return nil
}

// handleSetProtectedPath adds a protected path or replaces its username
// and password.
func (s *Server) handleSetProtectedPath(w http.ResponseWriter, r *http.Request, c *call) {
	var req protectedPathReq
	if !decodeOr400(w, r, &req) {
		return
	}
	switch {
	case !site.ValidProtectedPath(req.Path):
		respondErr(w, http.StatusBadRequest, "path must be a path below the site's root, such as /wp-admin: letters, digits and . _ ~ - between slashes, not / itself")
		return
	case !site.ValidBasicAuthUser(req.Username):
		respondErr(w, http.StatusBadRequest, "username must be 1–64 letters, digits or . _ @ -")
		return
	case len(req.Password) < minAuthPassword || len(req.Password) > maxAuthPassword:
		respondErr(w, http.StatusBadRequest, fmt.Sprintf("password must be %d–%d bytes", minAuthPassword, maxAuthPassword))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		internalError(w, "bcrypt", err, "could not hash the password")
		return
	}
	entry := state.ProtectedPath{Path: req.Path, Username: req.Username, PasswordHash: string(hash)}
	s.changeProtectedPaths(w, r, c, "add "+req.Path+" (user "+req.Username+")", func(paths []state.ProtectedPath) ([]state.ProtectedPath, error) {
		if i := slices.IndexFunc(paths, func(p state.ProtectedPath) bool { return p.Path == req.Path }); i >= 0 {
			paths[i] = entry
			return paths, nil
		}
		if len(paths) >= site.MaxProtectedPaths {
			return nil, decline(http.StatusBadRequest, "a site has at most %d protected paths", site.MaxProtectedPaths)
		}
		return append(paths, entry), nil
	})
}

// handleDeleteProtectedPath stops protecting one path.
func (s *Server) handleDeleteProtectedPath(w http.ResponseWriter, r *http.Request, c *call) {
	var req protectedPathReq
	if !decodeOr400(w, r, &req) {
		return
	}
	s.changeProtectedPaths(w, r, c, "remove "+req.Path, func(paths []state.ProtectedPath) ([]state.ProtectedPath, error) {
		i := slices.IndexFunc(paths, func(p state.ProtectedPath) bool { return p.Path == req.Path })
		if i < 0 {
			return nil, decline(http.StatusNotFound, "that path is not protected")
		}
		return slices.Delete(paths, i, i+1), nil
	})
}

// changeProtectedPaths applies change to the site's paths under the site's
// lock: the agent renders the changed site, and only then is it stored.
func (s *Server) changeProtectedPaths(w http.ResponseWriter, r *http.Request, c *call, detail string,
	change func([]state.ProtectedPath) ([]state.ProtectedPath, error)) {
	cur, unlock, err := s.freshSite(c.Site.ID)
	defer unlock()
	if err != nil {
		internalError(w, "GetSite", err, "could not read the site")
		return
	}
	if err := checkProtectable(cur); err != nil {
		fail(w, err)
		return
	}
	paths, err := change(slices.Clone(cur.Config.ProtectedPaths))
	if err != nil {
		fail(w, err)
		return
	}
	next := cur
	next.Config.ProtectedPaths = paths
	var res rpc.ApplyResult
	if err := agentrpc.Into(r.Context(), s.Agent, agentrpc.ApplySiteConfig, rpc.SiteRef{Site: next}, &res); err != nil {
		respondAgentErr(w, 0, "", err)
		return
	}
	stored, err := s.Store.MutateSiteApplied(cur.ID, func(st *state.Site) error {
		st.Config.ProtectedPaths = paths
		return nil
	})
	if err != nil {
		internalError(w, "MutateSiteApplied", err, "")
		return
	}
	s.recordRendered(stored, res.Files)
	s.audit(c.Actor(), "site.protected_path", cur.Domain, detail)
	respond(w, http.StatusOK, protectedPathsOf(stored))
}
