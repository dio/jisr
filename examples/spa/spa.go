// Package spa demonstrates serving a Vite SPA directly from an Envoy dynamic
// module — no file system access, no separate web server.
//
// Two filters ship in the same .so:
//
//	spa         — serves embedded static assets via w.SendBytes; falls back to
//	              index.html for unmatched paths (client-side routing support)
//	api-backend — handles /api/* requests and responds directly, acting as the
//	              backend the SPA calls
//
// # How assets are embedded
//
// The ui/dist directory is embedded at compile time using //go:embed. The Vite
// build output (index.html + assets/) lives there. In development you run the
// Vite dev server separately and point a reverse proxy at it; for production
// you run `vite build` and rebuild the .so.
//
// # Request routing in Envoy
//
//	GET /              → spa filter → index.html (200, text/html)
//	GET /assets/*.js   → spa filter → asset (200, application/javascript)
//	GET /api/*         → api-backend filter → JSON (200, application/json)
//	GET /unknown-page  → spa filter → index.html (SPA client-side routing)
//
// All responses are generated inside the filter — no upstream cluster needed.
// The Envoy router filter is still required at the end of the chain, but the
// route can point at a blackhole cluster since it is never reached.
package spa

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/dio/jisr"
)

// UIFS holds the compiled Vite output, exported for testing.
// Tests use it to discover fingerprinted asset filenames at runtime.
//
//go:embed ui/dist
var UIFS embed.FS

// indexHTML is the SPA shell — served for every path that isn't a known asset.
//
//go:embed ui/dist/index.html
var indexHTML []byte

func init() {
	jisr.Register("spa", SPAHandler)
	jisr.Register("api-backend", jisr.Chain(APIHandler, apiLogMiddleware))
}

// SPAHandler serves embedded static assets.
// Exported for unit testing.
func SPAHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	r.SkipBody()

	path := r.Header.Get(":path")

	// Strip query string.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}

	// Try to serve the exact asset from the embedded FS.
	fsPath := "ui/dist" + path
	data, err := fs.ReadFile(UIFS, fsPath)
	if err == nil {
		// Asset found — detect MIME type from extension.
		ct := mime.TypeByExtension(filepath.Ext(path))
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.SetResponseHeader("content-type", ct)
		w.SetResponseHeader("cache-control", cacheControl(path))
		w.SendBytes(http.StatusOK, data)
		return
	}

	// Not a known asset — serve index.html for SPA client-side routing.
	// The SPA's router will handle the path in the browser.
	w.SetResponseHeader("content-type", "text/html; charset=utf-8")
	w.SetResponseHeader("cache-control", "no-cache")
	w.SendBytes(http.StatusOK, indexHTML)
}

// cacheControl returns an appropriate Cache-Control value based on path.
// Hashed assets (e.g. index-Dh3v8Qx1.js) are immutable; others are not cached.
func cacheControl(path string) string {
	// Vite fingerprints assets with a hash in the filename.
	// Files in /assets/ with an extension are safe to cache indefinitely.
	if strings.HasPrefix(path, "/assets/") {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

// ── api-backend handler ───────────────────────────────────────────────────────

// APIHandler handles /api/* requests and responds directly from the .so.
// No upstream cluster is contacted — this IS the backend.
// Exported for unit testing.
func APIHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	r.SkipBody()

	path := r.Header.Get(":path")

	switch {
	case path == "/api/hello" || strings.HasPrefix(path, "/api/hello?"):
		serveHello(ctx, w, r)

	case path == "/api/time" || strings.HasPrefix(path, "/api/time?"):
		serveTime(ctx, w, r)

	default:
		jsonResponse(w, http.StatusNotFound, map[string]string{
			"error": "not found",
			"path":  path,
		})
	}
}

func serveHello(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"message": "hello from inside the .so",
		"filter":  r.FilterName,
		"path":    r.Header.Get(":path"),
	})
}

func serveTime(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"time":   time.Now().UTC().Format(time.RFC3339),
		"filter": r.FilterName,
	})
}

func jsonResponse(w jisr.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.SetResponseHeader("content-type", "application/json")
	w.SendBytes(status, b)
}

func apiLogMiddleware(next jisr.HandlerFunc) jisr.HandlerFunc {
	return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.Log(jisr.LogInfo, "[api-backend] %s %s", r.Header.Get(":method"), r.Header.Get(":path"))
		next(ctx, w, r)
	}
}
