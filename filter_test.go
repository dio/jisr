package jisr_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dio/jisr"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── fakeHandle ────────────────────────────────────────────────────────────────

type fakeHandle struct {
	jisr.EmptyHttpFilterHandle

	mu              sync.Mutex
	reqHeaders      *fake.FakeHeaderMap
	upstreamRespHdrs *fake.FakeHeaderMap // response headers from upstream
	metadata        map[string]any
	localResp       *localResponse
	respHeaders     []responseHeaderSent
	respData        []responseDataSent
	continued       bool
	continuedResp   bool
	scheduler       *fakeScheduler
}

type localResponse struct {
	status  uint32
	headers [][2]string
	body    []byte
}

type responseHeaderSent struct {
	headers   [][2]string
	endStream bool
}

type responseDataSent struct {
	data      []byte
	endStream bool
}

func newFakeHandle(headers map[string][]string) *fakeHandle {
	h := &fakeHandle{
		reqHeaders: fake.NewFakeHeaderMap(headers),
		metadata:   make(map[string]any),
	}
	h.scheduler = &fakeScheduler{handle: h}
	return h
}

func (h *fakeHandle) GetScheduler() shared.Scheduler { return h.scheduler }

func (h *fakeHandle) RequestHeaders() shared.HeaderMap {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reqHeaders
}

func (h *fakeHandle) SendLocalResponse(status uint32, headers [][2]string, body []byte, _ string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.localResp = &localResponse{status: status, headers: headers, body: body}
}

func (h *fakeHandle) SendResponseHeaders(headers [][2]string, endStream bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.respHeaders = append(h.respHeaders, responseHeaderSent{headers, endStream})
}

func (h *fakeHandle) SendResponseData(data []byte, endStream bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.respData = append(h.respData, responseDataSent{data, endStream})
}

func (h *fakeHandle) SetMetadata(namespace, key string, value any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.metadata[namespace+"/"+key] = value
}

func (h *fakeHandle) ContinueRequest() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.continued = true
}

func (h *fakeHandle) ContinueResponse() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.continuedResp = true
}

func (h *fakeHandle) ResponseHeaders() shared.HeaderMap {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.upstreamRespHdrs == nil {
		h.upstreamRespHdrs = fake.NewFakeHeaderMap(nil)
	}
	return h.upstreamRespHdrs
}

func (h *fakeHandle) Log(_ shared.LogLevel, _ string, _ ...any) {}

// fakeScheduler runs scheduled functions synchronously in tests.
type fakeScheduler struct {
	handle *fakeHandle
}

func (s *fakeScheduler) Schedule(fn func()) { fn() }

// ── harness ───────────────────────────────────────────────────────────────────

type harness struct {
	filter shared.HttpFilter
	handle *fakeHandle
}

func newHarness(t *testing.T, fn jisr.HandlerFunc) *harness {
	t.Helper()
	name := t.Name()
	jisr.Register(name, fn)
	t.Cleanup(func() { jisr.Unregister(name) })

	factories := jisr.WellKnownHttpFilterConfigFactories()
	factory, ok := factories[name]
	require.True(t, ok)

	filterFactory, err := factory.Create(nil, nil)
	require.NoError(t, err)

	handle := newFakeHandle(nil)
	filter := filterFactory.Create(handle)
	return &harness{filter: filter, handle: handle}
}

func newHarnessWithResponse(t *testing.T, fn jisr.HandlerFunc, rfn jisr.ResponseFunc) *harness {
	t.Helper()
	name := t.Name()
	jisr.RegisterWithResponse(name, fn, rfn)
	t.Cleanup(func() { jisr.Unregister(name) })

	factories := jisr.WellKnownHttpFilterConfigFactories()
	factory, ok := factories[name]
	require.True(t, ok)

	filterFactory, err := factory.Create(nil, nil)
	require.NoError(t, err)

	handle := newFakeHandle(nil)
	filter := filterFactory.Create(handle)
	return &harness{filter: filter, handle: handle}
}

