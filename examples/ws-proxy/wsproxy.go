// Package wsproxy demonstrates the embedded WebSocket proxy pattern using
// jisr/server, with a complete implementation for the OpenAI Realtime API
// at /v1/responses.
//
// # Protocol: OpenAI Realtime WebSocket (/v1/responses)
//
// The client sends one message to start a response:
//
//	{"type":"response.create","model":"gpt-4o-mini","input":[...],"max_output_tokens":N}
//
// The server streams events back:
//
//	{"type":"response.created", ...}
//	{"type":"response.output_text.delta", "delta":"Hi"}   // text chunks
//	{"type":"response.output_text.done",  ...}
//	{"type":"response.completed", "response":{"usage":{"input_tokens":N,"output_tokens":M}}}
//
// The WSProxy taps every frame:
//   - Client → Upstream: extracts model name from response.create
//   - Upstream → Client: extracts token usage from response.completed
//
// # Architecture
//
//	Client ──WS──► Envoy (port 10000, upgrade route)
//	                  │  ──► ws-proxy-local cluster (127.0.0.1:<random>)
//	                  │              │
//	                  │      WSProxy.ServeHTTP
//	                  │              │  ──► wss://api.openai.com/v1/responses
//	                  │              │      Authorization: Bearer $OPENAI_API_KEY
//	                  │
//	Normal HTTP ──► upstream cluster (TLS + ws-auth upstream filter)
package wsproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
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

// --- rawConfigFactory ---

type rawConfigFactory struct {
	shared.EmptyHttpFilterConfigFactory
}

func (f *rawConfigFactory) Create(
	handle shared.HttpFilterConfigHandle,
	raw []byte,
) (shared.HttpFilterFactory, error) {
	cfg := parseConfig(raw)
	proxy := newWSProxy(cfg)

	timeout := 5 * time.Second
	if cfg.ShutdownTimeout != "" {
		if d, err := time.ParseDuration(cfg.ShutdownTimeout); err == nil {
			timeout = d
		}
	}

	srv, stop, err := server.New(proxy, timeout)
	if err != nil {
		handle.Log(shared.LogLevelError, "ws-proxy: start failed: %v", err)
		return nil, fmt.Errorf("ws-proxy: %w", err)
	}

	handle.Log(shared.LogLevelInfo, "ws-proxy: listening on %s", srv.Addr())
	return &rawFilterFactory{stop: stop}, nil
}

func (f *rawConfigFactory) CreatePerRoute(_ []byte) (any, error) { return nil, nil }

// --- rawFilterFactory ---

type rawFilterFactory struct {
	shared.EmptyHttpFilterFactory
	stop func()
}

func (f *rawFilterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &passthroughFilter{}
}

func (f *rawFilterFactory) OnDestroy() {
	if f.stop != nil {
		f.stop()
	}
}

type passthroughFilter struct{ shared.EmptyHttpFilter }

// --- Config ---

// Config is the JSON config for the ws-proxy filter.
type Config struct {
	// UpstreamURL is the upstream wss:// base URL.
	// Default: "wss://api.openai.com"
	UpstreamURL string `json:"upstream_url"`

	// AuthHeader is the HTTP header name to inject when dialing upstream.
	// Default: "authorization"
	AuthHeader string `json:"auth_header"`

	// AuthValue is the header value. Supports ${ENV_VAR} expansion.
	// Default: "Bearer ${OPENAI_API_KEY}"
	AuthValue string `json:"auth_value"`

	// ShutdownTimeout for graceful shutdown. Default: "5s".
	ShutdownTimeout string `json:"shutdown_timeout"`
}

func parseConfig(raw []byte) Config {
	cfg := Config{
		UpstreamURL: "wss://api.openai.com",
		AuthHeader:  "authorization",
		AuthValue:   "Bearer ${OPENAI_API_KEY}",
	}
	if len(raw) > 0 {
		json.Unmarshal(raw, &cfg) //nolint:errcheck
	}
	return cfg
}

func newWSProxy(cfg Config) *WSProxy {
	return &WSProxy{
		upstreamURL: cfg.UpstreamURL,
		authHeader:  cfg.AuthHeader,
		authValue:   resolveEnv(cfg.AuthValue),
		log:         slog.Default(),
	}
}

// --- WSProxy ---

// WSProxy is an http.Handler that proxies WebSocket connections to an upstream
// provider, tapping frames for model extraction and token counting.
type WSProxy struct {
	upstreamURL string
	authHeader  string
	authValue   string
	log         *slog.Logger

	// OnClientFrame is called for each text frame the client sends.
	// Runs in the pump goroutine — must be fast and non-blocking.
	OnClientFrame func(websocket.MessageType, []byte)

	// OnUpstreamFrame is called for each text frame received from upstream.
	// Runs in the pump goroutine — must be fast and non-blocking.
	OnUpstreamFrame func(websocket.MessageType, []byte)
}

// NewProxy creates a WSProxy for direct use in tests or without the Envoy factory.
func NewProxy(upstreamURL, authHeader, authValue string) *WSProxy {
	return &WSProxy{
		upstreamURL: upstreamURL,
		authHeader:  authHeader,
		authValue:   authValue,
	}
}

