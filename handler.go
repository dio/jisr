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
//	        w.SendError(http.StatusUnauthorized, `{"error":"missing api key"}`)
//	        return
//	    }
//	    w.SetRequestHeader("x-user-id", "alice")
//	    // return without SendError → request forwarded upstream
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
// returns StopAllAndBuffer to suspend the Envoy filter chain. Body chunks pushed
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
// Mutations queue up and are applied on ContinueRequest.
// Same underlying type as http.Header for familiarity.
type Header = http.Header

// Request holds the incoming request state visible to a Handler.
type Request struct {
	// Header contains the request headers, copied into Go memory.
	// Mutations are queued and applied when the handler returns.
	Header Header

	// Body is a blocking io.Reader backed by the Envoy body buffer.
	// io.ReadAll(r.Body) returns the complete body and blocks until
	// all chunks have arrived. Not reading Body means chunks are
	// forwarded without buffering.
	Body io.Reader

	// FilterName is the Envoy filter name this request matched.
	FilterName string

	// log is wired up by the filter bridge after the goroutine starts.
	// It schedules the log call back onto the Envoy worker thread so
	// messages appear in Envoy's own log output with the correct metadata.
	log func(level shared.LogLevel, format string, args ...any)
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
	// SendError sends an HTTP error response to the downstream client and
	// terminates the stream. After calling SendError, returning from the
	// handler is a no-op (ContinueRequest is not called).
	SendError(statusCode int, body string)

	// SendErrorBytes is like SendError but accepts a pre-encoded byte slice.
	// Useful when you already have a []byte (e.g. json.Marshal output) and
	// want to avoid an extra string conversion.
	SendErrorBytes(statusCode int, body []byte)

	// SetRequestHeader queues a mutation to the request header map that will
	// be applied before the request is forwarded upstream.
	SetRequestHeader(key, value string)

	// SetMetadata sets a dynamic metadata value on the stream, applied before
	// ContinueRequest. namespace is the metadata namespace, key the key.
	// value must be string, int64, float64, or bool.
	SetMetadata(namespace, key string, value any)
}

// HandlerFunc is a function that handles an Envoy HTTP filter event.
// It is the primary way to use jisr:
//
//	jisr.Register("my-filter", jisr.HandlerFunc(func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
//	    apiKey := r.Header.Get("x-api-key")
//	    if apiKey == "" {
//	        w.SendError(401, "missing api key")
//	        return
//	    }
//	    // returning without SendError → request is forwarded upstream
//	}))
type HandlerFunc func(ctx context.Context, w ResponseWriter, r *Request)

// Middleware wraps a HandlerFunc, returning a new HandlerFunc.
// Middleware runs in order from outermost to innermost — the first
// middleware in a Chain call is the first to execute.
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
	// Apply in reverse so the first middleware is the outermost
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// registry maps filter names to HandlerFuncs.
var registry = map[string]HandlerFunc{}

// Register associates a HandlerFunc with an Envoy filter name.
// Call Register from an init() function so it runs before the ABI entry point.
// Panics if the same name is registered twice.
//
// Use Chain to apply middleware:
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

// WellKnownHttpFilterConfigFactories returns the SDK factory map for all registered
// filters. Pass this to sdk.RegisterHttpFilterConfigFactories in your main package.
func WellKnownHttpFilterConfigFactories() map[string]shared.HttpFilterConfigFactory {
	m := make(map[string]shared.HttpFilterConfigFactory, len(registry))
	for name, fn := range registry {
		m[name] = &configFactory{name: name, handler: fn}
	}
	return m
}