func (h *harness) headers(hdrs map[string][]string, endStream bool) shared.HeadersStatus {
	h.handle.mu.Lock()
	h.handle.reqHeaders = fake.NewFakeHeaderMap(hdrs)
	h.handle.mu.Unlock()
	return h.filter.OnRequestHeaders(h.handle.reqHeaders, endStream)
}

func (h *harness) body(data []byte, endStream bool) shared.BodyStatus {
	return h.filter.OnRequestBody(fake.NewFakeBodyBuffer(data), endStream)
}

func (h *harness) respHeaders(hdrs map[string][]string, endStream bool) shared.HeadersStatus {
	h.handle.mu.Lock()
	h.handle.upstreamRespHdrs = fake.NewFakeHeaderMap(hdrs)
	h.handle.mu.Unlock()
	return h.filter.OnResponseHeaders(fake.NewFakeHeaderMap(hdrs), endStream)
}

func (h *harness) respBody(data []byte, endStream bool) shared.BodyStatus {
	return h.filter.OnResponseBody(fake.NewFakeBodyBuffer(data), endStream)
}

// waitResponse polls until ContinueResponse is set, with a timeout.
func (h *harness) waitResponse(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.handle.mu.Lock()
		done := h.handle.continuedResp
		h.handle.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("response handler did not complete within 3s — possible deadlock")
}

// wait polls until ContinueRequest or SendLocalResponse is set, with a timeout.
func (h *harness) wait(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.handle.mu.Lock()
		done := h.handle.continued || h.handle.localResp != nil
		h.handle.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("handler goroutine did not complete within 3s — possible deadlock")
}

// waitStream polls until SendResponseHeaders has been called at least once.
func (h *harness) waitStream(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.handle.mu.Lock()
		n := len(h.handle.respHeaders)
		h.handle.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Stream() did not call SendResponseHeaders within 3s")
}

// ── jisr bridge tests ─────────────────────────────────────────────────────────

func TestForwardsUpstream_WhenHandlerReturns(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		// no w.Send → should ContinueRequest
	})

	status := h.headers(map[string][]string{":path": {"/"}}, true)
	assert.Equal(t, shared.HeadersStatusStop, status)

	h.wait(t)
	assert.True(t, h.handle.continued)
	assert.Nil(t, h.handle.localResp)
}

func TestSend_SendsLocalResponse(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		w.Send(http.StatusUnauthorized, `{"error":"no key"}`)
	})

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)

	require.NotNil(t, h.handle.localResp)
	assert.Equal(t, uint32(http.StatusUnauthorized), h.handle.localResp.status)
	assert.Equal(t, `{"error":"no key"}`, string(h.handle.localResp.body))
	assert.False(t, h.handle.continued, "ContinueRequest must not be called after Send")
}

func TestSendBytes_SendsLocalResponse(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		w.SendBytes(http.StatusOK, []byte(`{"ok":true}`))
	})

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)

	require.NotNil(t, h.handle.localResp)
	assert.Equal(t, uint32(http.StatusOK), h.handle.localResp.status)
	assert.False(t, h.handle.continued)
}

func TestSetResponseHeader_IncludedInLocalResponse(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		w.SetResponseHeader("content-type", "application/json")
		w.Send(http.StatusOK, `{}`)
	})

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)

	require.NotNil(t, h.handle.localResp)
	var found bool
	for _, kv := range h.handle.localResp.headers {
		if kv[0] == "content-type" && kv[1] == "application/json" {
			found = true
		}
	}
	assert.True(t, found, "content-type header missing from local response")
}

func TestHeaders_CopiedIntoGoMemory(t *testing.T) {
	var got string
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		// Envoy sends headers lowercase; filter stores them as-is.
		// http.Header.Get canonicalises the key, so both work.
		got = r.Header.Get("x-api-key")
	})

	h.headers(map[string][]string{"x-api-key": {"secret"}}, true)
	h.wait(t)
	assert.Equal(t, "secret", got)
}

