GO ?= go

# Everything a build writes goes here, so the repository root stays the source
# and `make clean` is one directory.
BUILD_DIR ?= build

# The integration tests need a Postgres. test-integration starts a throwaway one
# in a container and removes it again; KEERA_TEST_DATABASE_URL points them at a
# database of your own instead.
PODMAN ?= podman
COMPOSE ?= podman compose
PG_IMAGE ?= docker.io/library/postgres:latest
PG_CONTAINER ?= keera-test-pg
PG_PORT ?= 55432
PG_DSN ?= postgres://keera:keera@127.0.0.1:$(PG_PORT)/keera_test?sslmode=disable

# The Redis rate limiter needs a Redis for the same reason: what is tested is
# the Lua the server runs, which no fake covers.
REDIS_IMAGE ?= docker.io/library/redis:latest
REDIS_CONTAINER ?= keera-test-redis
REDIS_PORT ?= 56379
REDIS_URL ?= redis://127.0.0.1:$(REDIS_PORT)/0

.PHONY: build
build:
	@mkdir -p $(BUILD_DIR)
	@$(GO) build -trimpath -o $(BUILD_DIR)/keera-gateway ./cmd/keera-gateway
	@$(GO) build -trimpath -o $(BUILD_DIR)/keera ./cmd/keera

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

.PHONY: test-integration
test-integration: pg-up redis-up
	@KEERA_TEST_DATABASE_URL='$(PG_DSN)' KEERA_TEST_REDIS_URL='$(REDIS_URL)' \
		$(MAKE) --no-print-directory test-integration-only ; \
		status=$$?; $(MAKE) --no-print-directory pg-down redis-down; exit $$status

# The same tests against services that are already running: the containers
# above, a Postgres of your own, or CI's. It starts and removes nothing, and
# reads the two KEERA_TEST_* variables from its environment.
.PHONY: test-integration-only
test-integration-only:
	@$(GO) test -race -count=1 -p 1 $(INTEGRATION_PKGS)

.PHONY: check-all
check-all: check test-integration

.PHONY: pg-up
pg-up:
	@$(PODMAN) rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true
	@$(PODMAN) run --rm -d --name $(PG_CONTAINER) \
		-e POSTGRES_USER=keera -e POSTGRES_PASSWORD=keera -e POSTGRES_DB=keera_test \
		-p $(PG_PORT):5432 $(PG_IMAGE) >/dev/null
	@printf 'waiting for postgres'; \
	for i in $$(seq 1 60); do \
		if $(PODMAN) exec $(PG_CONTAINER) pg_isready -U keera -d keera_test >/dev/null 2>&1; then \
			echo " ready"; exit 0; \
		fi; \
		printf '.'; sleep 1; \
	done; \
	echo " timed out"; $(PODMAN) logs $(PG_CONTAINER); exit 1

.PHONY: pg-down
pg-down:
	@$(PODMAN) rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true

.PHONY: redis-up
redis-up:
	@$(PODMAN) rm -f $(REDIS_CONTAINER) >/dev/null 2>&1 || true
	@$(PODMAN) run --rm -d --name $(REDIS_CONTAINER) \
		-p $(REDIS_PORT):6379 $(REDIS_IMAGE) >/dev/null
	@printf 'waiting for redis'; \
	for i in $$(seq 1 30); do \
		if $(PODMAN) exec $(REDIS_CONTAINER) redis-cli ping >/dev/null 2>&1; then \
			echo " ready"; exit 0; \
		fi; \
		printf '.'; sleep 1; \
	done; \
	echo " timed out"; $(PODMAN) logs $(REDIS_CONTAINER); exit 1

.PHONY: redis-down
redis-down:
	@$(PODMAN) rm -f $(REDIS_CONTAINER) >/dev/null 2>&1 || true

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
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; }
	@$(GOLANGCI_LINT) run ./...

.PHONY: fmt-check
fmt-check:
	@test -z "$$(gofmt -l . | grep -v '^vendor/')" || \
		{ echo "not gofmt-clean:"; gofmt -l . | grep -v '^vendor/'; exit 1; }

.PHONY: clean
clean:
	@rm -rf $(BUILD_DIR)

# The whole platform in containers, the gateway included: the shape compose/
# describes, and the wrong one for a development loop, because every change is
# an image build.
#
# The tier is named the same way `dev` names it, and for the same reason it
# cannot be left out. The base file on its own is vLLM on CPU, which
# no documented path uses, and a `down` that forgets the override takes away a
# different keera-engine than the `up` started.
COMPOSE_TIER ?= cpu
TIER_COMPOSE := -f compose/compose.yaml \
	-f compose/compose.$(COMPOSE_TIER).yaml

.PHONY: compose-up
compose-up:
	@$(COMPOSE) $(TIER_COMPOSE) up -d --build

.PHONY: compose-down
compose-down:
	@$(COMPOSE) $(TIER_COMPOSE) down

