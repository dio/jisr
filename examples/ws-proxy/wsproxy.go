// Package wsproxy demonstrates the embedded WebSocket proxy pattern using
// jisr/server.
//
// # The Problem
//
// Envoy's HTTP filter chain cannot inspect WebSocket frames. After a 101
// Switching Protocols response, the connection becomes raw TCP — OnRequestBody
// and OnResponseBody are never called for WS frames.
//
// # The Solution
//
// Run a net/http server inside the .so. Envoy routes WebSocket upgrade requests
// to a local STATIC cluster pointing at this server. The server accepts the WS
// upgrade from the client, dials the real upstream, and pumps frames
// bidirectionally — tapping every frame for inspection.
//
// # Architecture
//
//	Client ──WS──► Envoy ──► ws-proxy cluster (127.0.0.1:<port>)
//	                               │
//	                        WSProxy.ServeHTTP
//	                               │
//	                        upstream dial (wss://...)
//	                               │
//	              ◄──frames──► upstream provider
//
// The filter factory (rawFactory) starts the WSProxy once per filter config
// and stores the stop function — called from OnDestroy when Envoy reloads.
//
// A companion jisr.Register filter can share the same .so for normal HTTP
// traffic (auth, header rewriting, etc.).
package wsproxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/dio/jisr"
	"github.com/dio/jisr/server"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

const ExtensionName = "ws-proxy"

func init() {
	jisr.RegisterRaw(ExtensionName, &rawConfigFactory{})
}

// Config is parsed from the filter_config JSON.
type Config struct {
	// UpstreamURL is the base wss:// URL of the upstream provider.
	// Example: "wss://api.openai.com"
	UpstreamURL string `json:"upstream_url"`

	// AuthHeader is the HTTP header name to inject into the upstream dial.
	// Example: "authorization"
	AuthHeader string `json:"auth_header"`

	// AuthValue is the value of the auth header. Supports ${ENV_VAR} expansion.
	AuthValue string `json:"auth_value"`

	// ShutdownTimeout is how long to wait for active WS sessions to finish
	// on Envoy reload/shutdown. Defaults to 5s.
	ShutdownTimeout string `json:"shutdown_timeout"`
}

// --- rawConfigFactory ---

type rawConfigFactory struct {
	shared.EmptyHttpFilterConfigFactory
}

func (f *rawConfigFactory) Create(
	handle shared.HttpFilterConfigHandle,
	raw []byte,
) (shared.HttpFilterFactory, error) {
	cfg := Config{
		UpstreamURL: "wss://api.openai.com",
		AuthHeader:  "authorization",
	}
	// In production, parse raw JSON into cfg here.
	_ = raw

	timeout := 5 * time.Second
	if cfg.ShutdownTimeout != "" {
		if d, err := time.ParseDuration(cfg.ShutdownTimeout); err == nil {
			timeout = d
		}
	}

	proxy := &WSProxy{
		upstreamURL: cfg.UpstreamURL,
		authHeader:  cfg.AuthHeader,
		authValue:   cfg.AuthValue,
	}

	srv, stop, err := server.New(proxy, timeout)
	if err != nil {
		handle.Log(shared.LogLevelError, "ws-proxy: failed to start embedded server: %v", err)
		return nil, fmt.Errorf("ws-proxy: %w", err)
	}

	handle.Log(shared.LogLevelInfo,
		"ws-proxy: embedded WS server listening on %s", srv.Addr())

	return &rawFilterFactory{stop: stop}, nil
}

func (f *rawConfigFactory) CreatePerRoute(_ []byte) (any, error) { return nil, nil }

// --- rawFilterFactory ---

// rawFilterFactory holds the stop function for the embedded server.
// It creates a pass-through filter for HTTP traffic — WS upgrades are
// routed by Envoy to the embedded server's cluster, not through this filter.
type rawFilterFactory struct {
	shared.EmptyHttpFilterFactory
	stop func()
}

func (f *rawFilterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &passthroughFilter{}
}

// OnDestroy stops the embedded WS server when Envoy hot-reloads.
func (f *rawFilterFactory) OnDestroy() {
	if f.stop != nil {
		f.stop()
	}
}

// passthroughFilter is a no-op filter for non-WS HTTP traffic.
// WS traffic never reaches this filter — it goes directly to the embedded server.
type passthroughFilter struct {
	shared.EmptyHttpFilter
}