func TestBody_AssemblesChunks(t *testing.T) {
	var body []byte
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		body, _ = io.ReadAll(r.Body)
	})

	h.headers(map[string][]string{":path": {"/"}}, false)
	h.body([]byte("hello "), false)
	h.body([]byte("world"), true)
	h.wait(t)

	assert.Equal(t, "hello world", string(body))
}

func TestBody_EmptyWhenEndStreamOnHeaders(t *testing.T) {
	var body []byte
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		body, _ = io.ReadAll(r.Body)
	})

	h.headers(map[string][]string{":path": {"/"}}, true) // endStream=true
	h.wait(t)
	assert.Empty(t, body)
}

func TestSkipBody_PassthroughChunks(t *testing.T) {
	skipDone := make(chan struct{})
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		close(skipDone)
	})

	h.headers(map[string][]string{":path": {"/"}}, false)

	// Wait until goroutine has called SkipBody before sending body chunks.
	select {
	case <-skipDone:
	case <-time.After(time.Second):
		t.Fatal("SkipBody not called in time")
	}

	status := h.body([]byte("large chunk"), false)
	assert.Equal(t, shared.BodyStatusContinue, status)

	status = h.body([]byte("last chunk"), true)
	assert.Equal(t, shared.BodyStatusContinue, status)

	h.wait(t)
	assert.True(t, h.handle.continued)
}

func TestSkipBody_CalledTwice_NoPanic(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		r.SkipBody() // second call must not panic (double-close protection)
	})

	assert.NotPanics(t, func() {
		h.headers(map[string][]string{":path": {"/"}}, true)
		h.wait(t)
	})
}

func TestSetRequestHeader_AppliedOnContinue(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		w.SetRequestHeader("x-injected", "yes")
	})

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)

	assert.True(t, h.handle.continued)
	vals := h.handle.reqHeaders.Headers["x-injected"]
	assert.Equal(t, []string{"yes"}, vals)
}

func TestSetMetadata_AppliedOnContinue(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		w.SetMetadata("my.ns", "user", "alice")
	})

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)

	assert.Equal(t, "alice", h.handle.metadata["my.ns/user"])
}

func TestOnDestroy_CancelsContext(t *testing.T) {
	ctxDone := make(chan struct{})
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		<-ctx.Done()
		close(ctxDone)
	})

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.filter.OnStreamComplete()

	select {
	case <-ctxDone:
	case <-time.After(time.Second):
		t.Fatal("context not cancelled after OnDestroy")
	}
}

func TestStream_SendsResponseHeaders(t *testing.T) {
	started := make(chan struct{})
	done := make(chan struct{})

	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		sw, err := w.Stream(ctx, [][2]string{
			{"content-type", "text/event-stream"},
		})
		require.NoError(t, err)
		close(started)

		sw.Flush(ctx, []byte("data: hello\n\n"))
		sw.Close()
		close(done)
	})

	h.headers(map[string][]string{":path": {"/events"}}, true)
	h.waitStream(t)

	<-done

	h.handle.mu.Lock()
	defer h.handle.mu.Unlock()

	require.Len(t, h.handle.respHeaders, 1)
	assert.Equal(t, "text/event-stream", h.handle.respHeaders[0].headers[0][1])

	require.Len(t, h.handle.respData, 2)
	assert.Equal(t, "data: hello\n\n", string(h.handle.respData[0].data))
	assert.False(t, h.handle.respData[0].endStream)
	assert.True(t, h.handle.respData[1].endStream) // Close()
}

func TestStream_AfterSend_ReturnsError(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		w.Send(http.StatusOK, "done")
		_, err := w.Stream(ctx, nil)
		assert.Error(t, err)
	})

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
}

