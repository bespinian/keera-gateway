GO ?= go

# Everything a build writes goes here, so the repository root stays the source
# and `make clean` is one directory.
BUILD_DIR ?= build

# The integration tests need a Postgres. test-integration starts a throwaway one
# in a container and removes it again, unless KEERA_TEST_DATABASE_URL and
# KEERA_TEST_REDIS_URL point at services of your own.
PODMAN ?= podman
COMPOSE ?= podman compose
# Pinned to the major versions CI runs. compose mounts its volume where
# Postgres 18 keeps its data, so a newer major would not find it.
PG_IMAGE ?= docker.io/library/postgres:18
PG_CONTAINER ?= keera-test-pg
PG_PORT ?= 55432
PG_DSN ?= postgres://keera:keera@127.0.0.1:$(PG_PORT)/keera_test?sslmode=disable

# The Redis rate limiter needs a Redis for the same reason: what is tested is
# the Lua the server runs, which no fake covers.
REDIS_IMAGE ?= docker.io/library/redis:8
REDIS_CONTAINER ?= keera-test-redis
REDIS_PORT ?= 56379
REDIS_URL ?= redis://127.0.0.1:$(REDIS_PORT)/0

# Without cgo, like the image, the release archives and the nix build, so what
# you test is what ships.
.PHONY: build
build:
	@mkdir -p $(BUILD_DIR)
	@CGO_ENABLED=0 $(GO) build -trimpath -o $(BUILD_DIR)/keera-gateway ./cmd/keera-gateway
	@CGO_ENABLED=0 $(GO) build -trimpath -o $(BUILD_DIR)/keera ./cmd/keera

.PHONY: check
check: test vet fmt-check

.PHONY: test
test:
	@$(GO) test -race -cover ./...

# The store's tests skip without a database, so `test` alone does not cover the
# schema, the queries or the migrations. The control plane joins them for the
# request log read live, and the rate limiter for the buckets shared through
# Redis - neither of which a fake covers.
#
# One package at a time, because they share the one database and the migration
# test drops its schema to prove a migration runs from nothing. Listed once
# here, so this target and CI cannot disagree.
INTEGRATION_PKGS := ./internal/store/... ./internal/control/... ./internal/ratelimit/...
INTEGRATION_TEST := $(GO) test -race -count=1 -p 1 $(INTEGRATION_PKGS)

.PHONY: test-integration
# With both KEERA_TEST_* variables set, as in CI, it starts and removes nothing.
ifneq ($(and $(KEERA_TEST_DATABASE_URL),$(KEERA_TEST_REDIS_URL)),)
test-integration:
	@$(INTEGRATION_TEST)
else
test-integration: pg-up redis-up
	@KEERA_TEST_DATABASE_URL='$(PG_DSN)' KEERA_TEST_REDIS_URL='$(REDIS_URL)' \
		$(INTEGRATION_TEST) ; \
		status=$$?; $(MAKE) --no-print-directory pg-down redis-down; exit $$status
endif

.PHONY: check-all
check-all: check test-integration

# wait-for polls a probe once a second until it succeeds. $(1) is what it
# waits for, $(2) how many seconds it waits, $(3) the probe, and $(4) prints
# the logs when it gives up.
define wait-for
printf 'waiting for $(1)'; \
for i in $$(seq 1 $(2)); do \
	if $(3) >/dev/null 2>&1; then \
		echo " ready"; exit 0; \
	fi; \
	printf '.'; sleep 1; \
done; \
echo " timed out"; $(4); exit 1
endef

# test-service starts a throwaway container and waits for it. $(1) is what
# it is called in messages, $(2) the container, $(3) the arguments to `podman
# run`, image included, $(4) how many seconds it may take and $(5) the probe
# run inside it. test-service-down removes container $(1) again.
test-service-down = $(PODMAN) rm -f $(1) >/dev/null 2>&1 || true

define test-service
$(call test-service-down,$(2))
@$(PODMAN) run --rm -d --name $(2) $(3) >/dev/null
@$(call wait-for,$(1),$(4),$(PODMAN) exec $(2) $(5),$(PODMAN) logs $(2))
endef

.PHONY: pg-up
pg-up:
	@$(call test-service,postgres,$(PG_CONTAINER),\
		-e POSTGRES_USER=keera -e POSTGRES_PASSWORD=keera -e POSTGRES_DB=keera_test \
		-p $(PG_PORT):5432 $(PG_IMAGE),60,pg_isready -U keera -d keera_test)

