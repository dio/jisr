// Package jisr provides a net/http-style API for writing Envoy dynamic module
// filters in Go.
//
// Instead of implementing the raw HttpFilter interface — with event-loop thread
// discipline, status enums, and UnsafeEnvoyBuffer management — you register a
// [HandlerFunc] and write ordinary blocking code. jisr bridges the goroutine
// onto Envoy's worker thread internally.
//
// # Quick start
//
//	func init() {
//	    jisr.Register("my-filter", jisr.Chain(myHandler, loggingMiddleware))
//	}
//
//	func myHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
//	    r.Log(jisr.LogInfo, "handling request: %s", r.Header.Get(":path"))
//	    if r.Header.Get("x-api-key") == "" {
//	        w.Send(http.StatusUnauthorized, `{"error":"missing api key"}`)
//	        return
//	    }
//	    w.SetRequestHeader("x-user-id", "alice")
//	    // return without Send → request forwarded upstream
//	}
//
// # Building a .so
//
// Create a cmd/main.go that imports the abi package and your filter package,
// then register with the SDK:
//
//	package main
//
//	import (
//	    _ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"
//	    sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
//	    "github.com/dio/jisr"
//	    _ "your/filter/package"
//	)
//
//	func init() { sdk.RegisterHttpFilterConfigFactories(jisr.WellKnownHttpFilterConfigFactories()) }
//	func main() {}
//
// Build:
//
//	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libmyfilter.so ./cmd
//
// # How it works
//
// Each request spawns one goroutine. [OnRequestHeaders] copies all header data
// into Go-owned memory, creates a channel-backed [io.Reader] for the body, and
// returns HeadersStatusStop to suspend the Envoy filter chain. Body chunks pushed
// by OnRequestBody are forwarded into the channel. When the handler returns,
// [Scheduler.Schedule] hops back onto the Envoy worker thread to apply mutations
// and call ContinueRequest. Client disconnects cancel the context via OnDestroy.
//
// See https://github.com/dio/jisr/tree/main/examples/hello for a runnable example.
package jisr