// --- WSProxy ---

// WSProxy is an http.Handler that accepts WebSocket upgrades from Envoy,
// dials the upstream provider, and pumps frames bidirectionally.
//
// OnClientFrame and OnUpstreamFrame are optional hooks called for every frame
// in each direction. Set them before the server starts to inspect or meter frames.
type WSProxy struct {
	upstreamURL string
	authHeader  string
	authValue   string
	log         *slog.Logger

	// OnClientFrame is called for each frame sent by the downstream client.
	// It runs in the pump goroutine — keep it fast and non-blocking.
	OnClientFrame func(websocket.MessageType, []byte)

	// OnUpstreamFrame is called for each frame received from the upstream.
	// It runs in the pump goroutine — keep it fast and non-blocking.
	OnUpstreamFrame func(websocket.MessageType, []byte)
}

// NewProxy creates a WSProxy. Use this directly in tests or when embedding
// without the Envoy factory.
func NewProxy(upstreamURL, authHeader, authValue string) *WSProxy {
	return &WSProxy{
		upstreamURL: upstreamURL,
		authHeader:  authHeader,
		authValue:   authValue,
	}
}

// ServeHTTP handles an incoming WebSocket upgrade from Envoy.
// It accepts the upgrade, dials the upstream, and runs the bidirectional pump.
func (p *WSProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := slog.Default()
	if p.log != nil {
		log = p.log
	}

	// Accept the WS upgrade from the downstream client.
	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // trust Envoy's TLS termination
	})
	if err != nil {
		log.Error("ws-proxy: accept failed", "err", err)
		return
	}
	defer clientConn.CloseNow()

	// Dial the upstream provider with auth injected.
	upstreamHeader := http.Header{}
	if p.authHeader != "" && p.authValue != "" {
		upstreamHeader.Set(p.authHeader, p.authValue)
	}

	ctx := r.Context()
	upstreamConn, _, err := websocket.Dial(ctx, p.upstreamURL+r.URL.Path, &websocket.DialOptions{
		HTTPHeader: upstreamHeader,
	})
	if err != nil {
		log.Error("ws-proxy: upstream dial failed", "url", p.upstreamURL+r.URL.Path, "err", err)
		clientConn.Close(websocket.StatusInternalError, "upstream unavailable")
		return
	}
	defer upstreamConn.CloseNow()

	log.Info("ws-proxy: session started", "path", r.URL.Path)
	start := time.Now()

	// Bidirectional frame pump.
	// Two goroutines run concurrently — one per direction.
	// The first to error (client disconnect or upstream close) signals the other via errc.
	errc := make(chan error, 2)

	// Client → Upstream
	go func() {
		for {
			msgType, data, err := clientConn.Read(ctx)
			if err != nil {
				errc <- fmt.Errorf("client read: %w", err)
				return
			}
			p.onClientFrame(msgType, data)
			if err := upstreamConn.Write(ctx, msgType, data); err != nil {
				errc <- fmt.Errorf("upstream write: %w", err)
				return
			}
		}
	}()

	// Upstream → Client
	go func() {
		for {
			msgType, data, err := upstreamConn.Read(ctx)
			if err != nil {
				errc <- fmt.Errorf("upstream read: %w", err)
				return
			}
			p.onUpstreamFrame(msgType, data)
			if err := clientConn.Write(ctx, msgType, data); err != nil {
				errc <- fmt.Errorf("client write: %w", err)
				return
			}
		}
	}()

	// Wait for either direction to close.
	firstErr := <-errc
	log.Info("ws-proxy: session ended",
		"path", r.URL.Path,
		"duration", time.Since(start).Round(time.Millisecond),
		"reason", firstErr,
	)
}

// onClientFrame is called for every frame the client sends upstream.
func (p *WSProxy) onClientFrame(msgType websocket.MessageType, data []byte) {
	if p.OnClientFrame != nil {
		p.OnClientFrame(msgType, data)
	}
}

// onUpstreamFrame is called for every frame the upstream sends to the client.
func (p *WSProxy) onUpstreamFrame(msgType websocket.MessageType, data []byte) {
	if p.OnUpstreamFrame != nil {
		p.OnUpstreamFrame(msgType, data)
	}
}