.PHONY: redis-up
redis-up:
	@$(call test-service,redis,$(REDIS_CONTAINER),-p $(REDIS_PORT):6379 $(REDIS_IMAGE),30,redis-cli ping)

.PHONY: pg-down
pg-down:
	@$(call test-service-down,$(PG_CONTAINER))

.PHONY: redis-down
redis-down:
	@$(call test-service-down,$(REDIS_CONTAINER))

.PHONY: vet
vet:
	@$(GO) vet ./...

# golangci-lint is not vendored and not required to build or test anything, so
# this is kept out of `check` and asks for it rather than installing it.
GOLANGCI_LINT ?= golangci-lint

.PHONY: lint
lint:
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		echo "golangci-lint is not on PATH. Install it with:"; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0"; \
		echo "CI pins v2.14, so a newer one may report what CI does not."; \
		exit 1; }
	@$(GOLANGCI_LINT) run ./...

.PHONY: fmt-check
fmt-check:
	@# Only the source directories, so vendor/ is skipped.
	@out=$$(gofmt -l cmd internal); \
		test -z "$$out" || { echo "not gofmt-clean:"; echo "$$out"; exit 1; }

.PHONY: clean
clean:
	@rm -rf $(BUILD_DIR)

GATEWAY_IMAGE ?= localhost/keera-gateway:latest

# What the binary inside the image and `make dist` report as their version.
# .git is not in the build context, so the toolchain cannot stamp it itself.
# Both follow what the toolchain stamps for `make build`: the tag when HEAD is
# one, "devel" otherwise, and the commit, marked dirty when the tree has
# changes - so one commit reads the same however it was built. The release
# workflow passes the tag.
VERSION ?= $(shell git describe --tags --exact-match --match 'v[0-9]*' HEAD 2>/dev/null)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null)$(if $(shell git status --porcelain 2>/dev/null),-dirty)

# What is inside the image we ship, in a format an auditor or a vulnerability
# scanner can read. The image is FROM scratch, so this is the Go binary
# and the module graph compiled into it, which syft reads out of the build
# info the toolchain stamps on them.
#
# The image is rebuilt first rather than taken as found: an SBOM of whatever
# happened to be tagged last is worse than none, and the rebuild is cached.
SYFT ?= syft
SYFT_IMAGE ?= docker.io/anchore/syft:latest
SBOM_DIR ?= $(BUILD_DIR)/sbom
# CycloneDX because that is what the scanners downstream take. syft also writes
# spdx-json and others; a different format wants a different SBOM_FILE.
SBOM_FORMAT ?= cyclonedx-json
SBOM_FILE ?= $(SBOM_DIR)/keera-gateway.cdx.json

.PHONY: image
image:
	@$(PODMAN) build -f Containerfile \
		--build-arg VERSION='$(VERSION)' --build-arg REVISION='$(REVISION)' \
		-t $(GATEWAY_IMAGE) .

# syft is run from its own image unless one is on PATH. It reads an OCI archive
# rather than the image store, which keeps it out of the container socket and
# works the same under podman and docker. run_syft hides which one it is: the
# container sees the archive under /scan and writes to /out.
#
# --source-name and --source-version are needed because, pointed at an archive,
# syft would otherwise name the SBOM's subject as a path under /tmp.
.PHONY: sbom
sbom: image
	@mkdir -p $(dir $(SBOM_FILE))
	@id=$$($(PODMAN) image inspect --format '{{.Id}}' $(GATEWAY_IMAGE)); \
	out=$$(cd $(dir $(SBOM_FILE)) && pwd); \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT INT TERM; \
	$(PODMAN) save --format oci-archive -o "$$tmp/image.tar" $(GATEWAY_IMAGE) >/dev/null 2>&1; \
	if command -v $(SYFT) >/dev/null 2>&1; then \
		run_syft() { $(SYFT) "$$@"; }; scan=$$tmp; dest=$$out; \
	else \
		run_syft() { $(PODMAN) run --rm -v "$$tmp:/scan:z" -v "$$out:/out:z" $(SYFT_IMAGE) "$$@"; }; \
		scan=/scan; dest=/out; \
	fi; \
	run_syft scan "oci-archive:$$scan/image.tar" \
		--source-name '$(GATEWAY_IMAGE)' --source-version "sha256:$$id" \
		-o "$(SBOM_FORMAT)=$$dest/$(notdir $(SBOM_FILE))" && \
	echo "wrote $(SBOM_FILE) for $(GATEWAY_IMAGE) ($$(echo "$$id" | cut -c1-12))"