// ServeHTTP accepts a WebSocket upgrade from Envoy, dials the upstream, and
// runs the bidirectional frame pump with per-session tapping.
func (p *WSProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := slog.Default()
	if p.log != nil {
		log = p.log
	}

	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Envoy handles downstream TLS
	})
	if err != nil {
		log.Error("ws-proxy: accept failed", "err", err)
		return
	}
	defer clientConn.CloseNow()

	upstreamHeader := http.Header{}
	if p.authHeader != "" && p.authValue != "" {
		upstreamHeader.Set(p.authHeader, p.authValue)
	}

	ctx := r.Context()
	upstreamURL := p.upstreamURL + r.URL.Path
	upstreamConn, _, err := websocket.Dial(ctx, upstreamURL, &websocket.DialOptions{
		HTTPHeader: upstreamHeader,
	})
	if err != nil {
		log.Error("ws-proxy: upstream dial failed", "url", upstreamURL, "err", err)
		clientConn.Close(websocket.StatusInternalError, "upstream unavailable")
		return
	}
	defer upstreamConn.CloseNow()

	tap := NewSessionTap()
	start := time.Now()
	log.Info("ws-proxy: session started", "path", r.URL.Path)

	errc := make(chan error, 2)

	// Client → Upstream
	go func() {
		for {
			msgType, data, err := clientConn.Read(ctx)
			if err != nil {
				errc <- fmt.Errorf("client read: %w", err)
				return
			}
			if msgType == websocket.MessageText {
				tap.FeedClient(data)
				if p.OnClientFrame != nil {
					p.OnClientFrame(msgType, data)
				}
			}
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
			if msgType == websocket.MessageText {
				tap.FeedUpstream(data)
				if p.OnUpstreamFrame != nil {
					p.OnUpstreamFrame(msgType, data)
				}
			}
			if err := clientConn.Write(ctx, msgType, data); err != nil {
				errc <- fmt.Errorf("client write: %w", err)
				return
			}
		}
	}()

	firstErr := <-errc

	u := tap.Usage()
	log.Info("ws-proxy: session ended",
		"path", r.URL.Path,
		"model", tap.Model(),
		"input_tokens", u.InputTokens,
		"output_tokens", u.OutputTokens,
		"duration", time.Since(start).Round(time.Millisecond),
		"reason", firstErr,
	)
}

// --- sessionTap ---

// SessionTap extracts the model name and token usage from OpenAI Realtime frames.
// It is created fresh for each WebSocket session.
// Exported for unit testing.
type SessionTap struct {
	model  string
	input  uint32
	output uint32
}

// NewSessionTap creates a new SessionTap.
func NewSessionTap() *SessionTap { return &SessionTap{} }

// FeedClient taps the response.create frame (first client message) to extract
// the model name. All subsequent client frames are ignored after model is known.
//
// OpenAI Realtime client frame format:
//
//	{"type":"response.create","model":"gpt-4o-mini","input":[...],"max_output_tokens":20}
func (t *SessionTap) FeedClient(data []byte) {
	if t.model != "" {
		return
	}
	if !bytes.Contains(data, []byte("response.create")) {
		return
	}
	var f struct {
		Type  string `json:"type"`
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &f) == nil && f.Type == "response.create" && f.Model != "" {
		t.model = f.Model
	}
}

// FeedUpstream taps the response.completed frame (final server event) to extract
// token usage. All other upstream frames are ignored.
//
// OpenAI Realtime server frame format:
//
//	{"type":"response.completed","response":{"usage":{"input_tokens":N,"output_tokens":M}}}
func (t *SessionTap) FeedUpstream(data []byte) {
	if !bytes.Contains(data, []byte("response.completed")) {
		return
	}
	var f struct {
		Type     string `json:"type"`
		Response struct {
			Usage struct {
				InputTokens  uint32 `json:"input_tokens"`
				OutputTokens uint32 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &f) == nil && f.Type == "response.completed" {
		t.input = f.Response.Usage.InputTokens
		t.output = f.Response.Usage.OutputTokens
	}
}

// Model returns the model name extracted from response.create, or "" if not yet seen.
func (t *SessionTap) Model() string { return t.model }

// TokenUsage holds the token counts from the completed response.
type TokenUsage struct {
	InputTokens  uint32
	OutputTokens uint32
}

// Usage returns the token usage extracted from response.completed.
func (t *SessionTap) Usage() TokenUsage {
	return TokenUsage{InputTokens: t.input, OutputTokens: t.output}
}

// ResolveEnv expands ${ENV_VAR} references in v using os.Expand.
// Returns v unchanged if the variable is unset.
func ResolveEnv(v string) string {
	return resolveEnv(v)
}

func resolveEnv(v string) string {
	// Use Expand but preserve ${VAR} when the variable is unset.
	return os.Expand(v, func(key string) string {
		if val := os.Getenv(key); val != "" {
			return val
		}
		return "${" + key + "}" // leave unexpanded
	})
}
