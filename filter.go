package jisr

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// --- configFactory: HttpFilterConfigFactory ---

type configFactory struct {
	name        string
	configFn    ConfigFunc // optional: nil for plain Register
	handler     HandlerFunc
	respHandler ResponseFunc
	respMode    ResponseMode
}

func (f *configFactory) Create(
	h shared.HttpFilterConfigHandle,
	raw []byte,
) (shared.HttpFilterFactory, error) {
	if f.configFn != nil {
		if err := f.configFn(&configHandleImpl{h: h, raw: raw}); err != nil {
			h.Log(shared.LogLevelError, "jisr: filter %q config failed: %v", f.name, err)
			return nil, err
		}
	}
	return &filterFactory{
		name:        f.name,
		handler:     f.handler,
		respHandler: f.respHandler,
		respMode:    f.respMode,
	}, nil
}

func (f *configFactory) CreatePerRoute(_ []byte) (any, error) { return nil, nil }

// --- filterFactory: HttpFilterFactory ---

type filterFactory struct {
	name        string
	handler     HandlerFunc
	respHandler ResponseFunc
	respMode    ResponseMode // fixed at registration, read-only after construction
}

func (f *filterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &handlerFilter{
		handle:      handle,
		handler:     f.handler,
		respHandler: f.respHandler,
		respMode:    f.respMode,
		name:        f.name,
	}
}

func (f *filterFactory) OnDestroy() {}

// --- handlerFilter: HttpFilter ---

type handlerFilter struct {
	shared.EmptyHttpFilter

	handle      shared.HttpFilterHandle
	handler     HandlerFunc
	respHandler ResponseFunc
	respMode    ResponseMode // fixed: safe to read from any goroutine without sync
	name        string

	// set in OnRequestHeaders, used across callbacks
	scheduler shared.Scheduler
	ctx       context.Context
	cancel    context.CancelFunc

	// request body pipeline
	bodyCh     chan<- []byte
	bodyReader *bodyReader
	bodyDone   atomic.Bool // true once endStream seen in OnRequestBody
	bodySkip   atomic.Bool // true after r.SkipBody(): passthrough mode
	headDone   atomic.Bool // true after r.LimitBody() threshold reached: stream remainder

	// response pipeline (non-nil only when respHandler != nil)
	respHeadersCh  chan http.Header // OnResponseHeaders pushes here; goroutine blocks on it
	respBodyCh     chan<- []byte
	respBodyReader *bodyReader
	respBodyDone   atomic.Bool

	// response writer state (accumulated on goroutine, flushed via scheduler)
	rw *responseWriterImpl
}

// responseWriterImpl accumulates mutations the user makes during the handler call.
// These are applied back on the Envoy worker thread via the Scheduler.
type responseWriterImpl struct {
	filter    *handlerFilter
	responded bool

	headerMuts  []headerMutation
	respHeaders [][2]string
	metaMuts    []metaMutation
	clearRoute  bool             // queued ClearRouteCache, applied before ContinueRequest
	metricMuts  []metricMutation // queued metric increments, applied before ContinueRequest

	// response-phase mutations (applied before ContinueResponse)
	respHeaderMuts []headerMutation
	replaceBody    []byte
	bodyReplaced   bool
}

type headerMutation struct{ key, value string }
type metaMutation struct {
	namespace, key string
	value          any
}
type metricMutation struct {
	id     shared.MetricID
	n      uint64
	labels []string
	hist   bool // false = counter, true = histogram
}

func (w *responseWriterImpl) ClearRouteCache() {
	w.clearRoute = true
}

func (w *responseWriterImpl) IncrementCounter(id MetricID, n uint64, labels ...string) {
	w.metricMuts = append(w.metricMuts, metricMutation{id: id, n: n, labels: labels})
}

func (w *responseWriterImpl) RecordHistogram(id MetricID, n uint64, labels ...string) {
	w.metricMuts = append(w.metricMuts, metricMutation{id: id, n: n, labels: labels, hist: true})
}

func (w *responseWriterImpl) Send(statusCode int, body string) {
	w.sendBytes(statusCode, []byte(body))
}

func (w *responseWriterImpl) SendBytes(statusCode int, body []byte) {
	w.sendBytes(statusCode, body)
}

func (w *responseWriterImpl) SetResponseHeader(key, value string) {
	w.respHeaders = append(w.respHeaders, [2]string{key, value})
}

