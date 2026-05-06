// Package example shows how to write an Envoy filter using jisr.
// This is user code — it imports jisr as a library and just writes handlers.
package example

import (
	"context"
	"encoding/json"
	"io"
	"log"

	"github.com/dio/jisr"
)

func init() {
	// Register the auth filter with logging middleware.
	jisr.Register("example-auth", jisr.Chain(
		authHandler,
		loggingMiddleware,
	))

	// A second filter in the same .so — just register another name.
	jisr.Register("example-echo", echoHandler)
}

// loggingMiddleware logs the request path before and after the handler.
func loggingMiddleware(next jisr.HandlerFunc) jisr.HandlerFunc {
	return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		path := r.Header.Get(":path")
		log.Printf("[%s] request: %s", r.FilterName, path)
		next(ctx, w, r)
		log.Printf("[%s] done: %s", r.FilterName, path)
	}
}

// authHandler checks for an x-api-key header. If missing, rejects with 401.
// If present, stamps x-user-id and lets the request through.
func authHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	apiKey := r.Header.Get("x-api-key")
	if apiKey == "" {
		w.SendError(401, `{"error":"missing api key"}`)
		return
	}

	// In a real filter you'd call an auth service here. For the example,
	// any non-empty key is accepted.
	w.SetRequestHeader("x-user-id", "user-from-"+apiKey)
	w.SetMetadata("example", "authenticated", true)
	// returning without SendError → request forwarded upstream
}

// echoHandler reads the full request body and reflects it back as a JSON response.
// Demonstrates io.ReadAll — blocking, but safe inside a jisr handler.
func echoHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.SendError(500, `{"error":"failed to read body"}`)
		return
	}

	response, _ := json.Marshal(map[string]any{
		"path":    r.Header.Get(":path"),
		"method":  r.Header.Get(":method"),
		"body":    string(body),
		"headers": r.Header,
	})

	w.SendErrorBytes(200, response) // SendErrorBytes with 200 = local response
}
