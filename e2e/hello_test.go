//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHello_InjectsXHelloHeader verifies that the hello jisr filter injects
// the x-hello: from-jisr header into every upstream request.
func TestHello_InjectsXHelloHeader(t *testing.T) {
	resp, err := http.Get(envoyAddr + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	assert.Equal(t, "from-jisr", body.Headers["X-Hello"],
		"expected x-hello header injected by jisr filter")
}

// TestHello_PassesThrough verifies that arbitrary requests are forwarded
// to the upstream backend unchanged (aside from jisr's own injection).
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
	assert.Equal(t, "test-value", body.Headers["X-Custom"],
		"expected original header to pass through")
	assert.Equal(t, "from-jisr", body.Headers["X-Hello"],
		"expected jisr header on all requests")
}
