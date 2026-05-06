//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipIfNoAdmin skips the test if the admin server port wasn't discovered.
func skipIfNoAdmin(t *testing.T) {
	t.Helper()
	if adminPortStr == "" {
		t.Skip("admin server port not available — .so may not have started prof")
	}
}

// TestProf_Healthz verifies /healthz returns {"status":"ok"}.
func TestProf_Healthz(t *testing.T) {
	skipIfNoAdmin(t)

	resp, err := http.Get(adminPortStr + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "ok", body["status"])
}

// TestProf_Readyz verifies /readyz returns {"status":"ok"}.
func TestProf_Readyz(t *testing.T) {
	skipIfNoAdmin(t)

	resp, err := http.Get(adminPortStr + "/readyz")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "ok", body["status"])
}

// TestProf_Version verifies /version returns a JSON object with module + version fields.
func TestProf_Version(t *testing.T) {
	skipIfNoAdmin(t)

	resp, err := http.Get(adminPortStr + "/version")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.NotEmpty(t, body["module"], "module field should be present")
	assert.NotEmpty(t, body["version"], "version field should be present")
}

// TestProf_PprofIndex verifies /debug/pprof/ returns the pprof HTML index.
func TestProf_PprofIndex(t *testing.T) {
	skipIfNoAdmin(t)

	resp, err := http.Get(adminPortStr + "/debug/pprof/")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// The pprof index page always contains "goroutine".
	assert.Contains(t, string(body), "goroutine")
}

// TestProf_PprofGoroutine verifies /debug/pprof/goroutine?debug=1 returns
// a goroutine dump with at least one goroutine listed.
func TestProf_PprofGoroutine(t *testing.T) {
	skipIfNoAdmin(t)

	resp, err := http.Get(adminPortStr + "/debug/pprof/goroutine?debug=1")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)
	// debug=1 returns a text summary: "goroutine profile: total N" followed by
	// per-stack-trace counts. At least one goroutine is always running.
	assert.True(t,
		strings.Contains(text, "goroutine profile: total") || strings.Contains(text, "goroutine 1 ["),
		"expected goroutine dump in body, got: %s", text[:min(200, len(text))])
}

// TestProf_PprofHeap verifies /debug/pprof/heap returns a valid gzip binary profile.
func TestProf_PprofHeap(t *testing.T) {
	skipIfNoAdmin(t)

	resp, err := http.Get(adminPortStr + "/debug/pprof/heap")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// pprof profiles are gzip-encoded; gzip magic bytes are 0x1f 0x8b.
	require.Greater(t, len(body), 2, "heap profile body too short")
	assert.Equal(t, byte(0x1f), body[0], "expected gzip magic byte 0")
	assert.Equal(t, byte(0x8b), body[1], "expected gzip magic byte 1")
}

// TestProf_UnknownRoute verifies unknown routes return 404 (not 200 from DefaultServeMux).
func TestProf_UnknownRoute(t *testing.T) {
	skipIfNoAdmin(t)

	resp, err := http.Get(adminPortStr + "/not-a-real-endpoint")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
