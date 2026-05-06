# jisr

**jisr** (جسر — Arabic for "bridge") is a Go middleware API for writing [Envoy dynamic module](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/advanced/dynamic_modules) filters.

Instead of implementing the raw `HttpFilter` interface with status enums, event-loop thread discipline, and `UnsafeEnvoyBuffer` management, you register a `HandlerFunc` and write blocking code. jisr handles the goroutine bridge internally.

## Quick start

```go
// my-filter/filter.go
package myfilter

import (
    "context"
    "net/http"
    "github.com/dio/jisr"
)

func init() {
    jisr.Register("my-auth", authHandler)
}

func authHandler(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
    r.SkipBody()
    if r.Header.Get("x-api-key") == "" {
        w.Send(http.StatusUnauthorized, `{"error":"missing api key"}`)
        return
    }
    w.SetRequestHeader("x-user-id", "alice")
    // return without Send → request forwarded upstream
}
```

```go
// cmd/main.go
package main

import (
    _ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"
    sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
    "github.com/dio/jisr"
    _ "github.com/my-org/my-filter"
)

func init() { sdk.RegisterHttpFilterConfigFactories(jisr.WellKnownHttpFilterConfigFactories()) }
func main() {}
```

```sh
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libmyfilter.so ./cmd
```

## Docs

| Document | Contents |
|----------|----------|
| [RATIONALE.md](RATIONALE.md) | Why jisr exists; design decisions behind the goroutine model, response modes, zero-copy body, metrics, ClearRouteCache, and escape hatches |
| [CONSTRAINTS.md](CONSTRAINTS.md) | What works, what doesn't, and why — tested against Envoy 1.37.1. Response header mutation, attribute support, body rules, metrics, SDK naming, go.work |

## Middleware

`Chain` composes middleware in declaration order. The first middleware is the
outermost wrapper — it runs first on the way in and last on the way out:

```go
jisr.Register("my-filter", jisr.Chain(myHandler, logging, auth))
// execution order: logging → auth → myHandler
```

A `jisr.Middleware` is a function that wraps a `HandlerFunc`:

```go
func logging(next jisr.HandlerFunc) jisr.HandlerFunc {
    return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
        r.Log(jisr.LogInfo, "%s %s", r.GetAttr(jisr.AttrRequestMethod), r.GetAttr(jisr.AttrRequestPath))
        next(ctx, w, r)
    }
}

func auth(next jisr.HandlerFunc) jisr.HandlerFunc {
    return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
        r.SkipBody()
        if r.Header.Get("X-Api-Key") == "" {
            w.Send(http.StatusUnauthorized, `{"error":"missing api key"}`)
            return // short-circuit: next is not called
        }
        next(ctx, w, r)
    }
}
```

Middleware runs in the same goroutine as the handler. `context.Context`
cancellation (client disconnect) propagates through the chain automatically.

Chain is zero-allocation after construction — the composed function is a
plain closure, no per-request allocation.

See [examples/auth](examples/auth) for a complete example: `loggingMiddleware`
is composed with `f.HandleRequest` inside a `RegisterFactoryWithResponse` factory,
showing how stateless middleware can wrap a struct-bound handler.

## Modifying the upstream response

Use `RegisterWithResponse` with `ResponseModeBuffer` to read, modify, or replace what the upstream sent before the client receives it.

### Inject a field into a JSON response body

```go
jisr.RegisterWithResponse("json-enricher", skipBodyFn, enricher, jisr.ResponseModeBuffer)

func enricher(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
    body, err := io.ReadAll(r.Body) // full upstream body — client waits
    if err != nil || len(body) == 0 {
        return
    }

    var obj map[string]any
    if json.Unmarshal(body, &obj) != nil {
        return // not JSON — leave body untouched (don't call ReplaceBody)
    }
    obj["processed"] = true

    rewritten, _ := json.Marshal(obj)
    w.SetUpstreamResponseHeader("content-length", strconv.Itoa(len(rewritten)))
    w.ReplaceBody(rewritten)
}
```

