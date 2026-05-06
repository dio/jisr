# spa

Serve a Vite + React SPA with client-side routing (React Router) directly from
an Envoy dynamic module `.so` — no file system access, no separate web server.

## Routes

| URL | Handled by | Description |
|-----|-----------|-------------|
| `/` | `spa` filter | Home page — explains the example |
| `/about` | `spa` filter → index.html | About page (client-side route) |
| `/dashboard` | `spa` filter → index.html | Dashboard — calls `/api/time` |
| `/assets/*` | `spa` filter | Fingerprinted JS/CSS — served with `immutable` cache headers |
| `/api/hello` | `api-backend` filter | Returns JSON from inside the `.so` |
| `/api/time` | `api-backend` filter | Returns current UTC time from inside the `.so` |
| `/*` (unknown) | `spa` filter → index.html | SPA fallback — React Router renders a 404 component |

Refreshing on `/about` or `/dashboard` works because the filter returns
`index.html` for any path that doesn't match a static asset. React Router
then renders the correct component client-side.

## Two filters, one .so

| Filter | Description |
|--------|-------------|
| `spa` | Serves embedded `ui/dist` assets; falls back to `index.html` for SPA routing |
| `api-backend` | Handles `/api/*` directly from Go — no upstream cluster needed |

## Build

```sh
# Full build: install npm deps, run vite build, compile .so
make

# Or step by step:
make ui        # npm install (if needed) + vite build
make build-so  # compile the .so only (ui/dist must already exist)
```

## Run

```sh
make          # build ui + .so
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(pwd) envoy -c envoy.yaml
# open http://localhost:10000
```

Requires Node.js ≥ 18 and Go with CGO enabled.

The Vite build output in `ui/dist/` is embedded into the `.so` at Go compile
time via `//go:embed ui/dist`. Rebuilding the frontend requires recompiling
the `.so`.

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

## Clean

```sh
make clean   # removes libspa.so and ui/dist/
```

`ui/node_modules/` and `ui/dist/` are gitignored — run `make` from scratch
on a fresh clone.

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
