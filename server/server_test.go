package server_test

import (
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/dio/jisr/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServer_ServesRequests(t *testing.T) {
	srv, stop, err := server.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello from embedded"))
	}), 0)
	require.NoError(t, err)
	defer stop()

	assert.Greater(t, srv.Port(), 0)

	resp, err := http.Get("http://" + srv.Addr() + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "hello from embedded", string(body))
}

func TestServer_StopsOnStopCall(t *testing.T) {
	srv, stop, err := server.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), 100*time.Millisecond)
	require.NoError(t, err)

	addr := srv.Addr()

	// Confirm it's up.
	resp, err := http.Get("http://" + addr + "/")
	require.NoError(t, err)
	resp.Body.Close()

	// Stop → graceful shutdown.
	stop()
	time.Sleep(50 * time.Millisecond)

	// Should no longer accept connections.
	_, err = http.Get("http://" + addr + "/")
	assert.Error(t, err, "expected connection refused after shutdown")
}

func TestServer_RandomPort(t *testing.T) {
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

func TestServer_AddrAndPort_Consistent(t *testing.T) {
	srv, stop, err := server.New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), 0)
	require.NoError(t, err)
	defer stop()

	assert.Equal(t, "127.0.0.1", srv.Addr()[:9])
	assert.Contains(t, srv.Addr(), strconv.Itoa(srv.Port()))
}
