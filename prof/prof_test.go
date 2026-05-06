package prof_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/dio/jisr/prof"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStart_ServesDebugPprof(t *testing.T) {
	srv, stop, err := prof.Start("")
	require.NoError(t, err)
	defer stop()

	assert.Greater(t, srv.Port(), 0)

	url := fmt.Sprintf("http://%s/debug/pprof/", srv.Addr())
	// Retry briefly — server starts in background goroutine.
	var resp *http.Response
	require.Eventually(t, func() bool {
		resp, err = http.Get(url)
		return err == nil
	}, time.Second, 10*time.Millisecond)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "goroutine", "pprof index must list goroutine profile")
	assert.Contains(t, string(body), "heap", "pprof index must list heap profile")
}

func TestStart_GoroutineEndpoint(t *testing.T) {
	srv, stop, err := prof.Start("")
	require.NoError(t, err)
	defer stop()

	url := fmt.Sprintf("http://%s/debug/pprof/goroutine?debug=1", srv.Addr())
	var resp *http.Response
	require.Eventually(t, func() bool {
		resp, err = http.Get(url)
		return err == nil
	}, time.Second, 10*time.Millisecond)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	// The goroutine profile must include at least one goroutine entry.
	assert.Contains(t, string(body), "goroutine", "goroutine endpoint must return stack traces")
}

func TestStart_HeapEndpoint(t *testing.T) {
	srv, stop, err := prof.Start("")
	require.NoError(t, err)
	defer stop()

	url := fmt.Sprintf("http://%s/debug/pprof/heap", srv.Addr())
	var resp *http.Response
	require.Eventually(t, func() bool {
		resp, err = http.Get(url)
		return err == nil
	}, time.Second, 10*time.Millisecond)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// Heap profile is binary (pprof format) — just verify it's non-empty.
	body, _ := io.ReadAll(resp.Body)
	assert.NotEmpty(t, body, "heap profile must be non-empty")
}

func TestStart_FixedAddr(t *testing.T) {
	srv, stop, err := prof.Start("127.0.0.1:0")
	require.NoError(t, err)
	defer stop()

	assert.Greater(t, srv.Port(), 0)
	assert.Contains(t, srv.Addr(), "127.0.0.1:")
}

func TestStart_StopShutdown(t *testing.T) {
	srv, stop, err := prof.Start("")
	require.NoError(t, err)

	addr := srv.Addr()
	url := fmt.Sprintf("http://%s/debug/pprof/", addr)

	// Confirm it's up.
	require.Eventually(t, func() bool {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return true
		}
		return false
	}, time.Second, 10*time.Millisecond)

	// Stop it.
	stop()
	time.Sleep(50 * time.Millisecond)

	// Must be down.
	_, err = http.Get(url)
	assert.Error(t, err, "pprof server must not respond after stop()")
}

func TestStart_AddrInUse(t *testing.T) {
	// Start once on a fixed port.
	srv1, stop1, err := prof.Start("127.0.0.1:0")
	require.NoError(t, err)
	defer stop1()

	// Try to bind the same port — must fail with a bind error.
	_, _, err = prof.Start(srv1.Addr())
	require.Error(t, err, "Start must return error when address is already in use")
	assert.Contains(t, err.Error(), "bind", "error must mention bind failure")
}