### Add a header to the upstream response

`SetUpstreamResponseHeader` must be used with `ResponseModeBuffer`. In `ResponseModePassthrough`, Envoy may have started forwarding the body before the scheduled header mutation fires, making the mutation ineffective.

```go
jisr.RegisterWithResponse("header-stamp", skipBodyFn, stamp, jisr.ResponseModeBuffer)

func stamp(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
    r.SkipBody() // not modifying the body — forward it as-is, unblock immediately
    w.SetUpstreamResponseHeader("x-processed-by", "jisr")
    w.SetUpstreamResponseHeader("x-upstream-status", strconv.Itoa(r.StatusCode))
}
```

### What each mode can do

| | `Passthrough` | `Observe` | `Buffer` |
|---|---|---|---|
| Read response headers | yes | yes | yes |
| Set upstream response headers | no* | no* | yes |
| Read response body | no | yes (streaming) | yes (full) |
| Replace response body | no | no | yes |
| Added downstream latency | zero | zero | full response |

\* `SetUpstreamResponseHeader` is silently ignored in Passthrough and Observe modes due to Envoy's header forwarding timing. Use Buffer mode for any response header mutations.

See [examples/hello/hello.go](examples/hello/hello.go) for `resp-rewrite` (body injection) and `resp-header-stamp` (header addition), both e2e-tested against real Envoy.

## Response phase

Filters can span the full request+response lifecycle in a single goroutine using `RegisterWithResponse`:

```go
jisr.RegisterWithResponse("resp-tap", requestFn, responseFn, jisr.ResponseModeObserve)
```

The request handler runs first, then blocks until upstream responds. The response handler receives upstream headers and (depending on mode) the response body:

```go
func requestFn(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
    r.SkipBody()
    w.SetRequestHeader("x-tapped", "1")
}

func responseFn(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
    // r.StatusCode, r.Header available in all modes.
    // r.Body available in Observe and Buffer modes.
    io.Copy(io.Discard, r.Body) // consume in Observe: client already receiving simultaneously
}
```

### Response modes

| Mode | `r.Body` | Downstream latency | Use for |
|------|----------|-------------------|---------|
| `ResponseModePassthrough` | nil | zero | Header inspection, stamping |
| `ResponseModeObserve` | streaming | zero | Token counting, logging, SSE tap |
| `ResponseModeBuffer` | full body | full response | Response rewriting, transformation |

The mode is declared once at registration and never changes at runtime — this lets `OnResponseHeaders` return the correct Envoy status immediately without blocking the worker thread.

## Struct-based handlers

When a handler needs per-config state — a parsed config struct, metric IDs,
a connection pool — use `RegisterFactory` instead of package-level variables.
The factory runs once at `.so` load time and returns a handler bound to that
state:

```go
type Router struct {
    cfg     *RouterConfig
    counter jisr.MetricID
    ttft    jisr.MetricID
}

func (r *Router) HandleRequest(_ context.Context, w jisr.ResponseWriter, req *jisr.Request) {
    r.LimitBody(8192)
    // ... use r.cfg, w.IncrementCounter(r.counter, ...)
}

func (r *Router) HandleResponse(_ context.Context, w jisr.ResponseWriter, resp *jisr.Response) {
    resp.SkipBody()
    w.RecordHistogram(r.ttft, measureTTFT())
}

func init() {
    jisr.RegisterFactoryWithResponse("llm-router",
        func(h jisr.ConfigHandle) (jisr.HandlerFunc, jisr.ResponseFunc, error) {
            cfg, err := parseConfig(h.RawConfig())
            if err != nil {
                return nil, nil, err
            }
            counter, _ := h.DefineCounter("router_requests_total", "cluster")
            ttft, _ := h.DefineHistogram("router_ttft_ms", "cluster")
            r := &Router{cfg: cfg, counter: counter, ttft: ttft}
            return r.HandleRequest, r.HandleResponse, nil
        },
        jisr.ResponseModeObserve,
    )
}
```

