package jisr

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// --- respMode constants ---

const (
	respModeUnset      int32 = 0
	respModePassthrough int32 = 1
	respModeObserve    int32 = 2
	respModeBuffer     int32 = 3
)

// --- configFactory: HttpFilterConfigFactory ---

type configFactory struct {
	name        string
	handler     HandlerFunc
	respHandler ResponseFunc
}

func (f *configFactory) Create(
	_ shared.HttpFilterConfigHandle,
	_ []byte,
) (shared.HttpFilterFactory, error) {
	return &filterFactory{name: f.name, handler: f.handler, respHandler: f.respHandler}, nil
}

func (f *configFactory) CreatePerRoute(_ []byte) (any, error) {
	return nil, nil
}

// --- filterFactory: HttpFilterFactory ---

type filterFactory struct {
	name        string
	handler     HandlerFunc
	respHandler ResponseFunc
}

func (f *filterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &handlerFilter{
		handle:      handle,
		handler:     f.handler,
		respHandler: f.respHandler,
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
	name        string

	// set in OnRequestHeaders, used across callbacks
	scheduler shared.Scheduler
	ctx       context.Context
	cancel    context.CancelFunc

	// request body pipeline
	bodyCh     chan<- []byte
	bodyReader *bodyReader
	bodyDone   atomic.Bool // true once endStream seen in OnRequestBody
	bodySkip   atomic.Bool // true after r.SkipBody() — passthrough mode
	headDone   atomic.Bool // true after r.LimitBody() threshold reached — stream remainder

	// response pipeline
	respHeadersCh  chan http.Header // OnResponseHeaders pushes here; goroutine blocks on it
	respModeReady  chan struct{}    // closed once mode is set by the ResponseFunc
	respBodyCh     chan<- []byte
	respBodyReader *bodyReader
	respBodyDone   atomic.Bool
	respMode       atomic.Int32 // respModeUnset/Passthrough/Observe/Buffer

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
	// Capture scheduler BEFORE spawning goroutine — must be on worker thread.
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
		// No body coming — close channel immediately so io.ReadAll returns empty.
		close(bodyCh)
		f.bodyDone.Store(true)
	}

	// Set up response pipeline if a ResponseFunc is registered.
	if f.respHandler != nil {
		f.respHeadersCh = make(chan http.Header, 1)
		f.respModeReady = make(chan struct{})
		var respBodyCh chan<- []byte
		f.respBodyReader, respBodyCh = newBodyReader()
		f.respBodyCh = respBodyCh
	}

	req := &Request{
		Header:     h,
		Body:       f.bodyReader,
		FilterName: f.name,
		log: func(level shared.LogLevel, format string, args ...any) {
			// handle.Log is thread-safe in the Envoy SDK.
			f.handle.Log(level, format, args...)
		},
		skipBody: func() {
			// CAS prevents double-close. Also check bodyDone — if endStream
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
func (f *handlerFilter) OnResponseHeaders(headers shared.HeaderMap, endStream bool) shared.HeadersStatus {
	if f.respHandler == nil {
		return shared.HeadersStatusContinue
	}

	// Copy response headers into Go memory.
	h := make(http.Header)
	for _, kv := range headers.GetAll() {
		h.Add(kv[0].ToString(), kv[1].ToString())
	}

	// If eos on headers (no body), close the resp body channel immediately.
	if endStream {
		f.respBodyDone.Store(true)
		close(f.respBodyCh)
	}

	// Push headers to goroutine — it's blocked waiting on this channel.
	f.respHeadersCh <- h

	// Wait for the ResponseFunc to declare its mode (Passthrough/Observe/Buffer).
	// The goroutine sets the mode synchronously then closes respModeReady.
	<-f.respModeReady

	switch f.respMode.Load() {
	case respModePassthrough:
		// Header mutation only — stop to allow SetUpstreamResponseHeader,
		// then continue without touching the body.
		return shared.HeadersStatusStop
	case respModeObserve:
		// Observe: let headers continue (mutations applied later via ContinueResponse).
		return shared.HeadersStatusStop
	case respModeBuffer:
		// Buffer: stop and buffer everything.
		return shared.HeadersStatusStop
	default:
		return shared.HeadersStatusContinue
	}
}

// OnResponseBody is called by Envoy for each upstream response body chunk.
func (f *handlerFilter) OnResponseBody(body shared.BodyBuffer, endStream bool) shared.BodyStatus {
	if f.respHandler == nil || f.respBodyDone.Load() {
		return shared.BodyStatusContinue
	}

	mode := f.respMode.Load()

	switch mode {
	case respModePassthrough:
		// Header-only mode — never buffer response body.
		if endStream {
			f.respBodyDone.Store(true)
		}
		return shared.BodyStatusContinue

	case respModeObserve:
		// Tap chunks: copy to Go channel AND let Envoy continue streaming.
		for _, chunk := range body.GetChunks() {
			data := chunk.ToBytes()
			select {
			case f.respBodyCh <- data:
			case <-f.ctx.Done():
				return shared.BodyStatusContinue
			}
		}
		if endStream {
			close(f.respBodyCh)
			f.respBodyDone.Store(true)
		}
		return shared.BodyStatusContinue // upstream keeps streaming to downstream

	case respModeBuffer:
		// Full buffer: accumulate in Go memory.
		for _, chunk := range body.GetChunks() {
			data := chunk.ToBytes()
			select {
			case f.respBodyCh <- data:
			case <-f.ctx.Done():
				return shared.BodyStatusContinue
			}
		}
		if endStream {
			close(f.respBodyCh)
			f.respBodyDone.Store(true)
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
	// Close response body channel if open — unblocks any ResponseFunc reading r.Body.
	if f.respBodyCh != nil && !f.respBodyDone.Load() {
		f.respBodyDone.Store(true)
		close(f.respBodyCh)
	}
}

// run executes the request handler, then optionally the response handler,
// in a single goroutine for the full request+response lifecycle.
func (f *handlerFilter) run(req *Request) {
	f.handler(f.ctx, f.rw, req)

	// Finalize request phase on the worker thread.
	f.scheduler.Schedule(func() {
		if f.rw.responded {
			return
		}
		// Apply queued request header mutations.
		reqHeaders := f.handle.RequestHeaders()
		for _, m := range f.rw.headerMuts {
			reqHeaders.Set(m.key, m.value)
		}
		// Apply queued metadata mutations.
		for _, m := range f.rw.metaMuts {
			f.handle.SetMetadata(m.namespace, m.key, m.value)
		}
		f.handle.ContinueRequest()
	})

	if f.respHandler == nil {
		return
	}

	// Block until response headers arrive from OnResponseHeaders.
	var respHeaders http.Header
	select {
	case respHeaders = <-f.respHeadersCh:
	case <-f.ctx.Done():
		return
	}

	// Parse status code from :status pseudo-header.
	statusCode := 0
	fmt.Sscanf(respHeaders.Get(":status"), "%d", &statusCode)

	resp := &Response{
		Header:     respHeaders,
		StatusCode: statusCode,
		Body:       f.respBodyReader,
		passthrough: func() {
			if f.respMode.CompareAndSwap(respModeUnset, respModePassthrough) {
				// Passthrough: no body delivered to handler — close channel now.
				if !f.respBodyDone.Load() {
					close(f.respBodyCh)
					f.respBodyDone.Store(true)
				}
				close(f.respModeReady)
			}
		},
		observe: func() {
			if f.respMode.CompareAndSwap(respModeUnset, respModeObserve) {
				close(f.respModeReady)
			}
		},
		buffer: func() {
			if f.respMode.CompareAndSwap(respModeUnset, respModeBuffer) {
				close(f.respModeReady)
			}
		},
	}

	// Default to passthrough if the handler doesn't declare a mode before returning.
	// We close respModeReady so OnResponseHeaders unblocks regardless.
	defer func() {
		if f.respMode.CompareAndSwap(respModeUnset, respModePassthrough) {
			if !f.respBodyDone.Load() {
				close(f.respBodyCh)
				f.respBodyDone.Store(true)
			}
			close(f.respModeReady)
		}
	}()

	f.respHandler(f.ctx, f.rw, resp)

	// Finalize response phase on the worker thread.
	f.scheduler.Schedule(func() {
		// Apply response header mutations.
		if len(f.rw.respHeaderMuts) > 0 {
			respHdrs := f.handle.ResponseHeaders()
			for _, m := range f.rw.respHeaderMuts {
				respHdrs.Set(m.key, m.value)
			}
		}
		f.handle.ContinueResponse()
	})
}

