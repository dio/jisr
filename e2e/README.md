# e2e

These tests build `e2e/libe2e.so`, start a real Envoy process, and exercise
jisr through Envoy's dynamic module ABI. The e2e module registers multiple
examples in one `.so` so a single Envoy process can cover normal filters and
embedded actors. They are intentionally
separate from unit tests because several behaviours depend on Envoy timing,
admin stats, access logs, or dynamic module callbacks that the fake harness
cannot model.

Run them from the repository root:

```sh
make e2e
```

Set `ENVOY_BIN=/path/to/envoy` to test a specific Envoy binary. The default is
the local Envoy used by the development environment. CI downloads the pinned
Envoy binary before running this target.

## What is covered

The suite verifies the request path, local responses, response modes,
disconnect handling, and observability paths:

| Area | Test surface |
|------|--------------|
| Request mutation | `hello` injects `x-hello` and forwards to the backend |
| Local response | `hello-echo` responds with JSON without hitting upstream |
| Structured logs | `Request.LogAttrs` reaches Envoy's `dynamic_modules` logger |
| Dynamic metadata logs | `w.SetMetadata` values render through Envoy access-log `%DYNAMIC_METADATA(...)%` |
| Envoy stats | `hello_requests_total` increments and `hello_request_handler_duration_ms` appears in `/stats` after a request |
| OpenTelemetry export | Envoy exports the hello counter and histogram to an in-process OTLP/gRPC metrics sink |
| Embedded actors | `ws-proxy` accepts an Envoy-routed WebSocket upgrade, proxies it to a local mock upstream, and emits actor metrics through `github.com/dio/logging` |
| Response modes | Passthrough, Observe, and Buffer response handlers run against real upstream responses |
| Disconnects | Client and upstream disconnect cases do not deadlock or leak the filter chain |
| Admin server | The embedded jisr/prof admin server exposes health, readiness, version, and pprof endpoints |

## Observability harness

`TestMain` captures Envoy stdout/stderr in memory while still streaming it to
the test log. Log assertions search that buffer for the exact line emitted by
Envoy, not for a synthetic fake-harness message.

The metrics tests use two independent paths:

1. `GET /stats?filter=hello_requests_total` proves the counter exists in
   Envoy's native stats store and increases after a request. A second stats
   assertion checks that the handler-duration histogram appears after an
   observation is recorded.
2. A small OTLP/gRPC metrics service receives Envoy's OpenTelemetry stat sink
   exports and records exported metric names. This proves jisr-defined counters
   and histograms can leave Envoy through an OTel sink.

The dynamic metadata test uses an Envoy access log configured with:

```text
%DYNAMIC_METADATA(jisr:filter)% %DYNAMIC_METADATA(jisr:route)%
```

The `hello` filter sets those values with `w.SetMetadata`, and the e2e test
asserts that Envoy's access log renders them.

The embedded actor test starts a local WebSocket upstream in-process. Envoy
routes the downstream WebSocket upgrade to the `ws-proxy` actor's fixed
`listen_addr`, and the actor dials the local upstream instead of OpenAI. The
test verifies the proxied response, the structured session log emitted by
`WSProxy.ServeHTTP`, and actor-side `ws_proxy_sessions_total` /
`ws_proxy_session_duration_ms` metrics exported to the same in-process OTLP
sink.

## Port usage

Envoy listeners use fixed local ports `10000` through `10007`, and the Envoy
admin listener uses `9901`. The backend, reset backend, ws-proxy mock upstream,
ws-proxy embedded actor listener, jisr/prof admin server, and OTLP/gRPC metrics
sink bind random loopback ports. If a fixed Envoy port is already in use, stop
the stale Envoy process and rerun `make e2e`.
