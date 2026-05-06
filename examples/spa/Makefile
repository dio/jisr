OUTPUT  := libspa.so
GOCMD   := CGO_ENABLED=1 go
UI_DIR  := ui
DIST    := $(UI_DIR)/dist
NM      := $(UI_DIR)/node_modules

.PHONY: all build build-so ui install clean dev

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

## clean: remove the .so and built frontend assets
clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)
	rm -rf $(DIST)
