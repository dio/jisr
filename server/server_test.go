package server_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/dio/jisr/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbedded_ServesRequests(t *testing.T) {
	ctx, stop := server.Background(func(ctx context.Context) {
		<-ctx.Done() // keep alive until stopped
	})
	defer stop()

	srv, err := server.NewEmbedded(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello from embedded"))
	}))
	require.NoError(t, err)
	assert.Greater(t, srv.Port(), 0)

	resp, err := http.Get("http://" + srv.Addr() + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "hello from embedded", string(body))
}

func TestEmbedded_StopsOnContextCancel(t *testing.T) {
	ctx, stop := server.Background(func(ctx context.Context) {
		<-ctx.Done()
	})

	srv, err := server.NewEmbedded(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	require.NoError(t, err)

	addr := srv.Addr()

	// Confirm it's up.
	resp, err := http.Get("http://" + addr + "/")
	require.NoError(t, err)
	resp.Body.Close()

	// Cancel context → server should shut down.
	stop()
	time.Sleep(100 * time.Millisecond)

	// Should no longer accept connections.
	_, err = http.Get("http://" + addr + "/")
	assert.Error(t, err, "expected connection refused after shutdown")
}

func TestEmbedded_RandomPort(t *testing.T) {
	ctx, stop := server.Background(func(ctx context.Context) { <-ctx.Done() })
	defer stop()

	ports := make(map[int]bool)
	for i := 0; i < 5; i++ {
		srv, err := server.NewEmbedded(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		require.NoError(t, err)
		assert.False(t, ports[srv.Port()], "port collision: "+strconv.Itoa(srv.Port()))
		ports[srv.Port()] = true
	}
}

func TestBackground_CancelStopsGoroutine(t *testing.T) {
	done := make(chan struct{})
	ctx, stop := server.Background(func(ctx context.Context) {
		<-ctx.Done()
		close(done)
	})
	_ = ctx

	stop()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutine did not stop after cancel")
	}
}
