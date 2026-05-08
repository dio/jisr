# ws-proxy Observability

`ws-proxy` has two observability planes:

1. **Envoy edge plane**: jisr filter callbacks, Envoy access logs, dynamic
   metadata, and Envoy-native stats.
2. **Embedded actor plane**: the Go `http.Server` started by `RegisterRaw` and
   `jisr/server`, including WebSocket sessions, frame taps, local background
   goroutines, and any foreign process supervised by the module.

These planes can share names, dimensions, and the same OpenTelemetry backend,
but they should not share one API. `Request.LogAttrs`, `w.SetMetadata`,
`w.IncrementCounter`, and `w.RecordHistogram` are only valid while Envoy is
calling the HTTP filter. `WSProxy.ServeHTTP` is normal Go code running behind a
local Envoy cluster, so it should use ordinary Go observability APIs.

## What to Emit Where

Use Envoy/jisr for edge facts that Envoy owns:

- downstream method, path, route, authority, response code
- request rejection or mutation done in a jisr filter
- dynamic metadata for Envoy access logs
- counters and histograms that should live in Envoy's stats store

Use actor-side Go observability for facts the embedded process owns:

- WebSocket session start/end
- frame-tap outcomes such as model, provider, and token counts
- background actor lifecycle events
- actor-side latency histograms, error counters, and queue gauges
- foreign process stdout/stderr and health probes

Keep the dimensions consistent across both planes. Useful low-cardinality keys
for this example are `component`, `route`, `provider`, `model`, `result`, and
`close_code`. Avoid labels such as user IDs, full prompts, or full URLs.

## Current Example

The example emits structured actor logs from `WSProxy.ServeHTTP`. When
`otel_endpoint` is set, the same session-end path also uses
`github.com/dio/logging` to emit actor-side OpenTelemetry metrics:

```text
level=INFO msg="ws-proxy: session ended" path=/v1/responses model=gpt-4o-mini input_tokens=21 output_tokens=8 duration=42ms reason="client read: ..."
```

```yaml
filter_config:
  "@type": type.googleapis.com/google.protobuf.StringValue
  value: '{"listen_addr":"127.0.0.1:10001","upstream_url":"ws://127.0.0.1:18080","auth_value":"","otel_endpoint":"127.0.0.1:4317"}'
```

The e2e suite verifies this path with a local WebSocket upstream:

```sh
make e2e
```

`TestWSProxy_EmbeddedActor_ProxiesToLocalUpstream` starts Envoy, starts a mock
WebSocket upstream, routes a WebSocket upgrade through the embedded actor, and
asserts the session log plus `ws_proxy_sessions_total` and
`ws_proxy_session_duration_ms` exports. It never calls OpenAI.

## Envoy OTel Stat Sink

For edge metrics recorded through jisr, configure Envoy's OpenTelemetry stat
sink and point it at an OTLP/gRPC receiver:

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

The repository e2e suite has an in-process OTLP/gRPC metrics service that
validates this sink for a counter and a histogram.

## Actor Metrics with github.com/dio/logging

For embedded actors and supervised foreign processes, prefer a regular Go
observability stack. `github.com/dio/logging` is used by this example because
one actor-side call can emit both a structured slog record and an OpenTelemetry
metric.

```go
package wsobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/dio/logging"
	"github.com/tetratelabs/telemetry"
	"github.com/tetratelabs/telemetry/scope"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/sdk/metric"
)

var (
	modelLabel     telemetry.Label
	resultLabel    telemetry.Label
	sessionTotal   telemetry.Metric
	sessionLatency telemetry.Metric
	logger         = scope.Register("ws-proxy", "embedded WebSocket proxy")
)

func init() {
	telemetry.ToGlobalMetricSink(func(ms telemetry.MetricSink) {
		modelLabel = ms.NewLabel("model")
		resultLabel = ms.NewLabel("result")
		sessionTotal = ms.NewSum("ws_proxy_sessions_total", "WebSocket sessions")
		sessionLatency = ms.NewDistribution(
			"ws_proxy_session_duration_ms",
			"WebSocket session duration",
			[]float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000},
		)
	})
}

func WireOTelMetrics(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	exp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	reader := metric.NewPeriodicReader(exp, metric.WithInterval(2*time.Second))
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	sink := logging.NewOTelSink(mp, "ws-proxy")

	telemetry.SetGlobalMetricSink(sink)
	scope.UseLogger(logging.New(slog.Default()))

	return func(ctx context.Context) error {
		_ = sink.Shutdown(ctx)
		return mp.Shutdown(ctx)
	}, nil
}

func RecordSession(ctx context.Context, model, result string, elapsed time.Duration) {
	labels := []telemetry.LabelValue{
		modelLabel.Upsert(model),
		resultLabel.Upsert(result),
	}

	logger.Context(ctx).
		Metric(sessionTotal.With(labels...)).
		Info("ws-proxy session ended", "model", model, "result", result)

	sessionLatency.With(labels...).Record(float64(elapsed.Milliseconds()))
}
```

This keeps actor metrics independent from Envoy worker-thread timing, while the
sink can still be the same OpenTelemetry Collector or `otel-front` endpoint as
Envoy's stat sink.

## Local Collector

`otel-collector.yaml` is a minimal local collector config with OTLP/gRPC on
`4317`, OTLP/HTTP on `4318`, and debug exporters for logs, metrics, and traces.
Run it with:

```sh
docker run --rm -p 4317:4317 -p 4318:4318 \
  -v "$(pwd)/otel-collector.yaml:/etc/otelcol/config.yaml:ro" \
  otel/opentelemetry-collector-contrib:latest \
  --config=/etc/otelcol/config.yaml
```

If you use `otel-front`, point both Envoy and actor-side OTLP exporters at its
OTLP/gRPC endpoint, usually `127.0.0.1:4317`.
