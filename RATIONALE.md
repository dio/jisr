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
                return // forgot to return here? upstream gets the request anyway
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
cancel the context via `OnDestroy`.

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

## What You Give Up — and the Escape Hatches

jisr is not free. Three real costs, each with a targeted escape hatch.

---

**Full body buffering.** The abstraction always delivers the complete request
body before the handler runs. Raw SDK filters can process and forward chunks
before the full body arrives — jisr cannot without breaking the blocking API.
For auth, rate limiting, token counting, header rewriting, this is fine.
For large file upload proxying, it is not.

*Escape hatch: `r.SkipBody()`.*
If your handler only needs headers and never reads `r.Body`, call `r.SkipBody()`
at the top of the handler. jisr closes the body channel immediately — any
pending `io.ReadAll` returns EOF — and subsequent `OnRequestBody` calls forward
chunks to Envoy without buffering. Zero memory overhead for header-only filters:

```go
func authHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
    r.SkipBody() // never reads body — skip to avoid buffering
    if r.Header.Get("x-api-key") == "" {
        w.Send(http.StatusUnauthorized, `{"error":"missing api key"}`)
        return
    }
    w.SetRequestHeader("x-user-id", "alice")
}
```

---

**No chunk-by-chunk response streaming.** `Send`/`SendBytes` flush a complete
buffer. SSE or chunked response generation must happen upstream — jisr cannot
stream a local response incrementally.

*Escape hatch: `w.Stream(ctx, headers)`.*
Returns a `StreamWriter` that lets you push chunks one at a time. Each
`Flush(ctx, data)` call schedules `SendResponseData` onto the Envoy worker
thread and blocks until delivered — providing natural backpressure. `Close()`
sends the final `endOfStream`.

```go
func sseHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
    r.SkipBody()
    sw, err := w.Stream(ctx, [][2]string{
        {"content-type", "text/event-stream"},
        {"cache-control", "no-cache"},
    })
    if err != nil {
        return
    }
    defer sw.Close()

    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return // client disconnected
        case t := <-ticker.C:
            event := fmt.Sprintf("data: {\"time\":%q}\n\n", t.Format(time.RFC3339))
            if err := sw.Flush(ctx, []byte(event)); err != nil {
                return
            }
        }
    }
}
```

---

**One goroutine per request.** Same model as `net/http`. Go handles tens of
thousands of goroutines efficiently — this is not a practical concern at any
realistic filter concurrency. But it is not zero overhead like a pure
`HttpCallout`-based filter.

*No escape hatch needed.* If you genuinely need zero goroutine overhead, use
`jisr.RegisterRaw` (see below) and implement the raw SDK filter directly.

---

**Some protocols cannot be intercepted at all.** WebSocket frames arrive after
a 101 Switching Protocols handshake. Envoy does not call `OnRequestBody` or
`OnResponseBody` for WebSocket frames — they are raw TCP data. A jisr handler
(or any dynamic module filter) cannot inspect them.

*Escape hatch: `jisr.RegisterRaw` + `jisr/server.NewEmbedded`.*
Register a raw SDK factory for filters that need response-phase callbacks,
per-chunk body processing, or full protocol control. In the same `.so`, run an
embedded `net/http` server on a background goroutine — Envoy routes the
problematic traffic (e.g. WebSocket upgrades) to that server via a local
STATIC cluster, bypassing the filter chain entirely.

```go
func init() {
    // jisr-managed filter for normal HTTP
    jisr.Register("my-auth", authHandler)

    // raw SDK factory for a filter that needs OnResponseBody callbacks
    jisr.RegisterRaw("my-decoder", &rawDecoderFactory{})
}
```

For the embedded server pattern (WebSocket proxy, protocol escape):

```go
// In your raw config factory Create():
ctx, stop := server.Background(func(ctx context.Context) {
    <-ctx.Done() // keep alive
})

srv, err := server.NewEmbedded(ctx, myWSHandler)
// srv.Port() → configure in Envoy STATIC cluster
// store stop → call from OnDestroy
```

`jisr/server` handles lifecycle: `Background` ties the server's lifetime to
`OnDestroy` via context cancellation. `NewEmbedded` binds a random free port
and shuts down cleanly on context cancel.

---

**Tapping SSE streams without buffering.** jisr's handler cannot intercept
upstream response bodies — that requires `OnResponseBody`, which is a raw SDK
concern. But when you do implement a raw `OnResponseBody` filter (via
`RegisterRaw`), extracting token counts from a large SSE stream without
buffering the whole thing is non-trivial.

*Helper: `jisr/buffer.HeadTail`.*
Captures the first N bytes and last M bytes of a stream in a fixed-size
ring — zero allocation after construction. Designed for the LLM SSE pattern
where input tokens appear near the start and output tokens near the end:

```go
ht := buffer.NewHeadTail(8*1024, 64*1024) // head 8KB, tail 64KB

// In OnResponseBody:
for _, chunk := range body.GetChunks() {
    ht.Write(chunk.ToUnsafeBytes())
}

// After endOfStream:
extractInputTokens(ht.Head())  // first 8KB — message_start
extractOutputTokens(ht.Tail()) // last 64KB — message_delta / usage
```

## What You Keep

Everything that matters for typical filter work:

- Blocking code, no manual async wiring
- `r.Header` as plain `http.Header` — Go-owned, safe everywhere
- `io.ReadAll(r.Body)` — just works
- `context.Context` cancellation when the client disconnects
- `r.Log(level, ...)` routes through Envoy's own logger — correct thread,
  correct format, correct log level filtering
- `http.Status*` constants for response codes
- Middleware via `Chain(handler, mw1, mw2, ...)`
- Direct responses via `w.Send` / `w.SendBytes` — no upstream needed
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
