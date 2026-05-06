package jisr

import "io"

// bodyReader is a channel-backed io.Reader that assembles Envoy body chunks
// into a blocking stream without internal buffering.
//
// Zero-copy design: each chunk received from the channel is held by reference
// (cur/off). Read() serves bytes directly from the current chunk via a single
// copy(p, cur[off:]) into the caller's slice. No intermediate buffer.
//
// Copy count per chunk:
//   - ToBytes() on the Envoy SDK side:  1 copy (ABI contract, unavoidable)
//   - Read() into caller's p:           1 copy (io.Reader contract, unavoidable)
//   - bytes.Buffer.Write():             0 copies (eliminated)
type bodyReader struct {
	ch      chan []byte
	cur     []byte // current chunk being served
	off     int    // read offset into cur
	eof     bool
	limit   int64  // 0 = unlimited
	read    int64  // bytes delivered to caller so far
	onLimit func() // called exactly once when read >= limit
}

func newBodyReader() (*bodyReader, chan<- []byte) {
	ch := make(chan []byte, 16) // small buffer to absorb burst chunks
	return &bodyReader{ch: ch}, ch
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
// Serves bytes directly from the current chunk. No intermediate buffer copy.
func (b *bodyReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		// Serve from the current chunk.
		if b.off < len(b.cur) {
			// Respect the limit: cap the read to what's allowed.
			src := b.cur[b.off:]
			if b.limit > 0 {
				remaining := b.limit - b.read
				if remaining <= 0 {
					b.eof = true
					b.fireLimit()
					return 0, io.EOF
				}
				if int64(len(src)) > remaining {
					src = src[:remaining]
				}
				if int64(len(p)) > remaining {
					p = p[:remaining]
				}
			}
			n := copy(p, src)
			b.off += n
			b.read += int64(n)
			if b.limit > 0 && b.read >= b.limit {
				b.eof = true
				b.fireLimit()
				return n, io.EOF
			}
			return n, nil
		}

		// Current chunk exhausted: release the reference.
		b.cur = nil
		b.off = 0

		if b.eof {
			return 0, io.EOF
		}

		// Block for the next chunk (or channel close = EOF).
		chunk, ok := <-b.ch
		if !ok {
			b.eof = true
			return 0, io.EOF
		}
		b.cur = chunk
	}
}