The factory pattern vs `RegisterWithConfig`:

| | `RegisterWithConfig` | `RegisterFactory` |
|---|---|---|
| State storage | package-level vars | struct fields |
| Multiple configs | shared vars (not safe) | each gets its own struct |
| Testability | requires global state reset | construct struct directly in tests |

See [examples/auth](examples/auth) for a complete runnable example: a configurable
API key auth filter where two Envoy listeners can use the same `.so` with different
allowed key sets, each getting its own independent `*AuthFilter` instance.

## Envoy-native metrics and routing

Use `RegisterWithConfig` (request-only) or `RegisterWithConfigAndResponse` (full lifecycle) to define Envoy metrics at `.so` load time and use them per-request:

```go
var (
    requestsTotal jisr.MetricID
    ttftMs        jisr.MetricID
)

func init() {
    jisr.RegisterWithConfigAndResponse("llm-router",
        func(h jisr.ConfigHandle) error {
            var err error
            requestsTotal, err = h.DefineCounter("router_requests_total", "cluster")
            if err != nil {
                return err
            }
            ttftMs, err = h.DefineHistogram("router_ttft_ms", "cluster")
            return err
        },
        decoderRequest,
        decoderResponse,
        jisr.ResponseModeObserve,
    )
}

func decoderRequest(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
    r.LimitBody(8192) // read first 8KB for model field, stream the rest
    head, _ := io.ReadAll(r.Body)

    var req struct{ Model string `json:"model"` }
    json.Unmarshal(head, &req)

    cluster := resolveCluster(req.Model)
    w.SetRequestHeader("x-cluster", cluster)
    w.SetMetadata("router", "cluster", cluster)
    w.ClearRouteCache() // re-evaluate cluster_header route with new x-cluster value
    w.IncrementCounter(requestsTotal, 1, cluster)
}

func decoderResponse(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
    // tap SSE token counts, record TTFT, etc.
    w.RecordHistogram(ttftMs, measureTTFT(), cluster)
}
```

See [examples/decoder](examples/decoder) for the full runnable example.

## API reference

### Registration

| Function | Description |
|----------|-------------|
| `Register(name, fn)` | Request-only filter |
| `RegisterWithResponse(name, reqFn, respFn, mode)` | Request + response filter |
| `RegisterWithConfig(name, cfgFn, fn)` | Request-only with config/metrics setup |
| `RegisterWithConfigAndResponse(name, cfgFn, reqFn, respFn, mode)` | Full lifecycle with config/metrics |
| `RegisterFactory(name, factoryFn)` | Request-only; factory constructs handler from config context — use when handler needs per-config state (parsed config, metric IDs) without package-level vars |
| `RegisterFactoryWithResponse(name, factoryFn, mode)` | Full lifecycle; factory constructs both request and response handlers from config context |
| `RegisterRaw(name, factory)` | Escape hatch: raw SDK factory |
| `Chain(handler, middlewares...)` | Compose middleware (outermost first) |

### Request (`*jisr.Request`)

| Field / Method | Description |
|----------------|-------------|
| `r.Header` | Request headers as `http.Header` (canonical keys, Go-owned) |
| `r.Body` | Blocking `io.Reader` backed by Envoy body chunks |
| `r.FilterName` | Envoy filter name matched for this request |
| `r.SkipBody()` | Skip body buffering; chunks forwarded without copying into Go memory |
| `r.LimitBody(n)` | Buffer first n bytes, stream the rest zero-copy |
| `r.GetAttr(id)` | Pre-snapshotted Envoy stream attribute (path, method, host, …) |
| `r.Log(level, fmt, args...)` | Log via Envoy's logger |

Available attribute IDs: `jisr.AttrRequestPath`, `AttrRequestMethod`, `AttrRequestHost`, `AttrRequestScheme`, `AttrRequestQuery`, `AttrRequestProtocol`, `AttrRequestID`, `AttrRequestUserAgent`.

