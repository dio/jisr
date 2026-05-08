# hello

The canonical jisr example. Six filters in one `.so` covering all three
response modes and the most common filter patterns.

## Filters

| Filter name | Mode | What it does |
|-------------|------|--------------|
| `hello` | request-only | Injects `x-hello: from-jisr` into every upstream request. Increments `hello_requests_total` counter |
| `hello-echo` | request-only | Responds directly to the client with a JSON echo of path, method, headers, and body. No upstream involved |
| `resp-stamp` | Passthrough | Runs on the response path. Inspects `r.StatusCode` and `r.Header` without touching the body |
| `resp-tap` | Observe | Consumes the response body as it streams to the client simultaneously. Zero added latency |
| `resp-rewrite` | Buffer | Reads the full JSON response body, injects `x_jisr_rewritten: true`, updates `content-length`, replaces body |
| `resp-header-stamp` | Buffer | Drains the buffered response body and adds `x-jisr-stamp: <status>` to the upstream response headers |

## Build

```sh
make
# or:
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libhello.so ./cmd
```

## Run

```sh
# start a backend on port 8080
python3 -m http.server 8080

# start Envoy
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(pwd) envoy -c envoy.yaml

# hello filter (port 10000): injects x-hello header
curl -v http://localhost:10000/

# hello-echo (port 10001): direct JSON response, no upstream
curl -X POST http://localhost:10001/ping \
  -H "content-type: application/json" \
  -H "x-request-id: abc123" \
  -d '{"hello":"world"}'
```

## Metrics and dynamic metadata

The `hello` filter records an Envoy-native counter on every forwarded request
and a histogram observation for handler duration:

```go
w.IncrementCounter(requestsTotal, 1)
w.RecordHistogram(requestHandlerDurationMs, elapsedMs, "hello")
```

Query it through Envoy admin:

```sh
curl -s 'http://localhost:9901/stats?filter=hello_requests_total'
curl -s 'http://localhost:9901/stats?filter=hello_request_handler_duration_ms'
```

The filter also sets dynamic metadata:

```go
w.SetMetadata("jisr", "filter", r.FilterName)
w.SetMetadata("jisr", "route", "hello")
```

The example Envoy config renders those values in the access log:

```text
access dynamic_metadata_filter=hello dynamic_metadata_route=hello
```

To send the same `hello_requests_total` counter and
`hello_request_handler_duration_ms` histogram to an OpenTelemetry sink, add an
Envoy OpenTelemetry stat sink and point it at an OTLP/gRPC receiver. For local
testing, run an OpenTelemetry Collector or `otel-front`, then add:

```yaml
stats_flush_interval: 1s
stats_sinks:
  - name: envoy.stat_sinks.open_telemetry
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.stat_sinks.open_telemetry.v3.SinkConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel_collector
      report_counters_as_deltas: true
      emit_tags_as_attributes: true

static_resources:
  clusters:
    - name: otel_collector
      type: STRICT_DNS
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: otel_collector
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: 4317 }
```

With `otel-front`, open the local UI and search metrics for
`hello_requests_total` and `hello_request_handler_duration_ms` after sending a
request through Envoy.

## Admin server

On startup, the `.so` binds a pprof/admin server on a random loopback port
and logs the address:

```
hello: admin server listening on 127.0.0.1:XXXXX
```

Endpoints: `/healthz`, `/readyz`, `/version`, `/debug/pprof/*`

Set `JISR_ADMIN_PORT_FILE=/tmp/port` before starting Envoy to write the port
to a file (used by the e2e test harness).

## What this demonstrates

- `RegisterWithConfig` for defining Envoy counters at `.so` load time
- `w.RecordHistogram` for latency-style distribution metrics
- Envoy OpenTelemetry stat sink export for jisr-defined counters
- `w.SetMetadata` with Envoy access-log `%DYNAMIC_METADATA(...)%`
- `RegisterWithResponse` with all three modes (Passthrough / Observe / Buffer)
- `r.GetAttr(jisr.AttrRequestPath)` for pre-snapshotted stream attributes
- `r.SkipBody()` on both `Request` and `Response`
- `w.ReplaceBody` + `w.SetUpstreamResponseHeader` for response rewriting
- `jisr/prof` admin server wired via `init()`
