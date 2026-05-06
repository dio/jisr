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

// ── Disconnect test cases ─────────────────────────────────────────────────────
//
// Four scenarios, two axes:
//
//   client disconnect:
//     A. before upstream responds        (goroutine parked on respHeadersCh)
//     B. after upstream headers + data   (goroutine reading r.Body mid-stream)
//
//   upstream disconnect:
//     C. after sending partial data      (/slow — incomplete chunked response)
//     D. before sending any data (RST)   (/noconnect cluster — Envoy 503)

// TestDisconnect_ClientBeforeUpstreamResponds — case A.
// Client closes the TCP connection while the filter goroutine is blocked
// waiting on respHeadersCh (ContinueRequest has been called but the upstream
// hasn't responded yet). OnStreamComplete fires, cancels the context, the
// goroutine unblocks from the select and exits cleanly.
func TestDisconnect_ClientBeforeUpstreamResponds(t *testing.T) {
	// /hold: backend sends headers + one chunk, then blocks.
	// We disconnect *before* even reading the response status line so we
	// race with the upstream connection — the goroutine is most likely
	// parked on respHeadersCh when OnStreamComplete fires.
	addr := strings.TrimPrefix(envoyTapAddr, "http://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)

	// Send request.
	req := "GET /hold HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
	_, err = conn.Write([]byte(req))
	require.NoError(t, err)

	// Close immediately — before reading any response.
	// The goroutine is either waiting for upstream headers or has just received them.
	conn.Close()

	// Give Envoy time to process the client RST.
	time.Sleep(200 * time.Millisecond)

	require.True(t, envoyHealthy(t), "Envoy must survive client disconnect before upstream responds")

	// Subsequent request must complete normally.
	resp, err := http.Get(envoyTapAddr + "/body")
	require.NoError(t, err, "next request must work after case-A disconnect")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestDisconnect_ClientAfterPartialResponse — case B.
// Client closes TCP after receiving response headers + first body chunk,
// while the filter goroutine is reading r.Body. OnStreamComplete closes
// respBodyCh, io.Copy in the ResponseFunc returns, goroutine exits cleanly.
func TestDisconnect_ClientAfterPartialResponse(t *testing.T) {
	// /hold: backend sends "partial-data" then blocks. We read the status line
	// + headers + first chunk, then close — simulating a tab close mid-stream.
	addr := strings.TrimPrefix(envoyTapAddr, "http://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()

	req := "GET /hold HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
	_, err = conn.Write([]byte(req))
	require.NoError(t, err)

	// Read until we see some body data — confirms the filter's OnResponseBody fired.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	var received string
	for !strings.Contains(received, "partial-data") {
		n, err := conn.Read(buf)
		if n > 0 {
			received += string(buf[:n])
		}
		if err != nil {
			break
		}
	}
	assert.Contains(t, received, "partial-data", "must receive at least the first chunk before disconnecting")

	// Now abruptly close — goroutine is reading r.Body.
	conn.Close()
	time.Sleep(200 * time.Millisecond)

	require.True(t, envoyHealthy(t), "Envoy must survive client disconnect mid-body")

	resp, err := http.Get(envoyTapAddr + "/body")
	require.NoError(t, err, "next request must work after case-B disconnect")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestDisconnect_UpstreamAfterPartialData — case C.
// Upstream sends HTTP headers + one valid chunk, then abruptly closes the TCP
// connection without the terminating 0-length chunk. Envoy detects the error,
// calls OnStreamComplete, which closes respBodyCh. The goroutine's io.Copy
// returns and the goroutine exits cleanly.
func TestDisconnect_UpstreamAfterPartialData(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}

	// /slow: backend sends headers + "hello" chunk then RSTs.
	// The downstream client may receive an error or a truncated response.
	resp, err := client.Get(envoyTapAddr + "/slow")
	if err == nil {
		// Envoy may forward partial data before detecting the upstream RST.
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// If Envoy forwards the partial body before detecting the RST, it's fine.
		t.Logf("upstream partial data received: status=%d body=%q", resp.StatusCode, body)
	} else {
		// Envoy aborted the response to us — also acceptable.
		t.Logf("client got error (expected): %v", err)
	}

	require.True(t, envoyHealthy(t), "Envoy must survive upstream disconnect after partial data")

	// Subsequent request must work — goroutine must have exited cleanly.
	resp2, err := client.Get(envoyTapAddr + "/body")
	require.NoError(t, err, "next request must work after case-C upstream disconnect")
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	body2, _ := io.ReadAll(resp2.Body)
	assert.Contains(t, string(body2), "hello from backend")
}

// TestDisconnect_UpstreamResetBeforeData — case D.
// Upstream accepts the TCP connection then immediately closes it (RST) without
// sending any HTTP data. Envoy never calls OnResponseHeaders — the filter
// goroutine is parked on respHeadersCh forever. OnStreamComplete fires
// (context cancelled), the select unblocks with ctx.Done(), goroutine exits.
// Envoy sends 503 to the downstream client.
func TestDisconnect_UpstreamResetBeforeData(t *testing.T) {
	// Port 10004 routes to noconnect cluster (RST backend).
	rstAddr := "http://localhost:10004"
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(rstAddr + "/")
	if err == nil {
		defer resp.Body.Close()
		io.ReadAll(resp.Body)
		// Envoy returns 503 (upstream connection failure) — the filter goroutine
		// must have exited via ctx.Done() on the respHeadersCh select.
		t.Logf("upstream RST: Envoy returned %d (expected 503)", resp.StatusCode)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
			"Envoy must return 503 when upstream resets before sending data")
	} else {
		t.Logf("client got error (Envoy closed connection): %v", err)
	}

	require.True(t, envoyHealthy(t), "Envoy must survive upstream RST before any data")

	// Goroutine must have exited — subsequent request on tap port must work.
	resp2, err := client.Get(envoyTapAddr + "/body")
	require.NoError(t, err, "next request on tap port must work after case-D disconnect")
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
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
