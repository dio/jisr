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
    "github.com/dio/jisr"
)

func init() {
    jisr.Register("my-auth", jisr.Chain(authHandler, loggingMiddleware))
}

func authHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
    if r.Header.Get("x-api-key") == "" {
        w.SendError(401, `{"error":"missing api key"}`)
        return
    }
    w.SetRequestHeader("x-user-id", "alice")
    // return without SendError = forward request upstream
}
```

## Build

```sh
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o my-filter.so ./cmd
```

## API

- `jisr.Register(name, fn)` — register a HandlerFunc for an Envoy filter name
- `jisr.Chain(handler, middlewares...)` — compose middleware (net/http style)
- `jisr.Do(ctx, scheduler, handle, cluster, headers, body, timeoutMs)` — blocking Envoy HttpCallout
- `w.SendError(code, body)` / `w.SendErrorBytes(code, body)` — send local response
- `w.SetRequestHeader(key, value)` — mutate request header before forwarding
- `w.SetMetadata(namespace, key, value)` — set Envoy dynamic metadata
- `r.Header` — request headers as `http.Header` (Go-owned, safe to use anywhere)
- `r.Body` — request body as `io.Reader` (blocking, channel-backed)

## How it works

Each incoming request spawns one goroutine. The raw `OnRequestHeaders` callback copies all header data into Go memory, creates a channel-backed `io.Reader` for the body, and returns `StopAllAndBuffer` to suspend the Envoy filter chain. Body chunks arrive via `OnRequestBody` and are pushed into the channel. When the handler goroutine returns, `Scheduler.Schedule` hops back onto the Envoy worker thread to apply mutations and call `ContinueRequest`. Client disconnects cancel the context via `OnDestroy`.

## License

Apache-2.0
