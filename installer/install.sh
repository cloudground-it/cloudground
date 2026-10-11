#!/usr/bin/env bash
# CloudGround bootstrap: finds the binaries and hands over to `cgctl install`,
# which does the work (internal/install/SPEC.md). Run as root:
#   CLOUDGROUND_LOCAL_BUILD=<dist dir> bash installer/install.sh    a local build
#   bash install.sh                  the release this copy was stamped for
#   CLOUDGROUND_VERSION=latest bash install.sh       the newest release instead
# A release's copy carries its version and its cgctl's SHA-256 (stamped over
# @CGCTL_VERSION@ and @CGCTL_SHA256@): it runs that cgctl only if it matches,
# and cgctl checks the release's signature. That trusts the place this script
# came from (SPEC §5); for an anchor of your own, set CLOUDGROUND_CGCTL_SHA256
# or CLOUDGROUND_RELEASE_PUBKEY to the values published on the website.
set -euo pipefail
main() { # all in a function: a download cut off half way runs nothing
  local v=${CLOUDGROUND_VERSION:-@CGCTL_VERSION@} pin=${CLOUDGROUND_CGCTL_SHA256:-@CGCTL_SHA256@} dl f url
  local base=https://github.com/${CLOUDGROUND_REPO:-cloudground-it/cloudground}/releases
  die() { printf 'cloudground: %s\n' "$*" >&2; exit 1; }
  get() { curl -fsSL --proto '=https' --proto-redir '=https' -o "$dl/$1" "$2/$1" || die "download $1 from $2"; }
  [[ $EUID -eq 0 ]] || die "run as root: sudo bash $0"
  . /etc/os-release 2>/dev/null || die "cannot detect the OS (/etc/os-release missing)"
  [[ "$ID-$VERSION_ID" == ubuntu-24.04 || "$ID-$VERSION_ID" == ubuntu-26.04 ]] || die "Ubuntu 24.04 or 26.04 LTS required (found ${PRETTY_NAME:-unknown})"
  [[ $(uname -m) == x86_64 ]] || die "amd64 required (found $(uname -m))"
  if [[ -n "${CLOUDGROUND_LOCAL_BUILD:-}" ]]; then
    exec "$CLOUDGROUND_LOCAL_BUILD/cgctl" install --from "$CLOUDGROUND_LOCAL_BUILD" "$@"
  fi
  dl=$(mktemp -d) && trap 'rm -rf "$dl"' EXIT
  if [[ $v == latest ]]; then # no pin for "latest": run that release's own stamped copy
    [[ -z ${CLOUDGROUND_BOOTSTRAP_HOP:-} ]] || die "the latest release's install.sh asks for latest again"
    get install.sh "${CLOUDGROUND_RELEASE_URL:-$base/latest/download}"
    [[ -z ${CLOUDGROUND_INSTALL_SHA256:-} || $(sha256sum "$dl/install.sh" | cut -d' ' -f1) == "$CLOUDGROUND_INSTALL_SHA256" ]] \
      || die "install.sh does not match CLOUDGROUND_INSTALL_SHA256: refusing to run it"
    CLOUDGROUND_BOOTSTRAP_HOP=1 CLOUDGROUND_VERSION='' bash "$dl/install.sh" "$@"
    exit
  fi
  [[ $v =~ ^v[0-9] && $pin =~ ^[0-9a-f]{64}$ ]] || die "this script names no release: use a release's install.sh, set CLOUDGROUND_VERSION=latest, or CLOUDGROUND_LOCAL_BUILD"
  url=${CLOUDGROUND_RELEASE_URL:-$base/download/$v}
  for f in SHA256SUMS SHA256SUMS.sig panel-api panel-agent cgctl cg-site-shell; do get "$f" "$url"; done
  [[ $(sha256sum "$dl/cgctl" | cut -d' ' -f1) == "$pin" ]] || die "the downloaded cgctl is not the one this script names: refusing to run it"
  chmod 0700 "$dl/cgctl"
  trap - EXIT # from here cgctl removes the download, however the install ends
  CLOUDGROUND_VERSION=$v exec "$dl/cgctl" install --release "$dl" --remove-release-dir "$@"
}
main "$@"
