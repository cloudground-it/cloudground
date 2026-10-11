// Package units holds CloudGround's systemd unit files. They are compiled
// into every binary, so the installer writes the units of the release it
// belongs to and the release publishes the same bytes beside the binaries
// for the self-update (SPEC.md).
package units

import (
	"embed"
	"slices"
)

//go:embed *.service *.socket
var files embed.FS

// Names are the unit files of a release, in the order they are installed.
var Names = []string{
	"cloudground-agent.service",
	"cloudground-api.socket",
	"cloudground-api.service",
}

// Get returns the content of the unit called name, and false for a name that
// is not one of Names.
func Get(name string) ([]byte, bool) {
	if !slices.Contains(Names, name) {
		return nil, false
	}
	b, err := files.ReadFile(name)
	return b, err == nil
}
