package wsproxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	wsproxy "github.com/dio/jisr/examples/ws-proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── mock upstream ─────────────────────────────────────────────────────────────

// openaiMockUpstream is an in-process WebSocket server that simulates the
// OpenAI /v1/responses Realtime API.
//
// On receiving response.create it replies with:
//  1. response.created
//  2. response.output_text.delta (one or more text chunks)
//  3. response.output_text.done
//  4. response.completed (with token counts)
func openaiMockUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()

		ctx := r.Context()

		// Read the client's response.create message.
		var create struct {
			Type  string `json:"type"`
			Model string `json:"model"`
		}
		if err := wsjson.Read(ctx, conn, &create); err != nil || create.Type != "response.create" {
			return
		}

		send := func(v any) {
			wsjson.Write(ctx, conn, v) //nolint:errcheck
		}

		send(map[string]any{"type": "response.created"})

		// Stream a few text delta events.
		words := []string{"Hi", " there", "!"}
		for _, w := range words {
			send(map[string]any{
				"type":  "response.output_text.delta",
				"delta": w,
			})
		}

		send(map[string]any{"type": "response.output_text.done"})

		// Final event with token usage.
		send(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"usage": map[string]any{
					"input_tokens":  12,
					"output_tokens": 3,
				},
			},
		})
	}))
}

// ── helpers ───────────────────────────────────────────────────────────────────

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dialProxy(t *testing.T, proxySrv *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, wsURL(proxySrv)+path, nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

// ── unit tests: sessionTap ────────────────────────────────────────────────────

func TestSessionTap_ExtractsModel(t *testing.T) {
	tap := wsproxy.NewSessionTap()

	// Unrelated frame — model should remain empty.
	tap.FeedClient([]byte(`{"type":"session.created"}`))
	assert.Empty(t, tap.Model())

	// response.create with model.
	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini","max_output_tokens":20}`))
	assert.Equal(t, "gpt-4o-mini", tap.Model())

	// Subsequent frame is ignored — model already captured.
	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4"}`))
	assert.Equal(t, "gpt-4o-mini", tap.Model()) // unchanged
}

func TestSessionTap_ExtractsTokenUsage(t *testing.T) {
	tap := wsproxy.NewSessionTap()

	// Intermediate frames — no usage yet.
	tap.FeedUpstream([]byte(`{"type":"response.created"}`))
	tap.FeedUpstream([]byte(`{"type":"response.output_text.delta","delta":"Hi"}`))
	assert.Equal(t, uint32(0), tap.Usage().InputTokens)

	// response.completed carries the usage.
	tap.FeedUpstream([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}`))
	u := tap.Usage()
	assert.Equal(t, uint32(10), u.InputTokens)
	assert.Equal(t, uint32(5), u.OutputTokens)
}

func TestSessionTap_FastPath_SkipsNonMatchingFrames(t *testing.T) {
	tap := wsproxy.NewSessionTap()

	// Many delta frames — none should update usage (no "response.completed" substring).
	for i := 0; i < 1000; i++ {
		tap.FeedUpstream([]byte(`{"type":"response.output_text.delta","delta":"x"}`))
	}
	assert.Equal(t, uint32(0), tap.Usage().InputTokens)
}

// ── integration tests: WSProxy ────────────────────────────────────────────────

func TestWSProxy_OpenAI_Responses_RoundTrip(t *testing.T) {
	upstream := openaiMockUpstream(t)
	defer upstream.Close()

	proxy := wsproxy.NewProxy(wsURL(upstream), "", "")
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(proxySrv)+"/v1/responses", nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send response.create.
	create := map[string]any{
		"type":              "response.create",
		"model":             "gpt-4o-mini",
		"max_output_tokens": 20,
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": "say hi"}},
		}},
	}
	require.NoError(t, wsjson.Write(ctx, conn, create))

	// Collect all events until response.completed.
	var deltas []string
	var completed bool
	var inputTokens, outputTokens uint32

	for !completed {
		var evt map[string]json.RawMessage
		require.NoError(t, wsjson.Read(ctx, conn, &evt))

		var evtType string
		json.Unmarshal(evt["type"], &evtType)

		switch evtType {
		case "response.output_text.delta":
			var delta string
			json.Unmarshal(evt["delta"], &delta)
			deltas = append(deltas, delta)
		case "response.completed":
			completed = true
			var resp struct {
				Response struct {
					Usage struct {
						InputTokens  uint32 `json:"input_tokens"`
						OutputTokens uint32 `json:"output_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			json.Unmarshal(evt["response"], &resp.Response)
			// Inline parse from raw message
			var full struct {
				Response struct {
					Usage struct {
						InputTokens  uint32 `json:"input_tokens"`
						OutputTokens uint32 `json:"output_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			raw, _ := json.Marshal(evt)
			json.Unmarshal(raw, &full)
			inputTokens = full.Response.Usage.InputTokens
			outputTokens = full.Response.Usage.OutputTokens
		}
	}

	assert.True(t, completed)
	assert.NotEmpty(t, deltas, "expected text delta events")
	assert.Equal(t, "Hi there!", strings.Join(deltas, ""))
	assert.Equal(t, uint32(12), inputTokens)
	assert.Equal(t, uint32(3), outputTokens)
}

func TestWSProxy_TapsModelAndTokens(t *testing.T) {
	upstream := openaiMockUpstream(t)
	defer upstream.Close()

	proxy := wsproxy.NewProxy(wsURL(upstream), "", "")

	var clientFrames, upstreamFrames atomic.Int64
	proxy.OnClientFrame = func(_ websocket.MessageType, _ []byte) { clientFrames.Add(1) }
	proxy.OnUpstreamFrame = func(_ websocket.MessageType, _ []byte) { upstreamFrames.Add(1) }

	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	conn := dialProxy(t, proxySrv, "/v1/responses")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, wsjson.Write(ctx, conn, map[string]any{
		"type": "response.create", "model": "gpt-4o-mini",
	}))

	// Drain until completed.
	for {
		var evt map[string]string
		if err := wsjson.Read(ctx, conn, &evt); err != nil {
			break
		}
		if evt["type"] == "response.completed" {
			break
		}
	}

	assert.Equal(t, int64(1), clientFrames.Load(), "one client frame (response.create)")
	// upstream sends: created + 3 deltas + done + completed = 6
	assert.GreaterOrEqual(t, upstreamFrames.Load(), int64(4))
}

func TestWSProxy_UpstreamUnavailable(t *testing.T) {
	proxy := wsproxy.NewProxy("ws://127.0.0.1:1", "", "")
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL(proxySrv)+"/v1/responses", nil)
	if err != nil {
		return // dial rejected — acceptable
	}
	defer conn.CloseNow()

	_, _, err = conn.Read(ctx)
	assert.Error(t, err, "expected close after upstream failure")
}

func TestResolveEnv(t *testing.T) {
	t.Setenv("TEST_TOKEN", "sk-test123")

	// Direct ${VAR} form.
	assert.Equal(t, "sk-test123", wsproxy.ResolveEnv("${TEST_TOKEN}"))

	// Embedded in string.
	assert.Equal(t, "Bearer sk-test123", wsproxy.ResolveEnv("Bearer ${TEST_TOKEN}"))

	// No match — returned unchanged.
	assert.Equal(t, "${UNSET_VAR}", wsproxy.ResolveEnv("${UNSET_VAR}"))

	// Plain string — pass through.
	assert.Equal(t, "literal", wsproxy.ResolveEnv("literal"))
}
