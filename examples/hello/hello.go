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
}

func logMiddleware(next jisr.HandlerFunc) jisr.HandlerFunc {
	return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.Log(jisr.LogInfo, "[%s] %s %s", r.FilterName, r.Header.Get(":method"), r.Header.Get(":path"))
		next(ctx, w, r)
	}
}

func helloHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	w.SetRequestHeader("x-hello", "from-jisr")
}

// echoHandler reads the full request body and reflects it back as JSON.
func echoHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		r.Log(jisr.LogError, "echoHandler: failed to read body: %v", err)
		w.SendErrorBytes(http.StatusInternalServerError, jsonErr("failed to read body"))
		return
	}

	resp, _ := json.Marshal(map[string]any{
		"path":    r.Header.Get(":path"),
		"method":  r.Header.Get(":method"),
		"body":    string(body),
		"headers": r.Header,
	})
	w.SendErrorBytes(http.StatusOK, resp)
}

func jsonErr(msg string) []byte {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return b
}
