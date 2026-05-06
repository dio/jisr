// Package server provides primitives for running background services inside an
// Envoy dynamic module (.so).
//
// The core abstraction is [Group] — a set of background actors that start
// together and stop together when Envoy calls OnDestroy on the filter factory.
// This generalises the embedded server pattern beyond WebSocket: anything that
// needs a background goroutine (HTTP server, gRPC server, periodic task, cache
// warmer) can be registered as an actor.
//
// [New] is a convenience wrapper for the common single-HTTP-server case.
//
// # Lifecycle contract
//
// Actors are registered before [Group.Start]. Start launches all actors in
// background goroutines and returns immediately. When any actor finishes
// (for any reason), all others are stopped. Call [Group.Stop] from your filter
// factory's OnDestroy to trigger graceful shutdown.
//
// # Minimal usage — single HTTP server
//
//	// In your raw config factory Create():
//	srv, stop, err := server.New(myHandler, 0)
//	if err != nil {
//	    return nil, err
//	}
//	handle.Log(LogInfo, "listening on %s", srv.Addr())
//	// store stop, call from OnDestroy
//
//	func (f *myFactory) OnDestroy() { f.stop() }
//
// # Multiple actors
//
//	g := server.NewGroup()
//
//	ws, err := g.AddHTTP(wsHandler, 5*time.Second)  // WS proxy server
//	if err != nil { return nil, err }
//
//	g.AddGoroutine(func(ctx context.Context) {       // background metrics
//	    ticker := time.NewTicker(30 * time.Second)
//	    defer ticker.Stop()
//	    for {
//	        select {
//	        case <-ctx.Done(): return
//	        case <-ticker.C: collectMetrics()
//	        }
//	    }
//	})
//
//	g.Start()
//	// store g, call g.Stop() from OnDestroy
//
//	func (f *myFactory) OnDestroy() { f.group.Stop() }
package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// ── Group ──────────────────────────────────────────────────────────────────────

// Group manages a set of background actors sharing a common lifecycle.
// All actors start together and stop together when [Group.Stop] is called.
//
// Inspired by oklog/run — adapted for .so use: Start is non-blocking, there
// is no signal handling, and panics are recovered rather than crashing the process.
type Group struct {
	actors []actor
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

type actor struct {
	execute func() error
	stop    func()
}

// NewGroup creates a new Group. Register actors with Add, AddHTTP, or
// AddGoroutine, then call Start.
func NewGroup() *Group {
	ctx, cancel := context.WithCancel(context.Background())
	return &Group{ctx: ctx, cancel: cancel}
}

// Add registers a raw actor.
//
// execute blocks until the actor finishes. It runs in a background goroutine.
// stop is called (from another goroutine) to interrupt the actor — it must
// cause execute to return promptly.
//
// Panics inside execute are recovered and treated as errors.
func (g *Group) Add(execute func() error, stop func()) {
	g.actors = append(g.actors, actor{
		execute: wrapPanic(execute),
		stop:    stop,
	})
}

// AddGoroutine adds a background function that receives a context.
// The context is cancelled automatically when Stop is called.
// Use this for periodic tasks, cache warmers, or any background loop:
//
//	g.AddGoroutine(func(ctx context.Context) {
//	    ticker := time.NewTicker(30 * time.Second)
//	    defer ticker.Stop()
//	    for {
//	        select {
//	        case <-ctx.Done(): return
//	        case <-ticker.C: doWork()
//	        }
//	    }
//	})
func (g *Group) AddGoroutine(fn func(ctx context.Context)) {
	ctx := g.ctx
	g.Add(
		func() error { fn(ctx); return nil },
		func() {}, // context cancellation handles the stop signal
	)
}

// AddHTTP starts a net/http server on a random free port and registers it as
// an actor. The server is bound immediately — Addr() is valid before Start.
//
// handler covers all HTTP-family protocols — pass the appropriate handler:
//
//	WebSocket:        your http.Handler that calls websocket.Accept
//	HTTP/2 cleartext: h2c.NewHandler(mux, &http2.Server{})
//	gRPC (TLS):       grpc.Server via ServeHTTP — requires HTTP/2 over TLS;
//	                  for cleartext gRPC use grpc.Serve(ln) with g.Add directly
//	gRPC + REST:      vanguard.Transcoder or h2c mux dispatching by Content-Type
//
// For a fixed port (e.g. declared in Envoy config), use [Group.AddListener].
//
// timeout controls how long graceful shutdown waits for active connections.
// Use 0 for the default (5 seconds).
func (g *Group) AddHTTP(handler http.Handler, timeout time.Duration) (*Server, error) {
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("server.Group.AddHTTP: listen: %w", err)
	}

	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  0, // no timeout — WebSocket connections are long-lived
		WriteTimeout: 0,
	}

	s := &Server{listener: ln, srv: srv}

	g.Add(
		func() error {
			if err := srv.Serve(ln); err != http.ErrServerClosed {
				return err
			}
			return nil
		},
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_ = srv.Shutdown(ctx)
		},
	)

	return s, nil
}

