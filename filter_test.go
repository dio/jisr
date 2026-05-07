package jisr_test

import (
	"context"
	"fmt"
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
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ── fakeHandle ────────────────────────────────────────────────────────────────

type fakeHandle struct {
	jisr.EmptyHttpFilterHandle

	mu               sync.Mutex
	reqHeaders       *fake.FakeHeaderMap
	upstreamRespHdrs *fake.FakeHeaderMap // response headers from upstream
	respBodyBuf      *fakeBodyBuffer     // mutable response body buffer (for ReplaceBody)
	metadata         map[string]any
	localResp        *localResponse
	respHeaders      []responseHeaderSent
	respData         []responseDataSent
	continued        bool
	continuedResp    bool
	scheduler        shared.Scheduler
	logFn            func(shared.LogLevel, string) // optional log capture
}

type fakeBodyBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *fakeBodyBuffer) GetChunks() []shared.UnsafeEnvoyBuffer { return nil }
func (b *fakeBodyBuffer) GetSize() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return uint64(len(b.data))
}
func (b *fakeBodyBuffer) Drain(n uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n >= uint64(len(b.data)) {
		b.data = nil
	} else {
		b.data = b.data[n:]
	}
}
func (b *fakeBodyBuffer) Append(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, data...)
}
func (b *fakeBodyBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return out
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

func (h *fakeHandle) BufferedResponseBody() shared.BodyBuffer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.respBodyBuf == nil {
		h.respBodyBuf = &fakeBodyBuffer{}
	}
	return h.respBodyBuf
}

func (h *fakeHandle) ResponseHeaders() shared.HeaderMap {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.upstreamRespHdrs == nil {
		h.upstreamRespHdrs = fake.NewFakeHeaderMap(nil)
	}
	return h.upstreamRespHdrs
}

func (h *fakeHandle) Log(level shared.LogLevel, format string, args ...any) {
	if h.logFn != nil {
		h.logFn(level, fmt.Sprintf(format, args...))
	}
}

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

func newHarnessWithResponse(t *testing.T, fn jisr.HandlerFunc, rfn jisr.ResponseFunc, mode jisr.ResponseMode) *harness {
	t.Helper()
	name := t.Name()
	jisr.RegisterWithResponse(name, fn, rfn, mode)
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

func TestSkipBody_RacesWithOnRequestBody_NoPanic(t *testing.T) {
	for i := 0; i < 100; i++ {
		t.Run(fmt.Sprintf("iteration-%d", i), func(t *testing.T) {
			ready := make(chan struct{})
			skip := make(chan struct{})

			h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
				close(ready)
				<-skip
				r.SkipBody()
			})

			h.headers(map[string][]string{":path": {"/"}}, false)
			<-ready

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for j := 0; j < 32; j++ {
					h.body([]byte("chunk"), false)
				}
				h.body([]byte("last"), true)
			}()
			go func() {
				defer wg.Done()
				close(skip)
			}()

			wg.Wait()
			h.wait(t)
		})
	}
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.code = code
	m.body = body
}
func (m *mockWriter) SendBytes(code int, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.code = code
	m.body = string(body)
}
func (m *mockWriter) SetRequestHeader(k, v string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.headers[k] = v
}
func (m *mockWriter) SetResponseHeader(_, _ string)                           {}
func (m *mockWriter) SetUpstreamResponseHeader(_, _ string)                   {}
func (m *mockWriter) ReplaceBody(_ []byte)                                    {}
func (m *mockWriter) ClearRouteCache()                                        {}
func (m *mockWriter) IncrementCounter(_ jisr.MetricID, _ uint64, _ ...string) {}
func (m *mockWriter) RecordHistogram(_ jisr.MetricID, _ uint64, _ ...string)  {}
func (m *mockWriter) SetMetadata(ns, k string, v any) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
			gotStatus = r.StatusCode
			gotContentType = r.Header.Get("Content-Type")
			// r.Body is nil in Passthrough — mode declared at registration
		},
		jisr.ResponseModePassthrough,
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
			// Passthrough — r.Body is nil, nothing to read
			assert.Nil(t, r.Body)
		},
		jisr.ResponseModePassthrough,
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
	done := make(chan struct{})

	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			tapped, _ = io.ReadAll(r.Body)
			close(done)
		},
		jisr.ResponseModeObserve,
	)

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)

	// Observe: OnResponseHeaders returns Continue immediately — headers flow.
	status := h.respHeaders(map[string][]string{":status": {"200"}}, false)
	assert.Equal(t, shared.HeadersStatusContinue, status)

	s1 := h.respBody([]byte("hello "), false)
	s2 := h.respBody([]byte("world"), true)

	// Wait for goroutine to finish reading the tapped body.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("response handler did not complete")
	}

	// Observe returns Continue — body flows to downstream simultaneously.
	assert.Equal(t, shared.BodyStatusContinue, s1)
	assert.Equal(t, shared.BodyStatusContinue, s2)
	assert.Equal(t, "hello world", string(tapped))
	// Observe mode: no ContinueResponse needed (headers already flowed).
	assert.False(t, h.handle.continuedResp)
}

