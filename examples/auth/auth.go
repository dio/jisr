// Package auth demonstrates the RegisterFactory pattern for struct-based filters.
//
// Why RegisterFactory instead of RegisterWithConfig?
//
// RegisterWithConfig stores metric IDs and parsed config in package-level vars.
// That works fine when there is exactly one Envoy listener using this filter.
// But if two listeners use the same filter name with different filter_config
// bytes (e.g. different allowed key sets per route), RegisterWithConfig calls
// the ConfigFunc twice — the second call overwrites the package vars.
//
// RegisterFactory constructs a new *AuthFilter per Create call. Each listener
// gets its own instance with its own config and metric IDs. No shared state,
// no race, no package vars.
//
// # Filter behaviour
//
// Request phase:
//   - Reads x-api-key header
//   - Rejects with 401 if the key is not in the configured allow-list
//   - Injects x-user-id: <key> for accepted requests
//   - Increments auth_requests_total{result=allowed|rejected}
//
// Response phase (Passthrough: zero body overhead):
//   - Records the upstream status code in filter metadata
//   - Increments auth_responses_total{status=2xx|4xx|5xx}
//
// # Config
//
// Provide filter_config in envoy.yaml as a JSON object:
//
//	filter_config:
//	  "@type": type.googleapis.com/google.protobuf.StringValue
//	  value: |
//	    {"allowed_keys":["key-admin","key-readonly"],"metadata_ns":"auth"}
//
// Or pass raw JSON bytes via any typed config mechanism.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/dio/jisr"
)

// Config holds the per-listener auth configuration.
type Config struct {
	AllowedKeys []string `json:"allowed_keys"`
	MetadataNS  string   `json:"metadata_ns"`
}

// AuthFilter holds per-instance state: parsed config + metric IDs.
// One instance is created per Envoy listener that uses this filter.
type AuthFilter struct {
	cfg      *Config
	allowed  map[string]struct{} // fast lookup set
	reqTotal jisr.MetricID       // auth_requests_total{result}
	respTotal jisr.MetricID      // auth_responses_total{status}
}

func init() {
	jisr.RegisterFactoryWithResponse("auth",
		func(h jisr.ConfigHandle) (jisr.HandlerFunc, jisr.ResponseFunc, error) {
			raw := h.RawConfig()
			cfg := &Config{MetadataNS: "auth"} // defaults
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, cfg); err != nil {
					return nil, nil, fmt.Errorf("auth: bad config: %w", err)
				}
			}

			reqTotal, err := h.DefineCounter("auth_requests_total", "result")
			if err != nil {
				return nil, nil, err
			}
			respTotal, err := h.DefineCounter("auth_responses_total", "status")
			if err != nil {
				return nil, nil, err
			}

			allowed := make(map[string]struct{}, len(cfg.AllowedKeys))
			for _, k := range cfg.AllowedKeys {
				allowed[k] = struct{}{}
			}

			f := &AuthFilter{
				cfg:       cfg,
				allowed:   allowed,
				reqTotal:  reqTotal,
				respTotal: respTotal,
			}

			// Chain composes middleware in declaration order.
			// loggingMiddleware runs first, then f.HandleRequest.
			// If HandleRequest short-circuits (401), loggingMiddleware still
			// logs the outcome because it wraps the full call.
			return jisr.Chain(f.HandleRequest, loggingMiddleware),
				jisr.ResponseChain(f.HandleResponse, responseLoggingMiddleware),
				nil
		},
		jisr.ResponseModePassthrough,
	)
}

// loggingMiddleware logs every request with its path and method.
// Stateless — defined at package level, no per-config state needed.
func loggingMiddleware(next jisr.HandlerFunc) jisr.HandlerFunc {
	return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.Log(jisr.LogInfo, "auth: %s %s",
			r.GetAttr(jisr.AttrRequestMethod),
			r.GetAttr(jisr.AttrRequestPath),
		)
		next(ctx, w, r)
	}
}

// responseLoggingMiddleware demonstrates jisr.ResponseMiddleware — it wraps a
// ResponseFunc with before/after logic, running around the upstream response.
// The response phase runs in the same goroutine as the request phase.
func responseLoggingMiddleware(next jisr.ResponseFunc) jisr.ResponseFunc {
	return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
		next(ctx, w, r)
		// Post-response: set a metadata marker to show the middleware ran.
		// In production this could emit a metric, record latency, etc.
		w.SetMetadata("auth", "response_logged", true)
	}
}

// HandleRequest is the request phase. Validates x-api-key and either rejects
// or forwards the request with x-user-id injected.
func (f *AuthFilter) HandleRequest(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	r.SkipBody()

	key := r.Header.Get("X-Api-Key")
	if _, ok := f.allowed[key]; !ok {
		w.IncrementCounter(f.reqTotal, 1, "rejected")
		w.SetMetadata(f.cfg.MetadataNS, "result", "rejected")
		w.SetResponseHeader("content-type", "application/json")
		w.Send(http.StatusUnauthorized, `{"error":"invalid or missing api key"}`)
		return
	}

	w.IncrementCounter(f.reqTotal, 1, "allowed")
	w.SetMetadata(f.cfg.MetadataNS, "result", "allowed")
	w.SetMetadata(f.cfg.MetadataNS, "key", key)
	w.SetRequestHeader("x-user-id", key)
}

// HandleResponse is the response phase (Passthrough: headers only, zero body overhead).
// Records upstream status in metadata and increments the response counter.
func (f *AuthFilter) HandleResponse(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
	bucket := statusBucket(r.StatusCode)
	w.IncrementCounter(f.respTotal, 1, bucket)
	w.SetMetadata(f.cfg.MetadataNS, "upstream_status", r.StatusCode)
}

// statusBucket coarsens an HTTP status code into 2xx / 4xx / 5xx.
func statusBucket(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	default:
		return "2xx"
	}
}
