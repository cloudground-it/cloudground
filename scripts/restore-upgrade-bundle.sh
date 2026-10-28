#!/usr/bin/env bash
# The releases of the e2e section "R-6.6 restore across updates" (docs/e2e.md,
# #37), built by `make docker-restore-upgrade-bundle` into
# dist/restore-upgrade/restore-upgrade-bundle.tar.gz:
#
#   <old tag>...   each one built from its own tag (git archive), with the Go
#                  toolchain its own go.mod names, in that version's image
#   <head>         this tree, labelled the highest tag plus one patch (so the
#                  section keeps working after every release), with its own
#                  toolchain
#
# As in scripts/upgrade-bundle.sh, every binary is linked with
# UpdateURL=http://127.0.0.1:18931 (the feed the section serves on the box)
# and UpdatePublicKey=<a throwaway key made here>, and each release directory
# is signed with that key by its own tag's scripts/release-sign and checked by
# its own cgctl. The private key stays in the work directory, out of the
# tarball. None of this is a release: the throwaway key is trusted only by
# these binaries.
#
# It runs on the workstation (git reads the tags; each build runs in the
# golang:<toolchain> image of its tag, as scripts/build-release.sh requires
# the exact toolchain):
#
#   scripts/restore-upgrade-bundle.sh <out dir> [old tag ...]   (default: v0.1.0 v0.9.0)
set -euo pipefail
cd "$(dirname "$0")/.."
out=${1:?usage: restore-upgrade-bundle.sh <out dir> [old tag ...]}
shift
tags=("$@")
[[ ${#tags[@]} -gt 0 ]] || tags=(v0.1.0 v0.9.0)
port=18931
origin="http://127.0.0.1:$port"
pkg=github.com/cloudground-it/cloudground/internal/version
volumes=${GO_VOLUMES:-}

latest=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | awk '!/-/ {print; exit}')
[[ -n $latest ]] || { echo "no release tag to label HEAD after" >&2; exit 1; }
IFS=. read -r major minor patch <<<"${latest#v}"
head_tag="v$major.$minor.$((patch + 1))"
for t in "${tags[@]}"; do
  git rev-parse -q --verify "refs/tags/$t" >/dev/null || { echo "no tag $t" >&2; exit 1; }
done

rm -rf "$out"
mkdir -p "$out/restore-upgrade-bundle" "$out/src" "$out/work"
abs=$(cd "$out" && pwd)
bundle="$abs/restore-upgrade-bundle"

# toolchain <go.mod contents>: the version its toolchain line names (go1.26.9
# -> 1.26.9), else its go line.
toolchain() {
  awk '$1=="toolchain" {sub(/^go/, "", $2); t=$2} $1=="go" {g=$2} END {print (t != "" ? t : g)}'
}
# gorun <go version> <source dir> <command...>: the command in that version's
# image, the source at /src and the output at /out.
gorun() {
  local v=$1 src=$2
  shift 2
  # shellcheck disable=SC2086 # GO_VOLUMES is a list of -v flags.
  docker run --rm -v "$src":/src -v "$abs":/out -w /src -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local $volumes "golang:$v" "$@"
}
# build <tag> <source dir> <go version>: the four binaries and the units,
# linked for the bundle, then signed and verified with the tag's own tools.
build() {
  local tag=$1 src=$2 v=$3
  gorun "$v" "$src" sh -euc '
    have=$(go version | awk "{print \$3}")
    [ "$have" = "go$1" ] || { echo "$2 needs go$1, the image has $have" >&2; exit 1; }
    mkdir -p "/out/restore-upgrade-bundle/$2"
    for b in panel-api panel-agent cgctl cg-site-shell; do
      [ -d "./cmd/$b" ] || continue
      GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X $3.Version=$2 -X $3.UpdateURL=$4 -X $3.UpdatePublicKey=$5" \
        -o "/out/restore-upgrade-bundle/$2/$b" "./cmd/$b"
    done
    cp internal/units/*.service internal/units/*.socket "/out/restore-upgrade-bundle/$2/"
    go run ./scripts/release-sign sign /out/work/throwaway.key "$2" "/out/restore-upgrade-bundle/$2"
    go run -ldflags "-X $3.UpdatePublicKey=$5" ./cmd/cgctl release verify "/out/restore-upgrade-bundle/$2"
  ' sh "$v" "$tag" "$pkg" "$origin" "$pub"
}

head_go=$(toolchain <go.mod)
pub=$(gorun "$head_go" "$PWD" go run ./scripts/release-sign keygen /out/work/throwaway.key)
echo "$pub" >"$bundle/pubkey"
echo "$origin" >"$bundle/origin"

for tag in "${tags[@]}"; do
  src="$abs/src/$tag"
  mkdir -p "$src"
  git archive --format=tar "$tag" | tar -x -C "$src"
  v=$(git show "$tag:go.mod" | toolchain)
  echo "== $tag (go$v)"
  build "$tag" "$src" "$v"
done
echo "== $head_tag: this tree (go$head_go)"
build "$head_tag" "$PWD" "$head_go"

# The section reads the order from here: the old releases, then the one it
# updates them to.
echo "${tags[*]}" >"$bundle/old"
echo "$head_tag" >"$bundle/head"
rm -rf "$abs/src"
tar --no-xattrs -C "$abs" -czf "$abs/restore-upgrade-bundle.tar.gz" restore-upgrade-bundle
echo "restore-upgrade bundle: $out/restore-upgrade-bundle.tar.gz (old ${tags[*]}, head $head_tag; copy it to the box, see docs/e2e.md)"