# What is inside the image we ship, in a format an auditor or a vulnerability
# scanner can read. The image is FROM scratch, so this is the two Go binaries
# and the module graph compiled into them, which syft reads out of the build
# info the toolchain stamps on them.
#
# The image is rebuilt first rather than taken as found: an SBOM of whatever
# happened to be tagged last is worse than none, and the rebuild is cached.
GATEWAY_IMAGE ?= localhost/keera-gateway:latest

# What the binaries inside the image report as their version. .git is not in the
# build context, so the toolchain cannot stamp them itself. A local build leaves
# VERSION empty and the binary says "devel"; the release workflow passes the tag.
VERSION ?=
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null)

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
# works the same under podman and docker.
#
# --source-name and --source-version are needed because, pointed at an archive,
# syft would otherwise name the SBOM's subject as a path under /tmp.
.PHONY: sbom
sbom: image
	@mkdir -p $(SBOM_DIR)
	@id=$$($(PODMAN) image inspect --format '{{.Id}}' $(GATEWAY_IMAGE)); \
	out=$$(cd $(SBOM_DIR) && pwd); \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT INT TERM; \
	$(PODMAN) save --format oci-archive -o "$$tmp/image.tar" $(GATEWAY_IMAGE) >/dev/null 2>&1; \
	if command -v $(SYFT) >/dev/null 2>&1; then \
		$(SYFT) scan "oci-archive:$$tmp/image.tar" \
			--source-name '$(GATEWAY_IMAGE)' --source-version "sha256:$$id" \
			-o '$(SBOM_FORMAT)=$(SBOM_FILE)'; \
	else \
		$(PODMAN) run --rm \
			-v "$$tmp:/scan:z" -v "$$out:/out:z" $(SYFT_IMAGE) \
			scan oci-archive:/scan/image.tar \
			--source-name '$(GATEWAY_IMAGE)' --source-version "sha256:$$id" \
			-o '$(SBOM_FORMAT)=/out/$(notdir $(SBOM_FILE))'; \
	fi
	@echo "wrote $(SBOM_FILE) for $(GATEWAY_IMAGE) ($$($(PODMAN) image inspect --format '{{.Id}}' $(GATEWAY_IMAGE) | cut -c1-12))"

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

# Which inference backend the loop brings up. cpu is llama.cpp and needs no GPU;
# gpu is vLLM and does. Tool calls do not work on the cpu tier - see
# compose/README.md - so use gpu when working on anything that reads tool_calls.
GATEWAY_DEV_TIER ?= cpu
DEV_COMPOSE := -f compose/compose.yaml \
	-f compose/compose.$(GATEWAY_DEV_TIER).yaml \
	-f compose/compose.dev.yaml

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
	@# The two sandbox settings are defaulted only when a driver is named: a
	@# catalogue applied for a feature that is switched off puts classes in the
	@# database that nothing can create a machine from.
	@set -a; . ./$(DEV_ENV); set +a; \
		KEERA_DATABASE_URL='postgres://keera:keera@127.0.0.1:5432/keera?sslmode=disable' \
		KEERA_MODELS_FILE=compose/models.dev.yaml \
		KEERA_SANDBOXES_FILE="$${KEERA_SANDBOX_DRIVER:+$${KEERA_SANDBOXES_FILE:-compose/sandboxes.yaml}}" \
		KEERA_SANDBOX_PUBLIC_URL="$${KEERA_SANDBOX_DRIVER:+$${KEERA_SANDBOX_PUBLIC_URL:-http://host.containers.internal:8080}}" \
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
# deployment builds its own on top.
SANDBOX_IMAGE ?= localhost/keera-sandbox:dev

.PHONY: sandbox-image
sandbox-image:
	@$(PODMAN) build -f sandbox/Containerfile -t $(SANDBOX_IMAGE) .
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
	@# A containerised gateway from an earlier `make compose-up` would still be
	@# holding :8080, which air is about to want.
	@$(COMPOSE) $(DEV_COMPOSE) stop keera-gateway >/dev/null 2>&1 || true
	@# store.Open pings Postgres and fails fast rather than retrying, so air's
	@# first build would start a binary that exits at once and then sit there
	@# until the next save. Wait for the database instead.
	@printf 'waiting for postgres'; \
	for i in $$(seq 1 60); do \
		if $(COMPOSE) $(DEV_COMPOSE) exec -T keera-db pg_isready -U keera >/dev/null 2>&1; then \
			echo " ready"; exit 0; \
		fi; \
		printf '.'; sleep 1; \
	done; \
	echo " timed out"; $(COMPOSE) $(DEV_COMPOSE) logs keera-db; exit 1

# Takes the containers down but keeps the volumes, so the model weights and the
# database survive and the next `make dev` is instant. podman-compose prints
# a harmless "no container ... keera_keera-gateway_1" here, because it
# tries to remove the one service this loop never starts.
.PHONY: dev-down
dev-down:
	@$(COMPOSE) $(DEV_COMPOSE) down
