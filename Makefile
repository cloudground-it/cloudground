# CloudGround build, test and release targets. Go and Node may be missing on
# the workstation: every docker-* target runs the same step in a container.

# ---------------------------------------------------------------- settings --

# What a development build calls itself.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Where releases come from (internal/version SPEC §4). The keys a release
# must be signed with are source (internal/version/keys.go); UPDATE_PUBKEY
# adds a throwaway key to a test build, never to a published release.
UPDATE_URL    ?= https://github.com/cloudground-it/cloudground
UPDATE_PUBKEY ?=

VERSION_PKG = github.com/cloudground-it/cloudground/internal/version
LDFLAGS     = -s -w -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).UpdateURL=$(UPDATE_URL) -X $(VERSION_PKG).UpdatePublicKey=$(UPDATE_PUBKEY)
BINARIES    = panel-api panel-agent cgctl cg-site-shell

# Containers. The tests run on the host's own Linux architecture: under amd64
# emulation on an arm64 Mac openat2 is missing and the symlink-confinement
# tests fail for reasons that have nothing to do with the code.
GO_IMAGE   ?= golang:1.26
NODE_IMAGE ?= node:22.23.3
GO_VOLUMES  = -v cloudground-gomod:/go/pkg/mod -v cloudground-gocache:/root/.cache/go-build
DOCKER_GO   = docker run --rm -v "$(CURDIR)":/src -w /src -e GOFLAGS=-buildvcs=false $(GO_VOLUMES) $(GO_IMAGE)
# node_modules lives in a volume: the host's copy holds macOS-native tools a
# Linux container cannot run.
# ui/dist depends on the path it is built at (scripts/build-release.sh, UI_BUILD_DIR).
UI_BUILD_DIR = /tmp/cloudground-ui
DOCKER_NODE = docker run --rm -v "$(CURDIR)/ui":$(UI_BUILD_DIR) -v cloudground-ui-modules:$(UI_BUILD_DIR)/node_modules -w $(UI_BUILD_DIR) $(NODE_IMAGE)

.PHONY: build ui test vet dist clean e2e-bin e2e-index api-doc \
	docker-test docker-dist docker-demo docker-e2e-bin docker-e2e-index docker-api-doc docker-nginx-test docker-install-test docker-php-test docker-upgrade-bundle docker-restore-upgrade-bundle docker-vulncheck \
	release release-build release-sign release-verify release-require-key \
	docker-release docker-release-build docker-release-repro

# ------------------------------------------------------------- development --

ui:
	bash scripts/build-release.sh ui

build: ui
	mkdir -p bin
	for b in $(BINARIES); do go build -ldflags '$(LDFLAGS)' -o bin/$$b ./cmd/$$b || exit 1; done

# Linux amd64 binaries for a test server (a release is `make release`).
dist: ui
	mkdir -p dist
	for b in $(BINARIES); do GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o dist/$$b ./cmd/$$b || exit 1; done
	# The benchmark driver (bench/SPEC.md); the installer leaves it alone.
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/cgbench ./cmd/cgbench

vet:
	go vet ./...
	go vet -tags e2e ./e2e/...

test: vet
	go test ./...

# The end-to-end suite (e2e/README.md), built to be copied to a server and
# run there as root.
e2e-bin:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -tags e2e -o dist/cg-e2e ./e2e

# e2e/sections.index, which sections reach each function of the suite
# (scripts/e2e-affected.sh, scripts/e2e-new-checks.sh). `make test` fails
# while it is stale.
e2e-index:
	CG_E2E_UPDATE_INDEX=1 go test -count=1 -run TestSectionsIndex ./e2e

# docs/api.md from the route table (internal/api/routes.go); `make test`
# fails while it is stale.
api-doc:
	go generate ./internal/api

clean:
	rm -rf bin dist ui/dist

# -------------------------------------------------------------- containers --

docker-test:
	$(DOCKER_GO) sh -c 'bad=$$(gofmt -l internal cmd e2e scripts); if [ -n "$$bad" ]; then echo "$$bad"; echo "gofmt: the files above need formatting"; exit 1; fi; go vet ./... && go vet -tags e2e ./e2e/... && go test -race -timeout 30m ./...'

# Known vulnerabilities in the Go standard library and the modules, reported
# only where the code calls the vulnerable function (as CI does).
GOVULNCHECK = golang.org/x/vuln/cmd/govulncheck@v1.9.0
docker-vulncheck:
	$(DOCKER_GO) go run $(GOVULNCHECK) ./...

docker-dist:
	$(DOCKER_NODE) sh -c 'npm ci --silent --no-audit --no-fund && npm run build --silent'
	$(DOCKER_GO) sh -c 'mkdir -p dist && for b in $(BINARIES); do GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$$b ./cmd/$$b || exit 1; done'
	$(DOCKER_GO) env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/cgbench ./cmd/cgbench

