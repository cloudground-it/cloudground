// Package connector embeds the PHP that CloudGround installs into WordPress
// sites. The files under php/ are the only copy: the agent writes these
// exact bytes. SPEC.md in this directory is their contract; they are
// GPL-2.0-or-later (LICENSE).
package connector

import _ "embed"

// PHP is the mu-plugin, installed as
// wp-content/mu-plugins/cloudground-connector.php in every WordPress release.
// It purges the page-cache entries a change affects, signs the panel in with
// a one-shot link, reports the object cache's numbers and prints Performance
// Guard's footer for Guard's own requests.
//
//go:embed php/cloudground-connector.php
var PHP string

// ObjectCacheAPCu is the object-cache drop-in, installed as
// shared/object-cache.php and linked from the release's wp-content. It keeps
// WordPress's object cache in the site's APCu segment, works per request
// where there is none (wp-cli), and namespaces every key with a generation
// read from shared/.cloudground-cache-epoch, so a flush in one process
// reaches the other.
//
//go:embed php/object-cache.php
var ObjectCacheAPCu string
