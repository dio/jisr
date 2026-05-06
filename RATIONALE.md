# RATIONALE

Why jisr exists, and the design decisions behind it.

## The Problem

Envoy dynamic modules expose a powerful extension mechanism — write a `.so`,
load it at runtime, intercept HTTP traffic. The Go SDK is functional and
correct. But writing a filter against the raw SDK looks like this:

```go
func (f *MyFilter) OnRequestHeaders(headers shared.HeaderMap, endStream bool) shared.HeadersStatus {
    scheduler := f.handle.GetScheduler()
    apiKey := headers.GetOne("x-api-key").ToString() // copy — or you get a data race

    go func() {
        result, err := callAuthService(apiKey) // blocking call, off event loop

        scheduler.Schedule(func() { // hop back onto worker thread
            if err != nil || !result.ok {
                f.handle.SendLocalResponse(403, nil, []byte("forbidden"), "jisr")
                return
            }
            f.handle.SetMetadata("ns", "user", result.userID)
            f.handle.ContinueRequest()
        })
    }()

    return shared.HeadersStatusStopAllAndBuffer
}
```

This is not wrong — it is how Envoy dynamic modules work. But every filter
author has to understand the event loop threading model, the difference between
`StopAllAndBuffer` and `Stop`, which APIs are safe to call from which thread,
and why `UnsafeEnvoyBuffer` must not escape a callback. These are Envoy
internals, not business logic.

Compare to `net/http`:

```go
func myHandler(w http.ResponseWriter, r *http.Request) {
    result, err := callAuthService(r.Header.Get("x-api-key"))
    if err != nil || !result.ok {
        http.Error(w, "forbidden", 403)
        return
    }
    r.Header.Set("x-user-id", result.userID)
}
```

The concurrency is handled by the runtime. The memory is owned by Go. The
control flow is `return`. The test is `httptest.NewRecorder()`.

jisr is the bridge between these two worlds.

## The Core Trick

Each incoming request spawns one goroutine. The raw `OnRequestHeaders`
callback copies all header data into Go-owned memory (`header.ToString()`),
creates a channel-backed `io.Reader` for the body, returns `HeadersStatusStop`
to suspend the Envoy filter chain, and starts the goroutine.

Body chunks arrive via `OnRequestBody` and are pushed into the channel. The
goroutine's `io.ReadAll(r.Body)` blocks naturally — no event loop thread is
held. When the handler returns, `Scheduler.Schedule` hops back onto the Envoy
worker thread to apply mutations and call `ContinueRequest`. Client disconnects
cancel the context via `OnStreamComplete`.

This is the same model as `net/http` — one goroutine per request — running
inside an Envoy dynamic module.

## Why HeadersStatusStop, not StopAllAndBuffer

`StopAllAndBuffer` tells Envoy to buffer the body internally without calling
`OnRequestBody`. The filter never sees the chunks. `io.ReadAll(r.Body)` would
block forever.

`HeadersStatusStop` tells Envoy to stop processing headers here but continue
delivering body callbacks (`OnRequestBody`) as chunks arrive. This feeds the
channel correctly.