func (w *responseWriterImpl) sendBytes(statusCode int, body []byte) {
	w.responded = true
	headers := w.respHeaders
	w.filter.scheduler.Schedule(func() {
		w.filter.handle.SendLocalResponse(
			uint32(statusCode), headers, body,
			fmt.Sprintf("jisr-filter:%s", w.filter.name),
		)
	})
}

func (w *responseWriterImpl) SetRequestHeader(key, value string) {
	w.headerMuts = append(w.headerMuts, headerMutation{key, value})
}

func (w *responseWriterImpl) SetMetadata(namespace, key string, value any) {
	w.metaMuts = append(w.metaMuts, metaMutation{namespace, key, value})
}

// SetUpstreamResponseHeader queues a mutation to the upstream response headers,
// applied before ContinueResponse. Only valid from a ResponseFunc.
func (w *responseWriterImpl) SetUpstreamResponseHeader(key, value string) {
	w.respHeaderMuts = append(w.respHeaderMuts, headerMutation{key, value})
}

// ReplaceBody replaces the upstream response body. Only valid in Buffer mode.
// The caller is responsible for setting an appropriate content-length response header.
func (w *responseWriterImpl) ReplaceBody(body []byte) {
	w.replaceBody = body
	w.bodyReplaced = true
}

// OnRequestHeaders is called by Envoy on the worker thread when request headers arrive.
func (f *handlerFilter) OnRequestHeaders(headers shared.HeaderMap, endStream bool) shared.HeadersStatus {
	// Capture scheduler BEFORE spawning goroutine. Must be on worker thread.
	f.scheduler = f.handle.GetScheduler()
	f.ctx, f.cancel = context.WithCancel(context.Background())

	// Copy all headers into Go-owned memory (no UnsafeEnvoyBuffer leaking).
	// http.Header.Add calls CanonicalHeaderKey internally.
	h := make(http.Header)
	for _, kv := range headers.GetAll() {
		h.Add(kv[0].ToString(), kv[1].ToString())
	}

	// Set up request body pipeline.
	var bodyCh chan<- []byte
	f.bodyReader, bodyCh = newBodyReader()
	f.bodyCh = bodyCh

	if endStream {
		// No body coming: close channel immediately so io.ReadAll returns empty.
		close(bodyCh)
		f.bodyDone.Store(true)
	}

	// Set up response pipeline if a ResponseFunc is registered.
	if f.respHandler != nil {
		f.respHeadersCh = make(chan http.Header, 1) // buffered: OnResponseHeaders never blocks
		var respBodyCh chan<- []byte
		f.respBodyReader, respBodyCh = newBodyReader()
		f.respBodyCh = respBodyCh
	}

	// Snapshot common Envoy stream attributes: read here on the worker thread
	// so handlers can call r.GetAttr() from any goroutine without locking.
	attrIDs := []shared.AttributeID{
		shared.AttributeIDRequestPath,
		shared.AttributeIDRequestMethod,
		shared.AttributeIDRequestHost,
		shared.AttributeIDRequestScheme,
		shared.AttributeIDRequestQuery,
		shared.AttributeIDRequestProtocol,
		shared.AttributeIDRequestId,
		shared.AttributeIDRequestUserAgent,
	}
	attrs := make(map[shared.AttributeID]string, len(attrIDs))
	for _, id := range attrIDs {
		if buf, ok := f.handle.GetAttributeString(id); ok {
			attrs[id] = buf.ToString()
		}
	}

	req := &Request{
		Header:     h,
		Body:       f.bodyReader,
		FilterName: f.name,
		attrs:      attrs,
		log: func(level shared.LogLevel, format string, args ...any) {
			// handle.Log is thread-safe in the Envoy SDK.
			f.handle.Log(level, format, args...)
		},
		skipBody: func() {
			// CAS prevents double-close. Also check bodyDone: if endStream
			// was true on headers, bodyCh is already closed.
			if !f.bodyDone.Load() && f.bodySkip.CompareAndSwap(false, true) {
				close(bodyCh)
			}
		},
		limitBody: func(n int64) {
			f.bodyReader.setLimit(n, func() {
				// Called from the handler goroutine when the limit is reached.
				// Signal OnRequestBody to stop buffering and stream the rest.
				f.headDone.Store(true)
				// Close the channel so the handler's io.ReadAll returns.
				if !f.bodyDone.Load() && !f.bodySkip.Load() {
					close(bodyCh)
				}
			})
		},
	}
	f.rw = &responseWriterImpl{filter: f}

	go f.run(req)

	// HeadersStatusStop (not StopAllAndBuffer) is required so that Envoy
	// calls OnRequestBody for each arriving chunk. StopAllAndBuffer silently
	// buffers the body without notifying the filter, which would deadlock
	// io.ReadAll in the handler goroutine.
	return shared.HeadersStatusStop
}