func TestChain_MiddlewareOrder(t *testing.T) {
	var order []string

	mw1 := func(next jisr.HandlerFunc) jisr.HandlerFunc {
		return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
			order = append(order, "mw1-before")
			next(ctx, w, r)
			order = append(order, "mw1-after")
		}
	}
	mw2 := func(next jisr.HandlerFunc) jisr.HandlerFunc {
		return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
			order = append(order, "mw2-before")
			next(ctx, w, r)
			order = append(order, "mw2-after")
		}
	}
	handler := func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		order = append(order, "handler")
	}

	h := newHarness(t, jisr.Chain(handler, mw1, mw2))
	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)

	assert.Equal(t, []string{"mw1-before", "mw2-before", "handler", "mw2-after", "mw1-after"}, order)
}

// ── mockResponseWriter — for testing user handler functions in isolation ──────

type mockWriter struct {
	code    int
	body    string
	headers map[string]string
	meta    map[string]any
	mu      sync.Mutex
}

func newMockWriter() *mockWriter {
	return &mockWriter{headers: map[string]string{}, meta: map[string]any{}}
}

func (m *mockWriter) Send(code int, body string) {
	m.mu.Lock(); defer m.mu.Unlock()
	m.code = code; m.body = body
}
func (m *mockWriter) SendBytes(code int, body []byte) {
	m.mu.Lock(); defer m.mu.Unlock()
	m.code = code; m.body = string(body)
}
func (m *mockWriter) SetRequestHeader(k, v string) {
	m.mu.Lock(); defer m.mu.Unlock()
	m.headers[k] = v
}
func (m *mockWriter) SetResponseHeader(_, _ string)                                  {}
func (m *mockWriter) SetMetadata(ns, k string, v any) {
	m.mu.Lock(); defer m.mu.Unlock()
	m.meta[ns+"/"+k] = v
}
func (m *mockWriter) Stream(_ context.Context, _ [][2]string) (jisr.StreamWriter, error) {
	return nil, nil
}

// ── user handler unit tests (pure Go, no Envoy) ───────────────────────────────

func TestUserHandler_AuthRejectsEmptyKey(t *testing.T) {
	authHandler := func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		if r.Header.Get("x-api-key") == "" {
			w.Send(http.StatusUnauthorized, `{"error":"missing key"}`)
			return
		}
		w.SetRequestHeader("x-user-id", "alice")
	}

	w := newMockWriter()
	r := &jisr.Request{
		Header: http.Header{"X-Api-Key": {""}},
		Body:   strings.NewReader(""),
	}

	authHandler(context.Background(), w, r)
	assert.Equal(t, http.StatusUnauthorized, w.code)
	assert.JSONEq(t, `{"error":"missing key"}`, w.body)
}

func TestUserHandler_AuthAcceptsValidKey(t *testing.T) {
	authHandler := func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		if r.Header.Get("x-api-key") == "" {
			w.Send(http.StatusUnauthorized, `{"error":"missing key"}`)
			return
		}
		w.SetRequestHeader("x-user-id", "alice")
	}

	w := newMockWriter()
	r := &jisr.Request{
		Header: http.Header{"X-Api-Key": {"secret"}},
		Body:   strings.NewReader(""),
	}

	authHandler(context.Background(), w, r)
	assert.Zero(t, w.code, "no local response — request should forward")
	assert.Equal(t, "alice", w.headers["x-user-id"])
}

func TestUserHandler_EchoReadsBody(t *testing.T) {
	echoHandler := func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		w.SendBytes(http.StatusOK, body)
	}

	w := newMockWriter()
	r := &jisr.Request{
		Header: http.Header{},
		Body:   strings.NewReader(`{"hello":"world"}`),
	}

	echoHandler(context.Background(), w, r)
	assert.Equal(t, http.StatusOK, w.code)
	assert.JSONEq(t, `{"hello":"world"}`, w.body)
}