If the handler never reads `r.Body`, the chunks still arrive and get dropped
(the goroutine exits, the channel is GC'd). This is safe — Envoy only requires
that `ContinueRequest` or `SendLocalResponse` is eventually called.

## Why Send, not Write

`.Write` in Go carries a strong signature contract: `Write([]byte) (int, error)`
from `io.Writer`. Violating that with a status-code-first signature would create
a false cognate — it looks like `io.Writer` but isn't.

`net/http` splits this into `WriteHeader(code)` + `Write(body)` because it
supports streaming. jisr does not stream — every response is a single complete
buffer (Envoy's `SendLocalResponse`). A combined `Send(code, body)` is the
honest API for a non-streaming, single-shot response.

## Response Phase: One Goroutine, Full Lifecycle

The same goroutine that runs the request handler can continue running through
the upstream response. `RegisterWithResponse` wires this up:

```go
jisr.RegisterWithResponse("my-filter", requestFn, responseFn, jisr.ResponseModeObserve)
```

After `ContinueRequest`, the goroutine blocks on a channel until
`OnResponseHeaders` fires on the Envoy worker thread. The response headers are
copied into Go memory and sent through the channel. The goroutine unblocks and
runs the `ResponseFunc`. When that returns, `Scheduler.Schedule` fires
`ContinueResponse`.

This means request context, metrics IDs, and any data computed during the
request phase are all available in the response handler — in the same goroutine,
with no synchronization needed.

### The Three Response Modes

The mode is fixed at registration time. This lets `OnResponseHeaders` return the
correct Envoy status immediately without blocking the worker thread.

**Passthrough** (`ResponseModePassthrough`): `OnResponseHeaders` returns
`HeadersStatusStop`, `OnResponseBody` returns `BodyStatusContinue`. Headers are
held; body flows through. The response handler runs with `r.Body == nil`. Use
for inspecting response headers and emitting metrics without touching the body.

**Observe** (`ResponseModeObserve`): `OnResponseHeaders` returns
`HeadersStatusContinue`. Headers flow immediately to the downstream client.
Body chunks are forwarded downstream and simultaneously copied into the Go
channel. The response handler reads from `r.Body` while the client is already
receiving the data — zero added latency. Use for SSE token counting, logging,
response tapping.

**Buffer** (`ResponseModeBuffer`): Both headers and body are held by Envoy
(`HeadersStatusStop`, `BodyStatusStopAndBuffer`). The response handler receives
the full body via `r.Body`. Use for body rewriting, JSON transformation, content
filtering. Client waits for the full upstream response.

### Why SetUpstreamResponseHeader Only Works in Buffer Mode

When you call `w.SetUpstreamResponseHeader(k, v)` from a response handler, the
mutation is queued and applied in the `Scheduler.Schedule` callback that calls
`ContinueResponse`. This works correctly in Buffer mode because Envoy holds both
headers and body — the mutation fires before anything reaches the client.

In Passthrough mode, `OnResponseBody` returns `BodyStatusContinue`, meaning
body data flows to the client immediately. By the time the scheduled callback
fires, the response body may have already been forwarded and the response header
map may be in an inconsistent state. In practice, the mutation is silently
ignored on Envoy 1.37.1.

If you need to add or modify a response header, use `ResponseModeBuffer` and
drain the body with `io.Copy(io.Discard, r.Body)` if you don't need to modify
it. This is the only reliable path for response header mutation today.

## Attribute Snapshotting

Envoy stream attributes (path, method, host, etc.) are accessible via
`handle.GetAttributeString(id)`. This call is only valid on the Envoy worker
thread — not from a goroutine. jisr reads all common string-typed attributes in
`OnRequestHeaders` before spawning the goroutine and stores them in a map on the
`Request`. Handlers call `r.GetAttr(jisr.AttrRequestPath)` with no locking.

Only string-typed attributes are snapshotted. Numeric attributes
(`AttributeIDRequestSize`, `AttributeIDRequestDuration`) are not supported by
`GetAttributeString` in Envoy 1.37.1 — calling them produces an error log and
returns empty. jisr omits them from the snapshot list.

## Envoy-Native Metrics

Raw SDK filters define metrics in the config factory `Create()` callback and
store the `MetricID` handles for use in per-request callbacks. jisr exposes this
via `RegisterWithConfig`:

```go
var requestsTotal jisr.MetricID

func init() {
    jisr.RegisterWithConfig("my-filter",
        func(h jisr.ConfigHandle) error {
            var err error
            requestsTotal, err = h.DefineCounter("my_requests_total", "cluster")
            return err
        },
        myHandler,
    )
}
```

The `ConfigFunc` runs once when Envoy loads the `.so`, on the worker thread.
`MetricID` is just a `uint64` — safe to read from any goroutine. Per-request
increments are queued in `responseWriterImpl.metricMuts` and flushed in the
same `Scheduler.Schedule` callback that calls `ContinueRequest`, so they always
run on the correct thread.

`RegisterWithConfigAndResponse` combines config setup, request handler, response
handler, and mode into a single call for filters that need all four.

## ClearRouteCache and Model Routing

Envoy's `cluster_header` route matches on a request header value and selects
a cluster. But the header must be set before the route is evaluated. If a filter
sets `x-cluster` after route selection has already run, the cluster is ignored.

`ClearRouteCache()` tells Envoy to discard the current route evaluation and
re-run it with the current header values. This is how zia-decoder routes LLM
requests: read the `model` field from the request body, map it to a provider
cluster name, set `x-cluster`, then call `ClearRouteCache`. Envoy picks the
right upstream on the re-evaluation.

In jisr, `w.ClearRouteCache()` queues the call alongside header mutations and
fires it in the `Scheduler.Schedule` callback before `ContinueRequest`.

## Zero-Copy Body Reader

Each body chunk from Envoy arrives via `OnRequestBody` as an
`UnsafeEnvoyBuffer`. `ToBytes()` copies it into Go-heap memory — this copy is
unavoidable because the Envoy C++ side owns the buffer and the Go GC cannot pin
it. The copy is then sent through the channel to the handler goroutine.

The original implementation used a `bytes.Buffer` as an intermediate: each
chunk was written to the buffer on arrival, then read out of the buffer by the
handler. This was a second copy (write into buffer) plus a third copy (read from
buffer into the caller's slice).

The current `bodyReader` eliminates the intermediate buffer entirely. Each chunk
is held by reference (`cur []byte`, `off int`). `Read(p)` serves bytes directly
from `cur[off:]` via a single `copy(p, src)` into the caller's slice. The
dropped alloc is the `bytes.Buffer` struct and its backing array — 1 fewer alloc
and 64 fewer bytes per request.

The remaining copies are irreducible:
- `ToBytes()`: ABI contract, Go GC cannot pin Envoy memory
- `copy(p, src)`: `io.Reader` contract, caller provides the destination slice

## LimitBody: Partial Body Inspection

For LLM routing, you need only the first few KB of the request body (enough to
find the `model` field). `r.LimitBody(n)` caps `r.Body` at `n` bytes. After `n`
bytes are delivered, `r.Body` returns EOF and the body channel is closed. The
`headDone` flag tells `OnRequestBody` to switch from channel-push mode to
passthrough mode — subsequent chunks are forwarded directly to Envoy without
being copied into Go memory.

This gives you the LLM routing pattern with minimal overhead:

```go
r.LimitBody(8192)
head, _ := io.ReadAll(r.Body) // at most 8KB, then EOF
// remaining body streams to upstream without touching Go memory
```

## jisr/prof: pprof Inside a .so

A running `.so` is a full Go runtime embedded in an Envoy worker process.
`net/http/pprof` works — but the default `DefaultServeMux` registrations are
global and bleed across packages. `jisr/prof` registers all pprof handlers
explicitly on a private `ServeMux`, starts a background HTTP server on a
configurable address, and exposes:

- `/healthz`, `/readyz` — liveness/readiness probes (standard ops requirement)
- `/version` — `debug.ReadBuildInfo()` as JSON, showing which module version is
  actually running inside Envoy
- `/debug/pprof/*` — goroutine dumps, heap profiles, CPU profiles, execution traces

The goroutine dump at `/debug/pprof/goroutine?debug=2` is the primary leak
detector. After 200 concurrent requests (two bursts), the only goroutines
present should be the `jisr/server.Group` actor goroutines (accept loop,
stop-channel waiter) and any per-request handlers currently in flight. Zero
accumulation confirms the channel-based lifecycle is clean.

## What You Give Up — and the Escape Hatches

**Full body buffering.** The abstraction always delivers the complete request
body before the handler runs. Raw SDK filters can process and forward chunks
before the full body arrives.

*Escape hatch: `r.SkipBody()` and `r.LimitBody(n)`.*
`SkipBody` closes the body channel immediately and forwards subsequent chunks
without buffering. `LimitBody(n)` delivers the first `n` bytes, then switches
to passthrough for the remainder. Zero memory overhead for header-only filters;
minimal overhead for body-inspecting filters.

---

**No chunk-by-chunk response streaming.** `Send`/`SendBytes` flush a complete
buffer. SSE or chunked local response generation must happen upstream.

*Escape hatch: `w.Stream(ctx, headers)`.*
Returns a `StreamWriter` that lets you push chunks one at a time. Each
`Flush(ctx, data)` call schedules `SendResponseData` onto the Envoy worker
thread and blocks until delivered — natural backpressure. `Close()` sends the
final `endOfStream`.

---

**Response header mutation only works in Buffer mode.** `SetUpstreamResponseHeader`
is silently ignored in Passthrough and Observe modes due to Envoy's header
forwarding timing (see above).

*Workaround: use `ResponseModeBuffer` and drain the body if you don't need to
modify it.* The added latency is the time to receive the full upstream response.

---

**One goroutine per request.** Same model as `net/http`. Tens of thousands of
goroutines are routine for Go. But it is not zero overhead.

*Escape hatch: `jisr.RegisterRaw`.*
Register a raw SDK factory alongside jisr filters in the same `.so`. Implement
`OnRequestHeaders`, `OnRequestBody`, etc. directly. Full control, no goroutine
overhead, all Envoy SDK constraints apply.

---

**Some protocols cannot be intercepted.** WebSocket frames arrive after a 101
handshake. Envoy does not call `OnRequestBody` or `OnResponseBody` for them.

*Escape hatch: `jisr.RegisterRaw` + embedded server via `jisr/server.Group`.*
Run a `net/http` server on a background goroutine inside the `.so`. Envoy routes
WebSocket upgrades to a local STATIC cluster pointing at that server.

---

**SSE tapping without buffering.** Reading and counting SSE tokens while the
stream flows to the client requires `RegisterWithResponse(ResponseModeObserve)`.
The handler receives body chunks via `r.Body` while they are simultaneously
forwarded downstream.

*Helper: `jisr/buffer.HeadTail`.*
Captures the first N bytes and last M bytes of a stream in a fixed-size ring.
Zero allocation after construction. Designed for the LLM SSE pattern:

```go
buf := buffer.NewHeadTail(8*1024, 64*1024) // head 8KB, tail 64KB
chunk := make([]byte, 4096)
for {
    n, err := r.Body.Read(chunk)
    if n > 0 { buf.Write(chunk[:n]) }
    if err != nil { break }
}
// buf.Head() — first 8KB (message_start, input tokens)
// buf.Tail() — last 64KB (message_delta, output tokens)
```

## What You Keep

Everything that matters for typical filter work:

- Blocking code, no manual async wiring
- `r.Header` as plain `http.Header` — Go-owned, safe everywhere
- `io.ReadAll(r.Body)` — just works
- `context.Context` cancellation when the client disconnects
- `r.Log(level, ...)` routes through Envoy's own logger
- `r.GetAttr(jisr.AttrRequestPath)` — pre-snapshotted stream attributes
- `http.Status*` constants for response codes
- Middleware via `Chain(handler, mw1, mw2, ...)`
- Direct responses via `w.Send` / `w.SendBytes`
- `w.ClearRouteCache()` for model-to-cluster routing
- `w.IncrementCounter(id, n, labels...)` / `w.RecordHistogram(id, n, labels...)`
- `jisr.Do(ctx, ...)` for blocking Envoy-native upstream callouts

## Relationship to Composer

BOE's composer is a Go plugin framework layered on top of the same SDK.
jisr is different in two ways:

1. jisr is a standalone Go module — no dependency on composer, no
   goplugin-loader, no shared `.so` constraints. Your filter is its own `.so`.

2. jisr changes the programming model. Composer still exposes status enums,
   UnsafeEnvoyBuffer, and per-phase callbacks. jisr hides all of that behind
   a single `HandlerFunc`.

The two are complementary. Composer is the right choice when you need
fine-grained streaming control or are building into the BOE ecosystem.
jisr is the right choice when you want to write a filter the same way you
write a `net/http` handler.
