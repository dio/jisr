package jisr

import (
	"bytes"
	"io"
)

// bodyReader is a channel-backed io.Reader that assembles Envoy body chunks
// into a blocking stream. OnRequestBody pushes chunks in; Read blocks until
// a chunk arrives or the channel is closed (EOF).
type bodyReader struct {
	ch      chan []byte
	buf     *bytes.Buffer
	eof     bool
	limit   int64    // 0 = unlimited
	read    int64    // bytes delivered to caller so far
	onLimit func()   // called exactly once when read >= limit
}

func newBodyReader() (*bodyReader, chan<- []byte) {
	ch := make(chan []byte, 16) // small buffer to absorb burst chunks
	return &bodyReader{ch: ch, buf: &bytes.Buffer{}}, ch
}

// setLimit configures a read cap. Once n bytes have been delivered to the
// caller, Read returns EOF and onLimit is invoked (once, from the Read call
// that crosses the threshold). Must be called before the first Read.
func (b *bodyReader) setLimit(n int64, onLimit func()) {
	b.limit = n
	b.onLimit = onLimit
}

// fireLimit calls onLimit exactly once and clears it.
func (b *bodyReader) fireLimit() {
	if b.onLimit != nil {
		fn := b.onLimit
		b.onLimit = nil
		fn()
	}
}

// Read implements io.Reader. Blocks until data is available or EOF.
func (b *bodyReader) Read(p []byte) (int, error) {
	for {
		// Drain the internal buffer first.
		if b.buf.Len() > 0 {
			// Respect the limit: only deliver up to limit-read bytes.
			if b.limit > 0 {
				remaining := b.limit - b.read
				if remaining <= 0 {
					b.eof = true
					b.fireLimit()
					return 0, io.EOF
				}
				if int64(len(p)) > remaining {
					p = p[:remaining]
				}
			}
			n, err := b.buf.Read(p)
			b.read += int64(n)
			if b.limit > 0 && b.read >= b.limit {
				b.eof = true
				b.fireLimit()
				return n, io.EOF
			}
			return n, err
		}
		if b.eof {
			return 0, io.EOF
		}
		// Block until the next chunk arrives (or channel is closed).
		chunk, ok := <-b.ch
		if !ok {
			b.eof = true
			return 0, io.EOF
		}
		b.buf.Write(chunk)
	}
}
