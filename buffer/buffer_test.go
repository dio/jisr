package buffer_test

import (
	"testing"

	"github.com/dio/jisr/buffer"
	"github.com/stretchr/testify/assert"
)

func TestRing_BasicWrite(t *testing.T) {
	rb := buffer.NewRing(8)
	rb.Write([]byte("hello"))
	assert.Equal(t, "hello", string(rb.Bytes()))
}

func TestRing_ExactlyFull(t *testing.T) {
	rb := buffer.NewRing(5)
	rb.Write([]byte("hello"))
	assert.Equal(t, "hello", string(rb.Bytes()))
}

func TestRing_Overflow_KeepsLast(t *testing.T) {
	rb := buffer.NewRing(5)
	rb.Write([]byte("hello world")) // 11 bytes into 5-byte ring
	// Last 5 bytes of "hello world" = "world"
	assert.Equal(t, "world", string(rb.Bytes()))
}

func TestRing_MultipleChunks(t *testing.T) {
	rb := buffer.NewRing(10)
	rb.Write([]byte("hello "))
	rb.Write([]byte("world"))
	// "hello world" = 11 bytes, ring is 10 → last 10 = "ello world"
	assert.Equal(t, "ello world", string(rb.Bytes()))
}

func TestRing_MultipleChunks_Correct(t *testing.T) {
	rb := buffer.NewRing(10)
	rb.Write([]byte("abcdef"))   // 6 bytes
	rb.Write([]byte("ghijklmn")) // 8 more → total 14, ring 10 → last 10: "efghijklmn"
	result := string(rb.Bytes())
	assert.Equal(t, "efghijklmn", result)
}

func TestRing_Reset(t *testing.T) {
	rb := buffer.NewRing(8)
	rb.Write([]byte("hello"))
	rb.Reset()
	assert.Equal(t, "", string(rb.Bytes()))
	assert.Equal(t, 0, rb.Len())
}

func TestRing_Len(t *testing.T) {
	rb := buffer.NewRing(8)
	assert.Equal(t, 0, rb.Len())
	rb.Write([]byte("hi"))
	assert.Equal(t, 2, rb.Len())
	rb.Write([]byte("hello world")) // overflow
	assert.Equal(t, 8, rb.Len())
}

func TestHeadTail_CapturesBothEnds(t *testing.T) {
	ht := buffer.NewHeadTail(4, 4)

	// Write 12 bytes: head captures first 4, tail captures last 4
	ht.Write([]byte("abcdefghijkl"))

	assert.Equal(t, "abcd", string(ht.Head()))
	assert.Equal(t, "ijkl", string(ht.Tail()))
}

func TestHeadTail_ShortStream(t *testing.T) {
	ht := buffer.NewHeadTail(8, 8)
	ht.Write([]byte("hi"))

	assert.Equal(t, "hi", string(ht.Head()))
	assert.Equal(t, "hi", string(ht.Tail()))
}

func TestHeadTail_ChunkedWrites(t *testing.T) {
	ht := buffer.NewHeadTail(4, 4)

	ht.Write([]byte("ab"))
	ht.Write([]byte("cd"))
	ht.Write([]byte("ef"))
	ht.Write([]byte("gh"))

	// head: first 4 = "abcd"
	// tail (ring 4): last 4 = "efgh"
	assert.Equal(t, "abcd", string(ht.Head()))
	assert.Equal(t, "efgh", string(ht.Tail()))
}

func TestHeadTail_Reset(t *testing.T) {
	ht := buffer.NewHeadTail(8, 8)
	ht.Write([]byte("hello world"))
	ht.Reset()

	assert.Empty(t, string(ht.Head()))
	assert.Empty(t, string(ht.Tail()))
}

// SSE-like scenario: input tokens at start, output tokens at end
func TestHeadTail_SSEPattern(t *testing.T) {
	// Use generous head/tail sizes to ensure the tokens fit
	ht := buffer.NewHeadTail(128, 128)

	start := "event: message_start\ndata: {\"input_tokens\":42}\n\n"
	middle := make([]byte, 300) // larger than head+tail to verify separation
	for i := range middle {
		middle[i] = 'x'
	}
	end := "event: message_delta\ndata: {\"output_tokens\":88}\n\n"

	ht.Write([]byte(start))
	ht.Write(middle)
	ht.Write([]byte(end))

	head := string(ht.Head())
	tail := string(ht.Tail())

	assert.Contains(t, head, "message_start", "head should contain message_start")
	assert.Contains(t, head, "input_tokens", "head should contain input_tokens")
	assert.Contains(t, tail, "message_delta", "tail should contain message_delta")
	assert.Contains(t, tail, "output_tokens", "tail should contain output_tokens")
}
