package boehello

import (
	"context"
	"log"

	"github.com/dio/jisr"
)

func init() {
	jisr.Register("hello", jisr.Chain(helloHandler, logMiddleware))
}

func logMiddleware(next jisr.HandlerFunc) jisr.HandlerFunc {
	return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		log.Printf("hello: %s %s", r.Header.Get(":method"), r.Header.Get(":path"))
		next(ctx, w, r)
	}
}

func helloHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	w.SetRequestHeader("x-hello", "from-jisr")
}