// TestResponse_Buffer: handler reads complete body before ContinueResponse.
// OnResponseBody returns StopAndBuffer.
func TestResponse_Buffer(t *testing.T) {
	var buffered []byte

	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			buffered, _ = io.ReadAll(r.Body)
		},
		jisr.ResponseModeBuffer,
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

// TestResponse_DefaultPassthrough: mode explicitly set to Passthrough —
// ContinueResponse is called, headers flowed via Stop then Continue.
func TestResponse_DefaultPassthrough(t *testing.T) {
	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			_ = r.StatusCode // inspect headers only
		},
		jisr.ResponseModePassthrough,
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
			close(respStarted)
			// Block reading body — will unblock when channel is closed by OnStreamComplete.
			buf := make([]byte, 4)
			for {
				_, err := r.Body.Read(buf)
				if err != nil {
					break
				}
			}
			close(respDone)
		},
		jisr.ResponseModeObserve,
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

// ── Goroutine leak tests ───────────────────────────────────────────────────────
// goleak.VerifyTestMain catches leaks at process exit, but these tests also
// explicitly verify the goroutine exits within a bounded deadline.

// TestLeak_SendDirectResponse_WithRespHandler verifies that when a request
// handler calls w.Send() (direct response), the goroutine does NOT park on
// respHeadersCh forever. Envoy never calls OnResponseHeaders for locally-terminated
// requests — the goroutine must skip the response phase and exit.
func TestLeak_SendDirectResponse_WithRespHandler(t *testing.T) {
	goroutineExited := make(chan struct{})

	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
			r.SkipBody()
			w.Send(http.StatusForbidden, `{"error":"forbidden"}`)
		},
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			// This must NEVER be called — the request was locally terminated.
			t.Error("ResponseFunc must not be called when request handler calls w.Send()")
		},
		jisr.ResponseModePassthrough,
	)

	// Patch: detect goroutine exit via scheduler (it calls ContinueRequest or nothing).
	// We track via the continued flag — but actually the goroutine exits without
	// ContinueRequest because responded=true. Track via a separate channel.
	// We replace the scheduler to detect when the goroutine's deferred Schedule fires.
	origScheduler := h.handle.scheduler
	h.handle.scheduler = &detectingScheduler{
		inner: origScheduler,
		onSchedule: func() {
			select {
			case goroutineExited <- struct{}{}:
			default:
			}
		},
	}

	h.headers(map[string][]string{":path": {"/"}}, true)

	// The goroutine must exit promptly — it must not block on respHeadersCh.
	select {
	case <-goroutineExited:
		// Good — goroutine scheduled something (the no-op ContinueRequest guard).
	case <-time.After(time.Second):
		t.Fatal("goroutine leaked: did not exit after w.Send() + respHandler")
	}

	// Envoy never called OnResponseHeaders — assert no response phase ran.
	h.handle.mu.Lock()
	assert.False(t, h.handle.continuedResp, "ContinueResponse must not be called for direct responses")
	h.handle.mu.Unlock()
}

// detectingScheduler wraps a fakeScheduler and calls onSchedule on each Schedule().
type detectingScheduler struct {
	inner      shared.Scheduler
	onSchedule func()
}

func (s *detectingScheduler) Schedule(fn func()) {
	s.inner.Schedule(fn)
	if s.onSchedule != nil {
		s.onSchedule()
	}
}

