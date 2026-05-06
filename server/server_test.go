package server_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dio/jisr/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── server.New (single-server convenience) ────────────────────────────────────

func TestNew_ServesRequests(t *testing.T) {
	srv, stop, err := server.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "hello")
	}), 0)
	require.NoError(t, err)
	defer stop()

	assert.Greater(t, srv.Port(), 0)

	resp, err := http.Get("http://" + srv.Addr() + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "hello", string(body))
}

func TestNew_StopsOnStopCall(t *testing.T) {
	srv, stop, err := server.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), 100*time.Millisecond)
	require.NoError(t, err)

	addr := srv.Addr()
	resp, err := http.Get("http://" + addr + "/")
	require.NoError(t, err)
	resp.Body.Close()

	stop()
	time.Sleep(50 * time.Millisecond)

	_, err = http.Get("http://" + addr + "/")
	assert.Error(t, err, "expected connection refused after stop")
}

func TestNew_RandomPort(t *testing.T) {
	ports := make(map[int]bool)
	var stops []func()
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()

	for i := 0; i < 5; i++ {
		srv, stop, err := server.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), 0)
		require.NoError(t, err)
		stops = append(stops, stop)
		assert.False(t, ports[srv.Port()], "port collision: "+strconv.Itoa(srv.Port()))
		ports[srv.Port()] = true
	}
}

// ── Group ─────────────────────────────────────────────────────────────────────

func TestGroup_MultipleHTTPServers(t *testing.T) {
	g := server.NewGroup()

	srv1, err := g.AddHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "server1")
	}), 0)
	require.NoError(t, err)

	srv2, err := g.AddHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "server2")
	}), 0)
	require.NoError(t, err)

	g.Start()
	defer g.Stop()

	// Both servers are up simultaneously.
	for _, tc := range []struct {
		addr string
		want string
	}{
		{srv1.Addr(), "server1"},
		{srv2.Addr(), "server2"},
	} {
		resp, err := http.Get("http://" + tc.addr + "/")
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, tc.want, string(body))
	}
}

func TestGroup_StopStopsAllActors(t *testing.T) {
	g := server.NewGroup()

	srv, err := g.AddHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), 100*time.Millisecond)
	require.NoError(t, err)

	var goroutineDone atomic.Bool
	g.AddGoroutine(func(ctx context.Context) {
		<-ctx.Done()
		goroutineDone.Store(true)
	})

	g.Start()
	addr := srv.Addr()

	// Confirm both are up.
	resp, err := http.Get("http://" + addr + "/")
	require.NoError(t, err)
	resp.Body.Close()

	g.Stop()
	time.Sleep(50 * time.Millisecond)

	// HTTP server is down.
	_, err = http.Get("http://" + addr + "/")
	assert.Error(t, err)

	// Goroutine saw ctx.Done().
	assert.True(t, goroutineDone.Load())
}

func TestGroup_AddGoroutine_ReceivesContext(t *testing.T) {
	g := server.NewGroup()

	done := make(chan struct{})
	g.AddGoroutine(func(ctx context.Context) {
		<-ctx.Done()
		close(done)
	})

	g.Start()
	g.Stop()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutine did not receive ctx.Done() after Stop")
	}
}

func TestGroup_StopIdempotent(t *testing.T) {
	g := server.NewGroup()
	g.AddGoroutine(func(ctx context.Context) { <-ctx.Done() })
	g.Start()

	assert.NotPanics(t, func() {
		g.Stop()
		g.Stop()
		g.Stop()
	})
}

func TestGroup_ActorPanic_DoesNotCrash(t *testing.T) {
	g := server.NewGroup()

	panicked := make(chan struct{})
	g.Add(
		func() error {
			close(panicked)
			panic("intentional test panic")
		},
		func() {},
	)

	// A second goroutine so the group has something to stop.
	stopped := make(chan struct{})
	g.Add(
		func() error { <-stopped; return nil },
		func() { close(stopped) },
	)

	assert.NotPanics(t, func() {
		g.Start()
		<-panicked
		time.Sleep(20 * time.Millisecond) // let the watcher trigger Stop
	})
}

func TestGroup_AddrAndPort_ConsistentBeforeStart(t *testing.T) {
	// Addr() is valid immediately after AddHTTP — before Start().
	g := server.NewGroup()
	srv, err := g.AddHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), 0)
	require.NoError(t, err)

	assert.Greater(t, srv.Port(), 0)
	assert.Contains(t, srv.Addr(), strconv.Itoa(srv.Port()))

	g.Start()
	g.Stop()
}
