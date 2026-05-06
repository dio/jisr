package jisr

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// --- configFactory: HttpFilterConfigFactory ---

type configFactory struct {
	name    string
	handler HandlerFunc
}

func (f *configFactory) Create(
	_ shared.HttpFilterConfigHandle,
	_ []byte,
) (shared.HttpFilterFactory, error) {
	return &filterFactory{name: f.name, handler: f.handler}, nil
}

func (f *configFactory) CreatePerRoute(_ []byte) (any, error) {
	return nil, nil
}

// --- filterFactory: HttpFilterFactory ---

type filterFactory struct {
	name    string
	handler HandlerFunc
}

func (f *filterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &handlerFilter{
		handle:  handle,
		handler: f.handler,
		name:    f.name,
	}
}

func (f *filterFactory) OnDestroy() {}

// --- handlerFilter: HttpFilter ---

type handlerFilter struct {
	shared.EmptyHttpFilter

	handle  shared.HttpFilterHandle
	handler HandlerFunc
	name    string

	// set in OnRequestHeaders, used across callbacks
	scheduler shared.Scheduler
	ctx       context.Context
	cancel    context.CancelFunc

	// body pipeline
	bodyCh     chan<- []byte
	bodyReader *bodyReader
	bodyDone   bool       // true once endStream seen in OnRequestBody
	bodySkip   atomic.Bool // true after r.SkipBody() — passthrough mode

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

// OnRequestHeaders is called by Envoy on the worker thread when request headers arrive.
func (f *handlerFilter) OnRequestHeaders(headers shared.HeaderMap, endStream bool) shared.HeadersStatus {
	// Capture scheduler BEFORE spawning goroutine — must be on worker thread.
	f.scheduler = f.handle.GetScheduler()
	f.ctx, f.cancel = context.WithCancel(context.Background())

	// Copy all headers into Go-owned memory (no UnsafeEnvoyBuffer leaking).
	// Use http.CanonicalHeaderKey so r.Header.Get works with any casing.
	h := make(http.Header)
	for _, kv := range headers.GetAll() {
		k := http.CanonicalHeaderKey(kv[0].ToString())
		v := kv[1].ToString()
		h[k] = append(h[k], v)
	}

	// Set up body pipeline.
	var bodyCh chan<- []byte
	f.bodyReader, bodyCh = newBodyReader()
	f.bodyCh = bodyCh

	if endStream {
		// No body coming — close channel immediately so io.ReadAll returns empty.
		close(bodyCh)
		f.bodyDone = true
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
			if !f.bodyDone && f.bodySkip.CompareAndSwap(false, true) {
				close(bodyCh)
			}
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
	if f.bodyDone {
		return shared.BodyStatusContinue
	}
	// If SkipBody was called, pass chunks through without buffering.
	if f.bodySkip.Load() {
		if endStream {
			f.bodyDone = true
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
		f.bodyDone = true
	}
	return shared.BodyStatusStopAndBuffer
}

// OnDestroy is called by Envoy when the stream is destroyed (client disconnect, etc.).
func (f *handlerFilter) OnDestroy() {
	if f.cancel != nil {
		f.cancel()
	}
}

// run executes the user handler in a goroutine, then schedules finalization
// back onto the Envoy worker thread.
func (f *handlerFilter) run(req *Request) {
	f.handler(f.ctx, f.rw, req)

	// Schedule finalization on the worker thread.
	f.scheduler.Schedule(func() {
		if f.rw.responded {
			return
		}
		// Apply queued header mutations.
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
}
