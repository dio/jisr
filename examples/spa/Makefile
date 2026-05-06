OUTPUT  := libspa.so
GOCMD   := CGO_ENABLED=1 go
UI_DIR  := ui
DIST    := ui/dist

.PHONY: all build build-so ui clean tidy dev

all: build

# Full build: Vite → embed → .so
build: ui build-so

# Build the .so only (ui/dist must already exist).
build-so:
	$(GOCMD) build -trimpath -buildmode=c-shared -o $(OUTPUT) ./cmd

# Build the Vite SPA. Requires Node.js + npm.
# In CI: `npm ci && npm run build` in the ui/ directory.
ui:
	@if [ -d "$(UI_DIR)/node_modules" ] || [ -f "$(UI_DIR)/package.json" ]; then \
		cd $(UI_DIR) && npm run build; \
	else \
		echo "No Vite project found in $(UI_DIR)/. Using pre-built dist/ assets."; \
	fi

# Run the Vite dev server (hot reload). Use alongside a running Envoy for API proxying.
dev:
	cd $(UI_DIR) && npm run dev

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

tidy:
