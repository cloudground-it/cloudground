#!/usr/bin/env bash
# The upgrade test's releases (e2e section "R-6.5 upgrade", docs/e2e.md),
# built by `make docker-upgrade-bundle` into dist/upgrade-bundle.tar.gz:
#
#   v0.1.0, v0.2.0, v0.2.1   this tree, three times, each with its own tag
#   v0.3.0                   v0.2.1's files but panel-api, which exits 1 at
#                            once (scripts/upgrade-e2e/badapi): the rollback
#
# Every binary is linked with UpdateURL=http://127.0.0.1:18931 (the feed the
# e2e section serves on the box itself; version.CheckOrigin takes plain http
# only to loopback) and UpdatePublicKey=<a throwaway key made here>, and each
# release directory is signed with that key by scripts/release-sign, as a
# real release is with the real one. The private key stays in the build
# directory and is not in the tarball. None of this is a release: the
# throwaway key is trusted only by these binaries.
#
#   scripts/upgrade-bundle.sh <out dir>
set -euo pipefail
cd "$(dirname "$0")/.."
out=${1:?usage: upgrade-bundle.sh <out dir>}
port=18931
origin="http://127.0.0.1:$port"
pkg=github.com/cloudground-it/cloudground/internal/version

rm -rf "$out"
mkdir -p "$out/upgrade-bundle"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pub=$(go run ./scripts/release-sign keygen "$work/throwaway.key")
echo "$pub" >"$out/upgrade-bundle/pubkey"
echo "$origin" >"$out/upgrade-bundle/origin"

export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
build() { # build <tag> <dir>
  local tag=$1 dir=$2
  mkdir -p "$dir"
  for b in panel-api panel-agent cgctl cg-site-shell; do
    go build -trimpath -ldflags "-s -w -X $pkg.Version=$tag -X $pkg.UpdateURL=$origin -X $pkg.UpdatePublicKey=$pub" -o "$dir/$b" "./cmd/$b"
  done
  cp internal/units/*.service internal/units/*.socket "$dir/"
}
sign() { go run ./scripts/release-sign sign "$work/throwaway.key" "$1" "$out/upgrade-bundle/$1"; }

for tag in v0.1.0 v0.2.0 v0.2.1; do
  build "$tag" "$out/upgrade-bundle/$tag"
  sign "$tag"
done
bad="$out/upgrade-bundle/v0.3.0"
mkdir -p "$bad"
for f in panel-agent cgctl cg-site-shell cloudground-agent.service cloudground-api.service cloudground-api.socket; do
  cp "$out/upgrade-bundle/v0.2.1/$f" "$bad/"
done
go build -trimpath -ldflags "-s -w -X $pkg.Version=v0.3.0" -o "$bad/panel-api" ./scripts/upgrade-e2e/badapi
sign v0.3.0

# Each release verifies with the verifier the binaries run (built for this
# machine, with the same key).
for tag in v0.1.0 v0.2.0 v0.2.1 v0.3.0; do
  GOOS= GOARCH= go run -ldflags "-X $pkg.UpdatePublicKey=$pub" ./cmd/cgctl release verify "$out/upgrade-bundle/$tag"
done
tar -C "$out" -czf "$out/upgrade-bundle.tar.gz" upgrade-bundle
echo "upgrade bundle: $out/upgrade-bundle.tar.gz (copy it to the box, see docs/e2e.md)"