func TestUserHandler_ContextCancellation(t *testing.T) {
	handler := func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		select {
		case <-ctx.Done():
			w.Send(http.StatusServiceUnavailable, "cancelled")
		case <-time.After(100 * time.Millisecond):
			w.Send(http.StatusOK, "ok")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	w := newMockWriter()
	r := &jisr.Request{Header: http.Header{}, Body: strings.NewReader("")}
	handler(ctx, w, r)
	assert.Equal(t, http.StatusServiceUnavailable, w.code)
}

// ── LimitBody ─────────────────────────────────────────────────────────────────

// TestLimitBody_BodySmallerThanLimit: body fits within limit — all bytes
// delivered, handler sees the full body, no headDone triggered.
func TestLimitBody_BodySmallerThanLimit(t *testing.T) {
	var got []byte
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.LimitBody(8192)
		got, _ = io.ReadAll(r.Body)
	})

	h.headers(map[string][]string{":path": {"/"}}, false)
	// 100 bytes — well under 8192 limit
	status := h.body([]byte("hello world -- small body under limit -- padding padding padding padding padding padding paddingg"), true)
	h.wait(t)

	assert.Equal(t, shared.BodyStatusStopAndBuffer, status)
	assert.Equal(t, "hello world -- small body under limit -- padding padding padding padding padding padding paddingg", string(got))
}

// TestLimitBody_BodyLargerThanLimit: body exceeds limit — handler gets exactly
// n bytes, then EOF. OnRequestBody switches to Continue for the remainder.
func TestLimitBody_BodyLargerThanLimit(t *testing.T) {
	const limit = 10
	var got []byte
	var bodyBytesRead int
	limitReached := make(chan struct{})

	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.LimitBody(limit)
		got, _ = io.ReadAll(r.Body)
		bodyBytesRead = len(got)
		close(limitReached)
		<-ctx.Done()
	})

	h.headers(map[string][]string{":path": {"/"}}, false)

	// First chunk: 6 bytes — within limit, still buffering
	s1 := h.body([]byte("hello "), false)
	// Second chunk: 6 bytes — crosses the 10-byte limit
	s2 := h.body([]byte("world!"), false)

	// Wait for goroutine to consume past the limit and set headDone.
	select {
	case <-limitReached:
	case <-time.After(time.Second):
		t.Fatal("limit not reached")
	}

	// Third chunk: arrives after headDone — must return Continue, not StopAndBuffer
	s3 := h.body([]byte("TAIL"), true)

	h.filter.OnStreamComplete()
	h.wait(t)

	assert.Equal(t, shared.BodyStatusStopAndBuffer, s1, "before limit: StopAndBuffer")
	assert.Equal(t, shared.BodyStatusStopAndBuffer, s2, "chunk crossing limit: StopAndBuffer")
	assert.Equal(t, shared.BodyStatusContinue, s3, "after limit: Continue (stream through)")
	assert.Equal(t, limit, bodyBytesRead, "handler received exactly limit bytes")
	assert.Equal(t, "hello worl", string(got))
}

// TestLimitBody_ExactlyAtLimit: body length equals limit exactly.
// All bytes delivered, then natural EOF — headDone fires at the boundary.
func TestLimitBody_ExactlyAtLimit(t *testing.T) {
	const payload = "0123456789" // 10 bytes
	var got []byte

	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.LimitBody(int64(len(payload)))
		got, _ = io.ReadAll(r.Body)
	})

	h.headers(map[string][]string{":path": {"/"}}, false)
	h.body([]byte(payload), true)
	h.wait(t)

	assert.Equal(t, payload, string(got))
}

