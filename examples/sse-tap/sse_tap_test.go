package ssetap_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dio/jisr/buffer"
	ssetap "github.com/dio/jisr/examples/sse-tap"
	"github.com/stretchr/testify/assert"
)

// feedStream is a test helper that feeds an SSE response through a HeadTail
// buffer and returns the extracted usage — without any Envoy involvement.
func feedStream(chunks []string) ssetap.TokenUsage {
	ht := buffer.NewHeadTail(8*1024, 64*1024)
	for _, c := range chunks {
		ht.Write([]byte(c))
	}
	return ssetap.ExtractUsage(ht.Head(), ht.Tail())
}

func TestExtractUsage_OpenAI_Chat_NonStreaming(t *testing.T) {
	// OpenAI sends usage in the last data chunk before [DONE].
	sse := strings.Join([]string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n",
		"data: [DONE]\n\n",
	}, "")

	u := feedStream([]string{sse})
	assert.Equal(t, uint32(10), u.Input)
	assert.Equal(t, uint32(5), u.Output)
}

func TestExtractUsage_OpenAI_Responses_API(t *testing.T) {
	// Responses API uses input_tokens / output_tokens.
	sse := strings.Join([]string{
		"event: response.created\ndata: {}\n\n",
		"event: response.completed\ndata: {\"usage\":{\"input_tokens\":20,\"output_tokens\":8}}\n\n",
	}, "")

	u := feedStream([]string{sse})
	assert.Equal(t, uint32(20), u.Input)
	assert.Equal(t, uint32(8), u.Output)
}

func TestExtractUsage_Anthropic_Messages(t *testing.T) {
	// message_start (input) is first; message_delta (output) is near the end.
	sse := strings.Join([]string{
		"event: message_start\n",
		"data: {\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n",
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\"Hi\"}}\n\n",
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\" there\"}}\n\n",
		"event: message_delta\n",
		"data: {\"usage\":{\"output_tokens\":15}}\n\n",
		"event: message_stop\ndata: {}\n\n",
	}, "")

	u := feedStream([]string{sse})
	assert.Equal(t, uint32(42), u.Input)
	assert.Equal(t, uint32(15), u.Output)
}

func TestExtractUsage_LargeStream_OnlyHeadAndTailScanned(t *testing.T) {
	// Simulate a very large stream — middle is all filler.
	// Only head (first 8KB) and tail (last 64KB) should be examined.
	head := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":99}}}\n\n"

	// 200KB of filler in the middle — never stored in the ring.
	middle := strings.Repeat("data: {\"delta\":{\"text\":\"x\"}}\n\n", 5000)

	tail := "event: message_delta\ndata: {\"usage\":{\"output_tokens\":77}}\n\n"

	// Feed in chunks as Envoy would deliver them.
	ht := buffer.NewHeadTail(8*1024, 64*1024)
	ht.Write([]byte(head))
	ht.Write([]byte(middle))
	ht.Write([]byte(tail))

	u := ssetap.ExtractUsage(ht.Head(), ht.Tail())
	assert.Equal(t, uint32(99), u.Input)
	assert.Equal(t, uint32(77), u.Output)
}

func TestExtractUsage_NonSSE_ReturnsZero(t *testing.T) {
	// Plain JSON response — not SSE, no event prefix.
	body := `{"choices":[{"message":{"content":"Hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`
	u := feedStream([]string{body})
	// No "data: " prefix → zero (correct — this filter only handles SSE).
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

func TestExtractUsage_ChunkedDelivery(t *testing.T) {
	// Envoy may deliver SSE in small chunks — verify ring handles split lines.
	full := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":7}}}\n\nevent: message_delta\ndata: {\"usage\":{\"output_tokens\":3}}\n\n"

	// Split into 10-byte chunks.
	var chunks []string
	for i := 0; i < len(full); i += 10 {
		end := i + 10
		if end > len(full) {
			end = len(full)
		}
		chunks = append(chunks, full[i:end])
	}

	u := feedStream(chunks)
	assert.Equal(t, uint32(7), u.Input)
	assert.Equal(t, uint32(3), u.Output)
}

func TestExtractUsage_EmptyStream(t *testing.T) {
	u := feedStream([]string{})
	assert.Equal(t, uint32(0), u.Input)
	assert.Equal(t, uint32(0), u.Output)
}

// Benchmark: extractUsage on a realistic 64KB tail buffer.
func BenchmarkExtractUsage(b *testing.B) {
	tail := []byte(strings.Repeat("data: {\"choices\":[{\"delta\":{\"text\":\"x\"}}]}\n\n", 1000) +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50}}\n\n" +
		"data: [DONE]\n\n")
	head := []byte("data: {}\n\n")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ssetap.ExtractUsage(head, tail)
	}
}

func ExampleExtractUsage() {
	ht := buffer.NewHeadTail(8*1024, 64*1024)
	ht.Write([]byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n"))
	ht.Write([]byte("event: message_delta\ndata: {\"usage\":{\"output_tokens\":5}}\n\n"))

	u := ssetap.ExtractUsage(ht.Head(), ht.Tail())
	fmt.Printf("input=%d output=%d\n", u.Input, u.Output)
	// Output: input=10 output=5
}
