OUTPUT     := libspa.so
GOCMD      := CGO_ENABLED=1 go
UI_DIR     := ui
DIST       := $(UI_DIR)/dist
NM         := $(UI_DIR)/node_modules
IMAGE_TAG  ?= spa:latest

ZIG_VERSION := 0.16.0
ZIG_BIN     := $(CURDIR)/.bin/zig

# Detect host OS/arch for the zig download URL.
# zig release naming: zig-{arch}-{os}-{version}
# uname -m returns "arm64" on macOS but zig uses "aarch64" everywhere.
_OS      := $(shell uname -s | tr '[:upper:]' '[:lower:]')
_ARCH    := $(shell uname -m | sed 's/arm64/aarch64/')
ZIG_OS   := $(if $(filter darwin,$(_OS)),macos,$(_OS))
ZIG_URL  := https://ziglang.org/download/$(ZIG_VERSION)/zig-$(_ARCH)-$(ZIG_OS)-$(ZIG_VERSION).tar.xz

.PHONY: all build build-so ui install clean dev e2e zig \
        build-linux-amd64 build-linux-arm64 build-linux \
        docker docker-push

## all: full build — install npm deps, build Vite, compile .so
all: build

## build: install npm deps if needed, build Vite output, then compile .so
build: ui build-so

## build-so: compile the .so only (ui/dist must already exist)
build-so:
	$(GOCMD) build -trimpath -buildmode=c-shared -o $(OUTPUT) ./cmd

## ui: install npm deps (if not present) and run vite build
ui: $(NM)
	cd $(UI_DIR) && npm run build

## install: install npm dependencies only
$(NM): $(UI_DIR)/package.json
	cd $(UI_DIR) && npm install
	@touch $(NM)

## dev: run the Vite dev server with HMR (port 5173)
## Use this alongside a running Envoy to get hot-reload for UI changes.
## Vite proxies /api/* to Envoy in dev mode — see ui/vite.config.ts.
dev:
	cd $(UI_DIR) && npm run dev

## e2e: run end-to-end browser tests (requires Envoy to be running on :10000)
## Uses Lightpanda (headless browser) + Playwright via CDP.
## Start Envoy first:  ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(pwd) envoy -c envoy.yaml
e2e:
	cd e2e && node --test spa.test.mjs

# ── Zig (on demand) ──────────────────────────────────────────────────────────

## zig: download zig $(ZIG_VERSION) into .bin/ if not already present
zig: $(ZIG_BIN)

$(ZIG_BIN):
	@echo "downloading zig $(ZIG_VERSION) for $(_ARCH)-$(ZIG_OS)..."
	@mkdir -p .bin
	@curl -fsSL "$(ZIG_URL)" | tar -xJ --strip-components=1 -C .bin
	@echo "zig $(ZIG_VERSION) ready at $(ZIG_BIN)"

# ── Cross-compiled Linux builds (macOS or any host) ──────────────────────────

## build-linux-amd64: cross-compile libspa.linux-amd64.so via zig cc
build-linux-amd64: ui $(ZIG_BIN)
	@$(MAKE) _cross-build GOARCH=amd64 ZIG_TARGET=x86_64-linux-gnu  OUT=libspa.linux-amd64.so

## build-linux-arm64: cross-compile libspa.linux-arm64.so via zig cc
build-linux-arm64: ui $(ZIG_BIN)
	@$(MAKE) _cross-build GOARCH=arm64 ZIG_TARGET=aarch64-linux-gnu OUT=libspa.linux-arm64.so

## build-linux: cross-compile both amd64 and arm64 .so files
build-linux: build-linux-amd64 build-linux-arm64

# Internal target — do not call directly.
_cross-build:
	@printf '#!/usr/bin/env bash\nargs=()\nfor arg in "$$@"; do case "$$arg" in --unresolved-symbols=*) ;; -Wl,--unresolved-symbols=*) ;; *) args+=("$$arg") ;; esac; done\nexec $(ZIG_BIN) cc -target $(ZIG_TARGET) "$${args[@]}"\n' > /tmp/zigcc-$(ZIG_TARGET).sh
	@chmod +x /tmp/zigcc-$(ZIG_TARGET).sh
	CC=/tmp/zigcc-$(ZIG_TARGET).sh CGO_ENABLED=1 GOOS=linux GOARCH=$(GOARCH) \
	  go build -trimpath -buildmode=c-shared -o $(OUT) ./cmd
	@echo "built: $(OUT)"

# ── Docker ───────────────────────────────────────────────────────────────────

## docker: cross-compile on host, package into distroless image (loads locally)
docker: build-linux
	docker buildx build \
	  --platform linux/amd64,linux/arm64 \
	  --load \
	  -t $(IMAGE_TAG) .

## docker-push: same as docker but pushes to a registry
## Usage: make docker-push IMAGE_TAG=ghcr.io/dio/jisr/spa:latest
docker-push: build-linux
	docker buildx build \
	  --platform linux/amd64,linux/arm64 \
	  --push \
	  -t $(IMAGE_TAG) .

## smoke: run the image locally and verify all endpoints respond correctly
## Builds a local amd64 image first if IMAGE_TAG is the default.
smoke: _smoke-build
	@echo "--- starting container..."
	@docker run -d --rm --name spa-smoke -p 10002:10000 $(IMAGE_TAG)
	@trap 'docker stop spa-smoke 2>/dev/null; exit' INT TERM; \
	for i in $$(seq 1 10); do \
	  curl -sf http://localhost:10002/ >/dev/null 2>&1 && break; \
	  sleep 1; \
	done
	@echo "--- GET / (expect 200 HTML)"
	@curl -sf -o /dev/null -w "  %{http_code} %{content_type}\n" http://localhost:10002/
	@echo "--- GET /about (expect 200 HTML, SPA fallback)"
	@curl -sf -o /dev/null -w "  %{http_code} %{content_type}\n" http://localhost:10002/about
	@echo "--- GET /api/hello (expect 200 JSON)"
	@curl -sf http://localhost:10002/api/hello | python3 -c "import sys,json; d=json.load(sys.stdin); assert d['filter']=='api-backend'; print(' ', d)"
	@echo "--- GET /api/time (expect 200 JSON with timestamp)"
	@curl -sf http://localhost:10002/api/time | python3 -c "import sys,json; d=json.load(sys.stdin); assert 'T' in d['time']; print(' ', d)"
	@echo "--- GET /api/unknown (expect 404)"
	@curl -sf -o /dev/null -w "  %{http_code}\n" http://localhost:10002/api/unknown || true
	@echo "--- all checks passed"
	@docker stop spa-smoke

_smoke-build:
	@if [ "$(IMAGE_TAG)" = "spa:latest" ]; then \
	  docker buildx build --platform linux/amd64 --load -t $(IMAGE_TAG) . 2>/dev/null || \
	  docker buildx build --platform linux/arm64 --load -t $(IMAGE_TAG) .; \
	fi

## clean: remove .so files, cross-compiled outputs, and built frontend assets
clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h) libspa.linux-amd64.so libspa.linux-arm64.so
	rm -rf $(DIST)
