// Package jisr provides a Go middleware API for writing Envoy dynamic module
// filters. Register a [HandlerFunc] and write ordinary blocking code — jisr
// bridges the goroutine onto Envoy's worker thread internally.
//
// Middleware is composed with [Chain]:
//
//	jisr.Register("my-filter", jisr.Chain(myHandler, logging, auth))
//	// execution order: logging → auth → myHandler
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
// Client disconnects cancel the context via OnStreamComplete.
//
// See https://github.com/dio/jisr/tree/main/examples/hello for a runnable example.
package jisr

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// MetricID is an opaque handle to an Envoy metric defined at config time.
// It is a re-export of the SDK type. No wrapping, no conversion needed.
type MetricID = shared.MetricID

// ConfigHandle is the handle passed to a ConfigFunc at filter config creation time.
// Use it to define Envoy metrics and read raw filter config bytes.
// ConfigHandle methods must only be called from the ConfigFunc, not from handlers.
type ConfigHandle interface {
	// DefineCounter defines an Envoy counter metric with the given name and tag keys.
	// Tag keys are declared once; tag values are provided per-increment via IncrementCounter.
	DefineCounter(name string, tagKeys ...string) (MetricID, error)

	// DefineHistogram defines an Envoy histogram metric with the given name and tag keys.
	DefineHistogram(name string, tagKeys ...string) (MetricID, error)

	// RawConfig returns the raw filter_config bytes from the Envoy config.
	// Returns nil if no filter_config was provided.
	RawConfig() []byte

	// Log emits a message to Envoy's logger.
	Log(level shared.LogLevel, format string, args ...any)
}

// configHandleImpl wraps shared.HttpFilterConfigHandle to implement ConfigHandle.
type configHandleImpl struct {
	h   shared.HttpFilterConfigHandle
	raw []byte
}

func (c *configHandleImpl) DefineCounter(name string, tagKeys ...string) (MetricID, error) {
	id, res := c.h.DefineCounter(name, tagKeys...)
	if res != shared.MetricsSuccess {
		return 0, fmt.Errorf("jisr: DefineCounter %q failed (result=%d)", name, res)
	}
	return id, nil
}

func (c *configHandleImpl) DefineHistogram(name string, tagKeys ...string) (MetricID, error) {
	id, res := c.h.DefineHistogram(name, tagKeys...)
	if res != shared.MetricsSuccess {
		return 0, fmt.Errorf("jisr: DefineHistogram %q failed (result=%d)", name, res)
	}
	return id, nil
}

func (c *configHandleImpl) RawConfig() []byte { return c.raw }

func (c *configHandleImpl) Log(level shared.LogLevel, format string, args ...any) {
	c.h.Log(level, format, args...)
}

// HandlerFactory is a constructor called once at filter config creation time.
// It receives the ConfigHandle (for metrics, raw config) and returns a
// HandlerFunc bound to that config instance. Use this instead of package-level
// vars when the handler needs per-config state (parsed config, metric IDs, etc.)
//
//	type MyFilter struct {
//	    cfg     *Config
//	    counter jisr.MetricID
//	}
//
//	func (f *MyFilter) Handle(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
//	    w.IncrementCounter(f.counter, 1, f.cfg.Cluster)
//	}
//
//	func init() {
//	    jisr.RegisterFactory("my-filter", func(h jisr.ConfigHandle) (jisr.HandlerFunc, error) {
//	        cfg, err := parseConfig(h.RawConfig())
//	        if err != nil { return nil, err }
//	        counter, _ := h.DefineCounter("my_requests_total", "cluster")
//	        f := &MyFilter{cfg: cfg, counter: counter}
//	        return f.Handle, nil
//	    })
//	}
type HandlerFactory func(h ConfigHandle) (HandlerFunc, error)

