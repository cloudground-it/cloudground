package api

import (
	"net/http"
	"sync"

	cloudground "github.com/cloudground-it/cloudground"
	"github.com/cloudground-it/cloudground/internal/changelog"
	"github.com/cloudground-it/cloudground/internal/version"
)

// What's new (SPEC.md §2.9, issue #106): the changelog shipped in the
// binary, and which release's entry the caller last saw.

var (
	releaseNotes     []changelog.Entry
	releaseNotesOnce sync.Once
)

// notes is the embedded changelog, parsed once.
func notes() []changelog.Entry {
	releaseNotesOnce.Do(func() { releaseNotes = changelog.Parse(cloudground.Changelog) })
	return releaseNotes
}

func (s *Server) handleChangelog(w http.ResponseWriter, r *http.Request, c *call) {
	entries := notes()
	unseen := changelog.Unseen(entries, c.User.WhatsNewSeen, version.Version)
	if unseen == nil {
		unseen = []string{}
	}
	respond(w, http.StatusOK, map[string]any{
		"current": version.Version,
		"seen":    c.User.WhatsNewSeen,
		"unseen":  unseen,
		"entries": entries,
	})
}

// handleWhatsNewSeen records the running release as seen by the caller. The
// body names nothing: the release is the one running, not one a client
// claims.
func (s *Server) handleWhatsNewSeen(w http.ResponseWriter, r *http.Request, c *call) {
	if err := s.Store.SetUserWhatsNewSeen(c.User.ID, version.Version); err != nil {
		internalError(w, "SetUserWhatsNewSeen", err, "")
		return
	}
	respond(w, http.StatusOK, map[string]string{"seen": version.Version})
}
