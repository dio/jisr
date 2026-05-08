OUTPUT     := libspa.so
GOCMD      := CGO_ENABLED=1 go
UI_DIR     := ui
DIST       := $(UI_DIR)/dist
NM         := $(UI_DIR)/node_modules
ZIG        ?= /tmp/zig-aarch64-macos-0.16.0/zig
IMAGE_TAG  ?= spa:latest

.PHONY: all build build-so ui install clean dev e2e \
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

# ── Cross-compiled Linux builds (macOS or any host, requires zig) ────────────

## build-linux-amd64: cross-compile libspa.linux-amd64.so via zig cc
build-linux-amd64: ui
	@$(MAKE) _cross-build GOARCH=amd64 ZIG_TARGET=x86_64-linux-gnu  OUT=libspa.linux-amd64.so

## build-linux-arm64: cross-compile libspa.linux-arm64.so via zig cc
build-linux-arm64: ui
	@$(MAKE) _cross-build GOARCH=arm64 ZIG_TARGET=aarch64-linux-gnu OUT=libspa.linux-arm64.so

## build-linux: cross-compile both amd64 and arm64 .so files
build-linux: build-linux-amd64 build-linux-arm64

# Internal target — do not call directly.
# Writes a zig cc wrapper to /tmp, then runs the cross-compile.
_cross-build:
	@printf '#!/usr/bin/env bash\nargs=()\nfor arg in "$$@"; do case "$$arg" in --unresolved-symbols=*) ;; -Wl,--unresolved-symbols=*) ;; *) args+=("$$arg") ;; esac; done\nexec $(ZIG) cc -target $(ZIG_TARGET) "$${args[@]}"\n' > /tmp/zigcc-$(ZIG_TARGET).sh
	@chmod +x /tmp/zigcc-$(ZIG_TARGET).sh
	CC=/tmp/zigcc-$(ZIG_TARGET).sh CGO_ENABLED=1 GOOS=linux GOARCH=$(GOARCH) \
	  go build -trimpath -buildmode=c-shared -o $(OUT) ./cmd
	@echo "built: $(OUT)"

# ── Docker ───────────────────────────────────────────────────────────────────

## docker: cross-compile for both arches on the host, then package into distroless image
## Produces a multi-arch manifest (linux/amd64 + linux/arm64), loads locally.
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

## clean: remove the .so, cross-compiled outputs, and built frontend assets
clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h) libspa.linux-amd64.so libspa.linux-arm64.so
	rm -rf $(DIST)
