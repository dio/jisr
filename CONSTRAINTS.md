# Constraints

Behaviors discovered through real testing against Envoy 1.37.1. Each entry
states what works, what does not, and why — so you don't have to rediscover
them by running into a wall.

---

## Response header mutation

### SetUpstreamResponseHeader

| Mode | Works | Notes |
|------|-------|-------|
| `ResponseModeBuffer` | yes | Headers held by Envoy until `ContinueResponse`; mutation applied before anything reaches the client |
| `ResponseModePassthrough` | no | `OnResponseBody` returns `BodyStatusContinue`, so body flows before the scheduled `ContinueResponse` fires. The header map may already be inconsistent. Mutation is silently ignored on Envoy 1.37.1 |
| `ResponseModeObserve` | no | `OnResponseHeaders` returns `HeadersStatusContinue`, meaning headers are already forwarded to the client before the goroutine runs. Nothing to mutate |

**Rule:** if you need to set a response header, use `ResponseModeBuffer`.
If you don't want to modify the body, drain it with `io.Copy(io.Discard, r.Body)`.

### ReplaceBody

| Mode | Works | Notes |
|------|-------|-------|
| `ResponseModeBuffer` | yes | Envoy holds the buffered body; jisr drains it and appends replacement bytes via `BufferedResponseBody().Drain()` + `Append()` |
| `ResponseModePassthrough` | no | No body buffer exists; call is a no-op |
| `ResponseModeObserve` | no | Body is already flowing to the downstream client; replacement is too late |

**Rule:** always pair `ReplaceBody` with `SetUpstreamResponseHeader("content-length", ...)`.
Envoy will not recalculate content-length for you.

---

## Stream attributes (r.GetAttr)

Attributes are snapshotted in `OnRequestHeaders` via `GetAttributeString`. Only
string-typed attributes are included. Calling `GetAttributeString` on a numeric
attribute produces an Envoy error log and returns `(nil, false)`.

| Attribute | ID | Works | Notes |
|-----------|----|-------|-------|
| `AttrRequestPath` | 0 | yes | e.g. `/v1/chat/completions` |
| `AttrRequestMethod` | 4 | yes | e.g. `POST` |
| `AttrRequestHost` | 2 | yes | e.g. `api.example.com` |
| `AttrRequestScheme` | 3 | yes | e.g. `https` |
| `AttrRequestQuery` | 11 | yes | query string, e.g. `model=gpt-4o` |
| `AttrRequestProtocol` | 10 | yes | e.g. `HTTP/1.1` |
| `AttrRequestID` | 9 | yes | x-request-id value |
| `AttrRequestUserAgent` | 7 | yes | User-Agent header value |
| `RequestSize` (ID 13) | 13 | **no** | Numeric. Produces `Unsupported attribute ID 13 as string` error log on every request. Omitted from jisr's snapshot list |
| `RequestDuration` (ID 12) | 12 | **no** | Numeric. Same issue |
| `RequestTotalSize` (ID 14) | 14 | **no** | Numeric. Same issue |

---

## Request body

| Behavior | Works | Notes |
|----------|-------|-------|
| `io.ReadAll(r.Body)` | yes | Blocks until all chunks arrive or context is cancelled |
| `r.SkipBody()` on `Request` | yes | Closes body channel immediately; subsequent chunks forwarded without buffering. Must call for header-only filters to avoid stalling the worker thread |
| `r.LimitBody(n)` | yes | Delivers first `n` bytes, then switches to passthrough for the rest. Call before reading `r.Body` |
| Reading `r.Body` without calling `SkipBody` or `LimitBody` on large bodies | **no** | Worker thread stalls pushing chunks into the channel until the goroutine reads them. Always call `SkipBody` when not reading the body |
| Calling `LimitBody` after starting to read `r.Body` | **no** | Must be called before the first `Read` |
| Calling both `SkipBody` and `LimitBody` | **no** | Mutually exclusive. `SkipBody` wins if called first (CAS) |

---

## Response body (ResponseFunc)

| Behavior | Works | Notes |
|----------|-------|-------|
| `io.ReadAll(r.Body)` in `ResponseModeBuffer` | yes | Blocks until full body is buffered by Envoy |
| `io.Copy(io.Discard, r.Body)` to unblock | works but wrong | Unnecessary: use `r.SkipBody()` |
| `r.SkipBody()` on `Response` | yes | Closes response body channel immediately; `OnResponseBody` switches to passthrough (body forwarded to client unread). Use when you only need headers |
| `r.Body` in `ResponseModePassthrough` | nil | No body is delivered; `r.SkipBody()` is a no-op |
| `r.Body` in `ResponseModeObserve` | streaming | Read while body flows to client; `r.SkipBody()` closes channel, remaining chunks forwarded |

---

## Envoy headers

Envoy sends all HTTP/2 request headers in lowercase (e.g. `:method`, `:path`,
`content-type`). jisr copies them into `http.Header` via `h.Add(k, v)`, which
calls `http.CanonicalHeaderKey` internally. The result is that all headers in
`r.Header` use Go canonical form (`Content-Type`, `X-Request-Id`).