// ResponseHandlerFactory is like HandlerFactory but returns both a request
// HandlerFunc and a response ResponseFunc from the same config context.
// Use with RegisterFactoryWithResponse for filters that need per-config state
// across both the request and response phase.
//
//	type MyFilter struct {
//	    cfg   *Config
//	    ttft  jisr.MetricID
//	}
//
//	func init() {
//	    jisr.RegisterFactoryWithResponse("my-filter",
//	        func(h jisr.ConfigHandle) (jisr.HandlerFunc, jisr.ResponseFunc, error) {
//	            cfg, _ := parseConfig(h.RawConfig())
//	            ttft, _ := h.DefineHistogram("ttft_ms", "cluster")
//	            f := &MyFilter{cfg: cfg, ttft: ttft}
//	            return f.HandleRequest, f.HandleResponse, nil
//	        },
//	        jisr.ResponseModeObserve,
//	    )
//	}
type ResponseHandlerFactory func(h ConfigHandle) (HandlerFunc, ResponseFunc, error)


// (i.e. when Envoy loads the .so). Use it to define metrics and parse config.
// Return a non-nil error to abort .so load. Envoy will log the error.
type ConfigFunc func(h ConfigHandle) error

// Attr re-exports the SDK AttributeID constants for use with r.GetAttr.
// These map directly to Envoy stream attributes, snapshotted in OnRequestHeaders.
// Only string-typed attributes are included; numeric ones (Size, Duration) are
// not supported by GetAttributeString in Envoy 1.37.1.
const (
	AttrRequestPath      = shared.AttributeIDRequestPath
	AttrRequestMethod    = shared.AttributeIDRequestMethod
	AttrRequestHost      = shared.AttributeIDRequestHost
	AttrRequestScheme    = shared.AttributeIDRequestScheme
	AttrRequestQuery     = shared.AttributeIDRequestQuery
	AttrRequestProtocol  = shared.AttributeIDRequestProtocol
	AttrRequestID        = shared.AttributeIDRequestId
	AttrRequestUserAgent = shared.AttributeIDRequestUserAgent
)


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

	// attrs holds pre-snapshotted Envoy stream attributes, read in OnRequestHeaders
	// on the worker thread before the handler goroutine is spawned.
	attrs map[shared.AttributeID]string

	// internal: wired by the filter bridge
	log       func(level shared.LogLevel, format string, args ...any)
	skipBody  func()
	limitBody func(n int64)
}

// GetAttr returns a pre-snapshotted Envoy stream attribute by ID.
// Attributes are read once in OnRequestHeaders on the Envoy worker thread.
// Safe to call from any goroutine with no locking.
//
// Use the jisr Attr constants: [AttrRequestPath], [AttrRequestMethod], etc.
//
//	cluster := resolveCluster(r.Header.Get("x-model"))
//	path    := r.GetAttr(jisr.AttrRequestPath)
func (r *Request) GetAttr(id shared.AttributeID) string {
	if r.attrs == nil {
		return ""
	}
	return r.attrs[id]
}

// SkipBody signals jisr that this handler will not read the request body.
// The body channel is closed immediately (any pending io.ReadAll returns EOF)
// and subsequent OnRequestBody calls forward chunks without buffering.
//
// Always call SkipBody when you only need headers. Without it, large request
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

// LimitBody caps r.Body at n bytes. After n bytes have been delivered, r.Body
// returns EOF and any subsequent request body chunks stream directly to upstream
// without being copied into Go memory.
//
// Use when you only need to inspect the beginning of a large body. For example,
// reading a JSON model field from an LLM request while streaming the full body
// to the upstream provider:
//
//	r.LimitBody(8192)
//	head, _ := io.ReadAll(r.Body)  // at most 8192 bytes, then EOF
//	var req struct{ Model string `json:"model"` }
//	json.Unmarshal(head, &req)
//	// remainder of body streams to upstream without Go copies
//
// If the body is shorter than n, all bytes are delivered normally.
// LimitBody has no effect on small bodies.
//
// Must be called before reading r.Body. Mutually exclusive with SkipBody.
func (r *Request) LimitBody(n int64) {
	if n > 0 && r.limitBody != nil {
		r.limitBody(n)
	}
}

