# spa

Serve a Vite + React SPA with client-side routing (React Router) directly from
an Envoy dynamic module `.so` — no file system access, no separate web server.

## Routes

| URL | Handled by | Description |
|-----|-----------|-------------|
| `/` | `spa` filter | Home page |
| `/about` | `spa` filter → index.html | About page (client-side route) |
| `/dashboard` | `spa` filter → index.html | Dashboard — calls `/api/time` |
| `/assets/*` | `spa` filter | Fingerprinted JS/CSS with `immutable` cache headers |
| `/api/hello` | `api-backend` filter | JSON from inside the `.so` |
| `/api/time` | `api-backend` filter | Current UTC time from inside the `.so` |
| `/*` (unknown) | `spa` filter → index.html | SPA fallback — React Router renders a 404 component |

Refreshing on `/about` or `/dashboard` works because the filter returns
`index.html` for any path that doesn't match a static asset. React Router
then renders the correct component client-side.

## Two filters, one .so

| Filter | Description |
|--------|-------------|
| `spa` | Serves embedded `ui/dist` assets; falls back to `index.html` for SPA routing |
| `api-backend` | Handles `/api/*` directly from Go — no upstream cluster needed |

## Build (local, macOS / Linux)

```sh
# Full build: install npm deps, run vite build, compile .so
make

# Or step by step:
make ui        # npm install (if needed) + vite build
make build-so  # compile the .so only (ui/dist must already exist)
```

Requires Node.js >= 18 and Go with CGO enabled.

## Run (local)

```sh
make          # build ui + .so
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(pwd) envoy -c envoy.yaml
# open http://localhost:10000
```

The Vite build output in `ui/dist/` is embedded into the `.so` at Go compile
time via `//go:embed ui/dist`. Rebuilding the frontend requires recompiling
the `.so`.

## Docker

The Dockerfile is intentionally minimal — it just copies pre-built artifacts
into `envoyproxy/envoy:distroless-v1.37.1`. All compilation happens on the
host using `make build-linux`, which cross-compiles via **zig cc** for both
`linux/amd64` and `linux/arm64` without needing Docker build layers or
emulation.

```sh
# One command: cross-compile on host, then package
make docker                          # loads spa:latest locally
make docker-push IMAGE_TAG=ghcr.io/you/spa:latest

# Or step by step:
make build-linux                     # -> libspa.linux-amd64.so + libspa.linux-arm64.so
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --load -t spa:latest .
```

```sh
docker run --rm -p 10000:10000 spa:latest
curl http://localhost:10000/api/hello
```

The container exposes `:10000` (SPA + API) and `:9901` (Envoy admin).

### Why distroless?

`envoyproxy/envoy:distroless-v1.37.1` has no shell, no package manager, no OS
utilities — just the Envoy binary. The `.so` is copied to `/etc/envoy/libspa.so`
and loaded at runtime via `ENVOY_DYNAMIC_MODULES_SEARCH_PATH`.

## Cross-compiled Linux builds (without Docker)

If you need the `.so` files separately (e.g. to copy into an existing Envoy
deployment), you can cross-compile from macOS or Linux using zig:

```sh
# Requires zig 0.16.0 at /tmp/zig-aarch64-macos-0.16.0/zig
# Override with: make build-linux ZIG=/path/to/zig

make build-linux-amd64   # -> libspa.linux-amd64.so
make build-linux-arm64   # -> libspa.linux-arm64.so
make build-linux         # both
```

On the target machine, copy the `.so` alongside `envoy.yaml` and run:

```sh
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/path/to/dir \
GODEBUG=cgocheck=0 \
envoy -c envoy.yaml
```

## Development workflow

```sh
# Terminal 1 — build and start Envoy
make build-so
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(pwd) envoy -c envoy.yaml

# Terminal 2 — start Vite dev server (hot reload on port 5173)
make dev
# open http://localhost:5173
```

In dev mode, Vite proxies `/api/*` to Envoy (port 10000) so API calls work
without CORS issues. Edit any `.tsx` file and the browser reloads instantly.
When you're happy with changes, run `make` to rebuild the `.so` with the new
embedded assets.

## E2E tests

Tests use [Lightpanda](https://lightpanda.io) (headless browser) and
playwright-core over CDP. Envoy must be running first.

```sh
make          # build
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(pwd) envoy -c envoy.yaml &
make e2e      # 13 tests, ~1.5s
```

## Clean

```sh
make clean   # removes libspa.so, cross-compiled .so files, and ui/dist/
```

`ui/node_modules/`, `ui/dist/`, and `e2e/node_modules/` are gitignored.

## Cache strategy

| Path | Cache-Control | Why |
|------|---------------|-----|
| `index.html` | `no-cache` | Entry point must always reflect the latest deploy |
| `/assets/*` | `public, max-age=31536000, immutable` | Vite fingerprints filenames — safe to cache forever |

## What this demonstrates

- `//go:embed ui/dist` — bundle a complete Vite build into the `.so` at compile time
- SPA fallback routing — return `index.html` for any unmatched path so React Router works on refresh
- `w.SendBytes` — serve binary assets (JS bundles, CSS, fonts) directly from a handler
- Cache header strategy for fingerprinted assets vs the HTML entry point
- Two filters in one `.so` sharing the same embedded filesystem
- `jisr.Chain` with logging middleware on the API filter
- Vite dev proxy (`/api` → Envoy) for a smooth development loop
- Multi-arch Docker packaging via zig cc cross-compilation + distroless Envoy base
