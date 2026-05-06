//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHello_InjectsXHelloHeader verifies that the hello filter injects
// x-hello: from-jisr into every upstream request.
func TestHello_InjectsXHelloHeader(t *testing.T) {
	resp, err := http.Get(envoyAddr + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "from-jisr", body.Headers["X-Hello"])
}

// TestHello_PassesThrough verifies that custom request headers survive
// the filter and reach the upstream backend.
func TestHello_PassesThrough(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, envoyAddr+"/some/path", nil)
	require.NoError(t, err)
	req.Header.Set("x-custom", "test-value")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "/some/path", body.Path)
	assert.Equal(t, "test-value", body.Headers["X-Custom"])
	assert.Equal(t, "from-jisr", body.Headers["X-Hello"])
}

// TestEcho_DirectResponse verifies that the hello-echo filter responds
// directly via w.SendBytes — no upstream is involved.
// The backend server is never hit; the response comes entirely from jisr.
func TestEcho_DirectResponse(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, envoyEchoAddr+"/ping", strings.NewReader(`{"hello":"world"}`))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-request-id", "abc123")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// jisr responded with 200 directly — blackhole cluster never touched.
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("content-type"), "application/json")

	var body struct {
		Path    string            `json:"path"`
		Method  string            `json:"method"`
		Body    string            `json:"body"`
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "/ping", body.Path)
	assert.Equal(t, http.MethodPost, body.Method)
	assert.JSONEq(t, `{"hello":"world"}`, body.Body)
	// jisr applies http.CanonicalHeaderKey on ingress, so headers are
	// in canonical form (X-Request-Id, not x-request-id) from the handler's view.
	assert.Equal(t, "abc123", body.Headers["X-Request-Id"])
}
