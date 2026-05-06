// Package ssetap demonstrates how to tap an SSE response stream using
// jisr.RegisterRaw and jisr/buffer.HeadTail.
//
// This filter sits on the response path and extracts token usage from
// streaming LLM responses without buffering the entire body:
//
//   - Input tokens appear near the START of the stream (e.g. Anthropic
//     message_start, OpenAI first usage chunk)
//   - Output tokens appear near the END (message_delta, final usage chunk)
//
// The filter uses HeadTail to capture the first 8KB and last 64KB of each
// response, then scans those regions on stream completion. The middle of
// a large response is never stored.
//
// Because this needs OnResponseHeaders and OnResponseBody callbacks, it
// uses jisr.RegisterRaw — the HandlerFunc model only covers the request path.
// A companion jisr.Register filter (e.g. auth, routing) can share the same .so.
package ssetap

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/dio/jisr"
	"github.com/dio/jisr/buffer"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

const ExtensionName = "sse-tap"

// TokenUsage holds extracted token counts.
type TokenUsage struct {
	Input  uint32
	Output uint32
}

func init() {
	jisr.RegisterRaw(ExtensionName, &configFactory{})
}

// --- configFactory ---

type configFactory struct {
	shared.EmptyHttpFilterConfigFactory
}

func (f *configFactory) Create(
	handle shared.HttpFilterConfigHandle,
	_ []byte,
) (shared.HttpFilterFactory, error) {
	// Define Envoy metrics once per filter config.
	inputID, _ := handle.DefineCounter("sse_tap_input_tokens")
	outputID, _ := handle.DefineCounter("sse_tap_output_tokens")
	return &filterFactory{inputID: inputID, outputID: outputID}, nil
}

// --- filterFactory ---

type filterFactory struct {
	shared.EmptyHttpFilterFactory
	inputID  shared.MetricID
	outputID shared.MetricID
}

func (f *filterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &tapFilter{handle: handle, factory: f}
}

// --- tapFilter: per-request filter ---

type tapFilter struct {
	shared.EmptyHttpFilter

	handle  shared.HttpFilterHandle
	factory *filterFactory

	isSSE bool
	buf   *buffer.HeadTail
}

// OnResponseHeaders detects text/event-stream and decides whether to tap.
func (f *tapFilter) OnResponseHeaders(headers shared.HeaderMap, endStream bool) shared.HeadersStatus {
	if endStream {
		return shared.HeadersStatusContinue
	}
	ct := headers.GetOne("content-type").ToUnsafeString()
	if strings.Contains(ct, "text/event-stream") {
		f.isSSE = true
		// 8KB head captures message_start / early usage.
		// 64KB tail captures message_delta / final usage chunk.
		f.buf = buffer.NewHeadTail(8*1024, 64*1024)
	}
	// Always continue — never buffer the response, let it stream through.
	return shared.HeadersStatusContinue
}

// OnResponseBody feeds each chunk into the ring buffer without stopping the stream.
func (f *tapFilter) OnResponseBody(body shared.BodyBuffer, endStream bool) shared.BodyStatus {
	if !f.isSSE || f.buf == nil {
		return shared.BodyStatusContinue
	}

	// Feed chunks into head+tail ring. No copy into Go heap — ToUnsafeBytes is
	// safe here because we consume within the same callback before returning.
	for _, chunk := range body.GetChunks() {
		f.buf.Write(chunk.ToUnsafeBytes())
	}

	if endStream {
		f.finalize()
	}

	// Always continue — do not buffer the response body.
	return shared.BodyStatusContinue
}

// finalize scans head and tail for token usage and emits metrics.
func (f *tapFilter) finalize() {
	u := extractUsage(f.buf.Head(), f.buf.Tail())
	if u.Input > 0 {
		f.handle.IncrementCounterValue(f.factory.inputID, uint64(u.Input))
	}
	if u.Output > 0 {
		f.handle.IncrementCounterValue(f.factory.outputID, uint64(u.Output))
	}
	f.handle.SetMetadata("sse_tap", "input_tokens", u.Input)
	f.handle.SetMetadata("sse_tap", "output_tokens", u.Output)
	f.handle.Log(shared.LogLevelDebug,
		"sse-tap: input=%d output=%d", u.Input, u.Output)
}

// ExtractUsage scans head for input tokens and tail for output tokens.
// Handles both OpenAI and Anthropic SSE formats.
// Exported so it can be unit-tested independently of Envoy.
func ExtractUsage(head, tail []byte) TokenUsage {
	return extractUsage(head, tail)
}

// extractUsage is the internal implementation.
func extractUsage(head, tail []byte) TokenUsage {
	var u TokenUsage

	// Scan head: Anthropic message_start (input tokens appear first).
	var curEvent string
	scanLines(head, func(line []byte) {
		switch {
		case bytes.HasPrefix(line, []byte("event: ")):
			curEvent = string(line[7:])
		case u.Input == 0 && curEvent == "message_start" && bytes.HasPrefix(line, []byte("data: ")):
			var msg struct {
				Message struct {
					Usage struct {
						InputTokens uint32 `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line[6:], &msg) == nil && msg.Message.Usage.InputTokens > 0 {
				u.Input = msg.Message.Usage.InputTokens
			}
		}
	})

	// Scan tail: both formats for output + OpenAI input (if not found in head).
	curEvent = ""
	scanLines(tail, func(line []byte) {
		if !bytes.HasPrefix(line, []byte("data: ")) &&
			!bytes.HasPrefix(line, []byte("event: ")) {
			return
		}
		if bytes.HasPrefix(line, []byte("event: ")) {
			curEvent = string(line[7:])
			return
		}
		data := line[6:]
		if bytes.Equal(data, []byte("[DONE]")) {
			return
		}

		// OpenAI chat/responses: usage in data chunk.
		if u.Input == 0 || u.Output == 0 {
			var chunk struct {
				Usage *struct {
					PromptTokens     uint32 `json:"prompt_tokens"`
					CompletionTokens uint32 `json:"completion_tokens"`
					InputTokens      uint32 `json:"input_tokens"`
					OutputTokens     uint32 `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &chunk) == nil && chunk.Usage != nil {
				if u.Input == 0 {
					u.Input = chunk.Usage.PromptTokens + chunk.Usage.InputTokens
				}
				if u.Output == 0 {
					u.Output = chunk.Usage.CompletionTokens + chunk.Usage.OutputTokens
				}
			}
		}

		// Anthropic message_delta: output tokens.
		if curEvent == "message_delta" && u.Output == 0 {
			var delta struct {
				Usage struct {
					OutputTokens uint32 `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &delta) == nil {
				u.Output = delta.Usage.OutputTokens
			}
		}
	})

	return u
}

// scanLines calls fn for each complete SSE line. No allocation.
func scanLines(data []byte, fn func([]byte)) {
	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			return
		}
		fn(bytes.TrimRight(data[:idx], "\r"))
		data = data[idx+1:]
	}
}
