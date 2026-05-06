//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRewrite_BodyInjected verifies that resp-rewrite (ResponseModeBuffer) reads
// the full upstream JSON body, injects a new field, and delivers the modified
// body to the client with an updated content-length.
func TestRewrite_BodyInjected(t *testing.T) {
	resp, err := http.Get(envoyRewriteAddr + "/json")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "1", resp.Header.Get("X-Jisr-Rewritten"),
		"x-jisr-rewritten header should be set by the filter")

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	// Original fields from the backend.
	assert.Equal(t, "backend", body["service"])
	assert.NotNil(t, body["version"])

	// Field injected by respRewriteHandler.
	assert.Equal(t, true, body["x_jisr_rewritten"],
		"filter should have injected x_jisr_rewritten=true")
}

// TestRewrite_ContentLengthUpdated verifies the filter correctly updates
// content-length after replacing the body (otherwise clients may mis-read).
func TestRewrite_ContentLengthUpdated(t *testing.T) {
	resp, err := http.Get(envoyRewriteAddr + "/json")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	cl := resp.ContentLength
	if cl > 0 {
		// If Envoy forwarded content-length, it must match the actual body length.
		assert.Equal(t, int64(len(body)), cl,
			"content-length must match rewritten body length")
	}
}

// TestRewrite_NonJSONPassthrough verifies the filter leaves non-JSON responses
// untouched (ReplaceBody is not called when JSON parse fails).
func TestRewrite_NonJSONPassthrough(t *testing.T) {
	resp, err := http.Get(envoyRewriteAddr + "/chunked")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	// Backend /chunked returns plain text — filter must pass it through unchanged.
	assert.Contains(t, string(body), "chunk-one")
	assert.Contains(t, string(body), "chunk-two")

	// x-jisr-rewritten must NOT be present (filter skipped non-JSON body).
	assert.Empty(t, resp.Header.Get("X-Jisr-Rewritten"))
}

// TestHeaderStamp_HeaderPresent verifies resp-header-stamp (ResponseModePassthrough)
// adds x-jisr-stamp with the upstream status code, with zero body overhead.
func TestHeaderStamp_HeaderPresent(t *testing.T) {
	resp, err := http.Get(envoyHStampAddr + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	stamp := resp.Header.Get("X-Jisr-Stamp")
	assert.Equal(t, "200", stamp,
		"x-jisr-stamp should reflect upstream status code")
}

// TestHeaderStamp_BodyUnmodified verifies the body is not buffered or modified
// by the Passthrough filter.
func TestHeaderStamp_BodyUnmodified(t *testing.T) {
	resp, err := http.Get(envoyHStampAddr + "/body")
	require.NoError(t, err)
	defer resp.Body.Close()

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	// Backend /body returns {"message":"hello from backend","ok":true}.
	assert.Equal(t, "hello from backend", body["message"])
	assert.Equal(t, true, body["ok"])

	// Header stamp still present.
	assert.Equal(t, "200", resp.Header.Get("X-Jisr-Stamp"))
}
