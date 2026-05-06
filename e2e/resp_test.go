//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRespPhase_StampRequestHeaderPresent verifies the resp-stamp filter's
// request phase ran: skipBodyHandler injects x-jisr-filter into the upstream
// request. The echo backend reflects all received headers so we can assert it.
func TestRespPhase_StampRequestHeaderPresent(t *testing.T) {
	resp, err := http.Get(envoyStampAddr + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	// The request-phase handler injected x-jisr-filter: resp-stamp.
	// The backend echoes all headers it received — including those set by the filter.
	assert.Equal(t, "resp-stamp", body.Headers["X-Jisr-Filter"],
		"skipBodyHandler must inject x-jisr-filter into the upstream request")
}

// TestRespPhase_StampBodyForwarded verifies that Passthrough mode forwards
// the upstream response body to the downstream client without modification.
func TestRespPhase_StampBodyForwarded(t *testing.T) {
	const expected = `{"message":"hello from backend","ok":true}`

	resp, err := http.Get(envoyStampAddr + "/body")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, expected, strings.TrimSpace(string(body)),
		"Passthrough must forward body unchanged")
}

// TestRespPhase_TapRequestHeaderPresent verifies the resp-tap filter's request
// phase ran — same mechanism as stamp, different filter name.
func TestRespPhase_TapRequestHeaderPresent(t *testing.T) {
	resp, err := http.Get(envoyTapAddr + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	assert.Equal(t, "resp-tap", body.Headers["X-Jisr-Filter"],
		"skipBodyHandler must inject x-jisr-filter: resp-tap")
}

// TestRespPhase_TapBodyForwarded verifies that Observe mode forwards the
// upstream response body to the downstream client simultaneously — the client
// receives the full body even though the goroutine is also reading it.
func TestRespPhase_TapBodyForwarded(t *testing.T) {
	const expected = `{"message":"hello from backend","ok":true}`

	resp, err := http.Get(envoyTapAddr + "/body")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, expected, strings.TrimSpace(string(body)),
		"Observe mode must forward body intact to downstream")
}

// TestRespPhase_TapChunkedBodyForwarded verifies Observe mode with chunked
// transfer encoding: all chunks arrive at the downstream client.
func TestRespPhase_TapChunkedBodyForwarded(t *testing.T) {
	resp, err := http.Get(envoyTapAddr + "/chunked")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	s := string(body)
	assert.Contains(t, s, "chunk-one", "first chunk must arrive")
	assert.Contains(t, s, "chunk-two", "second chunk must arrive")
}

// TestRespPhase_UpstreamDisconnect verifies that when the upstream backend
// abruptly closes the TCP connection mid-response, the filter's ResponseFunc
// exits cleanly (via io.Copy returning an error / channel close) and Envoy
// remains healthy for subsequent requests.
func TestRespPhase_UpstreamDisconnect(t *testing.T) {
	// /slow: backend sends headers + one chunk then closes the TCP connection.
	// The client may receive an error or a partial response — both are OK.
	// The critical assertions: no panic, no goroutine leak, Envoy stays healthy.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(envoyTapAddr + "/slow")
	if err == nil {
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	// err != nil is acceptable — Envoy aborted the response to the client.

	// Envoy must still be healthy after the upstream disconnect.
	require.True(t, envoyHealthy(t),
		"Envoy must remain healthy after upstream abrupt close")

	// A subsequent request to the tap filter must complete normally.
	resp2, err := client.Get(envoyTapAddr + "/body")
	require.NoError(t, err, "next request must succeed after upstream disconnect")
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	body2, err := io.ReadAll(resp2.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body2), "hello from backend",
		"body must be intact on request after upstream disconnect")
}

// TestRespPhase_ClientDisconnect verifies that when the downstream client
// abruptly closes the TCP connection mid-stream, the ResponseFunc's io.Copy
// returns (channel closed by OnStreamComplete), and Envoy remains healthy.
func TestRespPhase_ClientDisconnect(t *testing.T) {
	// Connect via raw TCP so we can abort mid-stream.
	addr := strings.TrimPrefix(envoyTapAddr, "http://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)

	// Send a valid HTTP/1.1 GET for /chunked (slow enough to disconnect mid-body).
	req := "GET /chunked HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
	_, err = conn.Write([]byte(req))
	require.NoError(t, err)

	// Read the status line to confirm Envoy started responding.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 128)
	n, _ := conn.Read(buf)
	statusPart := string(buf[:n])
	assert.Contains(t, statusPart, "200", "Envoy must begin the response before we disconnect")

	// Abruptly close — simulates browser tab close / network drop.
	conn.Close()

	// Give Envoy a moment to detect the RST and call OnStreamComplete.
	time.Sleep(200 * time.Millisecond)

	// Envoy must still be healthy.
	require.True(t, envoyHealthy(t),
		"Envoy must remain healthy after client abrupt close")

	// Subsequent request must succeed cleanly.
	resp, err := http.Get(envoyTapAddr + "/body")
	require.NoError(t, err, "subsequent request must succeed after client disconnect")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "hello from backend")
}

// TestRespPhase_HelloFilterUnaffected verifies that the existing hello filter
// on port 10000 (no response handler) still works correctly after adding the
// response-phase filters. No cross-contamination between filter instances.
func TestRespPhase_HelloFilterUnaffected(t *testing.T) {
	resp, err := http.Get(envoyAddr + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	// hello filter injects x-hello — response-phase filters must not affect this.
	assert.Equal(t, "from-jisr", body.Headers["X-Hello"],
		"hello filter must still inject x-hello")

	// Response-phase headers must NOT appear — hello filter has no response phase.
	assert.Empty(t, body.Headers["X-Jisr-Filter"],
		"hello filter must not inject x-jisr-filter (no response handler)")
}
