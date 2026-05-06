package multiactor_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dio/jisr/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise each actor type directly — no Envoy required.
// They prove the point: Group is a lifecycle manager, not an HTTP library.

// ── Actor 1: HTTP server ──────────────────────────────────────────────────────

func TestGroup_HTTPActor_ServesRequests(t *testing.T) {
	g := server.NewGroup()

	srv, err := g.AddHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","actors":3}`)
	}), 0)
	require.NoError(t, err)

	g.Start()
	defer g.Stop()

	resp, err := http.Get("http://" + srv.Addr() + "/status")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), `"actors":3`)
}

// ── Actor 2: background goroutine ────────────────────────────────────────────

func TestGroup_GoroutineActor_RunsAndStops(t *testing.T) {
	g := server.NewGroup()

	var count atomic.Int64
	ready := make(chan struct{})

	g.AddGoroutine(func(ctx context.Context) {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		close(ready) // signal: goroutine is running
		for {
			select {
			case <-ctx.Done():
				return // context cancelled by g.Stop()
			case <-ticker.C:
				count.Add(1)
			}
		}
	})

	g.Start()
	<-ready // wait for goroutine to start

	time.Sleep(30 * time.Millisecond) // let it tick a few times
	before := count.Load()
	assert.Greater(t, before, int64(0), "goroutine should have ticked")

	g.Stop()
	time.Sleep(10 * time.Millisecond)

	after := count.Load()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, after, count.Load(), "goroutine must stop after g.Stop()")
}

// ── Actor 3: raw net.Listener ─────────────────────────────────────────────────

func TestGroup_RawListenerActor_PingPong(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := ln.Addr().String()

	g := server.NewGroup()

	// Register a raw actor — no http.Handler at all.
	g.Add(
		func() error {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return nil // ln.Close() called
				}
				go func(c net.Conn) {
					defer c.Close()
					c.SetDeadline(time.Now().Add(time.Second))
					buf := make([]byte, 5)
					if _, err := io.ReadFull(c, buf); err != nil {
						return
					}
					if string(buf) == "ping\n" {
						io.WriteString(c, "pong\n")
					}
				}(conn)
			}
		},
		func() { ln.Close() },
	)

	g.Start()
	defer g.Stop()

	// Connect and send "ping\n", expect "pong\n".
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(time.Second))
	_, err = io.WriteString(conn, "ping\n")
	require.NoError(t, err)

	got := make([]byte, 5)
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	assert.Equal(t, "pong\n", string(got))
}

// ── All three together: prove Group stops everything atomically ───────────────

func TestGroup_AllActors_StopTogether(t *testing.T) {
	g := server.NewGroup()

	// Actor 1: HTTP
	httpSrv, err := g.AddHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), 100*time.Millisecond)
	require.NoError(t, err)

	// Actor 2: background goroutine
	goroutineStopped := make(chan struct{})
	g.AddGoroutine(func(ctx context.Context) {
		<-ctx.Done()
		close(goroutineStopped)
	})

	// Actor 3: raw listener
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	listenerStopped := make(chan struct{})
	g.Add(
		func() error {
			for {
				conn, err := ln.Accept()
				if err != nil {
					close(listenerStopped)
					return nil
				}
				conn.Close()
			}
		},
		func() { ln.Close() },
	)

	g.Start()

	// All three are running — verify HTTP is up.
	resp, err := http.Get("http://" + httpSrv.Addr() + "/")
	require.NoError(t, err)
	resp.Body.Close()

	// One call stops all three.
	g.Stop()

	select {
	case <-goroutineStopped:
	case <-time.After(time.Second):
		t.Fatal("goroutine actor did not stop")
	}

	select {
	case <-listenerStopped:
	case <-time.After(time.Second):
		t.Fatal("raw listener actor did not stop")
	}

	// HTTP server is down.
	_, err = http.Get("http://" + httpSrv.Addr() + "/")
	assert.Error(t, err, "HTTP actor must be stopped")
}

// ── Prove the actors are independent: raw listener doesn't speak HTTP ─────────