// TestLimitBody_MultipleChunksBeforeLimit: many small chunks, limit hit
// mid-stream. Only chunks up to limit are copied into Go memory.
func TestLimitBody_MultipleChunksBeforeLimit(t *testing.T) {
	const limit = 5
	var got []byte
	limitReached := make(chan struct{})

	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.LimitBody(limit)
		got, _ = io.ReadAll(r.Body)
		close(limitReached)
		<-ctx.Done()
	})

	h.headers(map[string][]string{":path": {"/"}}, false)
	s1 := h.body([]byte("AB"), false) // 2 bytes — buffered
	s2 := h.body([]byte("CD"), false) // 4 bytes total — buffered
	s3 := h.body([]byte("EF"), false) // 6 bytes — crosses limit at byte 5

	// Wait for goroutine to read past the limit.
	select {
	case <-limitReached:
	case <-time.After(time.Second):
		t.Fatal("limit not reached")
	}

	s4 := h.body([]byte("GH"), true) // after limit — stream through

	h.filter.OnStreamComplete()
	h.wait(t)

	assert.Equal(t, shared.BodyStatusStopAndBuffer, s1)
	assert.Equal(t, shared.BodyStatusStopAndBuffer, s2)
	assert.Equal(t, shared.BodyStatusStopAndBuffer, s3)
	assert.Equal(t, shared.BodyStatusContinue, s4, "tail chunk must stream, not buffer")
	assert.Equal(t, limit, len(got))
	assert.Equal(t, "ABCDE", string(got))
}

// TestLimitBody_NoLimitSet: LimitBody not called — full body delivered as before.
func TestLimitBody_NoLimitSet(t *testing.T) {
	var got []byte
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		got, _ = io.ReadAll(r.Body)
	})

	h.headers(map[string][]string{":path": {"/"}}, false)
	h.body([]byte("full body no limit"), true)
	h.wait(t)

	assert.Equal(t, "full body no limit", string(got))
}

// TestLimitBody_OnRequestBodyReturnsCorrectStatus verifies the status sequence:
// StopAndBuffer while buffering, Continue once headDone is set.
func TestLimitBody_StatusTransition(t *testing.T) {
	ready := make(chan struct{})

	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.LimitBody(4)
		io.ReadAll(r.Body)
		close(ready)
		// Hold goroutine open so we can observe subsequent body statuses.
		<-ctx.Done()
	})

	h.headers(map[string][]string{":path": {"/"}}, false)
	s1 := h.body([]byte("AB"), false)
	s2 := h.body([]byte("CD"), false) // hits limit exactly at 4

	// Wait for handler to read the limit and signal headDone.
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("handler did not reach limit")
	}

	s3 := h.body([]byte("EF"), false) // after headDone — must Continue
	s4 := h.body([]byte("GH"), true)  // final chunk — must Continue

	h.filter.OnStreamComplete()
	h.wait(t)

	assert.Equal(t, shared.BodyStatusStopAndBuffer, s1)
	assert.Equal(t, shared.BodyStatusStopAndBuffer, s2)
	assert.Equal(t, shared.BodyStatusContinue, s3)
	assert.Equal(t, shared.BodyStatusContinue, s4)
}

// ── Response phase ────────────────────────────────────────────────────────────

// TestResponse_Passthrough: default mode — handler sees headers, body streams
// through without copying into Go memory. ContinueResponse is called.
func TestResponse_Passthrough(t *testing.T) {
	var gotStatus int
	var gotContentType string

	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
			r.SkipBody()
		},
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			r.Passthrough()
			gotStatus = r.StatusCode
			gotContentType = r.Header.Get("Content-Type")
		},
	)

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)

	// Simulate upstream response headers (eos=true, no body).
	status := h.respHeaders(map[string][]string{
		":status":      {"200"},
		"content-type": {"application/json"},
	}, true)
	h.waitResponse(t)

	assert.Equal(t, shared.HeadersStatusStop, status)
	assert.Equal(t, 200, gotStatus)
	assert.Equal(t, "application/json", gotContentType)
	assert.True(t, h.handle.continuedResp)
}

// TestResponse_Passthrough_BodyNotBuffered: in passthrough mode, response body
// chunks return Continue immediately — never enter Go memory.
func TestResponse_Passthrough_BodyNotBuffered(t *testing.T) {
	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			r.Passthrough()
		},
	)

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
	h.respHeaders(map[string][]string{":status": {"200"}}, false)
	h.waitResponse(t)

	// Body chunks in passthrough mode must return Continue immediately.
	s1 := h.respBody([]byte("chunk1"), false)
	s2 := h.respBody([]byte("chunk2"), true)
	assert.Equal(t, shared.BodyStatusContinue, s1)
	assert.Equal(t, shared.BodyStatusContinue, s2)
}