# The panel's demo (docs/demo.md): the interface against sample data that runs
# in the browser, into ui/dist-demo (not committed). ui/dist is not touched.
docker-demo:
	$(DOCKER_NODE) sh -c 'npm ci --silent --no-audit --no-fund && npm test --silent && npm run build:demo --silent'

docker-e2e-bin:
	$(DOCKER_GO) sh -c 'mkdir -p dist && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -tags e2e -o dist/cg-e2e ./e2e'

# The upgrade test's signed releases (scripts/upgrade-bundle.sh): copy
# dist/upgrade/upgrade-bundle.tar.gz to the box for the e2e section
# "R-6.5 upgrade" (docs/e2e.md).
docker-upgrade-bundle:
	$(DOCKER_GO) bash scripts/upgrade-bundle.sh dist/upgrade

# The restore test's releases (scripts/restore-upgrade-bundle.sh): real old
# tags, each built with its own toolchain, and this tree labelled the next
# patch; copy dist/restore-upgrade/restore-upgrade-bundle.tar.gz to the box
# for the e2e section "R-6.6 restore across updates" (docs/e2e.md). Runs
# docker itself, once per toolchain. RESTORE_TAGS overrides v0.1.0 v0.9.0.
RESTORE_TAGS ?=
docker-restore-upgrade-bundle:
	GO_VOLUMES='$(GO_VOLUMES)' bash scripts/restore-upgrade-bundle.sh dist/restore-upgrade $(RESTORE_TAGS)

docker-e2e-index:
	$(DOCKER_GO) env CG_E2E_UPDATE_INDEX=1 go test -count=1 -run TestSectionsIndex ./e2e

docker-api-doc:
	$(DOCKER_GO) go generate ./internal/api

# nginx -t over the whole render matrix (internal/engine/nginx/SPEC.md §6),
# with each distribution's own nginx.
NGINX_IMAGES ?= ubuntu:24.04 ubuntu:26.04
docker-nginx-test:
	rm -rf dist/nginx-matrix
	$(DOCKER_GO) env CG_NGINX_DUMP=/src/dist/nginx-matrix go test -count=1 -run TestRenderMatrix ./internal/engine/nginx
	for img in $(NGINX_IMAGES); do docker run --rm -v "$(CURDIR)/dist/nginx-matrix":/m:ro -v "$(CURDIR)/scripts":/s:ro $$img bash /s/nginx-matrix-check.sh /m || exit 1; done

# cgctl install --plan, cgctl install and cgctl doctor in each distro's
# container, systemd not PID 1 and apt skipped (internal/install/SPEC.md §8).
# The binaries are built for the container's own architecture.
INSTALL_IMAGES ?= ubuntu:24.04 ubuntu:26.04
docker-install-test:
	rm -rf dist/install-test
	$(DOCKER_GO) sh -c 'mkdir -p dist/install-test && for b in $(BINARIES); do CGO_ENABLED=0 go build -o dist/install-test/$$b ./cmd/$$b || exit 1; done'
	for img in $(INSTALL_IMAGES); do docker run --rm -v "$(CURDIR)/dist/install-test":/b:ro -v "$(CURDIR)/scripts":/s:ro -v "$(CURDIR)/installer":/i:ro $$img bash /s/install-container-check.sh /b || exit 1; done

# Every connector PHP suite (connector/php/tests/*.php, bar the check.php and
# wp.php helpers) in each PHP image; a suite exits 1 on any failed check.
PHP_IMAGES ?= php:8.4-cli php:8.5-cli
docker-php-test:
	for img in $(PHP_IMAGES); do docker run --rm -v "$(CURDIR)":/src:ro -w /src $$img sh -c 'for t in connector/php/tests/*.php; do case $$t in */check.php|*/wp.php) continue;; esac; echo "== $$t"; php $$t || exit 1; done' || exit 1; done

# ----------------------------------------------------------------- release --
#
# make release SIGNING_KEY=<file>          on the tagged commit, with go.mod's Go,
#                                           Node and GNU tar (the release workflow)
# make docker-release SIGNING_KEY=<file>   the same in pinned containers
#
# A release is dist/release/<tag>/: the binaries, the units, the stamped install.sh, the
# tarball, SHA256SUMS and SHA256SUMS.sig. It is built reproducibly
# (scripts/build-release.sh), signed by scripts/release-sign, and then
# checked by `cgctl release verify` with the keys the binaries carry: a
# release that is not signed, or not by a key the binaries trust, fails here
# and is never published.