# The licences of everything compiled into the binaries. MIT, BSD and Apache-2.0
# ask for them to ship with a binary, so the image and every release archive
# carry this file. The image writes its own copy with the same script.
NOTICES_FILE ?= $(BUILD_DIR)/THIRD_PARTY_NOTICES

.PHONY: notices
notices:
	@mkdir -p $(dir $(NOTICES_FILE))
	@GO='$(GO)' sh scripts/third-party-notices.sh > $(NOTICES_FILE).tmp
	@mv $(NOTICES_FILE).tmp $(NOTICES_FILE)

# The keera command as a release attaches it: one archive per platform, with the
# licence and the notices next to the binary, and SHA256SUMS over them.
#
# tar.gz for Windows too. Windows 10 and later ship tar, and one format is one
# thing less to get wrong.
DIST_DIR ?= $(BUILD_DIR)/dist
CLI_PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
VERSION_PKG := github.com/bespinian/keera-gateway/internal/version

.PHONY: dist
dist: notices
	@rm -rf $(DIST_DIR)
	@mkdir -p $(DIST_DIR)
	@for p in $(CLI_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		case $$os in windows) exe=.exe ;; *) exe= ;; esac; \
		name=keera_$(or $(VERSION),devel)_$${os}_$${arch}; \
		mkdir -p $(DIST_DIR)/$$name; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath \
			-ldflags='-s -w -X $(VERSION_PKG).release=$(VERSION) -X $(VERSION_PKG).revision=$(REVISION)' \
			-o $(DIST_DIR)/$$name/keera$$exe ./cmd/keera || exit 1; \
		cp LICENSE $(NOTICES_FILE) $(DIST_DIR)/$$name/; \
		tar -C $(DIST_DIR) -czf $(DIST_DIR)/$$name.tar.gz $$name || exit 1; \
		rm -rf $(DIST_DIR)/$$name; \
	done
	@cd $(DIST_DIR) && sha256sum *.tar.gz > SHA256SUMS
	@echo "wrote $$(ls $(DIST_DIR)/*.tar.gz | wc -l) archives and SHA256SUMS to $(DIST_DIR)"

# The development loop. The server runs on the host under air, so a save
# rebuilds and restarts it in about a second, while Postgres and the inference
# backend stay in containers behind it. ^C stops the gateway and leaves those
# up, which makes every start after the first instant; `make dev-down` stops
# them.
AIR ?= air
DEV_ENV := compose/.env

# Which inference backend `make dev` brings up. cpu is
# llama.cpp and needs no GPU; gpu is vLLM and does. Tool calls do not work on
# the cpu tier - see compose/README.md - so use gpu when working on anything
# that reads tool_calls.
TIER ?= cpu
DEV_COMPOSE := -f compose/compose.yaml \
	$(if $(filter gpu,$(TIER)),-f compose/compose.gpu.yaml) -f compose/compose.dev.yaml

.PHONY: dev
dev: dev-env dev-backends
	@command -v $(AIR) >/dev/null 2>&1 || { \
		echo "air is not on PATH. Install it with:"; \
		echo "  go install github.com/air-verse/air@latest"; \
		exit 1; }
	@echo
	@echo "  panel      http://127.0.0.1:8080"
	@echo "  inference  http://127.0.0.1:8080/api"
	@echo "  operator key  $$(sed -n 's/^KEERA_OPERATOR_KEY=//p' $(DEV_ENV))"
	@echo
	@echo "^C stops the gateway and leaves the containers up."
	@echo
	@# The two catalogue files are defaults, so .env can name others. The
	@# sandbox class file is set even without a driver, so the first
	@# organisation has classes once one is switched on; the sandbox address
	@# only when a driver is named. The backends are on loopback here, so the
	@# deny list is the default one without loopback: keep it in step with
	@# DefaultUpstreamDeny in internal/gateway/egress.go.
	@set -a; . ./$(DEV_ENV); set +a; \
		KEERA_DATABASE_URL='postgres://keera:keera@127.0.0.1:5432/keera?sslmode=disable' \
		KEERA_MODELS_FILE="$${KEERA_MODELS_FILE:-compose/models.dev.yaml}" \
		KEERA_SANDBOXES_FILE="$${KEERA_SANDBOXES_FILE:-compose/sandboxes.yaml}" \
		KEERA_SANDBOX_PUBLIC_URL="$${KEERA_SANDBOX_DRIVER:+$${KEERA_SANDBOX_PUBLIC_URL:-http://host.containers.internal:8080}}" \
		KEERA_UPSTREAM_DENY="$${KEERA_UPSTREAM_DENY:-169.254.0.0/16,fe80::/10,0.0.0.0/8,::/128,fd00:ec2::254/128}" \
		KEERA_LOG_FORMAT=text KEERA_LOG_LEVEL=debug \
		$(AIR)