### Response (`*jisr.Response`)

Passed to a `ResponseFunc`. Available only in `RegisterWithResponse` / `RegisterWithConfigAndResponse`.

| Field / Method | Description |
|----------------|-------------|
| `r.Header` | Upstream response headers as `http.Header` |
| `r.StatusCode` | Upstream HTTP status code |
| `r.Body` | `io.Reader` for the response body. Nil in `ResponseModePassthrough`; streaming in `ResponseModeObserve`; full-body in `ResponseModeBuffer` |
| `r.SkipBody()` | Skip reading the body; remaining chunks forwarded to client without copying into Go memory. Use when you only need headers. No-op in Passthrough (Body is already nil) |

### ResponseWriter

| Method | Description |
|--------|-------------|
| `w.Send(code, body)` | Send local response; no upstream forwarding |
| `w.SendBytes(code, body)` | Like Send but accepts `[]byte` |
| `w.SetRequestHeader(k, v)` | Mutate request header before forwarding |
| `w.SetResponseHeader(k, v)` | Set header on local response (before Send) |
| `w.SetMetadata(ns, key, val)` | Set Envoy dynamic metadata |
| `w.ClearRouteCache()` | Re-evaluate route after mutating a routing header |
| `w.IncrementCounter(id, n, labels...)` | Increment an Envoy counter metric |
| `w.RecordHistogram(id, n, labels...)` | Record an Envoy histogram observation |
| `w.SetUpstreamResponseHeader(k, v)` | Mutate upstream response header (response phase only) |
| `w.ReplaceBody(b)` | Replace upstream response body (`ResponseModeBuffer` only) |
| `w.Stream(ctx, headers)` | Begin streaming local response; returns `StreamWriter` |

### ConfigHandle (in ConfigFunc)

| Method | Description |
|--------|-------------|
| `h.DefineCounter(name, tagKeys...)` | Define an Envoy counter metric |
| `h.DefineHistogram(name, tagKeys...)` | Define an Envoy histogram metric |
| `h.RawConfig() []byte` | Raw `filter_config` bytes from envoy.yaml |
| `h.Log(level, fmt, args...)` | Log via Envoy's logger |

### StreamWriter

| Method | Description |
|--------|-------------|
| `sw.Flush(ctx, data)` | Send a chunk; blocks until delivered |
| `sw.Close()` | Send final empty chunk (end of stream) |

### Callout

```go
jisr.Do(ctx, scheduler, handle, cluster, headers, body, timeoutMs)
```

Blocking Envoy `HttpCallout` — connection pooling, retries, and circuit breaking from cluster config.

## Sub-packages

| Package | Description |
|---------|-------------|
| `jisr/server` | Background actor group for embedded servers and goroutines |
| `jisr/buffer` | Zero-allocation stream buffers (`Ring`, `HeadTail`) for SSE/chunked parsing |
| `jisr/prof` | pprof admin server for live `.so` debugging (`/healthz`, `/readyz`, `/version`, `/debug/pprof/*`) |

## How it works

Each request spawns one goroutine. `OnRequestHeaders` copies headers into Go memory, creates a channel-backed `io.Reader` for the body, and returns `HeadersStatusStop` to suspend the filter chain. Body chunks from `OnRequestBody` are pushed into the channel (zero intermediate copy after `ToBytes()`). When the handler returns, `Scheduler.Schedule` hops back onto the Envoy worker thread to apply mutations (`SetRequestHeader`, `ClearRouteCache`, `IncrementCounter`, …) and call `ContinueRequest`.

For response phase filters, the same goroutine blocks on a channel until `OnResponseHeaders` arrives, then runs the `ResponseFunc`. Passthrough and Buffer modes schedule `ContinueResponse` from the goroutine; Observe mode lets headers flow immediately (`HeadersStatusContinue`) and taps the body as it streams.

See [examples/hello](examples/hello) for a runnable request-phase example and [examples/decoder](examples/decoder) for a full request+response lifecycle example.

## License

Apache-2.0
