// Package cloudground holds what the repository's root gives every binary:
// the changelog, embedded at build time so the panel can show it offline.
package cloudground

import _ "embed"

// Changelog is CHANGELOG.md as it was when the binary was built
// (internal/changelog reads it).
//
//go:embed CHANGELOG.md
var Changelog string
