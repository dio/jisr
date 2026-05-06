// Package prof provides an admin HTTP server for Envoy dynamic module filters (.so).
//
// It bundles the standard ops endpoints into a single background server:
//
//   - /healthz, /readyz  — liveness/readiness probes (always 200 {"status":"ok"})
//   - /debug/pprof/*     — Go pprof: goroutines, heap, CPU, trace
//   - /version           — build info (module path + version)
//
// All endpoints are served on a dedicated port — never the Envoy admin port.
//
// # Usage
//
//	type myConfigFactory struct {
//	    shared.EmptyHttpFilterConfigFactory
//	    stopAdmin func()
//	}
//
//	func (cf *myConfigFactory) Create(handle shared.HttpFilterConfigHandle, _ []byte) (shared.HttpFilterFactory, error) {
//	    srv, stop, err := prof.Start("127.0.0.1:6060")
//	    if err != nil {
//	        handle.Log(shared.LogLevelWarn, "admin: bind failed: %v", err)
//	    } else {
//	        handle.Log(shared.LogLevelInfo, "admin: http://%s/debug/pprof/", srv.Addr())
//	        cf.stopAdmin = stop
//	    }
//	    return &myFilterFactory{}, nil
//	}
//
//	func (cf *myConfigFactory) OnDestroy() {
//	    if cf.stopAdmin != nil { cf.stopAdmin() }
//	}
//
// # Endpoints
//
//	GET /healthz                  → {"status":"ok"}
//	GET /readyz                   → {"status":"ok"}
//	GET /debug/pprof/             → pprof index (HTML)
//	GET /debug/pprof/goroutine    → goroutine stacks
//	GET /debug/pprof/heap         → heap profile
//	GET /debug/pprof/allocs       → alloc profile
//	GET /debug/pprof/profile      → CPU profile (?seconds=N)
//	GET /debug/pprof/trace        → execution trace
//	GET /debug/pprof/cmdline      → process cmdline
//	GET /debug/pprof/symbol       → symbol lookup
//	GET /version                  → {"module":"...","version":"..."}
//
// # Quick reference
//
//	# Goroutine dump — find leaks
//	curl http://localhost:6060/debug/pprof/goroutine?debug=2
//
//	# 30 s CPU profile
//	go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
//
//	# Heap snapshot
//	go tool pprof http://localhost:6060/debug/pprof/heap
//
//	# Goroutine count (quick)
//	curl -s http://localhost:6060/debug/pprof/goroutine?debug=1 | head -5
package prof

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime/debug"

	"github.com/dio/jisr/server"
)

// Server is a running admin+pprof HTTP server.
type Server struct {
	inner *server.Server
}

// Addr returns the address the server is listening on, e.g. "127.0.0.1:6060".
func (s *Server) Addr() string { return s.inner.Addr() }

// Port returns the TCP port the server is listening on.
func (s *Server) Port() int { return s.inner.Port() }

// Start starts an admin HTTP server on the given address.
//
// addr is the TCP address to bind. Use "" for a random free loopback port.
// Returns the server handle, a stop function (call from OnDestroy), and any
// bind error.
func Start(addr string) (*Server, func(), error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}

	g := server.NewGroup()
	inner := g.AddListener(ln, Handler(), 0)
	g.Start()

	return &Server{inner: inner}, g.Stop, nil
}

// Handler returns the admin http.Handler.
// Exported so tests can exercise endpoints directly via httptest without
// a live TCP server.
func Handler() http.Handler {
	mux := http.NewServeMux()

	// ── Health / readiness ───────────────────────────────────────────────────
	health := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("GET /readyz", health)

	// ── Build info ───────────────────────────────────────────────────────────
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		info, ok := debug.ReadBuildInfo()
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]string{"module": "unknown", "version": "unknown"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"module":  info.Main.Path,
			"version": info.Main.Version,
		})
	})

	// ── pprof — explicit handlers, no DefaultServeMux coupling ───────────────
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	// Named profiles (goroutine, heap, allocs, block, mutex, threadcreate).
	// pprof.Handler looks up by name in the pprof registry.
	for _, name := range []string{"goroutine", "heap", "allocs", "block", "mutex", "threadcreate"} {
		mux.Handle("GET /debug/pprof/"+name, pprof.Handler(name))
	}

	return mux
}
