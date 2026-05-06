// Package ssetap demonstrates how to tap an SSE response stream using
// jisr.RegisterWithConfig and jisr.RegisterWithResponse(ResponseModeObserve).
//
// This filter sits on the response path and extracts token usage from
// streaming LLM responses without buffering the entire body:
//
//   - Input tokens appear near the START of the stream (e.g. Anthropic
//     message_start, OpenAI first usage chunk)
//   - Output tokens appear near the END (message_delta, final usage chunk)
//
// The filter uses jisr/buffer.HeadTail to capture the first 8KB and last 64KB
// of each response, then scans those regions on stream completion. The middle
// of a large response is never stored.
//
// ResponseModeObserve delivers body chunks to the handler goroutine while
// simultaneously forwarding them to the downstream client, so there is zero
// added latency. This replaces the previous RegisterRaw approach.
package ssetap

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/dio/jisr"
	"github.com/dio/jisr/buffer"
)

const ExtensionName = "sse-tap"

// TokenUsage holds extracted token counts.
type TokenUsage struct {
	Input  uint32
	Output uint32
}

// Metrics defined once at config time.
var (
	inputTokensID  jisr.MetricID
	outputTokensID jisr.MetricID
)

func init() {
	jisr.RegisterWithConfigAndResponse(ExtensionName,
		func(h jisr.ConfigHandle) error {
			var err error
			inputTokensID, err = h.DefineCounter("sse_tap_input_tokens")
			if err != nil {
				return err
			}
			outputTokensID, err = h.DefineCounter("sse_tap_output_tokens")
			return err
		},
		func(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
			r.SkipBody()
			w.SetRequestHeader("x-sse-tap", "1")
		},
		tapResponseHandler,
		jisr.ResponseModeObserve,
	)
}

// tapResponseHandler taps the SSE body using a HeadTail ring buffer.
// Runs in the handler goroutine while the body streams to the client.
func tapResponseHandler(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
	ct := r.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		// Non-SSE response: drain and ignore.
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		return
	}

	// 8KB head captures message_start / early usage.
	// 64KB tail captures message_delta / final usage chunk.
	buf := buffer.NewHeadTail(8*1024, 64*1024)
	chunk := make([]byte, 4096)
	for {
		n, err := r.Body.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err != nil {
			break
		}
	}

	u := ExtractUsage(buf.Head(), buf.Tail())
	if u.Input > 0 {
		w.IncrementCounter(inputTokensID, uint64(u.Input))
	}
	if u.Output > 0 {
		w.IncrementCounter(outputTokensID, uint64(u.Output))
	}
	w.SetMetadata("sse_tap", "input_tokens", u.Input)
	w.SetMetadata("sse_tap", "output_tokens", u.Output)
}

// ExtractUsage scans head for input tokens and tail for output tokens.
// Handles both OpenAI and Anthropic SSE formats.
// Exported for unit testing independently of Envoy.
func ExtractUsage(head, tail []byte) TokenUsage {
	return extractUsage(head, tail)
}

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
