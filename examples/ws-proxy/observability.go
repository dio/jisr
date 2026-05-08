package wsproxy

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dio/logging"
	"github.com/tetratelabs/telemetry"
	"github.com/tetratelabs/telemetry/scope"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/sdk/metric"
)

var (
	actorObservabilityConfigured atomic.Bool

	wsModelLabel         telemetry.Label
	wsResultLabel        telemetry.Label
	wsProxySessionsTotal telemetry.Metric
	wsProxySessionMs     telemetry.Metric

	wsProxyLogger = scope.Register("ws-proxy", "embedded WebSocket proxy")
)

func init() {
	telemetry.ToGlobalMetricSink(func(ms telemetry.MetricSink) {
		wsModelLabel = ms.NewLabel("model")
		wsResultLabel = ms.NewLabel("result")
		wsProxySessionsTotal = ms.NewSum("ws_proxy_sessions_total", "WebSocket sessions handled by the embedded actor")
		wsProxySessionMs = ms.NewDistribution(
			"ws_proxy_session_duration_ms",
			"WebSocket session duration for the embedded actor",
			[]float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000},
		)
	})
}

type actorObservability struct {
	sink *logging.OTelSink
}

func newActorObservability(ctx context.Context, endpoint, rawInterval string) (*actorObservability, error) {
	interval := time.Second
	if rawInterval != "" {
		if parsed, err := time.ParseDuration(rawInterval); err == nil && parsed > 0 {
			interval = parsed
		}
	}

	exp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	reader := metric.NewPeriodicReader(exp, metric.WithInterval(interval))
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	sink := logging.NewOTelSink(mp, "ws-proxy")

	telemetry.SetGlobalMetricSink(sink)
	scope.UseLogger(logging.New(slog.Default()))
	actorObservabilityConfigured.Store(true)

	return &actorObservability{sink: sink}, nil
}

func (o *actorObservability) Shutdown(ctx context.Context) error {
	actorObservabilityConfigured.Store(false)
	if o == nil {
		return nil
	}
	return o.sink.Shutdown(ctx)
}

func recordActorSession(
	ctx context.Context,
	fallback *slog.Logger,
	path, model string,
	inputTokens, outputTokens uint32,
	elapsed time.Duration,
	reason error,
) {
	elapsed = elapsed.Round(time.Millisecond)
	attrs := []any{
		"path", path,
		"model", model,
		"input_tokens", inputTokens,
		"output_tokens", outputTokens,
		"duration", elapsed,
		"reason", reason,
	}

	if actorObservabilityConfigured.Load() && wsProxySessionsTotal != nil {
		result := sessionResult(reason)
		metric := wsProxySessionsTotal.With(
			wsModelLabel.Upsert(model),
			wsResultLabel.Upsert(result),
		)
		wsProxyLogger.Context(ctx).Metric(metric).Info("ws-proxy: session ended", attrs...)

		if wsProxySessionMs != nil {
			ms := float64(elapsed.Milliseconds())
			if ms == 0 {
				ms = 1
			}
			wsProxySessionMs.With(
				wsModelLabel.Upsert(model),
				wsResultLabel.Upsert(result),
			).RecordContext(ctx, ms)
		}
		return
	}

	fallback.Info("ws-proxy: session ended", attrs...)
}

func sessionResult(err error) string {
	if err == nil {
		return "ok"
	}
	msg := err.Error()
	if strings.Contains(msg, "EOF") || strings.Contains(msg, "closed") {
		return "closed"
	}
	return "error"
}