| Behavior | Notes |
|----------|-------|
| `r.Header.Get("content-type")` | returns the value (canonical lookup is case-insensitive) |
| `r.Header.Get("Content-Type")` | returns the value (same key after canonicalization) |
| `r.Header.Get(":path")` | returns `""` — pseudo-headers are not canonicalized to `Path`; use `r.GetAttr(jisr.AttrRequestPath)` instead |
| Pseudo-headers (`:method`, `:path`, `:scheme`, `:authority`) | accessible only via `r.Header.Get(":method")` etc. (exact lowercase key) or via `r.GetAttr` for path/method/host/scheme |

---

## Metrics (DefineCounter / DefineHistogram)

| Behavior | Works | Notes |
|----------|-------|-------|
| `h.DefineCounter` in `ConfigFunc` | yes | Runs on Envoy worker thread at `.so` load. `MetricID` is a `uint64`, safe to store in package vars and read from any goroutine |
| `w.IncrementCounter(id, n, labels...)` in handler | yes | Queued, flushed in `Scheduler.Schedule` before `ContinueRequest` |
| `w.RecordHistogram(id, n, labels...)` in handler | yes | Same — queued and flushed on worker thread |
| Calling `DefineCounter` from a handler (per-request) | **no** | `ConfigFunc` runs once at config factory `Create`. Per-request calls to `DefineCounter` are not supported by the SDK |
| `label count != tagKeys count` | **no** | Tag values at increment time must match tag keys declared at `DefineCounter` time. Mismatch produces `MetricsResult` error (not a panic, silently dropped) |

---

## ClearRouteCache

| Behavior | Works | Notes |
|----------|-------|-------|
| `w.ClearRouteCache()` after `w.SetRequestHeader(clusterHeader, cluster)` | yes | Re-evaluates `cluster_header` route with the new header value |
| Calling `ClearRouteCache` without setting a routing header first | silent no-op | Route re-evaluates to the same result |
| Calling `ClearRouteCache` for a WebSocket upgrade request | **no** | WS routes are matched by the `upgrade` header condition. `ClearRouteCache` may not re-match the WS route. Do not call it on WS upgrades |

---

## Goroutine lifecycle

| Behavior | Notes |
|----------|-------|
| Handler goroutine exits cleanly after `w.Send` / `w.SendBytes` | yes — `responded` flag prevents response phase from parking on `respHeadersCh` |
| Context cancellation on client disconnect | yes — `OnStreamComplete` cancels `ctx`; goroutines blocked on `r.Body.Read` or `select { case <-ctx.Done() }` unblock |
| `r.Body.Read` unblocks on disconnect | yes — `OnStreamComplete` closes `bodyCh`; `Read` returns `(0, io.EOF)` |
| Goroutine accumulation after load | **no** — verified by `scripts/leak-check.py` across 200 concurrent requests: zero user goroutine growth |
| `[syscall, locked to thread]` goroutines in pprof dump | not leaks — Go runtime stubs for Envoy's C worker threads; created on first CGO boundary crossing, permanent for process lifetime |

---

## pprof / jisr/prof

| Behavior | Works | Notes |
|----------|-------|-------|
| `/debug/pprof/goroutine?debug=1` | yes | Returns text summary: `goroutine profile: total N` |
| `/debug/pprof/goroutine?debug=2` | yes | Returns full stack traces per goroutine |
| `/debug/pprof/heap` | yes | Returns gzip binary pprof profile (magic bytes `0x1f 0x8b`) |
| `/debug/pprof/profile?seconds=N` | yes | CPU profile — blocks for N seconds |
| `/debug/pprof/goroutine` (no debug param) | yes | Returns gzip binary profile |
| `DefaultServeMux` for pprof registration | **no** | Global mux; `net/http/pprof`'s `init()` registers there automatically. Sharing `DefaultServeMux` between filters or with other packages causes bleed. Always use an explicit `http.ServeMux` and register handlers explicitly |

---

## SDK / module naming

| Behavior | Notes |
|----------|-------|
| `.so` file must be named `lib<name>.so` | Envoy strips the `lib` prefix and `.so` suffix to match the `dynamic_module_config.name` field |
| `ENVOY_DYNAMIC_MODULES_SEARCH_PATH` | Must point to the directory containing the `.so`. Envoy looks for `lib<name>.so` inside this directory |
| `filter_config` in envoy.yaml | Omit entirely when there is no config. An empty `filter_config: "{}"` is not equivalent — it may cause parse errors depending on the typed config |
| ABI version string | `v0.0.1` for Envoy 1.37.1. Using the v1.38.0 SDK (`v0.1.0`) produces an ABI mismatch warning on every `.so` load |

---

## Go workspace (go.work)

| Behavior | Notes |
|----------|-------|
| `go mod tidy` under `GOWORK` active | **do not use** — MVS re-resolves all module versions and upgrades the Envoy SDK to the highest available version, breaking the ABI pin |
| `go work sync` | **do not use** — reverts manual `go.mod` edits |
| Running `go mod tidy` for external deps only | use `GOWORK=off go mod tidy` in the specific submodule |
| `go.work.sum` | gitignored — regenerated automatically |
| `go.work` | committed — workspace resolves local module versions for development |

---

## macOS vs Linux

| Behavior | Notes |
|----------|-------|
| `reuse_port` in Envoy listener config | Warning on macOS: `reuse_port was configured for TCP listener 'X' and is being force disabled`. Harmless — Envoy falls back to a single socket. No action needed for local development |
| `GODEBUG=cgocheck=0` | Required when running Envoy with a Go `.so` on macOS. Without it, the CGO pointer checker fires on Envoy's internal memory passing |