// Log emits a message to Envoy's logger at the given level.
// Messages appear in Envoy's log output with the worker thread ID,
// component tag, and timestamp. Identical to logs from Envoy itself.
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
	// status code and terminates the filter chain. The request is NOT
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
	// Call r.SkipBody() before Stream; the body channel must not block the
	// worker thread while the handler is generating stream output.
	//
	// Returns an error if Send/SendBytes was already called, or ctx is done.
	Stream(ctx context.Context, headers [][2]string) (StreamWriter, error)

	// SetMetadata sets a dynamic metadata value on the stream, applied before
	// ContinueRequest. value must be string, int64, float64, or bool.
	// Has no effect if Send/SendBytes/Stream was called.
	SetMetadata(namespace, key string, value any)

	// ClearRouteCache clears Envoy's cached route for this stream, applied before
	// ContinueRequest. Call after mutating a cluster-selection header (e.g. x-cluster)
	// so Envoy re-evaluates the cluster_header route with the new value.
	// Has no effect if Send/SendBytes/Stream was called.
	ClearRouteCache()

	// IncrementCounter adds n to the counter metric identified by id.
	// labels are tag values in the same order as the tag keys declared in DefineCounter.
	// Applied on the Envoy worker thread before ContinueRequest.
	IncrementCounter(id MetricID, n uint64, labels ...string)

	// RecordHistogram records a histogram observation.
	// labels are tag values in the same order as the tag keys declared in DefineHistogram.
	// Applied on the Envoy worker thread before ContinueRequest.
	RecordHistogram(id MetricID, n uint64, labels ...string)

	// SetUpstreamResponseHeader queues a mutation to the upstream response
	// headers, applied before ContinueResponse. Only valid from a ResponseFunc.
	SetUpstreamResponseHeader(key, value string)

	// ReplaceBody replaces the upstream response body. Only valid in
	// ResponseModeBuffer. The caller must also set content-length accordingly.
	ReplaceBody(body []byte)
}

// ResponseMode declares how a ResponseFunc processes the upstream response body.
// It is set once at registration time and never changes, so OnResponseHeaders
// return the correct Envoy status immediately without blocking the worker thread.
type ResponseMode int32

const (
	// ResponseModePassthrough is the default. The ResponseFunc can inspect and
	// mutate response headers. The body streams through to downstream without
	// being copied into Go memory. r.Body is nil.
	ResponseModePassthrough ResponseMode = iota

	// ResponseModeObserve taps the response body as it streams. Chunks are
	// copied into Go memory and delivered via r.Body, AND forwarded to the
	// downstream client simultaneously, with zero added downstream latency.
	// Use for SSE token counting, logging, metrics.
	ResponseModeObserve

	// ResponseModeBuffer accumulates the full response body before the
	// ResponseFunc returns. r.Body delivers the complete body. The downstream
	// client waits. Adds full response latency.
	// Use for response rewriting, JSON transformation, content filtering.
	ResponseModeBuffer
)

// ResponseFunc handles the response phase of a filter.
// It runs in the same goroutine as the HandlerFunc for the same request,
// after ContinueRequest has been called and the upstream response has arrived.
//
// The response body mode (Passthrough/Observe/Buffer) is declared once at
// registration via RegisterWithResponse, not at runtime. This keeps
// OnResponseHeaders non-blocking on the Envoy worker thread.
type ResponseFunc func(ctx context.Context, w ResponseWriter, r *Response)

// Response holds the upstream response visible to a ResponseFunc.
type Response struct {
	// Header contains the upstream response headers.
	Header Header

	// StatusCode is the upstream HTTP status code.
	StatusCode int

	// Body is a streaming io.Reader for the response body.
	// Available only in ResponseModeObserve and ResponseModeBuffer.
	// Nil in ResponseModePassthrough: the body streams through without
	// being copied into Go memory.
	Body io.Reader

	// internal
	skipBody func()
}

