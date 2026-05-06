package jisr_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/dio/jisr"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── callout tests ─────────────────────────────────────────────────────────────
// These test callout.go using a mock handle and scheduler.

// mockCalloutHandle implements shared.HttpFilterHandle for callout tests.
// It records the callout parameters and immediately invokes the callback.
type mockCalloutHandle struct {
	jisr.EmptyHttpFilterHandle
	// calloutResult controls what OnHttpCalloutDone receives.
	calloutResult shared.HttpCalloutResult
	// callbackHeaders are the headers passed to OnHttpCalloutDone.
	callbackHeaders [][2]shared.UnsafeEnvoyBuffer
	// callbackBody is the body passed to OnHttpCalloutDone.
	callbackBody []shared.UnsafeEnvoyBuffer
	// initResult controls what HttpCallout returns.
	initResult shared.HttpCalloutInitResult
}

func (h *mockCalloutHandle) HttpCallout(
	_ string,
	_ [][2]string,
	_ []byte,
	_ uint64,
	cb shared.HttpCalloutCallback,
) (shared.HttpCalloutInitResult, uint64) {
	if h.initResult == shared.HttpCalloutInitSuccess {
		// Invoke callback synchronously — simulates Envoy delivering the response.
		cb.OnHttpCalloutDone(1, h.calloutResult, h.callbackHeaders, h.callbackBody)
	}
	return h.initResult, 1
}

// syncScheduler runs scheduled functions synchronously — same as fakeScheduler.
type syncScheduler struct{}

func (s *syncScheduler) Schedule(fn func()) { fn() }

func TestCallout_Success(t *testing.T) {
	handle := &mockCalloutHandle{
		initResult:    shared.HttpCalloutInitSuccess,
		calloutResult: shared.HttpCalloutSuccess,
		callbackHeaders: [][2]shared.UnsafeEnvoyBuffer{
			makeUnsafePair(":status", "200"),
			makeUnsafePair("content-type", "application/json"),
		},
		callbackBody: []shared.UnsafeEnvoyBuffer{
			makeUnsafeBuffer(`{"ok":true}`),
		},
	}

	resp, err := jisr.Do(
		context.Background(),
		&syncScheduler{},
		handle,
		"my-cluster",
		[][2]string{{":method", "GET"}, {":path", "/"}},
		nil,
		1000,
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("content-type"))
	assert.Equal(t, `{"ok":true}`, string(resp.Body))
}

func TestCallout_CalloutFails(t *testing.T) {
	// Envoy reports the callout failed (non-success result).
	handle := &mockCalloutHandle{
		initResult:    shared.HttpCalloutInitSuccess,
		calloutResult: shared.HttpCalloutResult(99), // some failure code
	}

	resp, err := jisr.Do(
		context.Background(),
		&syncScheduler{},
		handle,
		"my-cluster",
		nil, nil, 1000,
	)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "callout failed")
}

func TestCallout_InitFails(t *testing.T) {
	// HttpCallout itself fails to initiate (cluster not found, etc.).
	handle := &mockCalloutHandle{
		initResult: shared.HttpCalloutInitResult(99), // not HttpCalloutInitSuccess
	}

	resp, err := jisr.Do(
		context.Background(),
		&syncScheduler{},
		handle,
		"missing-cluster",
		nil, nil, 1000,
	)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "callout init failed")
}

func TestCallout_ContextCancelled(t *testing.T) {
	// Schedule runs the fn but the callback is never called — ctx is cancelled first.
	neverCallbackHandle := &neverCallbackMockHandle{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	resp, err := jisr.Do(ctx, &syncScheduler{}, neverCallbackHandle, "cluster", nil, nil, 1000)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, resp)
}

// neverCallbackMockHandle initiates the callout but never calls the callback.
// Used to simulate a pending callout when context is cancelled.
type neverCallbackMockHandle struct {
	jisr.EmptyHttpFilterHandle
}

func (h *neverCallbackMockHandle) HttpCallout(
	_ string, _ [][2]string, _ []byte, _ uint64, _ shared.HttpCalloutCallback,
) (shared.HttpCalloutInitResult, uint64) {
	return shared.HttpCalloutInitSuccess, 1 // initiated but callback never fires
}

func TestCallout_NoBodyChunks(t *testing.T) {
	// Response with headers but empty body.
	handle := &mockCalloutHandle{
		initResult:    shared.HttpCalloutInitSuccess,
		calloutResult: shared.HttpCalloutSuccess,
		callbackHeaders: [][2]shared.UnsafeEnvoyBuffer{
			makeUnsafePair(":status", "204"),
		},
		callbackBody: nil,
	}

	resp, err := jisr.Do(context.Background(), &syncScheduler{}, handle, "c", nil, nil, 1000)
	require.NoError(t, err)
	assert.Equal(t, 204, resp.StatusCode)
	assert.Empty(t, resp.Body)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func makeUnsafeBuffer(s string) shared.UnsafeEnvoyBuffer {
	b := []byte(s)
	return shared.UnsafeEnvoyBuffer{
		Ptr: &b[0],
		Len: uint64(len(b)),
	}
}

func makeUnsafePair(k, v string) [2]shared.UnsafeEnvoyBuffer {
	return [2]shared.UnsafeEnvoyBuffer{makeUnsafeBuffer(k), makeUnsafeBuffer(v)}
}

// Ensure makeUnsafePair is used (suppress unused warning).
var _ = fmt.Sprintf
