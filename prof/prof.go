// Package prof starts a background pprof HTTP server inside an Envoy dynamic
// module (.so). Call [Start] from your filter factory's Create() method and
// [Stop] from OnDestroy.
//
// The pprof server exposes the standard Go profiling endpoints:
//
//	/debug/pprof/           — index page, links to all profiles
//	/debug/pprof/goroutine  — goroutine stack traces (critical for leak detection)
//	/debug/pprof/heap       — heap allocation profile
//	/debug/pprof/allocs     — allocation sampling
//	/debug/pprof/cpu        — CPU profiling (enable via ?seconds=N)
//	/debug/pprof/trace      — execution trace
//
// # Usage
//
//	type myFactory struct {
//	    shared.EmptyHttpFilterConfigFactory
//	    stopProf func()
//	}
//
//	func (cf *myFactory) Create(handle shared.HttpFilterConfigHandle, _ []byte) (shared.HttpFilterFactory, error) {
//	    srv, stop, err := prof.Start("127.0.0.1:6060")
//	    if err != nil {
//	        handle.Log(shared.LogLevelWarn, "prof: %v", err)
//	    } else {
//	        handle.Log(shared.LogLevelInfo, "prof: listening on %s", srv.Addr())
//	        cf.stopProf = stop
//	    }
//	    return &myFilterFactory{}, nil
//	}
//
//	func (cf *myFactory) OnDestroy() {
//	    if cf.stopProf != nil {
//	        cf.stopProf()
//	    }
//	}
//
// # Collecting profiles
//
//	# Goroutine dump — check for leaks
//	curl http://localhost:6060/debug/pprof/goroutine?debug=2
//
//	# 30s CPU profile
//	go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
//
//	# Heap snapshot
//	go tool pprof http://localhost:6060/debug/pprof/heap
//
//	# Live goroutine count (quick check)
//	curl -s http://localhost:6060/debug/pprof/goroutine?debug=1 | head -5
//
// # Address selection
//
// Pass "" to [Start] to bind on a random free loopback port — use [Server.Addr]
// to discover the actual address. Pass a fixed address like "127.0.0.1:6060"
// when you want a stable URL to curl without service discovery.
//
// Only one pprof server should run per .so. If you have multiple filter config
// factories sharing the same .so, start the profiler in only one of them.
package prof

import (
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/ handlers on http.DefaultServeMux

	"github.com/dio/jisr/server"
)

// Server is a running pprof HTTP server.
type Server struct {
	inner *server.Server
}

// Addr returns the address the pprof server is listening on, e.g. "127.0.0.1:6060".
func (s *Server) Addr() string { return s.inner.Addr() }

// Port returns the TCP port the pprof server is listening on.
func (s *Server) Port() int { return s.inner.Port() }

// Start starts a pprof HTTP server on the given address and returns a stop function.
//
// addr is the TCP address to listen on. Use "" for a random free loopback port.
// Call stop() from your filter factory's OnDestroy to shut down cleanly.
//
// Returns an error only if the TCP bind fails (e.g. port already in use).
func Start(addr string) (*Server, func(), error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}

	g := server.NewGroup()
	// Use a dedicated ServeMux so we don't pollute http.DefaultServeMux if the
	// caller also runs other HTTP servers. pprof registers on DefaultServeMux
	// via its init(), so we proxy to it here.
	mux := http.NewServeMux()
	mux.Handle("/debug/pprof/", http.DefaultServeMux)

	inner := g.AddListener(ln, mux, 0)
	g.Start()

	return &Server{inner: inner}, g.Stop, nil
}
