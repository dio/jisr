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

## What You Give Up

jisr is not free. Three real costs:

**Full body buffering.** The abstraction always delivers the complete request
body before the handler runs. Raw SDK filters can process and forward chunks
before the full body arrives — jisr cannot without breaking the blocking API.
For auth, rate limiting, token counting, header rewriting, this is fine.
For large file upload proxying, use the raw SDK.

**One goroutine per request.** Same model as `net/http`. Go handles tens of
thousands of goroutines efficiently — this is not a practical concern at any
realistic filter concurrency. But it is not zero overhead like a pure
`HttpCallout`-based filter.

**No chunk-by-chunk response streaming.** `Send`/`SendBytes` flush a complete
buffer. SSE or chunked response generation must happen upstream — jisr cannot
stream a local response incrementally.

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
