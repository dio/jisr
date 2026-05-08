//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWSProxy_EmbeddedActor_ProxiesToLocalUpstream proves the ws-proxy example
// runs as an embedded actor behind Envoy and talks to a local mock upstream,
// not to OpenAI.
func TestWSProxy_EmbeddedActor_ProxiesToLocalUpstream(t *testing.T) {
	drainOTelMetrics()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, envoyWSProxyAddr+"/v1/responses", nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	require.NoError(t, wsjson.Write(ctx, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-4o-mini",
		"input": []map[string]any{{
			"type": "message",
			"role": "user",
		}},
	}))

	var deltas []string
	var inputTokens, outputTokens uint32

	for {
		var evt map[string]json.RawMessage
		require.NoError(t, wsjson.Read(ctx, conn, &evt))

		var typ string
		require.NoError(t, json.Unmarshal(evt["type"], &typ))

		switch typ {
		case "response.output_text.delta":
			var delta string
			require.NoError(t, json.Unmarshal(evt["delta"], &delta))
			deltas = append(deltas, delta)
		case "response.completed":
			var full struct {
				Response struct {
					Usage struct {
						InputTokens  uint32 `json:"input_tokens"`
						OutputTokens uint32 `json:"output_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			raw, err := json.Marshal(evt)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &full))
			inputTokens = full.Response.Usage.InputTokens
			outputTokens = full.Response.Usage.OutputTokens
			goto done
		}
	}

done:
	assert.Equal(t, "local upstream", strings.Join(deltas, ""))
	assert.Equal(t, uint32(21), inputTokens)
	assert.Equal(t, uint32(8), outputTokens)

	require.Eventually(t, func() bool {
		select {
		case path := <-wsPaths:
			return path == "/v1/responses"
		default:
			return false
		}
	}, 2*time.Second, 50*time.Millisecond, "local ws upstream did not receive the proxied connection")

	require.Eventually(t, func() bool {
		return envoyLogs != nil &&
			envoyLogs.contains("ws-proxy: session ended") &&
			envoyLogs.contains("model=gpt-4o-mini") &&
			envoyLogs.contains("input_tokens=21") &&
			envoyLogs.contains("output_tokens=8")
	}, 5*time.Second, 50*time.Millisecond, "missing embedded actor session log")

	require.Eventually(t, func() bool {
		return sawOTelMetric("ws_proxy_sessions_total")
	}, 8*time.Second, 100*time.Millisecond, "OTel sink did not receive actor session metric")

	require.Eventually(t, func() bool {
		return sawOTelMetric("ws_proxy_session_duration_ms")
	}, 8*time.Second, 100*time.Millisecond, "OTel sink did not receive actor session histogram")
}
