package prof_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dio/jisr/prof"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Handler unit tests (no TCP) ───────────────────────────────────────────────

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func wrongMethod(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHandler_Healthz(t *testing.T) {
	h := prof.Handler()
	rr := get(h, "/healthz")

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	var body map[string]string
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "ok", body["status"])
}

func TestHandler_Healthz_WrongMethod(t *testing.T) {
	assert.Equal(t, http.StatusMethodNotAllowed, wrongMethod(prof.Handler(), "/healthz").Code)
}

func TestHandler_Readyz(t *testing.T) {
	h := prof.Handler()
	rr := get(h, "/readyz")

	assert.Equal(t, http.StatusOK, rr.Code)
	var body map[string]string
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "ok", body["status"])
}

func TestHandler_Readyz_WrongMethod(t *testing.T) {
	assert.Equal(t, http.StatusMethodNotAllowed, wrongMethod(prof.Handler(), "/readyz").Code)
}

func TestHandler_Version(t *testing.T) {
	h := prof.Handler()
	rr := get(h, "/version")

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	var body map[string]string
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	// module and version keys must always be present.
	assert.Contains(t, body, "module")
	assert.Contains(t, body, "version")
}

func TestHandler_Pprof_Index(t *testing.T) {
	h := prof.Handler()
	rr := get(h, "/debug/pprof/")

	assert.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "goroutine", "pprof index must list goroutine profile")
	assert.Contains(t, body, "heap", "pprof index must list heap profile")
}

func TestHandler_Pprof_Symbol_GET(t *testing.T) {
	h := prof.Handler()
	rr := get(h, "/debug/pprof/symbol")

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "num_symbols")
}

func TestHandler_Pprof_NamedProfiles(t *testing.T) {
	h := prof.Handler()
	for _, name := range []string{"goroutine", "heap", "allocs"} {
		t.Run(name, func(t *testing.T) {
			rr := get(h, "/debug/pprof/"+name)
			assert.Equal(t, http.StatusOK, rr.Code, "profile %q must return 200", name)
			assert.NotEmpty(t, rr.Body.Bytes(), "profile %q must have non-empty body", name)
		})
	}
}

func TestHandler_Pprof_WrongMethod(t *testing.T) {
	h := prof.Handler()
	assert.Equal(t, http.StatusMethodNotAllowed, wrongMethod(h, "/debug/pprof/").Code)
}

// ── E2E table test via httptest.Server ───────────────────────────────────────

func TestHandler_E2E(t *testing.T) {
	ts := httptest.NewServer(prof.Handler())
	defer ts.Close()

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{"/healthz", 200, `"ok"`},
		{"/readyz", 200, `"ok"`},
		{"/version", 200, `"module"`},
		{"/debug/pprof/", 200, "goroutine"},
		{"/debug/pprof/goroutine?debug=1", 200, "goroutine"},
		{"/debug/pprof/heap", 200, ""}, // binary, just check 200
		{"/debug/pprof/symbol", 200, "num_symbols"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := ts.Client().Get(ts.URL + tt.path)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			if tt.wantBody != "" {
				buf := new(strings.Builder)
				_, _ = io.Copy(buf, resp.Body)
				assert.Contains(t, buf.String(), tt.wantBody)
			}
		})
	}
}

// ── Start / Stop lifecycle ────────────────────────────────────────────────────

func TestStart_RandomPort(t *testing.T) {
	srv, stop, err := prof.Start("")
	require.NoError(t, err)
	defer stop()

	assert.Greater(t, srv.Port(), 0)
	assert.True(t, strings.HasPrefix(srv.Addr(), "127.0.0.1:"))
}

func TestStart_FixedAddr(t *testing.T) {
	srv, stop, err := prof.Start("127.0.0.1:0")
	require.NoError(t, err)
	defer stop()

	assert.Greater(t, srv.Port(), 0)
}

func TestStart_ServesEndpoints(t *testing.T) {
	srv, stop, err := prof.Start("")
	require.NoError(t, err)
	defer stop()

	url := fmt.Sprintf("http://%s/healthz", srv.Addr())
	var resp *http.Response
	require.Eventually(t, func() bool {
		resp, err = http.Get(url)
		return err == nil
	}, time.Second, 10*time.Millisecond)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestStart_StopShutdown(t *testing.T) {
	srv, stop, err := prof.Start("")
	require.NoError(t, err)

	addr := srv.Addr()
	url := fmt.Sprintf("http://%s/healthz", addr)

	// Confirm it's up.
	require.Eventually(t, func() bool {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return true
		}
		return false
	}, time.Second, 10*time.Millisecond)

	stop()
	time.Sleep(50 * time.Millisecond)

	_, err = http.Get(url)
	assert.Error(t, err, "server must not respond after stop()")
}

func TestStart_AddrInUse(t *testing.T) {
	srv1, stop1, err := prof.Start("127.0.0.1:0")
	require.NoError(t, err)
	defer stop1()

	// Bind same port — must fail.
	_, _, err = prof.Start(srv1.Addr())
	require.Error(t, err, "Start must return error when address is in use")
	assert.Contains(t, err.Error(), "bind")
}

func TestStart_StopIdempotent(t *testing.T) {
	_, stop, err := prof.Start("")
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		stop()
		stop() // second call must not panic
	})
}