RELEASE_TAG ?= $(shell git describe --tags --exact-match 2>/dev/null)
RELEASE_DIR  = dist/release/$(RELEASE_TAG)
SIGNING_KEY ?=
GO_RELEASE_IMAGE   ?= golang:1.26.10
NODE_RELEASE_IMAGE ?= node:22.23.3
# A worktree's .git names the main repository by its host path: the
# container sees it at the same path, so -buildvcs can read the commit.
GIT_COMMON := $(shell git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)
DOCKER_RELEASE_GO = docker run --rm -v "$(CURDIR)":/src -w /src -v "$(GIT_COMMON)":"$(GIT_COMMON)":ro \
	-e UPDATE_URL='$(UPDATE_URL)' -e UPDATE_PUBKEY='$(UPDATE_PUBKEY)' -e CG_RELEASE_TEST $(GO_VOLUMES)
RELEASE_GIT_SAFE  = git config --global --add safe.directory "*"

release-require-key:
	@test -n "$(RELEASE_TAG)" || { echo "release: HEAD carries no tag; set RELEASE_TAG" >&2; exit 1; }
	@test -n "$(SIGNING_KEY)" && test -f "$(SIGNING_KEY)" || { echo "release: SIGNING_KEY=<private key file> is required: an unsigned release is never built" >&2; exit 1; }

release: release-require-key release-build release-sign

release-build:
	@test -n "$(RELEASE_TAG)" || { echo "release: HEAD carries no tag; set RELEASE_TAG" >&2; exit 1; }
	bash scripts/build-release.sh ui
	UPDATE_URL='$(UPDATE_URL)' UPDATE_PUBKEY='$(UPDATE_PUBKEY)' bash scripts/build-release.sh bin $(RELEASE_TAG) $(RELEASE_DIR)

release-sign: release-require-key
	go run ./scripts/release-sign sign "$(SIGNING_KEY)" $(RELEASE_TAG) $(RELEASE_DIR)
	$(MAKE) release-verify

# The release's own check, with the keys the release binaries were built
# with (same source, same UPDATE_PUBKEY); also the publish job's last step.
release-verify:
	go run -ldflags '-X $(VERSION_PKG).UpdatePublicKey=$(UPDATE_PUBKEY)' ./cmd/cgctl release verify $(RELEASE_DIR)

docker-release: release-require-key docker-release-build
	$(DOCKER_RELEASE_GO) -v "$(abspath $(SIGNING_KEY))":/run/release.key:ro $(GO_RELEASE_IMAGE) sh -c '$(RELEASE_GIT_SAFE) && \
		go run ./scripts/release-sign sign /run/release.key $(RELEASE_TAG) $(RELEASE_DIR) && \
		go run -ldflags "-X $(VERSION_PKG).UpdatePublicKey=$$UPDATE_PUBKEY" ./cmd/cgctl release verify $(RELEASE_DIR)'

docker-release-build:
	@test -n "$(RELEASE_TAG)" || { echo "release: HEAD carries no tag; set RELEASE_TAG" >&2; exit 1; }
	docker run --rm -v "$(CURDIR)/ui":$(UI_BUILD_DIR) -v cloudground-ui-modules:$(UI_BUILD_DIR)/node_modules -w $(UI_BUILD_DIR) $(NODE_RELEASE_IMAGE) \
		sh -c 'npm ci --no-audit --no-fund --silent && npm run build --silent'
	$(DOCKER_RELEASE_GO) $(GO_RELEASE_IMAGE) sh -c '$(RELEASE_GIT_SAFE) && bash scripts/build-release.sh bin $(RELEASE_TAG) $(RELEASE_DIR)'

# Two builds of HEAD from cold Go caches must be byte for byte the same.
# RELEASE_TAG defaults to v0.0.0 here: the check needs no tag on HEAD.
docker-release-repro: export CG_RELEASE_TEST = 1
docker-release-repro:
	rm -rf dist/repro
	$(MAKE) docker-release-build RELEASE_TAG=$(or $(RELEASE_TAG),v0.0.0) RELEASE_DIR=dist/repro/a GO_VOLUMES='-v cloudground-repro-mod-a:/go/pkg/mod -v cloudground-repro-cache-a:/root/.cache/go-build'
	$(MAKE) docker-release-build RELEASE_TAG=$(or $(RELEASE_TAG),v0.0.0) RELEASE_DIR=dist/repro/b GO_VOLUMES='-v cloudground-repro-mod-b:/go/pkg/mod -v cloudground-repro-cache-b:/root/.cache/go-build'
	docker volume rm cloudground-repro-cache-a cloudground-repro-cache-b >/dev/null
	docker run --rm -v "$(CURDIR)/dist/repro":/r -w /r $(GO_RELEASE_IMAGE) sh -c '(cd a && sha256sum *) > a.sums && (cd b && sha256sum *) > b.sums && cat a.sums && cmp a.sums b.sums && echo "reproducible: both builds are identical"'
