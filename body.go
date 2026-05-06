package jisr

import (
	"bytes"
	"io"
)

// bodyReader is a channel-backed io.Reader that assembles Envoy body chunks
// into a blocking stream. OnRequestBody pushes chunks in; Read blocks until
// a chunk arrives or the channel is closed (EOF).
type bodyReader struct {
	ch  chan []byte // receives copied chunks from OnRequestBody
	buf *bytes.Buffer
	eof bool
}

func newBodyReader() (*bodyReader, chan<- []byte) {
	ch := make(chan []byte, 16) // small buffer to absorb burst chunks
	return &bodyReader{ch: ch, buf: &bytes.Buffer{}}, ch
}

// Read implements io.Reader. Blocks until data is available or EOF.
func (b *bodyReader) Read(p []byte) (int, error) {
	for {
		// Drain the internal buffer first.
		if b.buf.Len() > 0 {
			return b.buf.Read(p)
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