// SkipBody signals jisr that this response handler will not read the response
// body. The body channel is closed immediately so the goroutine unblocks, and
// subsequent OnResponseBody chunks are forwarded to the downstream client
// without being copied into Go memory.
//
// Use when you only need response headers and want to add or inspect them
// without buffering the body:
//
//	func stampHandler(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
//	    r.SkipBody()
//	    w.SetUpstreamResponseHeader("x-processed-by", "jisr")
//	}
//
// SkipBody is a no-op in ResponseModePassthrough (Body is already nil).
func (r *Response) SkipBody() {
	if r.skipBody != nil {
		r.skipBody()
	}
}

// RegisterWithResponse associates both a request HandlerFunc and a response
// ResponseFunc with an Envoy filter name. Both run in the same goroutine
// and share the same context. Cancellation (client disconnect) propagates to both.
//
// mode declares how the response body is handled (set once, never changes).
// OnResponseHeaders returns the correct Envoy status without blocking:
//
//	ResponseModePassthrough: header-only, body streams through untouched
//	ResponseModeObserve:     tap body, zero downstream latency
//	ResponseModeBuffer:      accumulate full body, adds response latency
//
//	jisr.RegisterWithResponse("my-filter", requestHandler, responseHandler, jisr.ResponseModeObserve)
func RegisterWithResponse(name string, req HandlerFunc, resp ResponseFunc, mode ResponseMode) {
	if _, exists := registry[name]; exists {
		panic("jisr: filter already registered: " + name)
	}
	registry[name] = req
	respRegistry[name] = resp
	respModeRegistry[name] = mode
}


//
// Typical usage:
//
//	func authHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
//	    r.SkipBody() // header-only: skip to avoid blocking on body channel
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

// configRegistry maps filter names to ConfigFuncs (optional, for RegisterWithConfig).
var configRegistry = map[string]ConfigFunc{}

// respRegistry maps filter names to ResponseFuncs (optional, paired with registry).
var respRegistry = map[string]ResponseFunc{}

// respModeRegistry maps filter names to ResponseModes.
var respModeRegistry = map[string]ResponseMode{}

// rawRegistry maps filter names to raw SDK factories (escape hatch).
var rawRegistry = map[string]shared.HttpFilterConfigFactory{}

// factoryRegistry maps filter names to HandlerFactory functions.
var factoryRegistry = map[string]HandlerFactory{}

// respFactRegistry maps filter names to ResponseHandlerFactory functions.
var respFactRegistry = map[string]ResponseHandlerFactory{}

// RegisterFactory registers a HandlerFactory for a filter name.
// The factory is called once when Envoy loads the .so and returns a HandlerFunc
// constructed from the config context. Use this when the handler needs
// per-config state — parsed config, metric IDs, connections — without
// resorting to package-level variables.
//
// See [HandlerFactory] for a full example.
func RegisterFactory(name string, fn HandlerFactory) {
	if _, exists := registry[name]; exists {
		panic("jisr: filter already registered: " + name)
	}
	if _, exists := factoryRegistry[name]; exists {
		panic("jisr: filter already registered: " + name)
	}
	factoryRegistry[name] = fn
}

// RegisterFactoryWithResponse registers a ResponseHandlerFactory for a filter
// name. The factory returns both a request HandlerFunc and a response
// ResponseFunc constructed from the same config context.
//
// See [ResponseHandlerFactory] for a full example.
func RegisterFactoryWithResponse(name string, fn ResponseHandlerFactory, mode ResponseMode) {
	if _, exists := registry[name]; exists {
		panic("jisr: filter already registered: " + name)
	}
	if _, exists := respFactRegistry[name]; exists {
		panic("jisr: filter already registered: " + name)
	}
	respFactRegistry[name] = fn
	respModeRegistry[name] = mode
}


