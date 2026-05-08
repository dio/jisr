# e2e

These tests build `examples/hello/libhello.so`, start a real Envoy process, and
exercise jisr through Envoy's dynamic module ABI. They are intentionally
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
| Envoy stats | `hello_requests_total` increments in `/stats` after a request |
| OpenTelemetry export | Envoy exports `hello_requests_total` to an in-process OTLP/gRPC metrics sink |
| Response modes | Passthrough, Observe, and Buffer response handlers run against real upstream responses |
| Disconnects | Client and upstream disconnect cases do not deadlock or leak the filter chain |
| Admin server | The embedded jisr/prof admin server exposes health, readiness, version, and pprof endpoints |

## Observability harness

`TestMain` captures Envoy stdout/stderr in memory while still streaming it to
the test log. Log assertions search that buffer for the exact line emitted by
Envoy, not for a synthetic fake-harness message.

The metrics tests use two independent paths:

1. `GET /stats?filter=hello_requests_total` proves the counter exists in
   Envoy's native stats store and increases after a request.
2. A small OTLP/gRPC metrics service receives Envoy's OpenTelemetry stat sink
   exports and records exported metric names. This proves a jisr-defined metric
   can leave Envoy through an OTel sink.

The dynamic metadata test uses an Envoy access log configured with:

```text
%DYNAMIC_METADATA(jisr:filter)% %DYNAMIC_METADATA(jisr:route)%
```

The `hello` filter sets those values with `w.SetMetadata`, and the e2e test
asserts that Envoy's access log renders them.

## Port usage

Envoy listeners use fixed local ports `10000` through `10006`, and the Envoy
admin listener uses `9901`. The backend, reset backend, jisr/prof admin server,
and OTLP/gRPC metrics sink bind random loopback ports. If a fixed Envoy port is
already in use, stop the stale Envoy process and rerun `make e2e`.
