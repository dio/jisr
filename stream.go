package jisr

import (
	"context"
	"fmt"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// StreamWriter is returned by ResponseWriter.Stream. It allows sending a
// chunked/streaming local response to the downstream client. Useful for SSE,
// newline-delimited JSON, or any protocol that sends data incrementally.
//
// All Flush and Close calls are goroutine-safe: they schedule work onto the
// Envoy worker thread and block until delivered, providing natural backpressure.
type StreamWriter interface {
	// Flush sends a chunk of bytes to the client immediately.
	// Blocks until the chunk is delivered to Envoy's write buffer.
	// Returns ctx.Err() if the context is cancelled before delivery
	// (i.e. the client disconnected).
	Flush(ctx context.Context, data []byte) error

	// Close sends the final empty chunk (endOfStream=true) and closes
	// the stream. Always call Close, preferably via defer.
	Close() error
}

type streamWriter struct {
	handle    shared.HttpFilterHandle
	scheduler shared.Scheduler
}

func (sw *streamWriter) Flush(ctx context.Context, data []byte) error {
	done := make(chan struct{}, 1)
	sw.scheduler.Schedule(func() {
		sw.handle.SendResponseData(data, false)
		close(done)
	})
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sw *streamWriter) Close() error {
	done := make(chan struct{}, 1)
	sw.scheduler.Schedule(func() {
		sw.handle.SendResponseData(nil, true) // endOfStream
		close(done)
	})
	<-done
	return nil
}

// Stream begins a streaming local response and returns a StreamWriter.
// It schedules SendResponseHeaders onto the Envoy worker thread and blocks
// until headers are flushed before returning.
//
// Call r.SkipBody() before Stream if you don't need the request body;
// otherwise the body channel will block the Envoy worker thread while the
// handler is generating the stream.
//
// Returns an error if Send/SendBytes was already called, or if ctx is done.
func (w *responseWriterImpl) Stream(ctx context.Context, headers [][2]string) (StreamWriter, error) {
	if w.responded {
		return nil, fmt.Errorf("jisr: Stream called after Send/SendBytes")
	}
	w.responded = true

	done := make(chan struct{}, 1)
	w.filter.scheduler.Schedule(func() {
		w.filter.handle.SendResponseHeaders(headers, false)
		close(done)
	})
	select {
	case <-done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &streamWriter{
		handle:    w.filter.handle,
		scheduler: w.filter.scheduler,
	}, nil
}
