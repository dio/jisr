// Package server provides primitives for running background servers inside an
// Envoy dynamic module (.so).
//
// The core problem: some protocols (WebSocket, raw TCP) cannot be intercepted
// via Envoy's HTTP filter chain. The solution is an embedded server — a
// net/http server running as a background goroutine inside the .so. Envoy
// routes the problematic traffic to a local STATIC cluster pointing at this
// server, bypassing the filter chain entirely.
//
// Lifecycle is tied to the Envoy filter config: when Envoy hot-reloads or
// shuts down, it calls OnDestroy on the filter factory. The factory calls
// the stop function returned by [New], which shuts down the server gracefully.
//
// # Minimal usage
//
//	// In your raw config factory Create():
//	srv, stop, err := server.New(myWSHandler)
//	if err != nil {
//	    return nil, err
//	}
//	// Configure Envoy STATIC cluster with srv.Port().
//	// Store stop — call it from OnDestroy.
//
//	func (f *myFactory) OnDestroy() { f.stop() }
//
// # How it differs from net/http.Server
//
// [New] binds a random free port immediately (so Port() is known before
// Envoy starts routing traffic), starts serving in a background goroutine,
// and returns a stop function that triggers graceful shutdown. The caller
// never manages goroutines directly.
package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Server is an embedded HTTP/WebSocket server running on a random free port.
type Server struct {
	listener net.Listener
	srv      *http.Server
}

// New starts an HTTP server on a random free port and returns it along with
// a stop function. Call stop() from your filter factory's OnDestroy to
// initiate graceful shutdown.
//
// The server is immediately ready to accept connections after New returns —
// Port() and Addr() reflect the bound address.
//
// handler receives all requests routed by Envoy to this server's port.
// For WebSocket upgrades, implement http.Handler.ServeHTTP and call
// websocket.Accept inside it.
//
// shutdownTimeout controls how long graceful shutdown waits for active
// connections to finish before forcibly closing. Use 0 for the default (5s).
func New(handler http.Handler, shutdownTimeout time.Duration) (*Server, func(), error) {
	if shutdownTimeout == 0 {
		shutdownTimeout = 5 * time.Second
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("server.New: listen: %w", err)
	}

	s := &Server{
		listener: ln,
		srv: &http.Server{
			Handler: handler,
			// No read/write timeout — WebSocket connections are long-lived.
			// Handlers are responsible for their own session timeouts.
			ReadTimeout:  0,
			WriteTimeout: 0,
		},
	}

	go s.srv.Serve(ln) //nolint:errcheck // ErrServerClosed is expected on shutdown

	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	}

	return s, stop, nil
}

// Port returns the TCP port the server is listening on.
func (s *Server) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

// Addr returns the full listen address, e.g. "127.0.0.1:54321".
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}
