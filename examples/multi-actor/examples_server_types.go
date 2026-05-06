// This file shows how to wire different server types into server.Group.
// It is documentation — not compiled in normal builds.
//
// Copy the relevant snippet into your filter factory's Create() method.

package multiactor

import (
	// Uncomment the imports you need:
	//
	// "golang.org/x/net/http2"
	// "golang.org/x/net/http2/h2c"
	// "google.golang.org/grpc"
	// connectrpc "connectrpc.com/connect"
	// "connectrpc.com/vanguard"
	_ "net/http"

	"github.com/dio/jisr/server"
)

// ExampleHTTP shows a plain HTTP/1.1 server — the baseline.
//
//	g := server.NewGroup()
//	ln, err := net.Listen("tcp", "127.0.0.1:0")
//	if err != nil { return nil, err }
//	srv := g.AddListener(ln, myMux, 5*time.Second)
//	// srv.Addr() → configure Envoy STATIC cluster
//
// Any net/http handler works: ServeMux, gorilla/mux, chi, echo, etc.
var _ = server.NewGroup // prevent unused import

// ExampleWebSocket shows a WebSocket server.
// WebSocket is HTTP/1.1 with an Upgrade header — still http.Handler.
//
//	import "github.com/coder/websocket"
//
//	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	    conn, err := websocket.Accept(w, r, nil)
//	    // ... pump frames
//	})
//
//	g := server.NewGroup()
//	ln, err := net.Listen("tcp", "127.0.0.1:0")
//	if err != nil { return nil, err }
//	srv := g.AddListener(ln, handler, 0)

// ExampleH2C shows HTTP/2 cleartext — needed for gRPC without TLS.
// h2c.NewHandler wraps any http.Handler; the listener stays plain TCP.
//
//	import (
//	    "golang.org/x/net/http2"
//	    "golang.org/x/net/http2/h2c"
//	)
//
//	g := server.NewGroup()
//	ln, err := net.Listen("tcp", "127.0.0.1:0")
//	if err != nil { return nil, err }
//	srv := g.AddListener(ln, h2c.NewHandler(myMux, &http2.Server{}), 30*time.Second)

// ExampleGRPC_Cleartext shows cleartext gRPC — the common case without TLS.
// grpc.Server manages its own HTTP/2 transport via Serve(ln).
// It does NOT go through net/http, so use g.Add, NOT g.AddListener.
//
//	import "google.golang.org/grpc"
//
//	grpcSrv := grpc.NewServer()
//	mypb.RegisterMyServiceServer(grpcSrv, &myImpl{})
//
//	ln, err := net.Listen("tcp", "127.0.0.1:0")
//	if err != nil { return nil, err }
//
//	g := server.NewGroup()
//	g.Add(
//	    func() error { return grpcSrv.Serve(ln) }, // blocks; owns its transport
//	    func() { grpcSrv.Stop() },                 // triggers graceful drain
//	)
//	// ln.Addr().String() → configure Envoy STATIC cluster

// ExampleGRPC_REST_H2C shows gRPC + REST on one port via vanguard + h2c.
// vanguard transcodes between gRPC, gRPC-Web, and HTTP/JSON on the same port.
//
//	import (
//	    "connectrpc.com/vanguard"
//	    "golang.org/x/net/http2"
//	    "golang.org/x/net/http2/h2c"
//	)
//
//	transcoder, err := vanguard.NewTranscoder([]*vanguard.Service{
//	    vanguard.NewService(mypbconnect.NewMyServiceHandler(&myImpl{})),
//	})
//
//	g := server.NewGroup()
//	ln, err := net.Listen("tcp", "127.0.0.1:0")
//	if err != nil { return nil, err }
//	srv := g.AddListener(ln, h2c.NewHandler(transcoder, &http2.Server{}), 30*time.Second)
//	// srv.Addr() → configure Envoy STATIC cluster (h2c-capable)

// ExampleBackgroundTask shows a periodic background goroutine.
// Stops automatically when g.Stop() is called.
//
//	g := server.NewGroup()
//	g.AddGoroutine(func(ctx context.Context) {
//	    ticker := time.NewTicker(30 * time.Second)
//	    defer ticker.Stop()
//	    for {
//	        select {
//	        case <-ctx.Done(): return
//	        case <-ticker.C: refreshCache()
//	        }
//	    }
//	})
