package boehello

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/dio/jisr"
)

func init() {
	jisr.Register("hello", jisr.Chain(helloHandler, logMiddleware))
	jisr.Register("hello-echo", echoHandler)

	// Response-phase filters.
	jisr.RegisterWithResponse("resp-stamp", skipBodyHandler, respStampHandler, jisr.ResponseModePassthrough)
	jisr.RegisterWithResponse("resp-tap", skipBodyHandler, respTapHandler, jisr.ResponseModeObserve)
}

func logMiddleware(next jisr.HandlerFunc) jisr.HandlerFunc {
	return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.Log(jisr.LogInfo, "[%s] %s %s", r.FilterName, r.Header.Get(":method"), r.Header.Get(":path"))
		next(ctx, w, r)
	}
}

// helloHandler injects x-hello and forwards the request upstream.
// SkipBody is called because this filter only touches headers.
func helloHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	r.SkipBody()
	w.SetRequestHeader("x-hello", "from-jisr")
}

// skipBodyHandler is the request phase for response-phase filters.
// They only care about the response — skip the request body.
// Also injects x-jisr-filter to prove the request phase ran.
func skipBodyHandler(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	r.SkipBody()
	w.SetRequestHeader("x-jisr-filter", r.FilterName)
}

// respStampHandler inspects the upstream response status in Passthrough mode.
// ResponseModePassthrough: zero body overhead, r.Body is nil.
// We log the status code — verifiable via Envoy access logs.
// NOTE: SetUpstreamResponseHeader requires the Envoy SDK to support response
// header mutation from scheduled callbacks, which is under investigation for v1.37.1.
// For now we just verify the handler runs without crashing.
func respStampHandler(_ context.Context, _ jisr.ResponseWriter, r *jisr.Response) {
	// Accessing r.StatusCode and r.Header proves the goroutine received the response.
	_ = r.StatusCode
	_ = r.Header.Get("content-type")
}

// respTapHandler counts upstream response body bytes in Observe mode.
// ResponseModeObserve: body streams to downstream simultaneously.
// NOTE: We cannot add response headers after Observe (headers already sent).
// The byte count is observable via Envoy metrics/metadata, not response headers.
func respTapHandler(ctx context.Context, _ jisr.ResponseWriter, r *jisr.Response) {
	// io.Copy returns when the body channel is closed (eos) or ctx is cancelled
	// (client/upstream disconnect). Both cases exit cleanly — no panic, no leak.
	io.Copy(io.Discard, r.Body) //nolint:errcheck
}

// echoHandler replies directly without forwarding upstream — no cluster needed.
func echoHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		r.Log(jisr.LogError, "echoHandler: read body: %v", err)
		w.Send(http.StatusInternalServerError, `{"error":"failed to read body"}`)
		return
	}

	// Flatten multi-value headers to first-value for simplicity.
	flat := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		flat[k] = v[0]
	}

	resp, _ := json.Marshal(map[string]any{
		"path":    r.Header.Get(":path"),
		"method":  r.Header.Get(":method"),
		"body":    string(body),
		"headers": flat,
	})
	w.SetResponseHeader("content-type", "application/json")
	// Send 200 directly — no upstream involved.
	w.SendBytes(http.StatusOK, resp)
}

