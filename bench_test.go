package jisr

import (
	"context"
	"net/http"
	"testing"
)

// ── Benchmark: header copy

// BenchmarkHeaderCopy_Add benchmarks the current approach: h.Add(k, v) which
// calls net/textproto.CanonicalMIMEHeaderKey on every insert.
func BenchmarkHeaderCopy_Add(b *testing.B) {
	// Simulate what Envoy sends: 8 lowercase HTTP/2-style headers.
	keys := []string{
		":method", ":path", ":scheme", ":authority",
		"content-type", "x-request-id", "user-agent", "accept",
	}
	vals := []string{
		"GET", "/api/v1/resource", "http", "example.com",
		"application/json", "abc-123-def", "curl/8.0", "*/*",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := make(http.Header)
		for j := range keys {
			h.Add(keys[j], vals[j])
		}
		_ = h
	}
}

// BenchmarkHeaderCopy_Set benchmarks http.Header.Set — same canonicalization
// cost but avoids append (single-value headers only).
func BenchmarkHeaderCopy_Set(b *testing.B) {
	keys := []string{
		":method", ":path", ":scheme", ":authority",
		"content-type", "x-request-id", "user-agent", "accept",
	}
	vals := []string{
		"GET", "/api/v1/resource", "http", "example.com",
		"application/json", "abc-123-def", "curl/8.0", "*/*",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := make(http.Header)
		for j := range keys {
			h.Set(keys[j], vals[j])
		}
		_ = h
	}
}

// BenchmarkHeaderCopy_Presized benchmarks pre-sizing the map to avoid rehash.
func BenchmarkHeaderCopy_Presized(b *testing.B) {
	keys := []string{
		":method", ":path", ":scheme", ":authority",
		"content-type", "x-request-id", "user-agent", "accept",
	}
	vals := []string{
		"GET", "/api/v1/resource", "http", "example.com",
		"application/json", "abc-123-def", "curl/8.0", "*/*",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := make(http.Header, len(keys))
		for j := range keys {
			h.Add(keys[j], vals[j])
		}
		_ = h
	}
}

// BenchmarkHeaderCopy_DirectMap bypasses http.Header.Add entirely — direct map
// assign with manual canonicalization. Fastest possible, loses multi-value support.
func BenchmarkHeaderCopy_DirectMap(b *testing.B) {
	keys := []string{
		":method", ":path", ":scheme", ":authority",
		"content-type", "x-request-id", "user-agent", "accept",
	}
	vals := []string{
		"GET", "/api/v1/resource", "http", "example.com",
		"application/json", "abc-123-def", "curl/8.0", "*/*",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := make(http.Header, len(keys))
		for j := range keys {
			k := http.CanonicalHeaderKey(keys[j])
			h[k] = append(h[k], vals[j])
		}
		_ = h
	}
}

// ── Benchmark: Chain / closure allocation ─────────────────────────────────────

func noopHandler(_ context.Context, _ ResponseWriter, r *Request) { r.SkipBody() }
func noopMiddleware(next HandlerFunc) HandlerFunc                   { return next }

// BenchmarkChain_Single benchmarks calling a single handler (no middleware).
func BenchmarkChain_Single(b *testing.B) {
	h := Chain(noopHandler)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = h
	}
}

// BenchmarkChain_ThreeMiddleware benchmarks Chain building with 3 middlewares.
func BenchmarkChain_ThreeMiddleware(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := Chain(noopHandler, noopMiddleware, noopMiddleware, noopMiddleware)
		_ = h
	}
}

// ── Benchmark: bodyReader channel round-trip ──────────────────────────────────
// This is the goroutine-channel path every request body goes through.

// BenchmarkBodyReader_SmallChunk benchmarks reading a single small chunk
// (the common case for JSON API bodies).
func BenchmarkBodyReader_SmallChunk(b *testing.B) {
	chunk := []byte(`{"hello":"world","key":"value"}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		br, ch := newBodyReader()
		ch <- chunk
		close(ch)
		buf := make([]byte, len(chunk)+1)
		_, _ = br.Read(buf)
	}
}

// BenchmarkBodyReader_LargeChunk benchmarks reading a single large chunk (64KB).
func BenchmarkBodyReader_LargeChunk(b *testing.B) {
	chunk := make([]byte, 64*1024)
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		br, ch := newBodyReader()
		ch <- chunk
		close(ch)
		buf := make([]byte, len(chunk)+1)
		_, _ = br.Read(buf)
	}
}

// BenchmarkBodyReader_MultiChunk benchmarks reading 4 chunks (chunked transfer).
func BenchmarkBodyReader_MultiChunk(b *testing.B) {
	chunks := [][]byte{
		[]byte("chunk-one-data"),
		[]byte("chunk-two-data"),
		[]byte("chunk-three-data"),
		[]byte("chunk-four-data"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		br, ch := newBodyReader()
		go func() {
			for _, c := range chunks {
				ch <- c
			}
			close(ch)
		}()
		buf := make([]byte, 256)
		for {
			_, err := br.Read(buf)
			if err != nil {
				break
			}
		}
	}
}