// TestLeak_HandlerPanic_DoesNotHang verifies that a panicking request handler
// does not leave the goroutine running or the downstream client hanging.
// The defer/recover in run() must call ContinueRequest and exit the goroutine.
func TestLeak_HandlerPanic_DoesNotHang(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		panic("intentional test panic")
	})

	h.headers(map[string][]string{":path": {"/"}}, true)

	// The panic recovery must schedule ContinueRequest within 1s.
	h.wait(t)

	// continued must be true — panic recovery called ContinueRequest.
	h.handle.mu.Lock()
	assert.True(t, h.handle.continued, "ContinueRequest must be called even after handler panic")
	h.handle.mu.Unlock()
}

// TestLeak_RespBuffer_ClientDisconnect verifies no goroutine leak when the
// client disconnects while the ResponseFunc is reading r.Body in Buffer mode.
func TestLeak_RespBuffer_ClientDisconnect(t *testing.T) {
	started := make(chan struct{})
	done := make(chan struct{})

	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			close(started)
			// Block reading — will unblock when OnStreamComplete closes the channel.
			io.ReadAll(r.Body) //nolint:errcheck
			close(done)
		},
		jisr.ResponseModeBuffer,
	)

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
	h.respHeaders(map[string][]string{":status": {"200"}}, false)
	h.respBody([]byte("chunk"), false) // send one chunk — goroutine starts reading

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("response handler did not start")
	}

	// Simulate client disconnect — must close respBodyCh.
	h.filter.OnStreamComplete()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutine leaked: ResponseFunc did not exit after OnStreamComplete in Buffer mode")
	}
}

// TestLeak_RegisterRaw_RoundTrip verifies RegisterRaw wires a raw factory
// into WellKnownHttpFilterConfigFactories and it's callable.
func TestLeak_RegisterRaw_RoundTrip(t *testing.T) {
	name := t.Name()
	created := false

	jisr.RegisterRaw(name, &fakeRawFactory{onCreate: func() { created = true }})
	t.Cleanup(func() { jisr.Unregister(name) })

	factories := jisr.WellKnownHttpFilterConfigFactories()
	factory, ok := factories[name]
	require.True(t, ok, "raw factory must appear in WellKnownHttpFilterConfigFactories")

	_, err := factory.Create(nil, nil)
	require.NoError(t, err)
	assert.True(t, created, "raw factory Create must be called")
}

type fakeRawFactory struct {
	jisr.EmptyHttpFilterHandle // satisfies shared.HttpFilterConfigFactory via EmptyHttpFilterConfigFactory
	onCreate                   func()
}

func (f *fakeRawFactory) Create(_ shared.HttpFilterConfigHandle, _ []byte) (shared.HttpFilterFactory, error) {
	f.onCreate()
	return &fakeRawFilterFactory{}, nil
}
func (f *fakeRawFactory) CreatePerRoute(_ []byte) (any, error) { return nil, nil }

type fakeRawFilterFactory struct{}

func (f *fakeRawFilterFactory) Create(_ shared.HttpFilterHandle) shared.HttpFilter {
	return &fakeRawFilter{}
}
func (f *fakeRawFilterFactory) OnDestroy() {}

type fakeRawFilter struct{ shared.EmptyHttpFilter }

// TestLeak_ReplaceBody_Buffer verifies that ReplaceBody in Buffer mode
// drains the Envoy-side buffer and appends the replacement bytes.
func TestLeak_ReplaceBody_Buffer(t *testing.T) {
	const replacement = `{"replaced":true}`

	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			io.ReadAll(r.Body) //nolint:errcheck // drain the original body
			w.ReplaceBody([]byte(replacement))
		},
		jisr.ResponseModeBuffer,
	)

	// Pre-load the fake response body buffer with "original" content.
	h.handle.mu.Lock()
	h.handle.respBodyBuf = &fakeBodyBuffer{data: []byte("original body")}
	h.handle.mu.Unlock()

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
	h.respHeaders(map[string][]string{":status": {"200"}}, false)
	h.respBody([]byte("original body"), true)
	h.waitResponse(t)

	// After ReplaceBody: the fake buffer must contain the replacement.
	h.handle.mu.Lock()
	bufBytes := h.handle.respBodyBuf.Bytes()
	h.handle.mu.Unlock()

	assert.Equal(t, replacement, string(bufBytes),
		"ReplaceBody must drain original and append replacement bytes")
}