// OnRequestBody is called by Envoy on the worker thread for each body chunk.
func (f *handlerFilter) OnRequestBody(body shared.BodyBuffer, endStream bool) shared.BodyStatus {
	if f.bodyDone.Load() {
		return shared.BodyStatusContinue
	}
	// If SkipBody or LimitBody threshold was reached, stream chunks through
	// without copying into Go memory.
	if f.bodySkip.Load() || f.headDone.Load() {
		if endStream {
			f.bodyDone.Store(true)
		}
		return shared.BodyStatusContinue
	}
	// Push copied chunks into the body channel for the goroutine to consume.
	for _, chunk := range body.GetChunks() {
		data := chunk.ToBytes() // copies into Go heap
		select {
		case f.bodyCh <- data:
		case <-f.ctx.Done():
			return shared.BodyStatusContinue
		}
	}
	if endStream {
		close(f.bodyCh)
		f.bodyDone.Store(true)
	}
	return shared.BodyStatusStopAndBuffer
}

// OnResponseHeaders is called by Envoy when upstream response headers arrive.
// It never blocks the worker thread. Mode is known at construction time.
func (f *handlerFilter) OnResponseHeaders(headers shared.HeaderMap, endStream bool) shared.HeadersStatus {
	if f.respHandler == nil {
		return shared.HeadersStatusContinue
	}

	// Copy response headers into Go-owned memory.
	h := make(http.Header)
	for _, kv := range headers.GetAll() {
		h.Add(kv[0].ToString(), kv[1].ToString())
	}

	// If no body follows, close the channel immediately so the goroutine's
	// io.ReadAll returns EOF. CAS prevents double-close with OnStreamComplete.
	if endStream {
		if f.respBodyDone.CompareAndSwap(false, true) {
			close(f.respBodyCh)
		}
	}

	// Push to goroutine: buffered channel, never blocks.
	f.respHeadersCh <- h

	// Mode is fixed at registration; return immediately, no goroutine sync needed.
	switch f.respMode {
	case ResponseModeObserve:
		// Let headers flow to downstream immediately, zero latency for the client.
		// Body chunks will arrive via OnResponseBody returning Continue.
		return shared.HeadersStatusContinue
	default: // Passthrough, Buffer
		// Stop headers so we can apply mutations or hold body for transformation.
		// Goroutine will call ContinueResponse when done.
		return shared.HeadersStatusStop
	}
}

// OnResponseBody is called by Envoy for each upstream response body chunk.
func (f *handlerFilter) OnResponseBody(body shared.BodyBuffer, endStream bool) shared.BodyStatus {
	if f.respHandler == nil || f.respBodyDone.Load() {
		return shared.BodyStatusContinue
	}

	switch f.respMode {
	case ResponseModePassthrough:
		// Header-only: body streams through without touching Go memory.
		if endStream {
			f.respBodyDone.Store(true)
		}
		return shared.BodyStatusContinue

	case ResponseModeObserve:
		// Tap: copy chunk to goroutine AND return Continue so Envoy
		// simultaneously forwards the chunk downstream. Zero added latency.
		for _, chunk := range body.GetChunks() {
			data := chunk.ToBytes()
			select {
			case f.respBodyCh <- data:
			case <-f.ctx.Done():
				return shared.BodyStatusContinue
			}
		}
		if endStream {
			if f.respBodyDone.CompareAndSwap(false, true) {
				close(f.respBodyCh)
			}
		}
		return shared.BodyStatusContinue

	case ResponseModeBuffer:
		// Accumulate: push to goroutine and stop-buffer. Downstream waits
		// until the goroutine calls ContinueResponse.
		for _, chunk := range body.GetChunks() {
			data := chunk.ToBytes()
			select {
			case f.respBodyCh <- data:
			case <-f.ctx.Done():
				return shared.BodyStatusContinue
			}
		}
		if endStream {
			if f.respBodyDone.CompareAndSwap(false, true) {
				close(f.respBodyCh)
			}
		}
		return shared.BodyStatusStopAndBuffer

	default:
		return shared.BodyStatusContinue
	}
}

