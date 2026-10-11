#!/usr/bin/env bash
# The reproducible part of a CloudGround release (Makefile `release`,
# internal/version/SPEC.md). Two steps, so each can run in its own container:
#
#   scripts/build-release.sh ui               build ui/dist; it must equal the committed one
#   scripts/build-release.sh bin <tag> <out>  binaries, units, bootstrap and tarball into <out>
#
# `bin` leaves <out> unsigned: `release-sign sign` and `cgctl release verify`
# finish the release. It refuses a dirty tree, a tag that is not a release
# tag or not on HEAD (CG_RELEASE_TEST=1 lifts only the HEAD check, for the
# reproducibility check and CI dry runs), and a Go other than go.mod's
# toolchain. The same commit and toolchain give the same bytes: no build
# path (-trimpath), no build id, the commit's VCS stamp (-buildvcs), and a
# tarball whose order, owners and times come from the commit
# (SOURCE_DATE_EPOCH).
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "build-release: $*" >&2; exit 1; }

# @nuxtjs/i18n names each locale file by a hash of its absolute path, so
# ui/dist depends on where it is built: every build (this one, CI's and the
# containers of make docker-dist and docker-release-build) happens at this path.
UI_BUILD_DIR=/tmp/cloudground-ui

build_ui() {
  if [[ $(pwd -P)/ui == "$UI_BUILD_DIR" ]]; then
    (cd ui && npm ci --no-audit --no-fund --silent && npm run build --silent)
    return
  fi
  # Elsewhere the path differs (macOS: /tmp is /private/tmp): make docker-dist.
  [[ $(uname -s) == Linux ]] || die "build ui/dist on Linux, or with make docker-dist"
  rm -rf "$UI_BUILD_DIR"
  # Not -p: another build at work, or anything planted at the path, stops it.
  mkdir -m 0700 "$UI_BUILD_DIR" || die "$UI_BUILD_DIR exists: another UI build is running?"
  tar -C ui --exclude=./node_modules --exclude=./dist --exclude=./.nuxt --exclude=./.output -cf - . | tar -C "$UI_BUILD_DIR" -xf -
  (cd "$UI_BUILD_DIR" && npm ci --no-audit --no-fund --silent && npm run build --silent)
  rm -rf ui/dist
  cp -R "$UI_BUILD_DIR/dist" ui/dist
  rm -rf "$UI_BUILD_DIR"
}

build_bin() {
  local tag=$1 out=$2
  local channel
  channel=$(go run ./scripts/release-sign channel "$tag") || die "$tag is not a release tag"
  [[ -z $(git status --porcelain) ]] || {
    git status --short >&2
    die "the tree differs from the commit (a stale ui/dist shows up here): commit or clean it first"
  }
  if [[ ${CG_RELEASE_TEST:-} != 1 ]]; then
    git tag --points-at HEAD | grep -qxF "$tag" || die "HEAD is not tagged $tag"
  fi
  local want got
  want=$(sed -n 's/^toolchain //p' go.mod)
  got=$(go env GOVERSION)
  [[ $got == "$want" ]] || die "Go is $got, go.mod pins $want: build with GO_RELEASE_IMAGE"
  [[ -n ${UPDATE_URL:-} ]] || die "UPDATE_URL is empty: a release must know where its updates come from"
  [[ $UPDATE_URL == https://* ]] || die "UPDATE_URL is $UPDATE_URL: a release's update origin must be https"

  export SOURCE_DATE_EPOCH
  SOURCE_DATE_EPOCH=$(git log -1 --format=%ct)
  export CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=-buildvcs=true
  local pkg=github.com/cloudground-it/cloudground/internal/version
  local ldflags="-s -w -buildid= -X $pkg.Version=$tag -X $pkg.UpdateURL=${UPDATE_URL:-} -X $pkg.UpdatePublicKey=${UPDATE_PUBKEY:-}"

  rm -rf "$out"
  mkdir -p "$out"
  local stage
  stage=$(mktemp -d)
  trap 'rm -rf "$stage"' RETURN
  for b in panel-api panel-agent cgctl cg-site-shell; do
    go build -trimpath -ldflags "$ldflags" -o "$stage/$b" "./cmd/$b"
    chmod 0755 "$stage/$b"
    cp "$stage/$b" "$out/$b"
  done
  # The binaries name their commit (go version -m). Go does not stamp a
  # linked worktree, whose .git is a file; a checkout's must be stamped.
  if [[ -d .git ]]; then
    go version -m "$out/cgctl" | grep -q "vcs.revision=$(git rev-parse HEAD)" || die "the binaries carry no VCS stamp of HEAD"
  fi
  # The units are compiled into the binaries (internal/units); the release
  # also publishes them, signed, for servers whose self-update installs them.
  for u in internal/units/*.service internal/units/*.socket; do
    install -m 0644 "$u" "$out/"
  done
  # The installer bootstrap (get.cloudground.it and releases/latest/download
  # serve this release's copy) installs the release stamped over
  # @CGCTL_VERSION@, and runs the downloaded cgctl only if its SHA-256 is the
  # one stamped over @CGCTL_SHA256@ (internal/install/SPEC.md §6). A
  # placeholder left in the published copy fails the build.
  local cgctl_sum
  cgctl_sum=$(sha256sum "$out/cgctl" | cut -d' ' -f1)
  sed -e "s/@CGCTL_VERSION@/$tag/g" -e "s/@CGCTL_SHA256@/$cgctl_sum/g" installer/install.sh >"$out/install.sh"
  chmod 0755 "$out/install.sh"
  ! grep -qE '@CGCTL_(VERSION|SHA256)@' "$out/install.sh" || die "install.sh still carries a @CGCTL_VERSION@ or @CGCTL_SHA256@ placeholder"
  grep -qF "CLOUDGROUND_VERSION:-$tag}" "$out/install.sh" && grep -qF "CLOUDGROUND_CGCTL_SHA256:-$cgctl_sum}" "$out/install.sh" ||
    die "install.sh does not name $tag and its cgctl: the bootstrap's defaults changed shape"
  install -m 0644 LICENSE "$stage/"
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$SOURCE_DATE_EPOCH" --format=gnu \
    -C "$stage" -cf - panel-api panel-agent cgctl cg-site-shell LICENSE |
    gzip -n -9 >"$out/cloudground-$tag-linux-amd64.tar.gz"
  chmod 0644 "$out/cloudground-$tag-linux-amd64.tar.gz"
  echo "CloudGround $tag ($channel) built in $out, unsigned"
}

case "${1:-}" in
  ui) build_ui ;;
  bin) [[ $# == 3 ]] || die "usage: $0 bin <tag> <out>"; build_bin "$2" "$3" ;;
  *) die "usage: $0 ui | bin <tag> <out>" ;;
esac
