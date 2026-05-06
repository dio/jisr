// Package server provides primitives for running background servers inside an
// Envoy dynamic module (.so).
//
// The core problem: some protocols (WebSocket, raw TCP) cannot be intercepted
// via Envoy's HTTP filter chain. The solution is an embedded server — a
// net/http (or net.Listener) server running as a background goroutine inside
// the .so. Envoy routes the problematic traffic to a local cluster pointing at
// this server, bypassing the filter chain entirely.
//
// Lifecycle is tied to the Envoy filter config via context.Context: when Envoy
// hot-reloads or shuts down, it calls OnDestroy on the filter factory. The
// factory cancels the context, which stops the embedded server.
//
// # Usage pattern
//
//	// In your config factory Create():
//	srv, err := server.NewEmbedded(ctx, myHandler)
//	if err != nil {
//	    return nil, err
//	}
//	// Tell Envoy which port: log it or embed in your manifest.
//	log.Printf("embedded server on port %d", srv.Port())
//
// The context passed to NewEmbedded must be cancelled when Envoy calls
// OnDestroy. Use [Background] to create a child context tied to OnDestroy.
package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Embedded is a net/http server running on a random free port inside the .so.
// It is started immediately on construction and stopped when ctx is cancelled.
//
// Envoy must be configured with a STATIC cluster pointing at Addr() so it can
// route traffic here. The cluster definition should use type: STATIC with the
// port returned by Port().
type Embedded struct {
	listener net.Listener
	server   *http.Server
}

// NewEmbedded starts an HTTP server on a random free port.
// The server runs in a background goroutine and stops when ctx is cancelled.
// Returns an error if the port cannot be bound.
//
// handler receives all HTTP requests routed by Envoy to this server's port.
// For WebSocket upgrades, handle them in ServeHTTP using a WS library
// (e.g. github.com/coder/websocket).
func NewEmbedded(ctx context.Context, handler http.Handler) (*Embedded, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("server.NewEmbedded: listen: %w", err)
	}

	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  0, // no timeout — WebSocket connections are long-lived
		WriteTimeout: 0,
	}

	e := &Embedded{listener: ln, server: srv}

	go srv.Serve(ln)
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	return e, nil
}

// Port returns the port the embedded server is listening on.
func (e *Embedded) Port() int {
	return e.listener.Addr().(*net.TCPAddr).Port
}

// Addr returns the full listen address, e.g. "127.0.0.1:54321".
func (e *Embedded) Addr() string {
	return e.listener.Addr().String()
}

// Background runs fn in a goroutine and returns a cancel function.
// The returned context is cancelled when cancel is called — wire cancel
// to your filter factory's OnDestroy.
//
// This is a thin helper that enforces the lifecycle contract:
// one background goroutine per filter config, stopped on OnDestroy.
//
//	ctx, stop := server.Background(fn)
//	// store ctx for NewEmbedded or other background work
//	// store stop for OnDestroy
//
//	func (f *myFactory) OnDestroy() { stop() }
func Background(fn func(ctx context.Context)) (ctx context.Context, stop context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go fn(ctx)
	return ctx, cancel
}
