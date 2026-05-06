# spa

Serve a fully embedded Vite Single Page Application from an Envoy dynamic
module `.so` — no file system access, no separate web server, no sidecar.

## What it does

Two filters in one `.so`:

| Filter | Description |
|--------|-------------|
| `spa` | Serves embedded static assets with SPA fallback: `/` and any unknown route returns `index.html`; `/assets/*` returns fingerprinted files with long-lived cache headers |
| `api-backend` | Handles `/api/*` requests and responds directly from the `.so`: `/api/hello` and `/api/time` |

Assets are embedded at compile time via Go's `//go:embed` directive pointing
at `ui/dist` (Vite build output). The `.so` is self-contained — deploy it and
Envoy; nothing else needed.

## Cache strategy

| Path | Cache-Control |
|------|---------------|
| `index.html` | `no-cache` — always revalidated (entry point, must reflect latest deploy) |
| `/assets/*` | `public, max-age=31536000, immutable` — Vite fingerprints filenames; safe to cache forever |

## Build

```sh
# 1. build the frontend
cd ui && npm install && npm run build && cd ..

# 2. build the .so (embeds ui/dist at compile time)
make
# or:
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libspa.so ./cmd
```

## What this demonstrates

- `w.SendBytes` for serving binary assets (images, fonts, JS bundles) directly from a handler
- `//go:embed` for bundling a complete frontend build into the `.so` at compile time
- SPA fallback routing: serve `index.html` for any path not matched by a static asset
- Cache header strategy for Vite-fingerprinted assets
- `jisr.Chain` with logging middleware applied to the API filter
- Two independent filters sharing one `.so` binary