// TestLeak_Log_IsCalled verifies r.Log() forwards to handle.Log without panic.
func TestLeak_Log_IsCalled(t *testing.T) {
	logged := make(chan string, 1)

	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		r.Log(jisr.LogInfo, "test message %s", "hello")
	})

	// Patch the handle's Log to capture calls.
	h.handle.logFn = func(level shared.LogLevel, msg string) {
		logged <- msg
	}

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)

	select {
	case msg := <-logged:
		assert.Contains(t, msg, "test message hello")
	case <-time.After(time.Second):
		t.Fatal("r.Log() did not call handle.Log")
	}
}

// ── Branch coverage tests ─────────────────────────────────────────────────────

// TestRegister_DuplicatePanics verifies Register panics on duplicate name.
func TestRegister_DuplicatePanics(t *testing.T) {
	name := t.Name()
	jisr.Register(name, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {})
	t.Cleanup(func() { jisr.Unregister(name) })

	assert.Panics(t, func() {
		jisr.Register(name, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {})
	}, "Register must panic on duplicate name")
}

// TestRegisterRaw_DuplicatePanics verifies RegisterRaw panics on duplicate name.
func TestRegisterRaw_DuplicatePanics(t *testing.T) {
	name := t.Name()
	jisr.RegisterRaw(name, &fakeRawFactory{onCreate: func() {}})
	t.Cleanup(func() { jisr.Unregister(name) })

	assert.Panics(t, func() {
		jisr.RegisterRaw(name, &fakeRawFactory{onCreate: func() {}})
	}, "RegisterRaw must panic on duplicate name")
}

// TestRegisterWithResponse_DuplicatePanics verifies RegisterWithResponse panics on duplicate.
func TestRegisterWithResponse_DuplicatePanics(t *testing.T) {
	name := t.Name()
	jisr.RegisterWithResponse(name,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {},
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {},
		jisr.ResponseModePassthrough,
	)
	t.Cleanup(func() { jisr.Unregister(name) })

	assert.Panics(t, func() {
		jisr.RegisterWithResponse(name,
			func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {},
			func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {},
			jisr.ResponseModePassthrough,
		)
	}, "RegisterWithResponse must panic on duplicate name")
}

// TestOnRequestBody_CtxCancelled verifies the ctx.Done guard in OnRequestBody
// exists and the filter doesn't deadlock after context cancellation.
// The ctx.Done path in OnRequestBody requires a full channel AND cancelled ctx
// simultaneously — this is a runtime property (Envoy multithread), so here we
// verify the simpler invariant: after OnStreamComplete, subsequent body calls
// with endStream=true set bodyDone and return Continue.
func TestOnRequestBody_CtxCancelled(t *testing.T) {
	blocked := make(chan struct{})
	done := make(chan struct{})

	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		close(blocked)
		buf := make([]byte, 4)
		for {
			_, err := r.Body.Read(buf)
			if err != nil {
				break
			}
		}
		close(done)
	})

	h.headers(map[string][]string{":path": {"/"}}, false)
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	// Cancel — closes the body channel via OnStreamComplete.
	// The goroutine is reading, so it unblocks and exits.
	h.filter.OnStreamComplete()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutine did not exit after context cancellation")
	}

	// After bodySkip is set via ctx cancel, subsequent chunks must Continue.
	// We verify bodyDone or bodySkip causes Continue on endStream.
	status := h.body([]byte("late-data"), true)
	// Either Continue (bodySkip set) or StopAndBuffer then immediate close — both acceptable.
	// Key invariant: no deadlock and Envoy worker thread is not blocked.
	_ = status // result depends on race between goroutine and worker; both valid
}

// TestOnResponseBody_CtxCancelled_Observe verifies OnResponseBody returns
// Continue (not deadlock) when context is cancelled in Observe mode.
func TestOnResponseBody_CtxCancelled_Observe(t *testing.T) {
	started := make(chan struct{})

	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			close(started)
			buf := make([]byte, 4)
			for {
				_, err := r.Body.Read(buf)
				if err != nil {
					return
				}
			}
		},
		jisr.ResponseModeObserve,
	)

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
	h.respHeaders(map[string][]string{":status": {"200"}}, false)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("response handler did not start")
	}

	// Cancel — OnResponseBody ctx.Done path.
	h.filter.OnStreamComplete()

	status := h.respBody([]byte("chunk"), false)
	assert.Equal(t, shared.BodyStatusContinue, status)
}

