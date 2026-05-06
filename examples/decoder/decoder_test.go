package decoder

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveCluster(t *testing.T) {
	cases := []struct {
		model   string
		cluster string
	}{
		{"gpt-4o", ClusterOpenAI},
		{"gpt-3.5-turbo", ClusterOpenAI},
		{"o1-preview", ClusterOpenAI},
		{"o3-mini", ClusterOpenAI},
		{"claude-3-5-sonnet-20241022", ClusterAnthropic},
		{"claude-haiku-3-5", ClusterAnthropic},
		{"unknown-model", ClusterDefault},
		{"", ClusterDefault},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.cluster, resolveCluster(tc.model), "model=%q", tc.model)
	}
}

func TestExtractSSEUsage_OpenAI(t *testing.T) {
	// OpenAI final usage chunk format.
	tail := []byte(`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":42}}` + "\n")
	u := extractSSEUsage(tail)
	assert.Equal(t, uint32(10), u.input)
	assert.Equal(t, uint32(42), u.output)
}

func TestExtractSSEUsage_Anthropic(t *testing.T) {
	// Anthropic message_delta format (output tokens in tail).
	tail := []byte("event: message_delta\ndata: {\"usage\":{\"output_tokens\":77}}\n")
	// Our simple extractor looks for "completion_tokens" / "output_tokens" in data lines.
	// Anthropic message_delta has output_tokens directly.
	u := extractSSEUsage(tail)
	assert.Equal(t, uint32(77), u.output)
}

func TestExtractSSEUsage_DONE(t *testing.T) {
	// [DONE] sentinel must be skipped without panic.
	tail := []byte("data: [DONE]\n")
	u := extractSSEUsage(tail)
	assert.Equal(t, uint32(0), u.input)
	assert.Equal(t, uint32(0), u.output)
}

func TestExtractSSEUsage_Empty(t *testing.T) {
	u := extractSSEUsage(nil)
	assert.Equal(t, uint32(0), u.input)
	assert.Equal(t, uint32(0), u.output)
}

func TestExtractSSEUsage_MalformedJSON(t *testing.T) {
	// Malformed JSON must not panic.
	tail := []byte("data: {not valid json}\n")
	u := extractSSEUsage(tail)
	assert.Equal(t, uint32(0), u.input)
	assert.Equal(t, uint32(0), u.output)
}