// TestResponse_Observe: tap mode — handler reads body chunks via r.Body
// while they simultaneously flow to downstream. OnResponseBody returns Continue.
func TestResponse_Observe(t *testing.T) {
	var tapped []byte

	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			r.Observe()
			tapped, _ = io.ReadAll(r.Body)
		},
	)

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
	h.respHeaders(map[string][]string{":status": {"200"}}, false)

	s1 := h.respBody([]byte("hello "), false)
	s2 := h.respBody([]byte("world"), true)
	h.waitResponse(t)

	// Observe returns Continue — body flows to downstream simultaneously.
	assert.Equal(t, shared.BodyStatusContinue, s1)
	assert.Equal(t, shared.BodyStatusContinue, s2)
	assert.Equal(t, "hello world", string(tapped))
	assert.True(t, h.handle.continuedResp)
}

// TestResponse_Buffer: handler reads complete body before ContinueResponse.
// OnResponseBody returns StopAndBuffer.
func TestResponse_Buffer(t *testing.T) {
	var buffered []byte

	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			r.Buffer()
			buffered, _ = io.ReadAll(r.Body)
		},
	)

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
	h.respHeaders(map[string][]string{":status": {"200"}}, false)

	s1 := h.respBody([]byte("chunk1"), false)
	s2 := h.respBody([]byte("chunk2"), true)
	h.waitResponse(t)

	assert.Equal(t, shared.BodyStatusStopAndBuffer, s1)
	assert.Equal(t, shared.BodyStatusStopAndBuffer, s2)
	assert.Equal(t, "chunk1chunk2", string(buffered))
	assert.True(t, h.handle.continuedResp)
}

// TestResponse_NoRespHandler: filter registered without ResponseFunc — response
// callbacks return Continue immediately, no goroutine involvement.
func TestResponse_NoRespHandler(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
	})

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)

	s := h.respHeaders(map[string][]string{":status": {"200"}}, false)
	assert.Equal(t, shared.HeadersStatusContinue, s)

	sb := h.respBody([]byte("data"), true)
	assert.Equal(t, shared.BodyStatusContinue, sb)
}

// TestResponse_DefaultPassthrough: ResponseFunc declared but no mode called —
// defaults to Passthrough. ContinueResponse must still be called.
func TestResponse_DefaultPassthrough(t *testing.T) {
	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			// no mode declared — defaults to Passthrough
			_ = r.StatusCode // just read headers
		},
	)

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
	h.respHeaders(map[string][]string{":status": {"204"}}, true)
	h.waitResponse(t)

	assert.True(t, h.handle.continuedResp)
}

// TestResponse_ContextCancelledOnStreamComplete: client disconnect cancels
// the context, unblocking a ResponseFunc waiting on r.Body.
func TestResponse_ContextCancelledOnStreamComplete(t *testing.T) {
	respStarted := make(chan struct{})
	respDone := make(chan struct{})

	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			r.Observe()
			close(respStarted)
			// Block reading body — will unblock when ctx is cancelled.
			buf := make([]byte, 4)
			for {
				_, err := r.Body.Read(buf)
				if err != nil {
					break
				}
			}
			close(respDone)
		},
	)

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
	h.respHeaders(map[string][]string{":status": {"200"}}, false)

	// Wait for ResponseFunc to start blocking on Body.
	select {
	case <-respStarted:
	case <-time.After(time.Second):
		t.Fatal("response handler did not start")
	}

	// Simulate client disconnect.
	h.filter.OnStreamComplete()

	select {
	case <-respDone:
	case <-time.After(time.Second):
		t.Fatal("response handler did not unblock after OnStreamComplete")
	}
}