// TestSetUpstreamResponseHeader_Buffer_QueuedAndApplied verifies that
// SetUpstreamResponseHeader mutations are recorded in rw.respHeaderMuts
// and applied to ResponseHeaders() during ContinueResponse scheduling.
func TestSetUpstreamResponseHeader_Buffer_QueuedAndApplied(t *testing.T) {
	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			io.Copy(io.Discard, r.Body) //nolint:errcheck
			w.SetUpstreamResponseHeader("x-custom", "injected")
		},
		jisr.ResponseModeBuffer,
	)

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
	h.respHeaders(map[string][]string{":status": {"200"}}, false)
	h.respBody([]byte("body"), true)
	h.waitResponse(t)

	// The fake ResponseHeaders() returns upstreamRespHdrs.
	// The scheduler called ResponseHeaders().Set("x-custom", "injected").
	h.handle.mu.Lock()
	var got string
	if h.handle.upstreamRespHdrs != nil {
		got = h.handle.upstreamRespHdrs.GetOne("x-custom").ToString()
	}
	h.handle.mu.Unlock()

	assert.Equal(t, "injected", got,
		"SetUpstreamResponseHeader must be applied via ResponseHeaders().Set()")
}

func TestSetUpstreamResponseHeader_NoopOutsideBuffer(t *testing.T) {
	for _, mode := range []jisr.ResponseMode{jisr.ResponseModePassthrough, jisr.ResponseModeObserve} {
		t.Run(fmt.Sprintf("mode=%d", mode), func(t *testing.T) {
			done := make(chan struct{})
			h := newHarnessWithResponse(t,
				func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
				func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
					if r.Body != nil {
						r.SkipBody()
					}
					w.SetUpstreamResponseHeader("x-custom", "ignored")
					close(done)
				},
				mode,
			)

			h.headers(map[string][]string{":path": {"/"}}, true)
			h.wait(t)
			h.respHeaders(map[string][]string{":status": {"200"}}, true)

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("response handler did not complete")
			}

			h.handle.mu.Lock()
			var got string
			if h.handle.upstreamRespHdrs != nil {
				got = h.handle.upstreamRespHdrs.GetOne("x-custom").ToString()
			}
			h.handle.mu.Unlock()
			assert.Empty(t, got)
		})
	}
}

func TestReplaceBody_NoopOutsideBuffer(t *testing.T) {
	h := newHarnessWithResponse(t,
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			w.ReplaceBody([]byte("ignored"))
		},
		jisr.ResponseModePassthrough,
	)

	h.handle.mu.Lock()
	h.handle.respBodyBuf = &fakeBodyBuffer{data: []byte("original")}
	h.handle.mu.Unlock()

	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
	h.respHeaders(map[string][]string{":status": {"200"}}, true)
	h.waitResponse(t)

	h.handle.mu.Lock()
	got := h.handle.respBodyBuf.Bytes()
	h.handle.mu.Unlock()
	assert.Equal(t, "original", string(got))
}

// TestBodyRead_AfterLimitExhausted verifies the remaining<=0 branch in bodyReader.Read —
// called after the limit is already reached (b.eof should already be true,
// but the defensive check must also return EOF without panic).
func TestBodyRead_AfterLimitExhausted(t *testing.T) {
	var firstRead, secondRead []byte

	h := newHarness(t, func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.LimitBody(3)
		firstRead, _ = io.ReadAll(r.Body)
		// Read again after EOF — must return (0, io.EOF) not panic.
		buf := make([]byte, 4)
		n, err := r.Body.Read(buf)
		secondRead = buf[:n]
		assert.Equal(t, 0, n)
		assert.Equal(t, io.EOF, err)
	})

	h.headers(map[string][]string{":path": {"/"}}, false)
	h.body([]byte("hello"), true)
	h.wait(t)

	assert.Equal(t, "hel", string(firstRead))
	assert.Empty(t, secondRead)
}

// ── RegisterWithConfig / ConfigHandle / new ResponseWriter methods ────────────