import (
	"context"
	"io"
	"net/http"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// Log levels that map directly to Envoy's log levels.
// Use these with [Request.Log] so messages appear in Envoy's log output
// with the correct level, worker thread ID, and timestamp.
const (
	LogTrace    = shared.LogLevelTrace
	LogDebug    = shared.LogLevelDebug
	LogInfo     = shared.LogLevelInfo
	LogWarn     = shared.LogLevelWarn
	LogError    = shared.LogLevelError
	LogCritical = shared.LogLevelCritical
)

// Header is a copy of the request headers in Go-owned memory.
// Same underlying type as http.Header for familiarity.
type Header = http.Header

// Request holds the incoming request state visible to a Handler.
type Request struct {
	// Header contains the request headers, copied into Go memory.
	// Mutations are queued and applied when the handler returns.
	Header Header

	// Body is a blocking io.Reader backed by the Envoy body buffer.
	// io.ReadAll(r.Body) returns the complete body and blocks until
	// all chunks have arrived. Call SkipBody if you don't need it.
	Body io.Reader

	// FilterName is the Envoy filter name this request matched.
	FilterName string

	// internal — wired by the filter bridge
	log      func(level shared.LogLevel, format string, args ...any)
	skipBody func()
}

// SkipBody signals jisr that this handler will not read the request body.
// The body channel is closed immediately (any pending io.ReadAll returns EOF)
// and subsequent OnRequestBody calls forward chunks without buffering.
//
// Always call SkipBody when you only need headers — without it, large request
// bodies will block the Envoy worker thread on the body channel push:
//
//	func authHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
//	    r.SkipBody()
//	    if r.Header.Get("x-api-key") == "" {
//	        w.Send(http.StatusUnauthorized, `{"error":"missing key"}`)
//	        return
//	    }
//	    w.SetRequestHeader("x-user-id", "alice")
//	}
func (r *Request) SkipBody() {
	if r.skipBody != nil {
		r.skipBody()
	}
}

// Log emits a message to Envoy's logger at the given level.
// Messages appear in Envoy's log output with the worker thread ID,
// component tag, and timestamp — identical to logs from Envoy itself.
//
// Use the jisr log level constants: [LogTrace], [LogDebug], [LogInfo],
// [LogWarn], [LogError], [LogCritical].
//
//	r.Log(jisr.LogInfo, "auth ok for key=%s", apiKey)
func (r *Request) Log(level shared.LogLevel, format string, args ...any) {
	r.log(level, format, args...)
}

// ResponseWriter is the interface through which a Handler sends a response
// back to the client or mutates the request before forwarding.
type ResponseWriter interface {
	// Send sends a local HTTP response to the downstream client with any
	// status code and terminates the filter chain — the request is NOT
	// forwarded upstream.
	//
	//	w.Send(http.StatusOK, `{"status":"ok"}`)
	//	w.Send(http.StatusUnauthorized, `{"error":"missing api key"}`)
	Send(statusCode int, body string)

	// SendBytes is like Send but accepts a pre-encoded byte slice.
	SendBytes(statusCode int, body []byte)

	// SetRequestHeader queues a mutation to the request header map that will
	// be applied before the request is forwarded upstream.
	// Has no effect if Send/SendBytes/Stream was called.
	SetRequestHeader(key, value string)

	// SetResponseHeader sets a header on the local response sent by Send/SendBytes.
	// Must be called before Send/SendBytes.
	SetResponseHeader(key, value string)

	// Stream begins a streaming local response (SSE, chunked JSON, etc.).
	// Sends the given response headers immediately and returns a [StreamWriter].
	// The request is NOT forwarded upstream.
	//
	// Call r.SkipBody() before Stream — the body channel must not block the
	// worker thread while the handler is generating stream output.
	//
	// Returns an error if Send/SendBytes was already called, or ctx is done.
	Stream(ctx context.Context, headers [][2]string) (StreamWriter, error)

	// SetMetadata sets a dynamic metadata value on the stream, applied before
	// ContinueRequest. value must be string, int64, float64, or bool.
	// Has no effect if Send/SendBytes/Stream was called.
	SetMetadata(namespace, key string, value any)
}

// HandlerFunc is a function that handles an Envoy HTTP filter event.
//
// Typical usage:
//
//	func authHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
//	    r.SkipBody() // header-only — skip to avoid blocking on body channel
//	    if r.Header.Get("x-api-key") == "" {
//	        w.Send(http.StatusUnauthorized, `{"error":"missing api key"}`)
//	        return
//	    }
//	    w.SetRequestHeader("x-user-id", "alice")
//	    // return without Send → request forwarded upstream
//	}
type HandlerFunc func(ctx context.Context, w ResponseWriter, r *Request)

// Middleware wraps a HandlerFunc, returning a new HandlerFunc.
//
//	func LoggingMiddleware(next jisr.HandlerFunc) jisr.HandlerFunc {
//	    return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
//	        log.Printf("request: %s", r.Header.Get(":path"))
//	        next(ctx, w, r)
//	    }
//	}
type Middleware func(HandlerFunc) HandlerFunc

// Chain applies middleware to a HandlerFunc in declaration order.
// The first middleware is the outermost wrapper (runs first):
//
//	jisr.Chain(handler, logging, auth, rateLimit)
//	// execution order: logging → auth → rateLimit → handler
func Chain(h HandlerFunc, middlewares ...Middleware) HandlerFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// registry maps filter names to HandlerFuncs.
var registry = map[string]HandlerFunc{}

// rawRegistry maps filter names to raw SDK factories (escape hatch).
var rawRegistry = map[string]shared.HttpFilterConfigFactory{}

// Register associates a HandlerFunc with an Envoy filter name.
// Call from an init() function. Panics if the same name is registered twice.
//
//	func init() {
//	    jisr.Register("my-filter", jisr.Chain(myHandler, logging, auth))
//	}
func Register(name string, fn HandlerFunc) {
	if _, exists := registry[name]; exists {
		panic("jisr: filter already registered: " + name)
	}
	registry[name] = fn
}

// RegisterRaw registers a raw [shared.HttpFilterConfigFactory] for filters that
// need direct SDK access — raw OnRequestBody callbacks, response phase hooks,
// or anything jisr's HandlerFunc model cannot express.
//
// Use when a single .so needs both jisr-managed and raw filters:
//
//	func init() {
//	    jisr.Register("auth-filter", authHandler)          // jisr managed
//	    jisr.RegisterRaw("sse-passthrough", &rawFactory{}) // raw SDK control
//	}
func RegisterRaw(name string, factory shared.HttpFilterConfigFactory) {
	if _, exists := rawRegistry[name]; exists {
		panic("jisr: raw filter already registered: " + name)
	}
	rawRegistry[name] = factory
}

// Unregister removes a previously registered filter by name.
// Intended for use in tests — production code should not unregister filters.
func Unregister(name string) {
	delete(registry, name)
	delete(rawRegistry, name)
}

// WellKnownHttpFilterConfigFactories returns the SDK factory map for all
// registered filters (both jisr HandlerFunc and raw). Pass this to
// sdk.RegisterHttpFilterConfigFactories in your main package.
func WellKnownHttpFilterConfigFactories() map[string]shared.HttpFilterConfigFactory {
	m := make(map[string]shared.HttpFilterConfigFactory, len(registry)+len(rawRegistry))
	for name, fn := range registry {
		m[name] = &configFactory{name: name, handler: fn}
	}
	for name, factory := range rawRegistry {
		m[name] = factory
	}
	return m
}
