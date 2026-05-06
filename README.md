# jisr

**jisr** (جسر — Arabic for "bridge") is a net/http-style handler abstraction for writing [Envoy dynamic module](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/advanced/dynamic_modules) filters in Go.

Instead of implementing the raw `HttpFilter` interface with status enums, event-loop thread discipline, and `UnsafeEnvoyBuffer` management, you register a `HandlerFunc` and write blocking code — jisr handles the goroutine bridge internally.

## Example

```go
package main

import (
    _ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"
    sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"

    "github.com/dio/jisr"
    _ "github.com/my-org/my-filter" // registers handlers via init()
)

func init() {
    sdk.RegisterHttpFilterConfigFactories(jisr.WellKnownHttpFilterConfigFactories())
}

func main() {}
```

```go
// my-filter/filter.go
package myfilter

import (
    "context"
    "net/http"

    "github.com/dio/jisr"
)

func init() {
    jisr.Register("my-auth", jisr.Chain(authHandler, loggingMiddleware))
}

func authHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
    r.SkipBody() // header-only filter — skip body to avoid blocking
    if r.Header.Get("x-api-key") == "" {
        w.Send(http.StatusUnauthorized, `{"error":"missing api key"}`)
        return
    }
    w.SetRequestHeader("x-user-id", "alice")
    // return without Send → request forwarded upstream
}
```

## Build

```sh
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libmyfilter.so ./cmd
```

## API

### Handler registration
- `jisr.Register(name, fn)` — register a HandlerFunc for an Envoy filter name
- `jisr.RegisterRaw(name, factory)` — escape hatch: register a raw SDK factory alongside jisr filters in the same .so
- `jisr.Chain(handler, middlewares...)` — compose middleware (net/http style)

### Request
- `r.Header` — request headers as `http.Header` (Go-owned, canonical keys, safe anywhere)
- `r.Body` — request body as `io.Reader` (blocking, channel-backed)
- `r.SkipBody()` — skip body buffering; chunks forwarded without buffering. Call for header-only filters
- `r.Log(level, format, args...)` — log via Envoy's logger (`jisr.LogInfo`, `LogWarn`, etc.)
- `r.FilterName` — the Envoy filter name this request matched

### Response
- `w.Send(code, body)` — send local response, no upstream forwarding (any status)
- `w.SendBytes(code, body)` — like Send but accepts `[]byte`
- `w.SetRequestHeader(key, value)` — mutate request header before forwarding
- `w.SetResponseHeader(key, value)` — set header on local response (before Send/SendBytes)
- `w.SetMetadata(namespace, key, value)` — set Envoy dynamic metadata
- `w.Stream(ctx, headers)` — begin streaming local response; returns a `StreamWriter`

### StreamWriter (for SSE / chunked responses)
- `sw.Flush(ctx, data)` — send a chunk; blocks until delivered (natural backpressure)
- `sw.Close()` — send final empty chunk (endOfStream)

### Upstream callout
- `jisr.Do(ctx, scheduler, handle, cluster, headers, body, timeoutMs)` — blocking Envoy HttpCallout

### Sub-packages
- `jisr/server` — embedded background servers (WebSocket proxy, sidecar)
- `jisr/buffer` — zero-allocation stream buffers (`Ring`, `HeadTail`) for SSE/chunked response parsing

## How it works

Each request spawns one goroutine. `OnRequestHeaders` copies header data into Go memory, creates a channel-backed `io.Reader` for the body, and returns `HeadersStatusStop` to suspend the filter chain. Body chunks from `OnRequestBody` are pushed into the channel. When the handler returns, `Scheduler.Schedule` hops back onto the Envoy worker thread to apply mutations and call `ContinueRequest`. Client disconnects cancel the context via `OnDestroy`.

See [examples/hello](examples/hello) for a runnable example and [RATIONALE.md](RATIONALE.md) for design decisions.

## License

Apache-2.0
