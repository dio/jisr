package boehello

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/dio/jisr"
)

// Metrics defined once at config time, incremented per-request.
var (
	requestsTotal jisr.MetricID
	echoTotal     jisr.MetricID
)

func init() {
	jisr.RegisterWithConfig("hello",
		func(h jisr.ConfigHandle) error {
			var err error
			requestsTotal, err = h.DefineCounter("hello_requests_total")
			return err
		},
		jisr.Chain(helloHandler, logMiddleware),
	)

	jisr.RegisterWithConfig("hello-echo",
		func(h jisr.ConfigHandle) error {
			var err error
			echoTotal, err = h.DefineCounter("hello_echo_total")
			return err
		},
		echoHandler,
	)

	// Response-phase filters.
	jisr.RegisterWithResponse("resp-stamp", skipBodyHandler, respStampHandler, jisr.ResponseModePassthrough)
	jisr.RegisterWithResponse("resp-tap", skipBodyHandler, respTapHandler, jisr.ResponseModeObserve)
	jisr.RegisterWithResponse("resp-rewrite", skipBodyHandler, respRewriteHandler, jisr.ResponseModeBuffer)
	jisr.RegisterWithResponse("resp-header-stamp", skipBodyHandler, respHeaderStampHandler, jisr.ResponseModeBuffer)
}

func logMiddleware(next jisr.HandlerFunc) jisr.HandlerFunc {
	return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		// Use GetAttr for path and method: snapshotted on the worker thread,
		// more direct than parsing pseudo-headers.
		method := r.GetAttr(jisr.AttrRequestMethod)
		path := r.GetAttr(jisr.AttrRequestPath)
		r.Log(jisr.LogInfo, "[%s] %s %s", r.FilterName, method, path)
		next(ctx, w, r)
	}
}

// helloHandler injects x-hello and forwards the request upstream.
// SkipBody is called because this filter only touches headers.
func helloHandler(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	r.SkipBody()
	w.SetRequestHeader("x-hello", "from-jisr")
	w.IncrementCounter(requestsTotal, 1)
}

// skipBodyHandler is the request phase for response-phase filters.
// They only care about the response; skip the request body.
// Also injects x-jisr-filter to prove the request phase ran.
func skipBodyHandler(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	r.SkipBody()
	w.SetRequestHeader("x-jisr-filter", r.FilterName)
}

// respStampHandler inspects the upstream response status in Passthrough mode.
// ResponseModePassthrough: zero body overhead, r.Body is nil.
func respStampHandler(_ context.Context, _ jisr.ResponseWriter, r *jisr.Response) {
	_ = r.StatusCode
	_ = r.Header.Get("content-type")
}

// respTapHandler counts upstream response body bytes in Observe mode.
// ResponseModeObserve: body streams to downstream simultaneously.
func respTapHandler(_ context.Context, _ jisr.ResponseWriter, r *jisr.Response) {
	// io.Copy returns when the body pipe is closed (eos) or ctx is cancelled
	// (client/upstream disconnect). Both cases exit cleanly.
	io.Copy(io.Discard, r.Body) //nolint:errcheck
}

// respRewriteHandler rewrites the upstream JSON response body in Buffer mode.
// The full upstream body is accumulated before the client receives anything.
// We parse it, inject a "processed" field, fix content-length, then replace.
func respRewriteHandler(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		return
	}

	// Parse upstream JSON and inject a new field.
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return // not JSON — leave body untouched (ReplaceBody not called)
	}
	obj["x_jisr_rewritten"] = true

	rewritten, err := json.Marshal(obj)
	if err != nil {
		return
	}

	w.SetUpstreamResponseHeader("content-length", strconv.Itoa(len(rewritten)))
	w.SetUpstreamResponseHeader("x-jisr-rewritten", "1")
	w.ReplaceBody(rewritten)
}

// respHeaderStampHandler adds x-jisr-stamp to the upstream response headers
// using Buffer mode. Draining the body keeps Envoy's buffered response intact
// while ensuring header mutation happens before the response is continued.
func respHeaderStampHandler(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
	io.Copy(io.Discard, r.Body) //nolint:errcheck
	w.SetUpstreamResponseHeader("x-jisr-stamp", strconv.Itoa(r.StatusCode))
}
func echoHandler(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		r.Log(jisr.LogError, "echoHandler: read body: %v", err)
		w.Send(http.StatusInternalServerError, `{"error":"failed to read body"}`)
		return
	}

	flat := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		flat[k] = v[0]
	}

	resp, _ := json.Marshal(map[string]any{
		"path":    r.GetAttr(jisr.AttrRequestPath),
		"method":  r.GetAttr(jisr.AttrRequestMethod),
		"body":    string(body),
		"headers": flat,
	})
	w.SetResponseHeader("content-type", "application/json")
	w.SendBytes(http.StatusOK, resp)
	w.IncrementCounter(echoTotal, 1)
}