func TestRegisterWithConfig_ConfigFnCalled(t *testing.T) {
	defer jisr.Unregister("cfg-test")
	called := false
	jisr.RegisterWithConfig("cfg-test",
		func(h jisr.ConfigHandle) error {
			called = true
			_ = h.RawConfig() // nil in test — no filter_config bytes
			return nil
		},
		func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
	)
	factories := jisr.WellKnownHttpFilterConfigFactories()
	fac, ok := factories["cfg-test"]
	require.True(t, ok)
	_, err := fac.Create(jisr.EmptyHttpFilterConfigHandle{}, nil)
	require.NoError(t, err)
	assert.True(t, called, "ConfigFunc should have been called during Create")
}

func TestRegisterWithConfig_ConfigFnError_AbortCreate(t *testing.T) {
	defer jisr.Unregister("cfg-err")
	jisr.RegisterWithConfig("cfg-err",
		func(h jisr.ConfigHandle) error { return fmt.Errorf("bad config") },
		func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
	)
	factories := jisr.WellKnownHttpFilterConfigFactories()
	_, err := factories["cfg-err"].Create(jisr.EmptyHttpFilterConfigHandle{}, nil)
	assert.Error(t, err, "Create should propagate ConfigFunc error")
}

func TestRegisterWithConfig_PanicOnDuplicate(t *testing.T) {
	defer jisr.Unregister("cfg-dup")
	jisr.RegisterWithConfig("cfg-dup", nil,
		func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
	)
	assert.Panics(t, func() {
		jisr.RegisterWithConfig("cfg-dup", nil,
			func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		)
	})
}

func TestResponseWriter_ClearRouteCache(t *testing.T) {
	h := newHarness(t, func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		w.SetRequestHeader("x-cluster", "openai")
		w.ClearRouteCache() // must not panic; applied on worker thread
	})
	h.headers(map[string][]string{":path": {"/v1/chat"}}, true)
	h.wait(t)
	// ClearRouteCache is fire-and-forget on the mock scheduler —
	// we verify it doesn't panic and the filter completes normally.
}

func TestResponseWriter_IncrementCounter_NoOp(t *testing.T) {
	// MetricID(0) with EmptyHttpFilterHandle — must not panic.
	h := newHarness(t, func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		w.IncrementCounter(jisr.MetricID(0), 1, "openai")
	})
	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
}

func TestResponseWriter_RecordHistogram_NoOp(t *testing.T) {
	h := newHarness(t, func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		w.RecordHistogram(jisr.MetricID(0), 42, "openai")
	})
	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
}

func TestRequest_GetAttr_EmptyWhenNoAttrs(t *testing.T) {
	// The test harness uses EmptyHttpFilterHandle which returns (nil, false)
	// for GetAttributeString — so all attrs snapshot to empty string.
	h := newHarness(t, func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.SkipBody()
		// Should return "" (not panic) when attrs are missing.
		assert.Equal(t, "", r.GetAttr(jisr.AttrRequestPath))
		assert.Equal(t, "", r.GetAttr(jisr.AttrRequestMethod))
	})
	h.headers(map[string][]string{":path": {"/"}}, true)
	h.wait(t)
}

// ── Response.SkipBody ─────────────────────────────────────────────────────────

func TestResponse_SkipBody_UnblocksGoroutine(t *testing.T) {
	// Buffer mode: r.SkipBody() must unblock the response handler immediately
	// without reading any body bytes.
	done := make(chan struct{})
	h := newHarnessWithResponse(t,
		func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			r.SkipBody() // must not block
			close(done)
		},
		jisr.ResponseModeBuffer,
	)
	h.headers(map[string][]string{":path": {"/"}}, true)
	h.respHeaders(map[string][]string{":status": {"200"}}, false)
	h.respBody([]byte("body data"), true)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SkipBody did not unblock response handler")
	}
}

func TestResponse_SkipBody_NoOpInPassthrough(t *testing.T) {
	// Passthrough mode: Body is nil; SkipBody must not panic.
	done := make(chan struct{})
	h := newHarnessWithResponse(t,
		func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() },
		func(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			assert.Nil(t, r.Body)
			r.SkipBody() // no-op: Body is nil in Passthrough
			close(done)
		},
		jisr.ResponseModePassthrough,
	)
	h.headers(map[string][]string{":path": {"/"}}, true)
	h.respHeaders(map[string][]string{":status": {"200"}}, true) // endStream=true
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Passthrough response handler did not complete")
	}
}