// OnStreamComplete is called by Envoy when the stream is destroyed (client disconnect, etc.).
func (f *handlerFilter) OnStreamComplete() {
	if f.cancel != nil {
		f.cancel()
	}
	// Close request body channel if open: unblocks a handler goroutine
	// blocked on r.Body.Read (io.ReadAll) after client disconnect.
	if f.bodyCh != nil && !f.bodyDone.Load() {
		if f.bodySkip.CompareAndSwap(false, true) {
			close(f.bodyCh)
		}
	}
	// Close response body channel if open: unblocks ResponseFunc reading r.Body.
	// CAS prevents double-close with OnResponseHeaders/OnResponseBody.
	if f.respBodyCh != nil {
		if f.respBodyDone.CompareAndSwap(false, true) {
			close(f.respBodyCh)
		}
	}
}

// run executes the request handler then the response handler in a single
// goroutine for the full request+response lifecycle.
func (f *handlerFilter) run(req *Request) {
	defer func() {
		if r := recover(); r != nil {
			f.handle.Log(shared.LogLevelError,
				"jisr: panic in filter %q handler: %v", f.name, r)
			// Ensure the filter chain is unblocked even after a panic.
			// Schedule is safe to call from the deferred recover.
			f.scheduler.Schedule(func() {
				if !f.rw.responded {
					f.handle.ContinueRequest()
				}
			})
		}
	}()

	f.handler(f.ctx, f.rw, req)

	// Finalize request phase: apply mutations and unblock Envoy's filter chain.
	f.scheduler.Schedule(func() {
		if f.rw.responded {
			return
		}
		reqHeaders := f.handle.RequestHeaders()
		for _, m := range f.rw.headerMuts {
			reqHeaders.Set(m.key, m.value)
		}
		for _, m := range f.rw.metaMuts {
			f.handle.SetMetadata(m.namespace, m.key, m.value)
		}
		if f.rw.clearRoute {
			f.handle.ClearRouteCache()
		}
		for _, m := range f.rw.metricMuts {
			if m.hist {
				f.handle.RecordHistogramValue(m.id, m.n, m.labels...)
			} else {
				f.handle.IncrementCounterValue(m.id, m.n, m.labels...)
			}
		}
		f.handle.ContinueRequest()
	})

	// If the request handler sent a direct response (w.Send/SendBytes), Envoy
	// will never call OnResponseHeaders; skip the response phase to avoid
	// parking the goroutine on respHeadersCh forever.
	if f.rw.responded || f.respHandler == nil {
		return
	}

	// Block until response headers arrive from OnResponseHeaders.
	// This is a goroutine block. The worker thread is not involved.
	var respHeaders http.Header
	select {
	case respHeaders = <-f.respHeadersCh:
	case <-f.ctx.Done():
		return
	}

	// Parse status code from :status pseudo-header.
	statusCode := 0
	fmt.Sscanf(respHeaders.Get(":status"), "%d", &statusCode)

	// Build the Response. Body is nil for Passthrough (no body delivered to handler).
	var body io.Reader
	if f.respMode != ResponseModePassthrough {
		body = f.respBodyReader
	}

	resp := &Response{
		Header:     respHeaders,
		StatusCode: statusCode,
		Body:       body,
	}

	f.respHandler(f.ctx, f.rw, resp)

	// Observe: headers already flowed to downstream (OnResponseHeaders returned
	// Continue); no ContinueResponse needed. Just return.
	if f.respMode == ResponseModeObserve {
		return
	}

	// Passthrough and Buffer: headers were stopped; schedule ContinueResponse
	// on the worker thread so they flow to downstream now.
	f.scheduler.Schedule(func() {
		// Apply response header mutations.
		if len(f.rw.respHeaderMuts) > 0 {
			respHdrs := f.handle.ResponseHeaders()
			for _, m := range f.rw.respHeaderMuts {
				respHdrs.Set(m.key, m.value)
			}
		}
		// Replace body if requested (Buffer mode only).
		// Drain the Envoy-buffered body and append our replacement bytes.
		if f.rw.bodyReplaced {
			buf := f.handle.BufferedResponseBody()
			if buf != nil {
				buf.Drain(buf.GetSize())
				if len(f.rw.replaceBody) > 0 {
					buf.Append(f.rw.replaceBody)
				}
			}
		}
		f.handle.ContinueResponse()
	})
}