func TestRawActor_DoesNotSpeakHTTP(t *testing.T) {
	// This test makes the conceptual point explicit:
	// the raw actor uses its own wire protocol, completely independent of HTTP.
	// Group doesn't care — it just manages start/stop.

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	g := server.NewGroup()
	g.Add(
		func() error {
			conn, err := ln.Accept()
			if err != nil {
				return nil
			}
			defer conn.Close()
			// Custom protocol: echo back 3 bytes reversed.
			buf := make([]byte, 3)
			io.ReadFull(conn, buf)
			for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
				buf[i], buf[j] = buf[j], buf[i]
			}
			conn.Write(buf)
			return nil // actor done after one connection
		},
		func() { ln.Close() },
	)
	g.Start()
	defer g.Stop()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer conn.Close()

	conn.Write([]byte("abc"))
	got := make([]byte, 3)
	io.ReadFull(conn, got)
	assert.Equal(t, "cba", string(got))

	// Sending an HTTP request to this port would fail — it's not HTTP.
	_, err = http.Get("http://" + ln.Addr().String() + "/")
	assert.Error(t, err, "raw actor does not speak HTTP — connection should be mangled")
}

// TestGroup_gRPC_Cleartext_Pattern documents the correct way to run a cleartext
// gRPC server inside a Group. grpc.Server.ServeHTTP requires HTTP/2+TLS and
// is experimental — for cleartext, use grpc.Serve(ln) via g.Add directly.
//
// This test simulates that pattern without importing google.golang.org/grpc.
func TestGroup_gRPC_Cleartext_Pattern(t *testing.T) {
	// Simulate grpc.Server with a raw net.Listener — the real pattern is:
	//
	//   grpcSrv := grpc.NewServer()
	//   mypb.RegisterMyServiceServer(grpcSrv, &myImpl{})
	//   ln, _ := net.Listen("tcp", "127.0.0.1:0")
	//   g.Add(
	//       func() error { return grpcSrv.Serve(ln) },
	//       func() { grpcSrv.Stop() },
	//   )
	//
	// grpcSrv.Serve(ln) runs its own HTTP/2 transport — it does NOT use
	// net/http or http.Handler. Hence g.Add, not g.AddHTTP.

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()

	g := server.NewGroup()
	stopped := make(chan struct{})
	g.Add(
		func() error {
			// Simulates grpcSrv.Serve(ln) — blocks on Accept, owns its own transport.
			for {
				conn, err := ln.Accept()
				if err != nil {
					close(stopped)
					return nil // ln.Close() triggered by grpcSrv.Stop() equivalent
				}
				conn.Close() // in real gRPC: handle the connection
			}
		},
		func() { ln.Close() }, // equivalent of grpcSrv.Stop()
	)

	g.Start()

	// Listener is active.
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	conn.Close()

	g.Stop()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("gRPC-style actor did not stop")
	}
}



func TestGroup_HandlerIsJustHTTP_AllProtocols(t *testing.T) {
	// Documents the composition rule for HTTP-family protocols.
	//
	// WebSocket and h2c reduce cleanly to http.Handler.
	//
	// gRPC via ServeHTTP works ONLY with HTTP/2+TLS (experimental in grpc-go).
	// For cleartext gRPC (the common case without TLS), use grpc.Serve(ln)
	// with g.Add(execute, stop) — not AddHTTP.
	//
	// This test uses handler stubs to illustrate the shape, not real gRPC.

	protocols := []struct {
		name    string
		handler http.Handler
	}{
		{
			"http/1.1",
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "http1")
			}),
		},
		{
			// h2c wraps any http.Handler — AddHTTP handles it directly.
			"h2c-shape",
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "h2c")
			}),
		},
		{
			// grpc.Server.ServeHTTP requires HTTP/2+TLS — only usable with AddHTTP
			// when TLS is configured on the net/http server. For cleartext gRPC,
			// use g.Add(func() error { return grpcSrv.Serve(ln) }, grpcSrv.Stop).
			"grpc-via-ServeHTTP-TLS-only",
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					http.Error(w, "gRPC requires HTTP/2", http.StatusHTTPVersionNotSupported)
					return
				}
				fmt.Fprint(w, "grpc")
			}),
		},
	}

	g := server.NewGroup()
	var servers []*server.Server
	for _, p := range protocols {
		p := p
		srv, err := g.AddHTTP(p.handler, 0)
		require.NoError(t, err, p.name)
		servers = append(servers, srv)
	}
	g.Start()
	defer g.Stop()

	// All three bound to different random ports via the same AddHTTP call.
	for i, srv := range servers {
		assert.Greater(t, srv.Port(), 0, protocols[i].name)
	}
}
