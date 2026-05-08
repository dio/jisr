//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

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

// TestHello_EmitsStructuredRequestLog verifies that Request.LogAttrs reaches
// Envoy's logger with deterministic logfmt-style fields.
func TestHello_EmitsStructuredRequestLog(t *testing.T) {
	const path = "/structured-log-e2e"

	resp, err := http.Get(envoyAddr + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	want := "hello request filter=hello method=GET path=" + path
	require.Eventually(t, func() bool {
		return envoyLogs != nil && envoyLogs.contains(want)
	}, 5*time.Second, 50*time.Millisecond, "missing structured log line %q", want)
}

// TestHello_EmitsDynamicMetadataAccessLog verifies that metadata set by jisr is
// available to Envoy access logs through the DYNAMIC_METADATA formatter.
func TestHello_EmitsDynamicMetadataAccessLog(t *testing.T) {
	const path = "/dynamic-metadata-e2e"

	resp, err := http.Get(envoyAddr + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	want := "access dynamic_metadata_filter=hello dynamic_metadata_route=hello"
	require.Eventually(t, func() bool {
		return envoyLogs != nil && envoyLogs.contains(want)
	}, 5*time.Second, 50*time.Millisecond, "missing dynamic metadata access log %q", want)
}

// TestHello_IncrementsEnvoyCounter verifies that jisr metrics are emitted into
// Envoy's native stats store, independent of request log visibility.
func TestHello_IncrementsEnvoyCounter(t *testing.T) {
	const metric = "hello_requests_total"

	before, _ := readEnvoyCounter(metric)

	resp, err := http.Get(envoyAddr + "/metrics-e2e")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Eventually(t, func() bool {
		after, ok := readEnvoyCounter(metric)
		return ok && after >= before+1
	}, 5*time.Second, 50*time.Millisecond, "counter %q did not increase from %d", metric, before)
}

// TestHello_ExportsCounterToOTelSink verifies that Envoy can export a
// jisr-defined counter through its OpenTelemetry stat sink.
func TestHello_ExportsCounterToOTelSink(t *testing.T) {
	const metric = "hello_requests_total"

	drainOTelMetrics()

	resp, err := http.Get(envoyAddr + "/otel-metrics-e2e")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.Eventually(t, func() bool {
		return sawOTelMetric(metric)
	}, 8*time.Second, 100*time.Millisecond, "OTel sink did not receive metric %q", metric)
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

func readEnvoyCounter(name string) (uint64, bool) {
	resp, err := http.Get(adminAddr + "/stats?filter=" + url.QueryEscape(name))
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}

	var found bool
	var max uint64
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, name) {
			continue
		}

		_, rawValue, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSpace(rawValue), 10, 64)
		if err != nil {
			continue
		}
		if !found || value > max {
			found = true
			max = value
		}
	}
	if scanner.Err() != nil {
		return 0, false
	}
	return max, found
}

func drainOTelMetrics() {
	for {
		select {
		case <-otelMetrics:
		default:
			return
		}
	}
}

func sawOTelMetric(name string) bool {
	for {
		select {
		case metric := <-otelMetrics:
			if strings.Contains(metric, name) {
				return true
			}
		default:
			return false
		}
	}
}