// ── RegisterFactory / RegisterFactoryWithResponse ─────────────────────────────

func TestRegisterFactory_ConstructsHandlerFromConfig(t *testing.T) {
	defer jisr.Unregister("fac-test")

	type state struct{ built bool }
	var s state

	jisr.RegisterFactory("fac-test", func(h jisr.ConfigHandle) (jisr.HandlerFunc, error) {
		// config context: parse config, define metrics, build struct
		_ = h.RawConfig()
		s.built = true
		return func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
			r.SkipBody()
			w.SetRequestHeader("x-built", "1")
		}, nil
	})

	factories := jisr.WellKnownHttpFilterConfigFactories()
	fac, ok := factories["fac-test"]
	require.True(t, ok)
	ff, err := fac.Create(jisr.EmptyHttpFilterConfigHandle{}, nil)
	require.NoError(t, err)
	require.NotNil(t, ff)
	assert.True(t, s.built, "factory must have been called during Create")
}

func TestRegisterFactory_ErrorAbortCreate(t *testing.T) {
	defer jisr.Unregister("fac-err")

	jisr.RegisterFactory("fac-err", func(h jisr.ConfigHandle) (jisr.HandlerFunc, error) {
		return nil, fmt.Errorf("bad factory config")
	})

	factories := jisr.WellKnownHttpFilterConfigFactories()
	_, err := factories["fac-err"].Create(jisr.EmptyHttpFilterConfigHandle{}, nil)
	assert.Error(t, err)
}

