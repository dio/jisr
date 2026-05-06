// Package multiactor demonstrates that jisr/server.Group is not a server
// library — it is a lifecycle manager.
//
// This example runs three actors inside one .so, all tied to the same
// Envoy filter config lifecycle (OnDestroy = stop everything):
//
//  1. HTTP actor      — a net/http server (for Envoy to route traffic to)
//  2. Background task — a periodic goroutine (e.g. cache refresh, heartbeat)
//  3. Raw listener    — a net.Listener for a custom binary protocol (health probe)
//
// None of these are WebSocket-specific. Group works for any combination of
// background services that must start on Create() and stop on OnDestroy.
//
// # Architecture
//
//	Envoy filter chain
//	  └─ "multi-actor" filter (passes through all HTTP traffic)
//
//	Background actors (managed by server.Group):
//	  1. HTTP server on random port  — Envoy STATIC cluster routes here
//	  2. Goroutine: cache refresh    — runs every 10s, stops on ctx.Done()
//	  3. Raw listener on random port — custom probe protocol (write "ping\n", get "pong\n")
package multiactor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/dio/jisr"
	"github.com/dio/jisr/server"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

const ExtensionName = "multi-actor"

func init() {
	jisr.RegisterRaw(ExtensionName, &configFactory{})
}

// --- configFactory ---

type configFactory struct {
	shared.EmptyHttpFilterConfigFactory
}

func (f *configFactory) Create(
	handle shared.HttpFilterConfigHandle,
	_ []byte,
) (shared.HttpFilterFactory, error) {

	g := server.NewGroup()

	// Actor 1: HTTP server on a random port.
	// In production, Envoy's STATIC cluster would point here.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("multi-actor: http listen: %w", err)
	}
	httpSrv := g.AddListener(ln, newStatusHandler(), 5*time.Second)
	handle.Log(shared.LogLevelInfo,
		"multi-actor: HTTP actor listening on %s", httpSrv.Addr())

	// Actor 2: background goroutine — periodic cache refresh.
	// Gets a context that is cancelled when g.Stop() is called.
	var refreshCount atomic.Int64
	g.AddGoroutine(func(ctx context.Context) {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				handle.Log(shared.LogLevelInfo, "multi-actor: cache refresh stopped")
				return
			case <-ticker.C:
				n := refreshCount.Add(1)
				handle.Log(shared.LogLevelDebug,
					"multi-actor: cache refresh #%d", n)
			}
		}
	})

	// Actor 3: raw net.Listener — custom probe protocol.
	// Responds to "ping\n" with "pong\n". No HTTP involved.
	probeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("multi-actor: probe listener: %w", err)
	}
	g.Add(
		func() error { return serveProbe(probeLn) },
		func() { probeLn.Close() },
	)
	handle.Log(shared.LogLevelInfo,
		"multi-actor: probe actor listening on %s", probeLn.Addr())

	g.Start()

	return &filterFactory{
		group:        g,
		httpAddr:     httpSrv.Addr(),
		probeAddr:    probeLn.Addr().String(),
		refreshCount: &refreshCount,
	}, nil
}

func (f *configFactory) CreatePerRoute(_ []byte) (any, error) { return nil, nil }

// --- filterFactory ---

type filterFactory struct {
	shared.EmptyHttpFilterFactory
	group        *server.Group
	httpAddr     string
	probeAddr    string
	refreshCount *atomic.Int64
}

func (f *filterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &passthroughFilter{}
}

func (f *filterFactory) OnDestroy() {
	// One call stops all three actors atomically.
	f.group.Stop()
}

type passthroughFilter struct{ shared.EmptyHttpFilter }

// --- HTTP handler (Actor 1) ---

// newStatusHandler returns a handler that reports internal state.
// In a real filter this might serve metrics, admin endpoints, or debug info.
func newStatusHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","actors":3}`)
	})
	return mux
}

// --- probe server (Actor 3) ---

// serveProbe handles a trivial ping/pong probe protocol over raw TCP.
// Write "ping\n", receive "pong\n". Connection closed by client.
// This demonstrates that Group actors are not limited to HTTP.
func serveProbe(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// ln.Close() was called — clean shutdown.
			return nil
		}
		go handleProbeConn(conn)
	}
}

func handleProbeConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	buf := make([]byte, 5) // "ping\n"
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}
	if string(buf) == "ping\n" {
		io.WriteString(conn, "pong\n")
	}
}