// AddListener registers an existing net.Listener as an HTTP actor.
// Use when the port must match a pre-declared Envoy STATIC cluster config value
// rather than binding a random port.
//
//	ln, err := net.Listen("tcp", "127.0.0.1:10001") // port declared in envoy.yaml
//	if err != nil { return nil, err }
//	srv := g.AddListener(ln, myHandler, 5*time.Second)
//
// timeout controls how long graceful shutdown waits for active connections.
// Use 0 for the default (5 seconds).
func (g *Group) AddListener(ln net.Listener, handler http.Handler, timeout time.Duration) *Server {
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  0,
		WriteTimeout: 0,
	}

	s := &Server{listener: ln, srv: srv}

	g.Add(
		func() error {
			if err := srv.Serve(ln); err != http.ErrServerClosed {
				return err
			}
			return nil
		},
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_ = srv.Shutdown(ctx)
		},
	)

	return s
}
//
// When any actor finishes (for any reason — normal return, error, or panic),
// Stop is called automatically to interrupt all remaining actors.
//
// Call Start exactly once after all actors are registered.
func (g *Group) Start() {
	if len(g.actors) == 0 {
		return
	}

	errc := make(chan struct{}, len(g.actors))

	for _, a := range g.actors {
		a := a
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			a.execute() //nolint:errcheck // error is used only to trigger stop
			errc <- struct{}{}
		}()
	}

	// Watch for the first actor to finish, then stop all.
	go func() {
		<-errc
		g.Stop()
	}()
}

// Stop interrupts all actors and waits for them to finish gracefully.
// Safe to call multiple times — only the first call has effect.
// Call this from your filter factory's OnDestroy.
func (g *Group) Stop() {
	g.once.Do(func() {
		g.cancel() // cancels context for AddGoroutine actors
		for _, a := range g.actors {
			a.stop()
		}
		g.wg.Wait()
	})
}

// ── Server ─────────────────────────────────────────────────────────────────────

// Server is a bound HTTP listener registered inside a [Group].
type Server struct {
	listener net.Listener
	srv      *http.Server
}

// Port returns the TCP port the server is listening on.
func (s *Server) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

// Addr returns the full listen address, e.g. "127.0.0.1:54321".
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// ── New — single-server convenience ───────────────────────────────────────────

// New starts a single HTTP server on a random free port.
// It is a convenience wrapper over [Group] for the common case of one server.
//
// Returns the server, a stop function, and any bind error.
// Call stop() from your filter factory's OnDestroy.
//
// shutdownTimeout controls graceful shutdown duration. Use 0 for the default (5s).
func New(handler http.Handler, shutdownTimeout time.Duration) (*Server, func(), error) {
	g := NewGroup()
	s, err := g.AddHTTP(handler, shutdownTimeout)
	if err != nil {
		return nil, nil, err
	}
	g.Start()
	return s, g.Stop, nil
}

// ── wrapPanic ─────────────────────────────────────────────────────────────────

// wrapPanic catches panics inside actor execute functions and returns them as
// errors, preventing a misbehaving actor from crashing the entire .so.
func wrapPanic(fn func() error) func() error {
	return func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 4096)
				n := runtime.Stack(buf, false)
				err = fmt.Errorf("panic: %v\n%s", r, buf[:n])
			}
		}()
		return fn()
	}
}