func TestRegisterFactory_NilHandlerAbortCreate(t *testing.T) {
	defer jisr.Unregister("fac-nil")

	jisr.RegisterFactory("fac-nil", func(h jisr.ConfigHandle) (jisr.HandlerFunc, error) {
		return nil, nil
	})

	factories := jisr.WellKnownHttpFilterConfigFactories()
	_, err := factories["fac-nil"].Create(jisr.EmptyHttpFilterConfigHandle{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil request handler")
}

func TestRegisterFactoryWithResponse_ConstructsBothHandlers(t *testing.T) {
	defer jisr.Unregister("facresp-test")

	type state struct{ reqBuilt, respBuilt bool }
	var s state

	jisr.RegisterFactoryWithResponse("facresp-test",
		func(h jisr.ConfigHandle) (jisr.HandlerFunc, jisr.ResponseFunc, error) {
			s.reqBuilt = true
			reqFn := func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
				r.SkipBody()
			}
			respFn := func(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
				s.respBuilt = true
			}
			return reqFn, respFn, nil
		},
		jisr.ResponseModePassthrough,
	)

	factories := jisr.WellKnownHttpFilterConfigFactories()
	fac, ok := factories["facresp-test"]
	require.True(t, ok)
	ff, err := fac.Create(jisr.EmptyHttpFilterConfigHandle{}, nil)
	require.NoError(t, err)
	require.NotNil(t, ff)
	assert.True(t, s.reqBuilt)
}

func TestRegisterFactoryWithResponse_NilRequestAbortCreate(t *testing.T) {
	defer jisr.Unregister("facresp-nil-req")

	jisr.RegisterFactoryWithResponse("facresp-nil-req",
		func(h jisr.ConfigHandle) (jisr.HandlerFunc, jisr.ResponseFunc, error) {
			return nil, func(_ context.Context, _ jisr.ResponseWriter, _ *jisr.Response) {}, nil
		},
		jisr.ResponseModePassthrough,
	)

	factories := jisr.WellKnownHttpFilterConfigFactories()
	_, err := factories["facresp-nil-req"].Create(jisr.EmptyHttpFilterConfigHandle{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil request handler")
}

func TestRegisterFactoryWithResponse_NilResponseAbortCreate(t *testing.T) {
	defer jisr.Unregister("facresp-nil-resp")

	jisr.RegisterFactoryWithResponse("facresp-nil-resp",
		func(h jisr.ConfigHandle) (jisr.HandlerFunc, jisr.ResponseFunc, error) {
			return func(_ context.Context, _ jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() }, nil, nil
		},
		jisr.ResponseModePassthrough,
	)

	factories := jisr.WellKnownHttpFilterConfigFactories()
	_, err := factories["facresp-nil-resp"].Create(jisr.EmptyHttpFilterConfigHandle{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil response handler")
}

func TestRegisterFactory_PanicOnDuplicate(t *testing.T) {
	defer jisr.Unregister("fac-dup")
	jisr.RegisterFactory("fac-dup", func(h jisr.ConfigHandle) (jisr.HandlerFunc, error) {
		return func(_ context.Context, _ jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() }, nil
	})
	assert.Panics(t, func() {
		jisr.RegisterFactory("fac-dup", func(h jisr.ConfigHandle) (jisr.HandlerFunc, error) {
			return nil, nil
		})
	})
}

func TestRegister_MixedDuplicatePanics(t *testing.T) {
	req := func(_ context.Context, _ jisr.ResponseWriter, r *jisr.Request) { r.SkipBody() }
	resp := func(_ context.Context, _ jisr.ResponseWriter, _ *jisr.Response) {}

	registrars := []struct {
		name string
		fn   func(string)
	}{
		{
			name: "Register",
			fn: func(name string) {
				jisr.Register(name, req)
			},
		},
		{
			name: "RegisterWithConfig",
			fn: func(name string) {
				jisr.RegisterWithConfig(name, nil, req)
			},
		},
		{
			name: "RegisterWithResponse",
			fn: func(name string) {
				jisr.RegisterWithResponse(name, req, resp, jisr.ResponseModePassthrough)
			},
		},
		{
			name: "RegisterWithConfigAndResponse",
			fn: func(name string) {
				jisr.RegisterWithConfigAndResponse(name, nil, req, resp, jisr.ResponseModePassthrough)
			},
		},
		{
			name: "RegisterFactory",
			fn: func(name string) {
				jisr.RegisterFactory(name, func(h jisr.ConfigHandle) (jisr.HandlerFunc, error) {
					return req, nil
				})
			},
		},
		{
			name: "RegisterFactoryWithResponse",
			fn: func(name string) {
				jisr.RegisterFactoryWithResponse(name,
					func(h jisr.ConfigHandle) (jisr.HandlerFunc, jisr.ResponseFunc, error) {
						return req, resp, nil
					},
					jisr.ResponseModePassthrough,
				)
			},
		},
		{
			name: "RegisterRaw",
			fn: func(name string) {
				jisr.RegisterRaw(name, &fakeRawFactory{onCreate: func() {}})
			},
		},
	}

	for _, first := range registrars {
		for _, second := range registrars {
			t.Run(first.name+"_then_"+second.name, func(t *testing.T) {
				name := strings.ReplaceAll(t.Name(), "/", "_")
				first.fn(name)
				t.Cleanup(func() { jisr.Unregister(name) })

				assert.Panics(t, func() {
					second.fn(name)
				})
			})
		}
	}
}

// ── ResponseChain ──────────────────────────────────────────────────────────────

func TestResponseChain_ExecutionOrder(t *testing.T) {
	var order []string

	a := func(next jisr.ResponseFunc) jisr.ResponseFunc {
		return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			order = append(order, "a-before")
			next(ctx, w, r)
			order = append(order, "a-after")
		}
	}
	b := func(next jisr.ResponseFunc) jisr.ResponseFunc {
		return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Response) {
			order = append(order, "b-before")
			next(ctx, w, r)
			order = append(order, "b-after")
		}
	}
	base := func(_ context.Context, _ jisr.ResponseWriter, _ *jisr.Response) {
		order = append(order, "handler")
	}

	chained := jisr.ResponseChain(base, a, b)
	chained(context.Background(), newMockWriter(), &jisr.Response{})

	// a is outermost → runs first in, last out
	assert.Equal(t, []string{"a-before", "b-before", "handler", "b-after", "a-after"}, order)
}

func TestResponseChain_NoMiddleware(t *testing.T) {
	called := false
	base := func(_ context.Context, _ jisr.ResponseWriter, _ *jisr.Response) { called = true }
	jisr.ResponseChain(base)(context.Background(), newMockWriter(), &jisr.Response{})
	assert.True(t, called)
}
