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
