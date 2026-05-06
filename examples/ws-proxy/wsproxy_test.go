package wsproxy_test

import (
	"context"
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

// echoServer is a simple WebSocket server that echoes every message back.
// Used as the "upstream" in tests — no real provider needed.
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			conn.Write(ctx, typ, data)
		}
	}))
}

func TestWSProxy_EchoRoundTrip(t *testing.T) {
	// Start an in-process echo backend (stands in for the real upstream).
	upstream := echoServer(t)
	defer upstream.Close()

	// Build the proxy pointing at the echo backend.
	// ws:// because httptest.Server doesn't do TLS.
	upstreamURL := "ws" + strings.TrimPrefix(upstream.URL, "http")
	proxy := wsproxy.NewProxy(upstreamURL, "", "")

	// Wrap proxy in a test server (simulates Envoy's ws-proxy-local cluster).
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Connect a client to the proxy.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	proxyURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http")
	conn, _, err := websocket.Dial(ctx, proxyURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send a JSON message and expect it echoed back.
	msg := map[string]string{"type": "ping", "data": "hello"}
	require.NoError(t, wsjson.Write(ctx, conn, msg))

	var got map[string]string
	require.NoError(t, wsjson.Read(ctx, conn, &got))
	assert.Equal(t, msg, got)
}

func TestWSProxy_TapsClientFrames(t *testing.T) {
	upstream := echoServer(t)
	defer upstream.Close()

	upstreamURL := "ws" + strings.TrimPrefix(upstream.URL, "http")

	var clientFrames atomic.Int64
	var upstreamFrames atomic.Int64

	proxy := wsproxy.NewProxy(upstreamURL, "", "")
	proxy.OnClientFrame = func(_ websocket.MessageType, _ []byte) {
		clientFrames.Add(1)
	}
	proxy.OnUpstreamFrame = func(_ websocket.MessageType, _ []byte) {
		upstreamFrames.Add(1)
	}

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxyServer.URL, "http"), nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send 3 messages — expect 3 client taps and 3 upstream taps (echo).
	for i := 0; i < 3; i++ {
		require.NoError(t, wsjson.Write(ctx, conn, map[string]int{"i": i}))
		var resp map[string]int
		require.NoError(t, wsjson.Read(ctx, conn, &resp))
	}

	assert.Equal(t, int64(3), clientFrames.Load())
	assert.Equal(t, int64(3), upstreamFrames.Load())
}

func TestWSProxy_UpstreamUnavailable(t *testing.T) {
	// Point at a port nobody is listening on.
	proxy := wsproxy.NewProxy("ws://127.0.0.1:1", "", "")
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxyServer.URL, "http"), nil)
	if err != nil {
		// Some WS libs surface upstream errors as dial failures — also acceptable.
		return
	}
	defer conn.CloseNow()

	// The proxy accepted the upgrade but then closed it — next read must fail.
	_, _, err = conn.Read(ctx)
	assert.Error(t, err, "expected connection closed by proxy after upstream failure")
}