// The ConfigFunc runs once when the .so is loaded. Use it to define Envoy metrics
// and parse filter config. The HandlerFunc runs per-request.
//
// For filters that also need a response phase, use [RegisterWithConfigAndResponse].
//
//	var requestsTotal jisr.MetricID
//
//	func init() {
//	    jisr.RegisterWithConfig("zia-decoder",
//	        func(h jisr.ConfigHandle) error {
//	            var err error
//	            requestsTotal, err = h.DefineCounter("zia_requests_total", "cluster")
//	            return err
//	        },
//	        decoderHandler,
//	    )
//	}
//
//	func decoderHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
//	    w.SetRequestHeader("x-cluster", resolveCluster(r))
//	    w.ClearRouteCache()
//	    w.IncrementCounter(requestsTotal, 1, "openai")
//	}
func RegisterWithConfig(name string, cfg ConfigFunc, fn HandlerFunc) {
	if _, exists := registry[name]; exists {
		panic("jisr: filter already registered: " + name)
	}
	registry[name] = fn
	configRegistry[name] = cfg
}

// RegisterWithConfigAndResponse combines config setup, a request HandlerFunc,
// a response ResponseFunc, and a ResponseMode in a single call.
// Use this when the filter needs both Envoy metrics and a response phase.
//
//	var (
//	    requestsTotal jisr.MetricID
//	    ttftMs        jisr.MetricID
//	)
//
//	func init() {
//	    jisr.RegisterWithConfigAndResponse("zia-decoder",
//	        func(h jisr.ConfigHandle) error {
//	            requestsTotal, _ = h.DefineCounter("zia_requests_total", "cluster")
//	            ttftMs, _        = h.DefineHistogram("zia_ttft_ms", "cluster")
//	            return nil
//	        },
//	        requestHandler,
//	        responseHandler,
//	        jisr.ResponseModeObserve,
//	    )
//	}
func RegisterWithConfigAndResponse(name string, cfg ConfigFunc, req HandlerFunc, resp ResponseFunc, mode ResponseMode) {
	if _, exists := registry[name]; exists {
		panic("jisr: filter already registered: " + name)
	}
	registry[name] = req
	configRegistry[name] = cfg
	respRegistry[name] = resp
	respModeRegistry[name] = mode
}

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
// need direct SDK access: raw OnRequestBody callbacks, response phase hooks,
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
// Intended for use in tests. Production code should not unregister filters.
func Unregister(name string) {
	delete(registry, name)
	delete(configRegistry, name)
	delete(factoryRegistry, name)
	delete(respFactRegistry, name)
	delete(respRegistry, name)
	delete(respModeRegistry, name)
	delete(rawRegistry, name)
}

// WellKnownHttpFilterConfigFactories returns the SDK factory map for all
// registered filters (both jisr HandlerFunc and raw). Pass this to
// sdk.RegisterHttpFilterConfigFactories in your main package.
func WellKnownHttpFilterConfigFactories() map[string]shared.HttpFilterConfigFactory {
	m := make(map[string]shared.HttpFilterConfigFactory,
		len(registry)+len(factoryRegistry)+len(respFactRegistry)+len(rawRegistry))
	for name, fn := range registry {
		m[name] = &configFactory{
			name:        name,
			configFn:    configRegistry[name], // nil for plain Register, handled in Create
			handler:     fn,
			respHandler: respRegistry[name],
			respMode:    respModeRegistry[name],
		}
	}
	for name, fn := range factoryRegistry {
		m[name] = &configFactory{
			name:      name,
			factoryFn: fn,
		}
	}
	for name, fn := range respFactRegistry {
		m[name] = &configFactory{
			name:       name,
			respFactFn: fn,
			respMode:   respModeRegistry[name],
		}
	}
	maps.Copy(m, rawRegistry)
	return m
}