# The sandbox image the development catalogue names.
#
# Separate from `dev` rather than a dependency, because building it pulls a
# base image and installs a toolchain - a minute nobody wants in a loop they run
# twenty times a day.
#
# sandbox/Containerfile is a base: it owns the parts the gateway
# depends on - the fixed uid, sshd on 2222, the entrypoint - and a real
# deployment builds its own on top. Its build context is sandbox/ alone, so
# vendor/ and the rest of the repository are not sent to the builder.
SANDBOX_IMAGE ?= localhost/keera-sandbox:dev

.PHONY: sandbox-image
sandbox-image:
	@$(PODMAN) build -t $(SANDBOX_IMAGE) sandbox
	@echo
	@echo "built $(SANDBOX_IMAGE), which compose/sandboxes.yaml names."
	@echo "Add KEERA_SANDBOX_DRIVER=podman to $(DEV_ENV) and restart 'make dev'."

# The gateway refuses to start without an operator key, and compose will not
# render its own file while either credential is unset - so a first run on a
# fresh clone generates them. An existing .env is left as it is.
#
# The /dev/urandom fallback is for a stock Nix host, which has no openssl. It
# is written to a temporary file and moved into place, because a half-written
# .env with empty keys would be taken for a valid one on the next run.
.PHONY: dev-env
dev-env:
	@if [ ! -f $(DEV_ENV) ]; then \
		echo "generating $(DEV_ENV) with fresh development credentials"; \
		if command -v openssl >/dev/null 2>&1; then \
			operator=$$(openssl rand -hex 32); secret=$$(openssl rand -hex 32); \
		else \
			operator=$$(od -An -vtx1 -N32 /dev/urandom | tr -d ' \n'); \
			secret=$$(od -An -vtx1 -N32 /dev/urandom | tr -d ' \n'); \
		fi; \
		if [ "$${#operator}" -ne 64 ] || [ "$${#secret}" -ne 64 ]; then \
			echo "could not generate 32 random bytes - install openssl and retry" >&2; \
			exit 1; \
		fi; \
		sed -e "s|^KEERA_OPERATOR_KEY=.*|KEERA_OPERATOR_KEY=$$operator|" \
		    -e "s|^KEERA_SECRET_KEY=.*|KEERA_SECRET_KEY=$$secret|" \
		    compose/.env.example > $(DEV_ENV).tmp && mv $(DEV_ENV).tmp $(DEV_ENV); \
		chmod 0600 $(DEV_ENV); \
	fi

.PHONY: dev-backends
dev-backends:
	@$(COMPOSE) $(DEV_COMPOSE) up -d keera-db keera-engine
	@# A containerised gateway from an earlier `podman compose up` would still be
	@# holding :8080, which air is about to want.
	@$(COMPOSE) $(DEV_COMPOSE) stop keera-gateway >/dev/null 2>&1 || true
	@# store.Open pings Postgres and fails fast rather than retrying, so air's
	@# first build would start a binary that exits at once and then sit there
	@# until the next save. Wait for the database instead.
	@$(call wait-for,postgres,60,$(COMPOSE) $(DEV_COMPOSE) exec -T keera-db pg_isready -U keera,$(COMPOSE) $(DEV_COMPOSE) logs keera-db)

# Takes the containers down but keeps the volumes, so the model weights and the
# database survive and the next `make dev` is instant. podman-compose prints
# a harmless "no container ... keera_keera-gateway_1" here, because it
# tries to remove the one service this loop never starts.
.PHONY: dev-down
dev-down:
	@$(COMPOSE) $(DEV_COMPOSE) down
